/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controllers contains controllers for CAPV objects.
package controllers

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	pkgerrors "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	clusterutilv1 "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta2conditions "sigs.k8s.io/cluster-api/util/conditions/v1beta2"
	"sigs.k8s.io/cluster-api/util/finalizers"
	kcfg "sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/paused"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/feature"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/identity"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
	infrautilv1 "sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

const clusterRequeueAfter = 30 * time.Second

// requeueAfterSuccessfulReconcile keeps explicit waits, uses the controller-wide
// polling interval for a successful zero result, and leaves errors and paused
// reconciles to the controller-runtime event flow.
func requeueAfterSuccessfulReconcile(result reconcile.Result, err error, paused bool) reconcile.Result {
	if err != nil || paused {
		return reconcile.Result{}
	}
	if result.IsZero() {
		return reconcile.Result{RequeueAfter: clusterRequeueAfter}
	}
	return result
}

var (
	modulePluginGVK = schema.GroupVersionKind{
		Group:   "cluster.alauda.io",
		Version: "v1alpha1",
		Kind:    "ModulePlugin",
	}
	appReleaseGVR = schema.GroupVersionResource{
		Group:    "operator.alauda.io",
		Version:  "v1alpha1",
		Resource: "appreleases",
	}
)

type clusterReconciler struct {
	ControllerManagerContext *capvcontext.ControllerManagerContext
	Client                   client.Client
	Recorder                 record.EventRecorder

	vmService               services.VimMachineService
	clusterModuleReconciler Reconciler
}

// Reconcile ensures the back-end state reflects the Kubernetes resource state intent.
func (r *clusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	// Get the VSphereCluster resource for this request.
	vsphereCluster := &infrav1.VSphereCluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, vsphereCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Add finalizer first if not set to avoid the race condition between init and delete.
	if finalizerAdded, err := finalizers.EnsureFinalizer(ctx, r.Client, vsphereCluster, infrav1.ClusterFinalizer); err != nil || finalizerAdded {
		return ctrl.Result{}, err
	}

	// Fetch the CAPI Cluster.
	cluster, err := clusterutilv1.GetOwnerCluster(ctx, r.Client, vsphereCluster.ObjectMeta)
	if err != nil {
		return reconcile.Result{}, err
	}
	if cluster != nil {
		log = log.WithValues("Cluster", klog.KObj(cluster))
		ctx = ctrl.LoggerInto(ctx, log)
	}

	// Create the patch helper.
	patchHelper, err := patch.NewHelper(vsphereCluster, r.Client)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Create the cluster context for this request.
	clusterContext := &capvcontext.ClusterContext{
		Cluster:        cluster,
		VSphereCluster: vsphereCluster,
		PatchHelper:    patchHelper,
	}

	pausedReconcile := false

	// Always issue a patch when exiting this function so changes to the
	// resource are patched back to the API server.
	defer func() {
		if err := r.patch(ctx, clusterContext); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
		result = requeueAfterSuccessfulReconcile(result, reterr, pausedReconcile)
	}()

	// Handle deleted clusters
	if !vsphereCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, clusterContext)
	}

	if isPaused, requeue, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, vsphereCluster); err != nil || isPaused || requeue {
		pausedReconcile = isPaused
		return ctrl.Result{}, err
	}

	if cluster == nil {
		log.Info("Waiting for Cluster controller to set OwnerRef on VSphereCluster")
		return reconcile.Result{}, nil
	}

	// Handle non-deleted clusters
	return r.reconcileNormal(ctx, clusterContext)
}

// patch updates the VSphereCluster and its status on the API server.
func (r *clusterReconciler) patch(ctx context.Context, clusterCtx *capvcontext.ClusterContext) error {
	// always update the readyCondition.
	conditions.SetSummary(clusterCtx.VSphereCluster,
		conditions.WithConditions(
			infrav1.VCenterAvailableCondition,
		),
	)

	if err := v1beta2conditions.SetSummaryCondition(clusterCtx.VSphereCluster, clusterCtx.VSphereCluster, infrav1.VSphereClusterReadyV1Beta2Condition,
		v1beta2conditions.ForConditionTypes{
			infrav1.VSphereClusterVCenterAvailableV1Beta2Condition,
			// FailureDomainsReady and ClusterModuelsReady may not be always set.
			infrav1.VSphereClusterFailureDomainsReadyV1Beta2Condition,
			infrav1.VSphereClusterClusterModulesReadyV1Beta2Condition,
		},
		v1beta2conditions.IgnoreTypesIfMissing{
			infrav1.VSphereClusterFailureDomainsReadyV1Beta2Condition,
			infrav1.VSphereClusterClusterModulesReadyV1Beta2Condition,
		},
		// Using a custom merge strategy to override reasons applied during merge.
		v1beta2conditions.CustomMergeStrategy{
			MergeStrategy: v1beta2conditions.DefaultMergeStrategy(
				// Use custom reasons.
				v1beta2conditions.ComputeReasonFunc(v1beta2conditions.GetDefaultComputeMergeReasonFunc(
					infrav1.VSphereClusterNotReadyV1Beta2Reason,
					infrav1.VSphereClusterReadyUnknownV1Beta2Reason,
					infrav1.VSphereClusterReadyV1Beta2Reason,
				)),
			),
		},
	); err != nil {
		return pkgerrors.Wrapf(err, "failed to set %s condition", infrav1.VSphereClusterReadyV1Beta2Condition)
	}

	return clusterCtx.PatchHelper.Patch(ctx, clusterCtx.VSphereCluster,
		patch.WithOwnedV1Beta2Conditions{Conditions: []string{
			clusterv1.PausedV1Beta2Condition,
			infrav1.VSphereClusterReadyV1Beta2Condition,
			infrav1.VSphereClusterFailureDomainsReadyV1Beta2Condition,
			infrav1.VSphereClusterVCenterAvailableV1Beta2Condition,
			infrav1.VSphereClusterClusterModulesReadyV1Beta2Condition,
			infrav1.VSphereClusterKubeOvnAppReleaseReadyV1Beta2Condition,
			infrav1.VSphereClusterSelfBuiltLoadBalancerReadyV1Beta2Condition,
		}},
	)
}

