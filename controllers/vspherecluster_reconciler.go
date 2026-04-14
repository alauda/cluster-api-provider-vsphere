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
	"time"

	pkgerrors "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
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

	vmService               services.VimMachineService
	clusterModuleReconciler Reconciler
}

// Reconcile ensures the back-end state reflects the Kubernetes resource state intent.
func (r *clusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
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

	if isPaused, requeue, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, vsphereCluster); err != nil || isPaused || requeue {
		return ctrl.Result{}, err
	}

	// Create the cluster context for this request.
	clusterContext := &capvcontext.ClusterContext{
		Cluster:        cluster,
		VSphereCluster: vsphereCluster,
		PatchHelper:    patchHelper,
	}

	// Always issue a patch when exiting this function so changes to the
	// resource are patched back to the API server.
	defer func() {
		if err := r.patch(ctx, clusterContext); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	// Handle deleted clusters
	if !vsphereCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, clusterContext)
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
		conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.VCenterAvailableCondition, infrav1.VCenterUnreachableReason, clusterv1.ConditionSeverityError, err.Error())
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
		conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.VCenterAvailableCondition, infrav1.VCenterUnreachableReason, clusterv1.ConditionSeverityError, err.Error())
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
	err = r.reconcileVCenterVersion(clusterCtx, vcenterSession)
	if err != nil || clusterCtx.VSphereCluster.Status.VCenterVersion == "" {
		conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.ClusterModulesAvailableCondition, infrav1.MissingVCenterVersionReason, clusterv1.ConditionSeverityWarning, err.Error())
		v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
			Type:    infrav1.VSphereClusterClusterModulesReadyV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.VSphereClusterModulesInvalidVCenterVersionV1Beta2Reason,
			Message: err.Error(),
		})
		log.Error(err, "could not reconcile vCenter version")
	}

	affinityReconcileResult, err := r.reconcileClusterModules(ctx, clusterCtx)
	if err != nil {
		conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.ClusterModulesAvailableCondition, infrav1.ClusterModuleSetupFailedReason, clusterv1.ConditionSeverityWarning, err.Error())
		v1beta2conditions.Set(clusterCtx.VSphereCluster, metav1.Condition{
			Type:    infrav1.VSphereClusterClusterModulesReadyV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.VSphereClusterClusterModulesNotReadyV1Beta2Reason,
			Message: err.Error(),
		})
		return affinityReconcileResult, err
	}

	clusterCtx.VSphereCluster.Status.Ready = true

	if err := r.reconcileKubeOvnAppRelease(ctx, clusterCtx); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *clusterReconciler) reconcileKubeOvnAppRelease(ctx context.Context, clusterCtx *capvcontext.ClusterContext) error {
	cluster := clusterCtx.Cluster
	if cluster == nil || cluster.Annotations["cpaas.io/network-type"] != "kube-ovn" {
		return nil
	}
	var err error

	targetVersion := cluster.Annotations["cpaas.io/kube-ovn-version"]
	joinCIDR := cluster.Annotations["cpaas.io/kube-ovn-join-cidr"]
	registry := cluster.Annotations["cpaas.io/registry-address"]
	if registry == "" {
		return fmt.Errorf("cpaas.io/registry-address annotation is required to deploy kube-ovn")
	}

	if targetVersion == "" {
		modulePlugin := &unstructured.Unstructured{}
		modulePlugin.SetAPIVersion(modulePluginGVK.GroupVersion().String())
		modulePlugin.SetKind(modulePluginGVK.Kind)
		if err := r.Client.Get(ctx, client.ObjectKey{Name: "kube-ovn"}, modulePlugin); err != nil {
			return pkgerrors.Wrap(err, "failed to get kube-ovn ModulePlugin")
		}
		var found bool
		targetVersion, found, err = unstructured.NestedString(modulePlugin.Object, "status", "latestVersion")
		if err != nil {
			return pkgerrors.Wrap(err, "failed to read kube-ovn ModulePlugin status.latestVersion")
		}
		if !found || targetVersion == "" {
			return fmt.Errorf("kube-ovn module plugin latestVersion is empty")
		}
	}

	podCIDR := firstCIDRBlock(cluster.Spec.ClusterNetwork, true)
	serviceCIDR := firstCIDRBlock(cluster.Spec.ClusterNetwork, false)
	if podCIDR == "" || serviceCIDR == "" {
		return fmt.Errorf("cluster network pod/service CIDR must be set before deploying kube-ovn")
	}

	clientset, restConfig, err := r.newRemoteClients(ctx, cluster)
	if err != nil {
		return err
	}

	imagePullSecrets, err := sentryImagePullSecrets(ctx, clientset)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	appRelease := buildKubeOvnAppRelease(cluster, registry, targetVersion, podCIDR, serviceCIDR, joinCIDR, imagePullSecrets)
	dc, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return pkgerrors.Wrap(err, "failed to create dynamic client for workload cluster")
	}

	current, err := dc.Resource(appReleaseGVR).Namespace(appRelease.GetNamespace()).Get(ctx, appRelease.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = dc.Resource(appReleaseGVR).Namespace(appRelease.GetNamespace()).Create(ctx, appRelease, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return pkgerrors.Wrap(err, "failed to get existing kube-ovn AppRelease")
	}

	if !reflect.DeepEqual(current.Object["spec"], appRelease.Object["spec"]) {
		current.Object["spec"] = appRelease.Object["spec"]
		_, err = dc.Resource(appReleaseGVR).Namespace(appRelease.GetNamespace()).Update(ctx, current, metav1.UpdateOptions{})
		if err != nil {
			return pkgerrors.Wrap(err, "failed to update kube-ovn AppRelease")
		}
	}

	return nil
}