func (r *clusterReconciler) reconcileDelete(ctx context.Context, clusterCtx *capvcontext.ClusterContext) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
		Type:   infrav1.VSphereClusterVCenterAvailableV1Beta2Condition,
		Status: metav1.ConditionFalse,
		Reason: infrav1.VSphereClusterVCenterAvailableDeletingV1Beta2Reason,
	})
	v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
		Type:   infrav1.VSphereClusterClusterModulesReadyV1Beta2Condition,
		Status: metav1.ConditionFalse,
		Reason: infrav1.VSphereClusterClusterModulesDeletingV1Beta2Reason,
	})
	v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
		Type:   infrav1.VSphereClusterFailureDomainsReadyV1Beta2Condition,
		Status: metav1.ConditionFalse,
		Reason: infrav1.VSphereClusterFailureDomainsDeletingV1Beta2Reason,
	})

	var vsphereMachines []client.Object
	var err error
	if clusterCtx.Cluster != nil {
		vsphereMachines, err = r.vmService.GetMachinesInCluster(ctx, clusterCtx.Cluster.Namespace, clusterCtx.Cluster.Name)
		if err != nil {
			return reconcile.Result{}, pkgerrors.Wrapf(err,
				"unable to list VSphereMachines part of VSphereCluster %s/%s", clusterCtx.VSphereCluster.Namespace, clusterCtx.VSphereCluster.Name)
		}
	}

	if len(vsphereMachines) > 0 {
		log.Info("Waiting for VSphereMachines to be deleted", "count", len(vsphereMachines))
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// The cluster module info needs to be reconciled before the secret deletion
	// since it needs access to the vCenter instance to be able to perform LCM operations
	// on the cluster modules.
	affinityReconcileResult, err := r.reconcileClusterModules(ctx, clusterCtx)
	if err != nil {
		return affinityReconcileResult, err
	}

	// Remove finalizer on Identity Secret
	if identity.IsSecretIdentity(clusterCtx.VSphereCluster) {
		secret := &corev1.Secret{}
		secretKey := client.ObjectKey{
			Namespace: clusterCtx.VSphereCluster.Namespace,
			Name:      clusterCtx.VSphereCluster.Spec.IdentityRef.Name,
		}
		if err := r.Client.Get(ctx, secretKey, secret); err != nil {
			if apierrors.IsNotFound(err) {
				ctrlutil.RemoveFinalizer(clusterCtx.VSphereCluster, infrav1.ClusterFinalizer)
				return reconcile.Result{}, nil
			}
			return reconcile.Result{}, err
		}

		if ctrlutil.RemoveFinalizer(secret, infrav1.SecretIdentitySetFinalizer) {
			log.Info(fmt.Sprintf("Removing finalizer %s", infrav1.SecretIdentitySetFinalizer), "Secret", klog.KObj(secret))
			if err := r.Client.Update(ctx, secret); err != nil {
				return reconcile.Result{}, pkgerrors.Wrapf(err, "failed to update Secret %s", klog.KObj(secret))
			}
		}

		if secret.DeletionTimestamp.IsZero() {
			log.Info("Deleting Secret", "Secret", klog.KObj(secret))
			if err := r.Client.Delete(ctx, secret); err != nil {
				return reconcile.Result{}, pkgerrors.Wrapf(err, "failed to delete Secret %s", klog.KObj(secret))
			}
		}
	}

	// Cluster is deleted so remove the finalizer.
	ctrlutil.RemoveFinalizer(clusterCtx.VSphereCluster, infrav1.ClusterFinalizer)

	return reconcile.Result{}, nil
}

func (r *clusterReconciler) reconcileNormal(ctx context.Context, clusterCtx *capvcontext.ClusterContext) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// The control plane endpoint has to be settled before anything else: every
	// downstream consumer, from the bootstrap data to the workload client, reads it.
	if err := reconcileControlPlaneEndpoint(clusterCtx); err != nil {
		return reconcile.Result{}, err
	}

	// Configure CoreDNS before the infrastructure cluster becomes Ready so
	// kubeadm uses the selected repository during initial bootstrap.
	if err := r.reconcileKubeadmControlPlaneSystemComponents(ctx, clusterCtx); err != nil {
		return reconcile.Result{}, err
	}

	// Reconcile failure domains.
	ok, err := r.reconcileDeploymentZones(ctx, clusterCtx)
	if err != nil {
		v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
			Type:    infrav1.VSphereClusterFailureDomainsReadyV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.VSphereClusterFailureDomainsNotReadyV1Beta2Reason,
			Message: err.Error(),
		})
		return reconcile.Result{}, err
	}
	if !ok {
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Reconcile vCenter availability.
	if err := r.reconcileIdentitySecret(ctx, clusterCtx); err != nil {
		conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.VCenterAvailableCondition, infrav1.VCenterUnreachableReason, clusterv1.ConditionSeverityError, "%s", err.Error())
		v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
			Type:    infrav1.VSphereClusterVCenterAvailableV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.VSphereClusterVCenterUnreachableV1Beta2Reason,
			Message: err.Error(),
		})
		return reconcile.Result{}, err
	}

	vcenterSession, err := r.reconcileVCenterConnectivity(ctx, clusterCtx)
	if err != nil {
		conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.VCenterAvailableCondition, infrav1.VCenterUnreachableReason, clusterv1.ConditionSeverityError, "%s", err.Error())
		v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
			Type:    infrav1.VSphereClusterVCenterAvailableV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.VSphereClusterVCenterUnreachableV1Beta2Reason,
			Message: err.Error(),
		})
		return reconcile.Result{}, pkgerrors.Wrapf(err,
			"unexpected error while probing vcenter for %s", clusterCtx)
	}
	conditions.MarkTrue(clusterCtx.VSphereCluster, infrav1.VCenterAvailableCondition)
	v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
		Type:   infrav1.VSphereClusterVCenterAvailableV1Beta2Condition,
		Status: metav1.ConditionTrue,
		Reason: infrav1.VSphereClusterVCenterAvailableV1Beta2Reason,
	})

	// Reconcile cluster modules.
	if err = r.reconcileVCenterVersion(clusterCtx, vcenterSession); err != nil {
		message := err.Error()
		conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.ClusterModulesAvailableCondition, infrav1.MissingVCenterVersionReason, clusterv1.ConditionSeverityWarning, "%s", message)
		v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
			Type:    infrav1.VSphereClusterClusterModulesReadyV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.VSphereClusterModulesInvalidVCenterVersionV1Beta2Reason,
			Message: message,
		})
		log.Error(err, "could not reconcile vCenter version")
		return reconcile.Result{}, err
	}
	if clusterCtx.VSphereCluster.Status.VCenterVersion == "" {
		err = errors.New("vCenter version is missing")
		conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.ClusterModulesAvailableCondition, infrav1.MissingVCenterVersionReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
		v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
			Type:    infrav1.VSphereClusterClusterModulesReadyV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.VSphereClusterModulesInvalidVCenterVersionV1Beta2Reason,
			Message: err.Error(),
		})
		log.Error(err, "could not reconcile vCenter version")
		return reconcile.Result{}, err
	}

	affinityReconcileResult, err := r.reconcileClusterModules(ctx, clusterCtx)
	if err != nil {
		conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.ClusterModulesAvailableCondition, infrav1.ClusterModuleSetupFailedReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
		v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
			Type:    infrav1.VSphereClusterClusterModulesReadyV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.VSphereClusterClusterModulesNotReadyV1Beta2Reason,
			Message: err.Error(),
		})
		return affinityReconcileResult, err
	}

	clusterCtx.VSphereCluster.Status.Ready = true

	result, err := r.reconcileKubeOvnAppRelease(ctx, clusterCtx)
	if err != nil || !result.IsZero() {
		return result, err
	}

	// alive pods need a working CNI to be scheduled, so the self-built load
	// balancer comes after the CNI AppRelease.
	result, err = r.ensureSelfBuiltLB(ctx, clusterCtx)
	if err != nil || !result.IsZero() {
		return result, err
	}

	return r.reconcileWorkloadSystemComponentRepositories(ctx, clusterCtx)
}

func (r *clusterReconciler) reconcileKubeOvnAppRelease(ctx context.Context, clusterCtx *capvcontext.ClusterContext) (reconcile.Result, error) {
	cluster := clusterCtx.Cluster
	if cluster == nil || cluster.Annotations["cpaas.io/network-type"] != "kube-ovn" {
		conditions.Delete(clusterCtx.VSphereCluster, infrav1.KubeOvnAppReleaseReadyCondition)
		v1beta2conditions.Delete(clusterCtx.VSphereCluster, infrav1.VSphereClusterKubeOvnAppReleaseReadyV1Beta2Condition)
		return reconcile.Result{}, nil
	}
	var err error

	targetVersion := cluster.Annotations["cpaas.io/kube-ovn-version"]
	joinCIDR := cluster.Annotations["cpaas.io/kube-ovn-join-cidr"]
	registry := cluster.Annotations[registryAddressAnnotation]
	if registry == "" {
		msg := "cpaas.io/registry-address annotation is required to deploy kube-ovn"
		r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseInvalidConfigurationReason, clusterv1.ConditionSeverityError, msg)
		return reconcile.Result{}, errors.New(msg)
	}

	if targetVersion == "" {
		modulePlugin := &unstructured.Unstructured{}
		modulePlugin.SetAPIVersion(modulePluginGVK.GroupVersion().String())
		modulePlugin.SetKind(modulePluginGVK.Kind)
		if err := r.Client.Get(ctx, client.ObjectKey{Name: "kube-ovn"}, modulePlugin); err != nil {
			msg := "failed to get kube-ovn ModulePlugin"
			r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseInvalidConfigurationReason, clusterv1.ConditionSeverityError, msg)
			return reconcile.Result{}, pkgerrors.Wrap(err, msg)
		}
		var found bool
		targetVersion, found, err = unstructured.NestedString(modulePlugin.Object, "status", "latestVersion")
		if err != nil {
			msg := "failed to read kube-ovn ModulePlugin status.latestVersion"
			r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseInvalidConfigurationReason, clusterv1.ConditionSeverityError, msg)
			return reconcile.Result{}, pkgerrors.Wrap(err, msg)
		}
		if !found || targetVersion == "" {
			msg := "kube-ovn module plugin latestVersion is empty"
			r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseInvalidConfigurationReason, clusterv1.ConditionSeverityError, msg)
			return reconcile.Result{}, errors.New(msg)
		}
	}
	chartName, err := kubeOvnChartNameForVersion(targetVersion)
	if err != nil {
		return reconcile.Result{}, err
	}

	podCIDR := firstCIDRBlock(cluster.Spec.ClusterNetwork, true)
	serviceCIDR := firstCIDRBlock(cluster.Spec.ClusterNetwork, false)
	if podCIDR == "" || serviceCIDR == "" {
		msg := "cluster network pod/service CIDR must be set before deploying kube-ovn"
		r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseInvalidConfigurationReason, clusterv1.ConditionSeverityError, msg)
		return reconcile.Result{}, errors.New(msg)
	}

	clientset, restConfig, err := r.newRemoteClients(ctx, cluster)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Skipping kube-ovn AppRelease reconcile because workload cluster client is unavailable")
		r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionUnknown, infrav1.KubeOvnAppReleaseReconcilingReason, clusterv1.ConditionSeverityInfo, err.Error())
		return reconcile.Result{}, err
	}

	controlPlaneNodes, ready, err := r.controlPlaneNodesRegistered(ctx, cluster, clientset)
	if err != nil {
		r.setReadinessUnknown(clusterCtx.VSphereCluster, kubeOvnAppReleaseConditionSpec, err)
		return reconcile.Result{}, err
	}
	if !ready {
		r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseReconcilingReason, clusterv1.ConditionSeverityInfo, "waiting for all control plane Nodes before reconciling kube-ovn AppRelease")
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	chartPullSecret, imagePullSecrets, err := sentryPullSecrets(ctx, clientset)
	if err != nil && !apierrors.IsNotFound(err) {
		r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseNotReadyReason, clusterv1.ConditionSeverityWarning, err.Error())
		return reconcile.Result{}, err
	}

	appRelease := buildKubeOvnAppRelease(cluster, registry, chartName, targetVersion, podCIDR, serviceCIDR, joinCIDR, chartPullSecret, imagePullSecrets, controlPlaneNodes)
	dc, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		msg := "failed to create dynamic client for workload cluster"
		r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionUnknown, infrav1.KubeOvnAppReleaseReconcilingReason, clusterv1.ConditionSeverityInfo, msg)
		return reconcile.Result{}, pkgerrors.Wrap(err, msg)
	}

	current, err := dc.Resource(appReleaseGVR).Namespace(appRelease.GetNamespace()).Get(ctx, appRelease.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = dc.Resource(appReleaseGVR).Namespace(appRelease.GetNamespace()).Create(ctx, appRelease, metav1.CreateOptions{})
		if err != nil {
			r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseNotReadyReason, clusterv1.ConditionSeverityWarning, err.Error())
			return reconcile.Result{}, err
		}
		r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseReconcilingReason, clusterv1.ConditionSeverityInfo, "created kube-ovn AppRelease, waiting for it to sync and become healthy")
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if err != nil {
		msg := "failed to get existing kube-ovn AppRelease"
		r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionUnknown, infrav1.KubeOvnAppReleaseReconcilingReason, clusterv1.ConditionSeverityInfo, msg)
		return reconcile.Result{}, pkgerrors.Wrap(err, msg)
	}

	if !reflect.DeepEqual(current.Object["spec"], appRelease.Object["spec"]) {
		current.Object["spec"] = appRelease.Object["spec"]
		_, err = dc.Resource(appReleaseGVR).Namespace(appRelease.GetNamespace()).Update(ctx, current, metav1.UpdateOptions{})
		if err != nil {
			r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseNotReadyReason, clusterv1.ConditionSeverityWarning, err.Error())
			return reconcile.Result{}, pkgerrors.Wrap(err, "failed to update kube-ovn AppRelease")
		}
		r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseReconcilingReason, clusterv1.ConditionSeverityInfo, "updated kube-ovn AppRelease, waiting for it to sync and become healthy")
		return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
	}

	readiness := kubeOvnAppReleaseReadiness(current)
	if readiness.err != nil {
		r.setReadinessUnknown(clusterCtx.VSphereCluster, kubeOvnAppReleaseConditionSpec, readiness.err)
		return reconcile.Result{}, readiness.err
	}
	if readiness.ready {
		r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionTrue, infrav1.KubeOvnAppReleaseReadyReason, clusterv1.ConditionSeverityInfo, "")
		return reconcile.Result{}, nil
	}
	r.setKubeOvnAppReleaseCondition(clusterCtx.VSphereCluster, corev1.ConditionFalse, readiness.reason, clusterv1.ConditionSeverityInfo, readiness.message)
	return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
}

type appReleaseStatus struct {
	ready   bool
	reason  string
	message string
	err     error
}

// appReleaseReasons names the condition reasons a caller wants an AppRelease
// readiness verdict expressed in, so the shared readiness logic can be used by
// features that own different condition types.
type appReleaseReasons struct {
	ready       string
	reconciling string
	notReady    string
}

func (r *clusterReconciler) setKubeOvnAppReleaseCondition(vsphereCluster *infrav1.VSphereCluster, status corev1.ConditionStatus, reason string, severity clusterv1.ConditionSeverity, message string) {
	r.setReadinessCondition(vsphereCluster, kubeOvnAppReleaseConditionSpec, status, reason, severity, message)
}