func (r *clusterReconciler) newRemoteClients(ctx context.Context, cluster *clusterv1.Cluster) (kubernetes.Interface, *rest.Config, error) {
	clusterKey := client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}
	kubeconfig, err := kcfg.FromSecret(ctx, r.Client, clusterKey)
	if err != nil {
		return nil, nil, pkgerrors.Wrapf(err, "failed to retrieve kubeconfig secret for Cluster %q in namespace %q", cluster.Name, cluster.Namespace)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, nil, pkgerrors.Wrapf(err, "failed to create rest config for Cluster %q in namespace %q", cluster.Name, cluster.Namespace)
	}
	restConfig.Timeout = 10 * time.Second

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

func sentryImagePullSecrets(ctx context.Context, clientset kubernetes.Interface) ([]interface{}, error) {
	sa, err := clientset.CoreV1().ServiceAccounts("cpaas-system").Get(ctx, "sentry", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if len(sa.ImagePullSecrets) == 0 {
		return nil, nil
	}
	secrets := make([]interface{}, 0, len(sa.ImagePullSecrets))
	for _, s := range sa.ImagePullSecrets {
		secrets = append(secrets, s.Name)
	}
	return secrets, nil
}

func buildKubeOvnAppRelease(
	cluster *clusterv1.Cluster,
	registry string,
	targetVersion string,
	podCIDR string,
	serviceCIDR string,
	joinCIDR string,
	imagePullSecrets []interface{},
) *unstructured.Unstructured {
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
							"name":           "acp/chart-cpaas-kube-ovn",
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
	if len(imagePullSecrets) > 0 {
		_ = unstructured.SetNestedSlice(appRelease.Object, imagePullSecrets, "spec", "values", "global", "registry", "imagePullSecrets")
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
		return false, pkgerrors.Wrap(err, "unable to list VSphereDeploymentZones")
	}

	// Check machine config pool slot availability per datacenter to filter out
	// failure domains that have no allocatable slots.
	availableDatacenters, hasMachineConfigPools := r.computeAvailableDatacenters(ctx, clusterCtx)

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
			if hasMachineConfigPools && !r.zoneHasAvailableSlots(ctx, zone, availableDatacenters) {
				log.Info("Excluding failure domain: no machine config pool slots available for its datacenter",
					"zone", zone.Name, "failureDomain", zone.Spec.FailureDomain)
				excludedByPool++
				continue
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
			conditions.MarkFalse(clusterCtx.VSphereCluster, infrav1.FailureDomainsAvailableCondition, infrav1.FailureDomainsSkippedReason, clusterv1.ConditionSeverityInfo, msg)
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
func (r *clusterReconciler) computeAvailableDatacenters(ctx context.Context, clusterCtx *capvcontext.ClusterContext) (map[string]struct{}, bool) {
	log := ctrl.LoggerFrom(ctx)
	var poolList infrav1.VSphereMachineConfigPoolList
	if err := r.Client.List(ctx, &poolList, client.InNamespace(clusterCtx.Cluster.Namespace)); err != nil {
		log.Error(err, "Failed to list VSphereMachineConfigPools, skipping slot-based filtering")
		return nil, false
	}

	var clusterPools []infrav1.VSphereMachineConfigPool
	for i := range poolList.Items {
		if poolList.Items[i].Spec.ClusterRef.Name == clusterCtx.Cluster.Name &&
			poolList.Items[i].Spec.ClusterRef.Namespace == clusterCtx.Cluster.Namespace {
			clusterPools = append(clusterPools, poolList.Items[i])
		}
	}

	if len(clusterPools) == 0 {
		return nil, false
	}

	return services.DatacentersWithAvailableSlots(clusterPools), true
}

// zoneHasAvailableSlots checks whether the given deployment zone's datacenter
// has available machine config pool slots. Returns true conservatively if the
// failure domain cannot be resolved.
func (r *clusterReconciler) zoneHasAvailableSlots(ctx context.Context, zone infrav1.VSphereDeploymentZone, availableDatacenters map[string]struct{}) bool {
	log := ctrl.LoggerFrom(ctx)
	if zone.Spec.FailureDomain == "" {
		return true
	}

	var fd infrav1.VSphereFailureDomain
	if err := r.Client.Get(ctx, client.ObjectKey{Name: zone.Spec.FailureDomain}, &fd); err != nil {
		log.Error(err, "Failed to get VSphereFailureDomain, including zone conservatively", "zone", zone.Name, "failureDomain", zone.Spec.FailureDomain)
		return true
	}

	dc := fd.Spec.Topology.Datacenter
	if dc == "" {
		return true
	}

	_, ok := availableDatacenters[dc]
	return ok
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