var kubeOvnAppReleaseConditionSpec = readinessConditionSpec{
	v1beta1Type:       infrav1.KubeOvnAppReleaseReadyCondition,
	v1beta2Type:       infrav1.VSphereClusterKubeOvnAppReleaseReadyV1Beta2Condition,
	subject:           "kube-ovn AppRelease",
	reconcilingReason: infrav1.KubeOvnAppReleaseReconcilingReason,
}

type readinessConditionSpec struct {
	v1beta1Type       clusterv1.ConditionType
	v1beta2Type       string
	subject           string
	reconcilingReason string
}

func (r *clusterReconciler) setReadinessCondition(vsphereCluster *infrav1.VSphereCluster, spec readinessConditionSpec, status corev1.ConditionStatus, reason string, severity clusterv1.ConditionSeverity, message string) {
	r.setDualCondition(vsphereCluster, spec.v1beta1Type, spec.v1beta2Type, spec.subject, status, reason, severity, message)
}

func (r *clusterReconciler) setReadinessUnknown(vsphereCluster *infrav1.VSphereCluster, spec readinessConditionSpec, err error) {
	r.setReadinessCondition(vsphereCluster, spec, corev1.ConditionUnknown, spec.reconcilingReason, clusterv1.ConditionSeverityInfo, err.Error())
}

// setDualCondition writes the same verdict to both the v1beta1 and the v1beta2
// form of a condition, and emits an event whenever the verdict actually changed.
// subject names the thing the condition is about, for the event emitted when the
// caller has no message of its own.
func (r *clusterReconciler) setDualCondition(
	vsphereCluster *infrav1.VSphereCluster,
	v1beta1Type clusterv1.ConditionType,
	v1beta2Type string,
	subject string,
	status corev1.ConditionStatus,
	reason string,
	severity clusterv1.ConditionSeverity,
	message string,
) {
	oldCondition := v1beta2conditions.Get(vsphereCluster, v1beta2Type)
	var oldStatus metav1.ConditionStatus
	var oldReason, oldMessage string
	if oldCondition != nil {
		oldStatus = oldCondition.Status
		oldReason = oldCondition.Reason
		oldMessage = oldCondition.Message
	}

	switch status {
	case corev1.ConditionTrue:
		conditions.MarkTrue(vsphereCluster, v1beta1Type)
	case corev1.ConditionUnknown:
		conditions.MarkUnknown(vsphereCluster, v1beta1Type, reason, "%s", message)
	default:
		conditions.MarkFalse(vsphereCluster, v1beta1Type, reason, severity, "%s", message)
	}

	newCondition := metav1.Condition{
		Type:    v1beta2Type,
		Status:  metav1.ConditionStatus(status),
		Reason:  reason,
		Message: message,
	}
	v1beta2conditions.Set(vsphereCluster, newCondition)

	if r.Recorder == nil || (oldCondition != nil && oldStatus == newCondition.Status && oldReason == newCondition.Reason && oldMessage == newCondition.Message) {
		return
	}
	eventType := corev1.EventTypeNormal
	if status == corev1.ConditionFalse && severity != clusterv1.ConditionSeverityInfo {
		eventType = corev1.EventTypeWarning
	}
	if message == "" {
		message = fmt.Sprintf("%s condition is %s", subject, reason)
	}
	r.Recorder.Event(vsphereCluster, eventType, reason, message)
}

func kubeOvnAppReleaseReadiness(appRelease *unstructured.Unstructured) appReleaseStatus {
	return appReleaseReadiness(appRelease, "kube-ovn", appReleaseReasons{
		ready:       infrav1.KubeOvnAppReleaseReadyReason,
		reconciling: infrav1.KubeOvnAppReleaseReconcilingReason,
		notReady:    infrav1.KubeOvnAppReleaseNotReadyReason,
	})
}

// appReleaseReadiness reports whether an AppRelease is both synced and healthy
// for the generation currently on the object. releaseLabel only names the
// release in the messages surfaced to the user.
func appReleaseReadiness(appRelease *unstructured.Unstructured, releaseLabel string, reasons appReleaseReasons) appReleaseStatus {
	syncCondition, found, err := appReleaseCondition(appRelease, "Sync")
	if err != nil {
		return appReleaseStatus{err: pkgerrors.Wrapf(err, "failed to read %s AppRelease Sync condition", releaseLabel)}
	}
	if !found {
		return appReleaseStatus{reason: reasons.reconciling, message: fmt.Sprintf("waiting for %s AppRelease Sync condition", releaseLabel)}
	}
	healthCondition, found, err := appReleaseCondition(appRelease, "Health")
	if err != nil {
		return appReleaseStatus{err: pkgerrors.Wrapf(err, "failed to read %s AppRelease Health condition", releaseLabel)}
	}
	if !found {
		return appReleaseStatus{reason: reasons.reconciling, message: fmt.Sprintf("waiting for %s AppRelease Health condition", releaseLabel)}
	}

	stale, message, err := appReleaseStale(appRelease, releaseLabel)
	if err != nil {
		return appReleaseStatus{err: err}
	}
	if stale {
		return appReleaseStatus{reason: reasons.reconciling, message: message}
	}

	syncStatus, err := conditionString(syncCondition, "status")
	if err != nil {
		return appReleaseStatus{err: pkgerrors.Wrapf(err, "failed to read %s AppRelease Sync status", releaseLabel)}
	}
	if syncStatus != string(corev1.ConditionTrue) {
		message, err := appReleaseConditionMessage(releaseLabel, "Sync", syncCondition, syncStatus)
		if err != nil {
			return appReleaseStatus{err: err}
		}
		return appReleaseStatus{reason: reasons.notReady, message: message}
	}
	healthStatus, err := conditionString(healthCondition, "status")
	if err != nil {
		return appReleaseStatus{err: pkgerrors.Wrapf(err, "failed to read %s AppRelease Health status", releaseLabel)}
	}
	if healthStatus != string(corev1.ConditionTrue) {
		message, err := appReleaseConditionMessage(releaseLabel, "Health", healthCondition, healthStatus)
		if err != nil {
			return appReleaseStatus{err: err}
		}
		return appReleaseStatus{reason: reasons.notReady, message: message}
	}

	return appReleaseStatus{ready: true, reason: reasons.ready}
}

func appReleaseCondition(appRelease *unstructured.Unstructured, conditionType string) (map[string]any, bool, error) {
	conditionList, found, err := unstructured.NestedSlice(appRelease.Object, "status", "conditions")
	if err != nil || !found {
		return nil, false, err
	}
	for _, condition := range conditionList {
		conditionMap, ok := condition.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("AppRelease %s condition has invalid type %T", conditionType, condition)
		}
		conditionName, err := conditionString(conditionMap, "type")
		if err != nil {
			return nil, false, err
		}
		if conditionName == conditionType {
			return conditionMap, true, nil
		}
	}
	return nil, false, nil
}

func appReleaseStale(appRelease *unstructured.Unstructured, releaseLabel string) (bool, string, error) {
	generation := appRelease.GetGeneration()
	observedGeneration, found, err := unstructured.NestedInt64(appRelease.Object, "status", "observedGeneration")
	if err != nil {
		return false, "", pkgerrors.Wrapf(err, "invalid %s AppRelease status observedGeneration", releaseLabel)
	}
	if !found {
		return true, fmt.Sprintf("waiting for %s AppRelease observedGeneration", releaseLabel), nil
	}
	if observedGeneration != generation {
		return true, fmt.Sprintf("waiting for %s AppRelease to observe generation %d", releaseLabel, generation), nil
	}
	return false, "", nil
}

func appReleaseConditionMessage(releaseLabel, conditionType string, condition map[string]any, status string) (string, error) {
	reason, err := conditionString(condition, "reason")
	if err != nil {
		return "", err
	}
	message, err := conditionString(condition, "message")
	if err != nil {
		return "", err
	}
	if reason == "" && message == "" {
		return fmt.Sprintf("%s AppRelease %s condition status is %s", releaseLabel, conditionType, status), nil
	}
	if message == "" {
		return fmt.Sprintf("%s AppRelease %s condition is %s", releaseLabel, conditionType, reason), nil
	}
	if reason == "" {
		return fmt.Sprintf("%s AppRelease %s condition: %s", releaseLabel, conditionType, message), nil
	}
	return fmt.Sprintf("%s AppRelease %s condition is %s: %s", releaseLabel, conditionType, reason, message), nil
}

func conditionString(condition map[string]any, field string) (string, error) {
	value, found := condition[field]
	if !found {
		return "", nil
	}
	valueString, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("AppRelease condition field %q has invalid type %T", field, value)
	}
	return valueString, nil
}

// controlPlaneNodesRegistered lists the workload cluster's control plane Node
// names and reports whether the whole expected set has registered. Callers that
// need a complete view of the control plane (kube-ovn, the self-built load
// balancer) use it to hold off until then.
func (r *clusterReconciler) controlPlaneNodesRegistered(ctx context.Context, cluster *clusterv1.Cluster, workloadClient kubernetes.Interface) ([]string, bool, error) {
	log := ctrl.LoggerFrom(ctx)
	if cluster.Spec.ControlPlaneRef == nil || cluster.Spec.ControlPlaneRef.Kind != kubeadmControlPlaneKind || cluster.Spec.ControlPlaneRef.APIVersion != controlplanev1.GroupVersion.String() {
		return nil, true, nil
	}

	nodes, err := workloadClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			"node-role.kubernetes.io/control-plane": "",
		}).String(),
	})
	if err != nil {
		log.Error(err, "Control plane Nodes cannot be listed")
		return nil, false, pkgerrors.Wrap(err, "failed to list control plane Nodes")
	}

	controlPlaneNodes := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		controlPlaneNodes = append(controlPlaneNodes, node.Name)
	}
	sort.Strings(controlPlaneNodes)

	kcp, err := r.kubeadmControlPlaneForCluster(ctx, cluster)
	if err != nil {
		return controlPlaneNodes, false, err
	}
	if kcp == nil {
		log.Info("Waiting for the KubeadmControlPlane to be available")
		return controlPlaneNodes, false, nil
	}
	if !kcp.DeletionTimestamp.IsZero() {
		log.Info("Waiting while the KubeadmControlPlane is deleting", "KubeadmControlPlane", klog.KObj(kcp))
		return controlPlaneNodes, false, nil
	}
	if kcp.Spec.Replicas == nil {
		return controlPlaneNodes, true, nil
	}

	if int32(len(controlPlaneNodes)) < *kcp.Spec.Replicas {
		log.Info("Waiting for all control plane Nodes to register", "controlPlaneNodes", len(controlPlaneNodes), "desiredControlPlaneReplicas", *kcp.Spec.Replicas)
		return controlPlaneNodes, false, nil
	}
	return controlPlaneNodes, true, nil
}

func (r *clusterReconciler) kubeadmControlPlaneForCluster(ctx context.Context, cluster *clusterv1.Cluster) (*controlplanev1.KubeadmControlPlane, error) {
	if cluster.Spec.ControlPlaneRef != nil {
		if cluster.Spec.ControlPlaneRef.Kind != kubeadmControlPlaneKind || cluster.Spec.ControlPlaneRef.APIVersion != controlplanev1.GroupVersion.String() {
			return nil, nil
		}
		namespace := cluster.Spec.ControlPlaneRef.Namespace
		if namespace == "" {
			namespace = cluster.Namespace
		}
		kcp := &controlplanev1.KubeadmControlPlane{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cluster.Spec.ControlPlaneRef.Name}, kcp); err != nil {
			return nil, pkgerrors.Wrap(err, "failed to get KubeadmControlPlane")
		}
		return kcp, nil
	}

	kcpList := &controlplanev1.KubeadmControlPlaneList{}
	if err := r.Client.List(ctx, kcpList, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return nil, pkgerrors.Wrap(err, "failed to list KubeadmControlPlane objects")
	}
	if len(kcpList.Items) == 0 {
		return nil, nil
	}
	if len(kcpList.Items) > 1 {
		return nil, fmt.Errorf("multiple KubeadmControlPlane objects found, expected 1, found %d", len(kcpList.Items))
	}
	return &kcpList.Items[0], nil
}

// newRemoteRestConfig builds a hardened rest.Config for the workload cluster
// from its kubeconfig secret. It is the single construction point for the
// workload connection so callers that only need a client.Client do not also
// build (and discard) a clientset.
func (r *clusterReconciler) newRemoteRestConfig(ctx context.Context, cluster *clusterv1.Cluster) (*rest.Config, error) {
	clusterKey := client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}
	kubeconfig, err := kcfg.FromSecret(ctx, r.Client, clusterKey)
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "failed to retrieve kubeconfig secret for Cluster %q in namespace %q", cluster.Name, cluster.Namespace)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, pkgerrors.Wrapf(err, "failed to create rest config for Cluster %q in namespace %q", cluster.Name, cluster.Namespace)
	}
	restConfig.Timeout = 10 * time.Second

	return restConfig, nil
}

func (r *clusterReconciler) newRemoteClients(ctx context.Context, cluster *clusterv1.Cluster) (kubernetes.Interface, *rest.Config, error) {
	restConfig, err := r.newRemoteRestConfig(ctx, cluster)
	if err != nil {
		return nil, nil, err
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, pkgerrors.Wrap(err, "failed to create workload clientset")
	}

	return clientset, restConfig, nil
}

func firstCIDRBlock(network *clusterv1.ClusterNetwork, pod bool) string {
	if network == nil {
		return ""
	}
	if pod && network.Pods != nil && len(network.Pods.CIDRBlocks) > 0 {
		return network.Pods.CIDRBlocks[0]
	}
	if !pod && network.Services != nil && len(network.Services.CIDRBlocks) > 0 {
		return network.Services.CIDRBlocks[0]
	}
	return ""
}

func sentryPullSecrets(ctx context.Context, clientset kubernetes.Interface) (string, []interface{}, error) {
	sa, err := clientset.CoreV1().ServiceAccounts("cpaas-system").Get(ctx, "sentry", metav1.GetOptions{})
	if err != nil {
		return "", nil, err
	}
	if len(sa.ImagePullSecrets) == 0 {
		return "", nil, nil
	}
	secrets := make([]interface{}, 0, len(sa.ImagePullSecrets))
	for _, s := range sa.ImagePullSecrets {
		secrets = append(secrets, s.Name)
	}
	return sa.ImagePullSecrets[0].Name, secrets, nil
}

func buildKubeOvnAppRelease(
	cluster *clusterv1.Cluster,
	registry string,
	chartName string,
	targetVersion string,
	podCIDR string,
	serviceCIDR string,
	joinCIDR string,
	chartPullSecret string,
	imagePullSecrets []interface{},
	controlPlaneNodes []string,
) *unstructured.Unstructured {
	if chartName == "" {
		chartName = kubeOvnLegacyChartName
	}
	host := cluster.Spec.ControlPlaneEndpoint.Host
	if host == "" {
		host = cluster.Name
	}
	appRelease := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": appReleaseGVR.GroupVersion().String(),
			"kind":       "AppRelease",
			"metadata": map[string]interface{}{
				"name":      "cni-kube-ovn",
				"namespace": "cpaas-system",
				"annotations": map[string]interface{}{
					"auto-recycle":  "true",
					"interval-sync": "true",
				},
			},
			"spec": map[string]interface{}{
				"destination": map[string]interface{}{
					"cluster":   "",
					"namespace": "",
				},
				"source": map[string]interface{}{
					"repoURL": registry,
					"charts": []interface{}{
						map[string]interface{}{
							"name":           chartName,
							"releaseName":    "cpaas-kube-ovn",
							"targetRevision": targetVersion,
						},
					},
				},
				"timeout": int64(120),
				"values": map[string]interface{}{
					"func": map[string]interface{}{
						"ENABLE_OVN_LB_PREFER_LOCAL": false,
						"LS_CT_SKIP_DST_LPORT_IPS":   true,
					},
					"global": map[string]interface{}{
						"albName":         "cpaas-system",
						"labelBaseDomain": "cpaas.io",
						"namespace":       "cpaas-system",
						"platformUrl":     fmt.Sprintf("https://%s", host),
						"region":          cluster.Name,
						"scheme":          "https",
						"host":            host,
						"replicas":        int64(1),
						"labels":          map[string]interface{}{},
						"nodeSelector":    nil,
						"tolerations":     nil,
						"protectSecretFiles": map[string]interface{}{
							"enabled": false,
						},
						"auth": map[string]interface{}{
							"default_admin": "admin",
						},
						"cluster": map[string]interface{}{
							"name":        cluster.Name,
							"isGlobal":    false,
							"networkType": "kube-ovn",
							"type":        "Imported",
						},
						"registry": map[string]interface{}{
							"address": registry,
						},
						"ingress": map[string]interface{}{
							"ingressClassName": "cpaas-system",
						},
					},
					"ipv4": map[string]interface{}{
						"POD_CIDR":    podCIDR,
						"SVC_CIDR":    serviceCIDR,
						"JOIN_CIDR":   joinCIDR,
						"POD_GATEWAY": "",
					},
					"networking": map[string]interface{}{
						"NETWORK_TYPE":      "geneve",
						"NET_STACK":         "ipv4",
						"NODE_LOCAL_DNS_IP": "",
						"EXCLUDE_IPS":       nil,
						"IFACE":             "eth0",
						"vlan": map[string]interface{}{
							"VLAN_ID":             nil,
							"VLAN_INTERFACE_NAME": "eth0",
						},
					},
				},
			},
		},
	}
	spec := appRelease.Object["spec"].(map[string]interface{})
	source := spec["source"].(map[string]interface{})
	values := spec["values"].(map[string]interface{})
	global := values["global"].(map[string]interface{})
	registryValues := global["registry"].(map[string]interface{})
	if len(controlPlaneNodes) > 0 {
		nodes := make([]interface{}, 0, len(controlPlaneNodes))
		for _, node := range controlPlaneNodes {
			nodes = append(nodes, node)
		}
		values["controlPlaneNodes"] = nodes
	}
	if chartPullSecret != "" {
		source["chartPullSecret"] = chartPullSecret
	}
	if len(imagePullSecrets) > 0 {
		registryValues["imagePullSecrets"] = imagePullSecrets
	}
	return appRelease
}

func (r *clusterReconciler) reconcileIdentitySecret(ctx context.Context, clusterCtx *capvcontext.ClusterContext) error {
	vsphereCluster := clusterCtx.VSphereCluster
	if !identity.IsSecretIdentity(vsphereCluster) {
		return nil
	}
	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{
		Namespace: vsphereCluster.Namespace,
		Name:      vsphereCluster.Spec.IdentityRef.Name,
	}
	err := r.Client.Get(ctx, secretKey, secret)
	if err != nil {
		return err
	}

	// If a different VSphereCluster is an owner return an error.
	if !clusterutilv1.IsOwnedByObject(secret, vsphereCluster) && identity.IsOwnedByIdentityOrCluster(secret.GetOwnerReferences()) {
		return fmt.Errorf("another cluster has set the OwnerRef for Secret %s/%s", secret.Namespace, secret.Name)
	}

	helper, err := patch.NewHelper(secret, r.Client)
	if err != nil {
		return err
	}

	// Ensure the VSphereCluster is an owner and that the APIVersion is up to date.
	secret.SetOwnerReferences(clusterutilv1.EnsureOwnerRef(secret.GetOwnerReferences(),
		metav1.OwnerReference{
			APIVersion: infrav1.GroupVersion.String(),
			Kind:       "VSphereCluster",
			Name:       vsphereCluster.Name,
			UID:        vsphereCluster.UID,
		},
	))

	// Ensure the finalizer is added.
	if !ctrlutil.ContainsFinalizer(secret, infrav1.SecretIdentitySetFinalizer) {
		ctrlutil.AddFinalizer(secret, infrav1.SecretIdentitySetFinalizer)
	}

	return helper.Patch(ctx, secret)
}

func (r *clusterReconciler) reconcileVCenterConnectivity(ctx context.Context, clusterCtx *capvcontext.ClusterContext) (*session.Session, error) {
	params := session.NewParams().
		WithServer(clusterCtx.VSphereCluster.Spec.Server).
		WithThumbprint(clusterCtx.VSphereCluster.Spec.Thumbprint)

	if clusterCtx.VSphereCluster.Spec.IdentityRef != nil {
		creds, err := identity.GetCredentials(ctx, r.Client, clusterCtx.VSphereCluster, r.ControllerManagerContext.Namespace)
		if err != nil {
			return nil, pkgerrors.Wrap(err, "failed to get credentials from IdentityRef")
		}

		params = params.WithUserInfo(creds.Username, creds.Password)
		return session.GetOrCreate(ctx, params)
	}

	params = params.WithUserInfo(r.ControllerManagerContext.Username, r.ControllerManagerContext.Password)
	return session.GetOrCreate(ctx, params)
}

func (r *clusterReconciler) reconcileVCenterVersion(clusterCtx *capvcontext.ClusterContext, s *session.Session) error {
	version, err := s.GetVersion()
	if err != nil {
		return pkgerrors.Wrapf(err, "invalid vCenter version")
	}
	clusterCtx.VSphereCluster.Status.VCenterVersion = version
	return nil
}

func (r *clusterReconciler) reconcileDeploymentZones(ctx context.Context, clusterCtx *capvcontext.ClusterContext) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	// If there is no failure domain selector, skip reconciliation
	if clusterCtx.VSphereCluster.Spec.FailureDomainSelector == nil {
		return true, nil
	}

	var opts client.ListOptions
	var err error
	opts.LabelSelector, err = metav1.LabelSelectorAsSelector(clusterCtx.VSphereCluster.Spec.FailureDomainSelector)
	if err != nil {
		return false, pkgerrors.Wrapf(err, "zone label selector is misconfigured")
	}

	var deploymentZoneList infrav1.VSphereDeploymentZoneList
	err = r.Client.List(ctx, &deploymentZoneList, &opts)
	if err != nil {
		err = pkgerrors.Wrap(err, "unable to list VSphereDeploymentZones")
		invalidateFailureDomainStatus(clusterCtx.VSphereCluster, err)
		return false, err
	}

	// Check machine config pool slot availability per datacenter to filter out
	// failure domains that have no allocatable slots.
	availableDatacenters, hasMachineConfigPools, err := r.computeAvailableDatacenters(ctx, clusterCtx)
	if err != nil {
		invalidateFailureDomainStatus(clusterCtx.VSphereCluster, err)
		return false, err
	}

	readyNotReported, notReady, excludedByPool := 0, 0, 0
	failureDomains := clusterv1.FailureDomains{}
	allReadyDomains := clusterv1.FailureDomains{}
	for _, zone := range deploymentZoneList.Items {
		if zone.Spec.Server != clusterCtx.VSphereCluster.Spec.Server {
			continue
		}

		if zone.Status.Ready == nil {
			readyNotReported++
			failureDomains[zone.Name] = clusterv1.FailureDomainSpec{
				ControlPlane: ptr.Deref(zone.Spec.ControlPlane, true),
			}
			continue
		}

		if *zone.Status.Ready {
			fdSpec := clusterv1.FailureDomainSpec{
				ControlPlane: ptr.Deref(zone.Spec.ControlPlane, true),
			}
			allReadyDomains[zone.Name] = fdSpec
			if hasMachineConfigPools {
				hasAvailableSlots, err := r.zoneHasAvailableSlots(ctx, zone, availableDatacenters)
				if err != nil {
					invalidateFailureDomainStatus(clusterCtx.VSphereCluster, err)
					return false, err
				}
				if !hasAvailableSlots {
					log.Info("Excluding failure domain: no machine config pool slots available for its datacenter",
						"zone", zone.Name, "failureDomain", zone.Spec.FailureDomain)
					excludedByPool++
					continue
				}
			}
			failureDomains[zone.Name] = fdSpec
			continue
		}
		notReady++
	}

	if excludedByPool > 0 {
		log.Info("Failure domains excluded due to machine config pool slot exhaustion", "excludedCount", excludedByPool)
		// Safety net: never report an empty FailureDomains map when ready zones
		// exist. An empty map causes CAPI to create Machines without FD assignment,
		// which could grab any DC's slot when one later becomes available.
		if len(failureDomains) == 0 && len(allReadyDomains) > 0 {
			log.Info("All failure domains exhausted, keeping all ready zones to prevent nil FD assignment")
			failureDomains = allReadyDomains
		}
	}

	clusterCtx.VSphereCluster.Status.FailureDomains = failureDomains
	if readyNotReported > 0 {
		log.Info("Waiting for failure domains to be reconciled")
		conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.FailureDomainsAvailableCondition, infrav1.WaitingForFailureDomainStatusReason, clusterv1.ConditionSeverityInfo, "waiting for failure domains to report ready status")
		v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
			Type:    infrav1.VSphereClusterFailureDomainsReadyV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.VSphereClusterFailureDomainsNotReadyV1Beta2Reason,
			Message: "Waiting for failure domains to report ready status",
		})
		return false, nil
	}

	if len(failureDomains) > 0 {
		if excludedByPool > 0 && excludedByPool == len(allReadyDomains) {
			// All ready zones were exhausted; all were kept to prevent nil FD assignment.
			conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.FailureDomainsAvailableCondition,
				infrav1.FailureDomainsExhaustedByMachineConfigPoolReason, clusterv1.ConditionSeverityWarning,
				"all failure domains have no available machine config pool slots (total %d)", excludedByPool)
			v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
				Type:    infrav1.VSphereClusterFailureDomainsReadyV1Beta2Condition,
				Status:  metav1.ConditionFalse,
				Reason:  infrav1.VSphereClusterFailureDomainsNotReadyV1Beta2Reason,
				Message: fmt.Sprintf("All failure domains have no available machine config pool slots (total %d)", excludedByPool),
			})
		} else if notReady > 0 || excludedByPool > 0 {
			msg := "one or more failure domains are not ready"
			if excludedByPool > 0 {
				msg = fmt.Sprintf("one or more failure domains are not ready or have no available machine config pool slots (excluded %d)", excludedByPool)
			}
			conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.FailureDomainsAvailableCondition, infrav1.FailureDomainsSkippedReason, clusterv1.ConditionSeverityInfo, "%s", msg)
			v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
				Type:    infrav1.VSphereClusterFailureDomainsReadyV1Beta2Condition,
				Status:  metav1.ConditionFalse,
				Reason:  infrav1.VSphereClusterFailureDomainsNotReadyV1Beta2Reason,
				Message: msg,
			})
		} else {
			conditions.MarkTrue(clusterCtx.VSphereCluster, infrav1.FailureDomainsAvailableCondition)
			v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
				Type:   infrav1.VSphereClusterFailureDomainsReadyV1Beta2Condition,
				Status: metav1.ConditionTrue,
				Reason: infrav1.VSphereClusterFailureDomainsReadyV1Beta2Reason,
			})
		}
	} else {
		// Remove the condition if failure domains do not exist
		conditions.Delete(clusterCtx.VSphereCluster, infrav1.FailureDomainsAvailableCondition)
	}
	return true, nil
}

// invalidateFailureDomainStatus prevents consumers from using a previously
// successful failure-domain result after the pool or failure-domain lookup has
// become unavailable.
func invalidateFailureDomainStatus(vsphereCluster *infrav1.VSphereCluster, err error) {
	vsphereCluster.Status.FailureDomains = clusterv1.FailureDomains{}
	conditions.MarkFalse(vsphereCluster, infrav1.FailureDomainsAvailableCondition,
		infrav1.WaitingForFailureDomainStatusReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
}

func (r *clusterReconciler) reconcileClusterModules(ctx context.Context, clusterCtx *capvcontext.ClusterContext) (reconcile.Result, error) {
	if feature.Gates.Enabled(feature.NodeAntiAffinity) && !clusterCtx.VSphereCluster.Spec.DisableClusterModule {
		return r.clusterModuleReconciler.Reconcile(ctx, clusterCtx)
	}
	return reconcile.Result{}, nil
}

// controlPlaneMachineToCluster is a handler.ToRequestsFunc to be used
// to enqueue requests for reconciliation for VSphereCluster to update
// its status.apiEndpoints field.
func (r *clusterReconciler) controlPlaneMachineToCluster(ctx context.Context, o client.Object) []ctrl.Request {
	log := ctrl.LoggerFrom(ctx)

	vsphereMachine, ok := o.(*infrav1.VSphereMachine)
	if !ok {
		log.Error(nil, fmt.Sprintf("Expected a VSphereMachine but got a %T", o))
		return nil
	}
	log = log.WithValues("VSphereMachine", klog.KObj(vsphereMachine))
	ctx = ctrl.LoggerInto(ctx, log)

	if !infrautilv1.IsControlPlaneMachine(vsphereMachine) {
		log.V(6).Info("Skipping VSphereCluster reconcile as Machine is not a control plane Machine")
		return nil
	}

	if len(vsphereMachine.Status.Addresses) == 0 {
		log.V(6).Info("Skipping VSphereCluster reconcile as Machine does not have an IP address")
		return nil
	}

	// Get the VSphereMachine's preferred IP address.
	if _, err := infrautilv1.GetMachinePreferredIPAddress(vsphereMachine); err != nil {
		if errors.Is(err, infrautilv1.ErrNoMachineIPAddr) {
			log.V(6).Info("Skipping VSphereCluster reconcile as Machine does not have a preferred IP address")
			return nil
		}
		log.V(4).Error(err, "Failed to get preferred IP address for VSphereMachine")
		return nil
	}

	// Fetch the CAPI Cluster.
	cluster, err := clusterutilv1.GetClusterFromMetadata(ctx, r.Client, vsphereMachine.ObjectMeta)
	if err != nil {
		log.V(4).Error(err, "VSphereMachine is missing cluster label or cluster does not exist")
		return nil
	}
	log = log.WithValues("Cluster", klog.KObj(cluster))
	if cluster.Spec.InfrastructureRef != nil {
		log = log.WithValues("VSphereCluster", klog.KRef(cluster.Namespace, cluster.Spec.InfrastructureRef.Name))
	}
	ctx = ctrl.LoggerInto(ctx, log)

	if conditions.IsTrue(cluster, clusterv1.ControlPlaneInitializedCondition) {
		log.V(6).Info("Skipping VSphereCluster reconcile as control plane is already initialized")
		return nil
	}

	if !cluster.Spec.ControlPlaneEndpoint.IsZero() {
		log.V(6).Info("Skipping VSphereCluster reconcile as Cluster control plane endpoint is already set")
		return nil
	}

	if cluster.Spec.InfrastructureRef == nil {
		log.Error(nil, "Failed to get VSphereCluster: Cluster.spec.infrastructureRef is not yet set")
		return nil
	}

	// Fetch the VSphereCluster
	vsphereCluster := &infrav1.VSphereCluster{}
	vsphereClusterKey := client.ObjectKey{
		Namespace: vsphereMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(ctx, vsphereClusterKey, vsphereCluster); err != nil {
		log.V(4).Error(err, "Failed to get VSphereCluster")
		return nil
	}

	if !vsphereCluster.Spec.ControlPlaneEndpoint.IsZero() {
		log.V(6).Info("Skipping VSphereCluster reconcile as VSphereCluster control plane endpoint is already set")
		return nil
	}

	return []ctrl.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: vsphereClusterKey.Namespace,
			Name:      vsphereClusterKey.Name,
		},
	}}
}

func (r *clusterReconciler) deploymentZoneToCluster(ctx context.Context, o client.Object) []ctrl.Request {
	log := ctrl.LoggerFrom(ctx)

	var requests []ctrl.Request
	obj, ok := o.(*infrav1.VSphereDeploymentZone)
	if !ok {
		log.Error(nil, fmt.Sprintf("Expected a VSphereDeploymentZone but got a %T", o))
		return nil
	}

	var clusterList infrav1.VSphereClusterList
	err := r.Client.List(ctx, &clusterList)
	if err != nil {
		log.V(4).Error(err, "Failed to list VSphereClusters")
		return requests
	}

	for _, cluster := range clusterList.Items {
		if obj.Spec.Server == cluster.Spec.Server {
			r := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      cluster.Name,
					Namespace: cluster.Namespace,
				},
			}
			requests = append(requests, r)
		}
	}
	return requests
}

// computeAvailableDatacenters lists all VSphereMachineConfigPools for this cluster
// and returns the set of datacenters that have at least one allocatable slot.
// The second return value indicates whether any machine config pools exist.
func (r *clusterReconciler) computeAvailableDatacenters(ctx context.Context, clusterCtx *capvcontext.ClusterContext) (map[string]struct{}, bool, error) {
	var poolList infrav1.VSphereMachineConfigPoolList
	if err := r.Client.List(ctx, &poolList, client.InNamespace(clusterCtx.Cluster.Namespace)); err != nil {
		return nil, false, pkgerrors.Wrap(err, "failed to list VSphereMachineConfigPools")
	}

	var clusterPools []infrav1.VSphereMachineConfigPool
	for i := range poolList.Items {
		if poolList.Items[i].Spec.ClusterRef.Name == clusterCtx.Cluster.Name &&
			poolList.Items[i].Spec.ClusterRef.Namespace == clusterCtx.Cluster.Namespace {
			clusterPools = append(clusterPools, poolList.Items[i])
		}
	}

	if len(clusterPools) == 0 {
		return nil, false, nil
	}

	return services.DatacentersWithAvailableSlots(clusterPools), true, nil
}

// zoneHasAvailableSlots checks whether the given deployment zone's datacenter
// has available machine config pool slots. A lookup failure is returned so the
// caller can retry instead of silently including an unverified zone.
func (r *clusterReconciler) zoneHasAvailableSlots(ctx context.Context, zone infrav1.VSphereDeploymentZone, availableDatacenters map[string]struct{}) (bool, error) {
	if zone.Spec.FailureDomain == "" {
		return true, nil
	}

	var fd infrav1.VSphereFailureDomain
	if err := r.Client.Get(ctx, client.ObjectKey{Name: zone.Spec.FailureDomain}, &fd); err != nil {
		return false, pkgerrors.Wrapf(err, "failed to get VSphereFailureDomain %q for zone %q", zone.Spec.FailureDomain, zone.Name)
	}

	dc := fd.Spec.Topology.Datacenter
	if dc == "" {
		return true, nil
	}

	_, ok := availableDatacenters[dc]
	return ok, nil
}

// machineConfigPoolToCluster maps a VSphereMachineConfigPool to the VSphereCluster
// that owns the referenced CAPI Cluster.
func (r *clusterReconciler) machineConfigPoolToCluster(ctx context.Context, o client.Object) []ctrl.Request {
	log := ctrl.LoggerFrom(ctx)
	pool, ok := o.(*infrav1.VSphereMachineConfigPool)
	if !ok {
		return nil
	}

	if pool.Spec.ClusterRef.Name == "" || pool.Spec.ClusterRef.Namespace == "" {
		return nil
	}

	cluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: pool.Spec.ClusterRef.Namespace,
		Name:      pool.Spec.ClusterRef.Name,
	}, cluster); err != nil {
		log.V(4).Error(err, "Failed to get Cluster for VSphereMachineConfigPool", "pool", klog.KObj(pool))
		return nil
	}

	if cluster.Spec.InfrastructureRef == nil {
		return nil
	}

	return []ctrl.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: cluster.Spec.InfrastructureRef.Namespace,
			Name:      cluster.Spec.InfrastructureRef.Name,
		},
	}}
}
