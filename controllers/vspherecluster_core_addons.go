/*
Copyright 2026 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strings"
	"time"

	pkgerrors "github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
)

// Core addon handoff.
//
// From platform v4.5 the platform installs and upgrades kube-ovn and CoreDNS on
// these clusters through ModuleInfo, the way it does on Baremetal clusters, and
// its upgrade controller upgrades them with the platform. This provider hands
// them over by labelling the CAPI Cluster cni-type as the last step and, from
// then on, no longer writes the kube-ovn AppRelease or the CoreDNS Deployment.
// Clusters below v4.5 keep the provider-managed path.
const (
	// The platform version from which the platform owns kube-ovn and CoreDNS.
	// Only major and minor are compared, so v4.5 pre-releases count.
	coreAddonsHandoffMajor = 4
	coreAddonsHandoffMinor = 5

	// cniTypeLabel is the handoff switch: the kube-ovn ModulePlugin affinity and
	// cluster-transformer key on it. Only this provider writes it on its
	// clusters, and only once everything the platform renders from is in place.
	cniTypeLabel = "cni-type"
	// networkTypeAnnotation names the CNI of the cluster; "kube-ovn" is what
	// this provider installs kube-ovn for, and what the platform renders from.
	networkTypeAnnotation           = "cpaas.io/network-type"
	networkTypeKubeOvn              = "kube-ovn"
	legacyKubeOvnJoinCIDRAnnotation = "cpaas.io/kube-ovn-join-cidr"
	kubeOvnJoinCIDRAnnotation       = "kube-ovn.cpaas.io/join-cidr"
	// The platform renders the kube-ovn interface (IFACE and the VLAN
	// interface) from this annotation, and leaves it empty without it.
	kubeOvnNetworkDeviceAnnotation = "kube-ovn.cpaas.io/network-device"
	// kubeOvnInterface is the interface buildKubeOvnAppRelease gives kube-ovn.
	kubeOvnInterface = "eth0"

	kubeOvnAppReleaseNamespace = "cpaas-system"
	kubeOvnAppReleaseName      = "cni-kube-ovn"
	kubeOvnModuleName          = "kube-ovn"
	productBaseName            = "base"
	coreDNSAddonPhase          = "addon/coredns"
	systemNamespace            = "kube-system"
)

// The platform objects are read unstructured, which goes to the API server
// directly instead of starting an informer.
var (
	productBaseGVK = schema.GroupVersionKind{Group: "product.alauda.io", Version: "v1alpha1", Kind: "ProductBase"}
	appReleaseGVK  = appReleaseGVR.GroupVersion().WithKind("AppRelease")
)

// coreDNSDefaultPlugins are the plugins of the Corefile kubeadm installs.
// A Corefile using anything else, or more than one server block, was
// customised, and the platform chart would not carry the customisation over.
var coreDNSDefaultPlugins = []string{
	"errors", "health", "ready", "kubernetes", "prometheus", "forward",
	"cache", "loop", "reload", "loadbalance",
}

var coreAddonsHandedOffConditionSpec = readinessConditionSpec{
	v1beta1Type:       infrav1.CoreAddonsHandedOffCondition,
	v1beta2Type:       infrav1.VSphereClusterCoreAddonsHandedOffV1Beta2Condition,
	subject:           "core addon handoff",
	reconcilingReason: infrav1.CoreAddonsHandoffPendingReason,
}

func kubeOvnCluster(cluster *clusterv1.Cluster) bool {
	return cluster != nil && cluster.Annotations[networkTypeAnnotation] == networkTypeKubeOvn
}

func coreAddonsHandedOff(cluster *clusterv1.Cluster) bool {
	return cluster != nil && cluster.Labels[cniTypeLabel] != ""
}

func platformManagesCoreAddons(platformVersion string) bool {
	ok, err := versionAtLeastMinor(platformVersion, coreAddonsHandoffMajor, coreAddonsHandoffMinor)
	return err == nil && ok
}

func (r *clusterReconciler) setCoreAddonsHandoffCondition(vsphereCluster *infrav1.VSphereCluster, status corev1.ConditionStatus, reason string, severity clusterv1.ConditionSeverity, message string) {
	r.setReadinessCondition(vsphereCluster, coreAddonsHandedOffConditionSpec, status, reason, severity, message)
}

// productBaseVersions returns the spec and status versions of the platform.
func (r *clusterReconciler) productBaseVersions(ctx context.Context) (string, string, error) {
	productBase := &unstructured.Unstructured{}
	productBase.SetGroupVersionKind(productBaseGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Name: productBaseName}, productBase); err != nil {
		return "", "", pkgerrors.Wrapf(err, "failed to get ProductBase %s", productBaseName)
	}
	spec, _, _ := unstructured.NestedString(productBase.Object, "spec", "version")
	status, _, _ := unstructured.NestedString(productBase.Object, "status", "version")
	return spec, status, nil
}

// newClusterPlatformVersion is the version cluster-transformer gives the
// ClusterModule of a new cluster: the ProductBase status, or its spec while
// the status is still empty.
func newClusterPlatformVersion(spec, status string) string {
	if status != "" {
		return status
	}
	return spec
}

func (r *clusterReconciler) clusterModuleVersion(ctx context.Context, clusterName string) (string, bool, error) {
	clusterModule := &unstructured.Unstructured{}
	clusterModule.SetGroupVersionKind(clusterModuleGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Name: clusterName}, clusterModule); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, pkgerrors.Wrapf(err, "failed to get ClusterModule %s", clusterName)
	}
	version, _, _ := unstructured.NestedString(clusterModule.Object, "spec", "version")
	return version, true, nil
}

// platformInstallsKubeOvn reports whether the platform is going to install
// kube-ovn on the cluster: its ClusterModule exists at v4.5 or later. Without a
// ClusterModule this provider still installs kube-ovn and hands it over later.
func (r *clusterReconciler) platformInstallsKubeOvn(ctx context.Context, clusterName string) (bool, error) {
	clusterVersion, found, err := r.clusterModuleVersion(ctx, clusterName)
	if err != nil || !found || clusterVersion == "" {
		return false, err
	}
	return platformManagesCoreAddons(clusterVersion), nil
}

// kubeOvnTargetVersion is the kube-ovn version the platform installs on a
// cluster at clusterVersion.
func (r *clusterReconciler) kubeOvnTargetVersion(ctx context.Context, clusterVersion string) (string, error) {
	modulePlugin := &unstructured.Unstructured{}
	modulePlugin.SetGroupVersionKind(modulePluginGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Name: kubeOvnModuleName}, modulePlugin); err != nil {
		return "", pkgerrors.Wrapf(err, "failed to get ModulePlugin %s", kubeOvnModuleName)
	}
	version, _, _ := unstructured.NestedString(modulePlugin.Object, "status", "targetClusterVersions", clusterVersion, "version")
	return version, nil
}

// reconcileKubeadmControlPlaneCoreDNSSkip makes kubeadm skip its CoreDNS addon
// on a new kube-ovn cluster of platform v4.5 or later, where the platform
// installs CoreDNS from its chart. It reports whether the infrastructure may
// be marked ready.
//
// KCP copies the init configuration into the KubeadmConfig of the first
// control-plane machine and rolls the control plane when it changes later, so
// the phase has to be in the KubeadmControlPlane before that machine exists.
// KCP creates machines only once the infrastructure is ready, which is why
// readiness is held back until the patch has landed, also while the
// KubeadmControlPlane does not exist yet. Clusters that already have
// control-plane machines are never patched.
func (r *clusterReconciler) reconcileKubeadmControlPlaneCoreDNSSkip(ctx context.Context, clusterCtx *capvcontext.ClusterContext) (bool, error) {
	cluster := clusterCtx.Cluster
	if !kubeOvnCluster(cluster) || conditions.IsTrue(cluster, clusterv1.ControlPlaneInitializedCondition) {
		return true, nil
	}
	if ref := cluster.Spec.ControlPlaneRef; ref == nil || ref.Kind != kubeadmControlPlaneKind || ref.APIVersion != controlplanev1.GroupVersion.String() {
		return true, nil
	}
	kcp, err := r.kubeadmControlPlaneForCluster(ctx, cluster)
	if err != nil {
		if !apierrors.IsNotFound(pkgerrors.Cause(err)) {
			return false, err
		}
		kcp = nil
	}
	if kcp != nil && (hasCoreDNSSkipPhase(kcp) || kcp.Status.Replicas > 0) {
		return true, nil
	}
	hasMachines, err := r.hasControlPlaneMachines(ctx, cluster)
	if err != nil {
		return false, err
	}
	if hasMachines {
		return true, nil
	}
	spec, status, err := r.productBaseVersions(ctx)
	if err != nil {
		return false, err
	}
	if !platformManagesCoreAddons(newClusterPlatformVersion(spec, status)) {
		return true, nil
	}
	if kcp == nil {
		return false, nil
	}

	patchBase := kcp.DeepCopy()
	if kcp.Spec.KubeadmConfigSpec.InitConfiguration == nil {
		kcp.Spec.KubeadmConfigSpec.InitConfiguration = &bootstrapv1.InitConfiguration{}
	}
	kcp.Spec.KubeadmConfigSpec.InitConfiguration.SkipPhases = append(kcp.Spec.KubeadmConfigSpec.InitConfiguration.SkipPhases, coreDNSAddonPhase)
	if kcp.Annotations == nil {
		kcp.Annotations = map[string]string{}
	}
	if _, ok := kcp.Annotations[controlplanev1.SkipCoreDNSAnnotation]; !ok {
		kcp.Annotations[controlplanev1.SkipCoreDNSAnnotation] = "true"
	}
	if err := r.Client.Patch(ctx, kcp, client.MergeFrom(patchBase)); err != nil {
		return false, pkgerrors.Wrap(err, "failed to make the KubeadmControlPlane skip the kubeadm CoreDNS addon")
	}
	ctrl.LoggerFrom(ctx).Info("Configured the KubeadmControlPlane to skip the kubeadm CoreDNS addon", "KubeadmControlPlane", kcp.Name)
	return true, nil
}

// holdForCoreDNSSkip records whether the infrastructure waits for the
// KubeadmControlPlane to skip the kubeadm CoreDNS addon, and reports whether it
// has to.
func (r *clusterReconciler) holdForCoreDNSSkip(vsphereCluster *infrav1.VSphereCluster, coreDNSSkipped bool) bool {
	if !coreDNSSkipped {
		r.setCoreAddonsHandoffCondition(vsphereCluster, corev1.ConditionFalse, infrav1.CoreDNSSkipPhasePendingReason, clusterv1.ConditionSeverityInfo,
			"waiting to configure the KubeadmControlPlane to skip the kubeadm CoreDNS addon")
		return true
	}
	if conditions.GetReason(vsphereCluster, infrav1.CoreAddonsHandedOffCondition) == infrav1.CoreDNSSkipPhasePendingReason {
		r.setCoreAddonsHandoffCondition(vsphereCluster, corev1.ConditionFalse, infrav1.CoreAddonsHandoffPendingReason, clusterv1.ConditionSeverityInfo,
			"waiting for the control plane to initialise")
	}
	return false
}

func hasCoreDNSSkipPhase(kcp *controlplanev1.KubeadmControlPlane) bool {
	init := kcp.Spec.KubeadmConfigSpec.InitConfiguration
	return init != nil && slices.Contains(init.SkipPhases, coreDNSAddonPhase)
}

func (r *clusterReconciler) hasControlPlaneMachines(ctx context.Context, cluster *clusterv1.Cluster) (bool, error) {
	machines := &clusterv1.MachineList{}
	if err := r.Client.List(ctx, machines, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return false, pkgerrors.Wrap(err, "failed to list Machines")
	}
	for i := range machines.Items {
		if _, ok := machines.Items[i].Labels[clusterv1.MachineControlPlaneLabel]; ok {
			return true, nil
		}
	}
	return false, nil
}

// newRemoteClient builds a controller-runtime client for the workload cluster.
func (r *clusterReconciler) newRemoteClient(ctx context.Context, cluster *clusterv1.Cluster) (client.Client, error) {
	restConfig, err := r.newRemoteRestConfig(ctx, cluster)
	if err != nil {
		return nil, err
	}
	remoteClient, err := client.New(restConfig, client.Options{Scheme: r.Client.Scheme()})
	if err != nil {
		return nil, pkgerrors.Wrap(err, "failed to create workload client")
	}
	return remoteClient, nil
}

// reconcileCoreAddonsHandoff hands kube-ovn and CoreDNS of a kube-ovn cluster
// over to the platform once the cluster is at v4.5 or later, and reports
// whether the platform manages them. The caller has waited for every
// control-plane Node to register: kube-ovn sizes ovn-central from the
// control-plane Nodes it sees when it is rendered, and the platform does not
// render it again when more join.
//
// The cluster and the platform must be on the same version, so that
// cluster-transformer installs for the version the cluster is at. The kube-ovn
// AppRelease this provider wrote may be older than what the platform installs:
// the platform upgrade leaves kube-ovn alone on clusters it does not manage
// yet, and cluster-transformer upgrades it in the same release update that
// takes over its configuration, so kube-ovn rolls once.
func (r *clusterReconciler) reconcileCoreAddonsHandoff(ctx context.Context, clusterCtx *capvcontext.ClusterContext, remoteClient client.Client) (bool, error) {
	vsphereCluster := clusterCtx.VSphereCluster
	cluster := clusterCtx.Cluster
	if !kubeOvnCluster(cluster) {
		return false, nil
	}
	if coreAddonsHandedOff(cluster) {
		r.setCoreAddonsHandoffCondition(vsphereCluster, corev1.ConditionTrue, infrav1.CoreAddonsHandedOffReason, clusterv1.ConditionSeverityInfo, "")
		return true, nil
	}
	pending := func(format string, args ...any) (bool, error) {
		r.setCoreAddonsHandoffCondition(vsphereCluster, corev1.ConditionFalse, infrav1.CoreAddonsHandoffPendingReason, clusterv1.ConditionSeverityInfo, fmt.Sprintf(format, args...))
		return false, nil
	}

	clusterVersion, found, err := r.clusterModuleVersion(ctx, cluster.Name)
	if err != nil {
		return false, err
	}
	if !found || clusterVersion == "" {
		return pending("waiting for the platform to create the ClusterModule")
	}
	if !platformManagesCoreAddons(clusterVersion) {
		r.setCoreAddonsHandoffCondition(vsphereCluster, corev1.ConditionFalse, infrav1.CoreAddonsHandoffNotApplicableReason, clusterv1.ConditionSeverityInfo,
			fmt.Sprintf("the cluster is at %s; below v%d.%d this provider manages kube-ovn and CoreDNS", clusterVersion, coreAddonsHandoffMajor, coreAddonsHandoffMinor))
		return false, nil
	}
	specVersion, statusVersion, err := r.productBaseVersions(ctx)
	if err != nil {
		return false, err
	}
	if clusterVersion != specVersion || specVersion != statusVersion {
		return pending("waiting for the cluster (%s) and the platform (%s, rolling out %s) to be on one version", clusterVersion, statusVersion, specVersion)
	}

	check, err := r.coreAddonsHandoffCheck(ctx, cluster, remoteClient, clusterVersion)
	if err != nil {
		return false, err
	}
	if len(check.blockers) > 0 {
		r.setCoreAddonsHandoffCondition(vsphereCluster, corev1.ConditionFalse, infrav1.CoreAddonsHandoffBlockedReason, clusterv1.ConditionSeverityWarning,
			strings.Join(check.blockers, "; "))
		return false, nil
	}
	if check.waiting != "" {
		return pending("%s", check.waiting)
	}
	if check.existingKubeOvn {
		// Taking over an existing kube-ovn rolls it, so wait for the machines.
		rollout, err := r.machinesRollingOut(ctx, cluster)
		if err != nil {
			return false, err
		}
		if rollout != "" {
			return pending("%s; the handoff rolls kube-ovn and waits until it finishes", rollout)
		}
	}

	if err := ensureCoreDNSPullSecret(ctx, remoteClient); err != nil {
		return false, err
	}
	if err := r.handOverCoreAddons(ctx, cluster); err != nil {
		return false, err
	}
	r.setCoreAddonsHandoffCondition(vsphereCluster, corev1.ConditionTrue, infrav1.CoreAddonsHandedOffReason, clusterv1.ConditionSeverityInfo, "")
	ctrl.LoggerFrom(ctx).Info("Handed kube-ovn and CoreDNS over to the platform", "clusterVersion", clusterVersion)
	return true, nil
}

// reconcileHandedOffKubeOvn reports the readiness of the kube-ovn the platform
// installs once the cluster is handed over, without writing it:
// cluster-transformer owns the AppRelease now, while the self-built load
// balancer, which comes next, still needs a working CNI.
func (r *clusterReconciler) reconcileHandedOffKubeOvn(ctx context.Context, clusterCtx *capvcontext.ClusterContext, remoteClient client.Client) (reconcile.Result, error) {
	vsphereCluster := clusterCtx.VSphereCluster
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(appReleaseGVK)
	if err := remoteClient.Get(ctx, client.ObjectKey{Namespace: kubeOvnAppReleaseNamespace, Name: kubeOvnAppReleaseName}, app); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			r.setKubeOvnAppReleaseCondition(vsphereCluster, corev1.ConditionFalse, infrav1.KubeOvnAppReleaseReconcilingReason, clusterv1.ConditionSeverityInfo,
				"waiting for the platform to install kube-ovn")
			return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
		}
		r.setReadinessUnknown(vsphereCluster, kubeOvnAppReleaseConditionSpec, err)
		return reconcile.Result{}, pkgerrors.Wrap(err, "failed to get the kube-ovn AppRelease")
	}
	readiness := kubeOvnAppReleaseReadiness(app)
	if readiness.err != nil {
		r.setReadinessUnknown(vsphereCluster, kubeOvnAppReleaseConditionSpec, readiness.err)
		return reconcile.Result{}, readiness.err
	}
	if readiness.ready {
		r.setKubeOvnAppReleaseCondition(vsphereCluster, corev1.ConditionTrue, infrav1.KubeOvnAppReleaseReadyReason, clusterv1.ConditionSeverityInfo, "")
		return reconcile.Result{}, nil
	}
	r.setKubeOvnAppReleaseCondition(vsphereCluster, corev1.ConditionFalse, readiness.reason, clusterv1.ConditionSeverityInfo, readiness.message)
	return reconcile.Result{RequeueAfter: 10 * time.Second}, nil
}

// ensureCoreDNSPullSecret gives the kubeadm CoreDNS ServiceAccount the sentry
// image pull secret before the platform adopts CoreDNS. sentry never updates an
// existing ServiceAccount, so the chart cannot add the secret itself, and nodes
// without registry credentials could not pull the chart's CoreDNS image. A new
// cluster has no such ServiceAccount; the chart creates it with the secret.
func ensureCoreDNSPullSecret(ctx context.Context, remoteClient client.Client) error {
	sentry := &corev1.ServiceAccount{}
	if err := remoteClient.Get(ctx, client.ObjectKey{Namespace: kubeOvnAppReleaseNamespace, Name: "sentry"}, sentry); err != nil {
		return client.IgnoreNotFound(err)
	}
	if len(sentry.ImagePullSecrets) == 0 || sentry.ImagePullSecrets[0].Name == "" {
		return nil
	}
	serviceAccount := &corev1.ServiceAccount{}
	if err := remoteClient.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: "coredns"}, serviceAccount); err != nil {
		return client.IgnoreNotFound(err)
	}
	secret := sentry.ImagePullSecrets[0]
	for _, existing := range serviceAccount.ImagePullSecrets {
		if existing.Name == secret.Name {
			return nil
		}
	}
	patchBase := serviceAccount.DeepCopy()
	serviceAccount.ImagePullSecrets = append(serviceAccount.ImagePullSecrets, secret)
	return remoteClient.Patch(ctx, serviceAccount, client.MergeFrom(patchBase))
}

// coreAddonsHandoffCheck is what stands between a cluster and its handoff.
type coreAddonsHandoffCheck struct {
	// blockers need someone to act before the platform can take over.
	blockers []string
	// waiting is something that settles by itself.
	waiting string
	// existingKubeOvn is set when this provider installed kube-ovn already.
	existingKubeOvn bool
}

// coreAddonsHandoffCheck finds what keeps the platform from taking over
// kube-ovn and CoreDNS. A new cluster has neither a kube-ovn AppRelease nor a
// kubeadm CoreDNS and has nothing in the way.
func (r *clusterReconciler) coreAddonsHandoffCheck(ctx context.Context, cluster *clusterv1.Cluster, remoteClient client.Client, clusterVersion string) (coreAddonsHandoffCheck, error) {
	podCIDR := firstCIDRBlock(cluster.Spec.ClusterNetwork, true)
	serviceCIDR := firstCIDRBlock(cluster.Spec.ClusterNetwork, false)
	var check coreAddonsHandoffCheck

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(appReleaseGVK)
	err := remoteClient.Get(ctx, client.ObjectKey{Namespace: kubeOvnAppReleaseNamespace, Name: kubeOvnAppReleaseName}, app)
	switch {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
	case err != nil:
		return check, pkgerrors.Wrap(err, "failed to get the kube-ovn AppRelease")
	default:
		target, err := r.kubeOvnTargetVersion(ctx, clusterVersion)
		if err != nil {
			return check, err
		}
		check.existingKubeOvn = true
		check.blockers = append(check.blockers, kubeOvnAppReleaseBlockers(app, target, clusterVersion, podCIDR, serviceCIDR,
			cluster.Annotations[legacyKubeOvnJoinCIDRAnnotation], handoffNetworkDevice(cluster))...)
		if !appReleaseSyncedAndHealthy(app) {
			check.waiting = "waiting for the kube-ovn AppRelease to be synced and healthy"
		}
	}

	coreDNSBlockers, err := coreDNSHandoffBlockers(ctx, remoteClient, serviceCIDR)
	if err != nil {
		return check, err
	}
	check.blockers = append(check.blockers, coreDNSBlockers...)

	// Taking over a running kube-ovn rolls it, so the network of the management
	// cluster has to be healthy first. A new global cluster has no kube-ovn to
	// roll, and its nodes become ready only once the platform has installed it.
	if check.existingKubeOvn && cluster.Name == globalClusterName {
		globalBlockers, err := globalHandoffBlockers(ctx, remoteClient)
		if err != nil {
			return check, err
		}
		check.blockers = append(check.blockers, globalBlockers...)
	}
	return check, nil
}

// machinesRollingOut names the KubeadmControlPlane or MachineDeployment of the
// cluster that is rolling out, if any.
func (r *clusterReconciler) machinesRollingOut(ctx context.Context, cluster *clusterv1.Cluster) (string, error) {
	kcp, err := r.kubeadmControlPlaneForCluster(ctx, cluster)
	if err != nil && !apierrors.IsNotFound(pkgerrors.Cause(err)) {
		return "", err
	}
	if kcp != nil && rollingOut(kcp.Spec.Replicas, kcp.Status.Replicas, kcp.Status.UpdatedReplicas, kcp.Status.UnavailableReplicas) {
		return fmt.Sprintf("KubeadmControlPlane %s is rolling out", kcp.Name), nil
	}
	machineDeployments := &clusterv1.MachineDeploymentList{}
	if err := r.Client.List(ctx, machineDeployments, client.InNamespace(cluster.Namespace), client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return "", pkgerrors.Wrap(err, "failed to list MachineDeployments")
	}
	for i := range machineDeployments.Items {
		md := &machineDeployments.Items[i]
		if rollingOut(md.Spec.Replicas, md.Status.Replicas, md.Status.UpdatedReplicas, md.Status.UnavailableReplicas) {
			return fmt.Sprintf("MachineDeployment %s is rolling out", md.Name), nil
		}
	}
	return "", nil
}

func rollingOut(desired *int32, replicas, updated, unavailable int32) bool {
	return (desired != nil && *desired != replicas) || updated != replicas || unavailable > 0
}

// kubeOvnUpgradableTo reports whether the platform can take kube-ovn from
// current to target: the same major and not a downgrade. Like the platform's
// upgrade controller, cluster-transformer upgrades to the target in one step;
// which source versions may upgrade is the upgrade policy's concern, checked
// when the cluster upgrades.
func kubeOvnUpgradableTo(current, target string) bool {
	c, err := utilversion.ParseGeneric(current)
	if err != nil {
		return false
	}
	t, err := utilversion.ParseGeneric(target)
	if err != nil {
		return false
	}
	return c.Major() == t.Major() && !c.GreaterThan(t)
}

// handoffNetworkDevice is the interface the platform renders for kube-ovn
// after the handoff: the cluster's network-device annotation, or else the one
// this provider renders, which the handoff writes into the annotation.
func handoffNetworkDevice(cluster *clusterv1.Cluster) string {
	if device := cluster.Annotations[kubeOvnNetworkDeviceAnnotation]; device != "" {
		return device
	}
	return kubeOvnInterface
}

// kubeOvnAppReleaseBlockers checks that the platform can take over the release
// this provider wrote: the same chart and network, at the version the platform
// installs or an older one, which cluster-transformer upgrades in the same
// release update. The values it renders on top differ only in defaults such as
// TLS settings. This provider renders no VLAN or underlay values, so neither
// does the platform as long as the cluster carries no such annotation; the
// interface it renders is the one the handoff writes.
//
// Below v4.4 the same chart (cpaas-kube-ovn) was published at
// acp/chart-cpaas-kube-ovn. Moving a release to acp/chart-kube-ovn is an
// in-place upgrade of release cpaas-kube-ovn: this provider does it when it
// renders v4.4 or later, and the platform does it for the kube-ovn it installs
// itself. A cluster upgraded from v4.3, or one whose kube-ovn stayed below v4.4,
// is therefore handed over on the legacy chart too.
func kubeOvnAppReleaseBlockers(app *unstructured.Unstructured, target, clusterVersion, podCIDR, serviceCIDR, joinCIDR, networkDevice string) []string {
	charts, _, _ := unstructured.NestedSlice(app.Object, "spec", "source", "charts")
	if len(charts) == 0 {
		return []string{"the kube-ovn AppRelease has no chart"}
	}
	chart, _ := charts[0].(map[string]interface{})
	chartName, _ := chart["name"].(string)
	revision, _ := chart["targetRevision"].(string)

	var blockers []string
	if chartName != kubeOvnChartNameV44 && chartName != kubeOvnLegacyChartName {
		blockers = append(blockers, fmt.Sprintf("the kube-ovn AppRelease uses %s, not %s", chartName, kubeOvnChartNameV44))
	}
	switch {
	case target == "":
		blockers = append(blockers, fmt.Sprintf("the kube-ovn ModulePlugin has no version for cluster version %s", clusterVersion))
	case revision != target && !kubeOvnUpgradableTo(revision, target):
		blockers = append(blockers, fmt.Sprintf("kube-ovn is at %s, the platform installs %s on %s and does not downgrade it", revision, target, clusterVersion))
	}
	values, _, _ := unstructured.NestedMap(app.Object, "spec", "values")
	for _, field := range []struct {
		path []string
		want string
	}{
		{[]string{"ipv4", "POD_CIDR"}, podCIDR},
		{[]string{"ipv4", "SVC_CIDR"}, serviceCIDR},
		{[]string{"ipv4", "JOIN_CIDR"}, joinCIDR},
		{[]string{"networking", "NETWORK_TYPE"}, "geneve"},
		{[]string{"networking", "NET_STACK"}, "ipv4"},
		{[]string{"networking", "IFACE"}, networkDevice},
		{[]string{"networking", "vlan", "VLAN_INTERFACE_NAME"}, networkDevice},
	} {
		if got := nestedValueString(values, field.path...); got != field.want {
			blockers = append(blockers, fmt.Sprintf("kube-ovn %s is %q, the platform renders %q", strings.Join(field.path, "."), got, field.want))
		}
	}
	return blockers
}

// appReleaseSyncedAndHealthy reports whether sentry has applied the current
// spec of app, installed the chart revision it names and found it healthy.
// Right after the spec changes, the Sync and Health conditions still describe
// the previous revision.
func appReleaseSyncedAndHealthy(app *unstructured.Unstructured) bool {
	if !kubeOvnAppReleaseReadiness(app).ready {
		return false
	}
	charts, _, _ := unstructured.NestedSlice(app.Object, "spec", "source", "charts")
	for _, chart := range charts {
		chartMap, _ := chart.(map[string]interface{})
		name, _ := chartMap["name"].(string)
		revision, _ := chartMap["targetRevision"].(string)
		installed, _, _ := unstructured.NestedString(app.Object, "status", "charts", name, "installedRevision")
		if installed != revision {
			return false
		}
	}
	return true
}

// coreDNSHandoffBlockers checks the CoreDNS kubeadm installed, if any, for what
// the platform chart would not keep: a customised Corefile, or a kube-dns
// Service address other than the one the chart assigns.
func coreDNSHandoffBlockers(ctx context.Context, remoteClient client.Client, serviceCIDR string) ([]string, error) {
	deployment := &appsv1.Deployment{}
	if err := remoteClient.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: "coredns"}, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, pkgerrors.Wrap(err, "failed to get the CoreDNS Deployment")
	}
	var blockers []string

	configMap := &corev1.ConfigMap{}
	if err := remoteClient.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: "coredns"}, configMap); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, pkgerrors.Wrap(err, "failed to get the CoreDNS ConfigMap")
		}
	} else if customisation := corefileCustomisation(configMap.Data["Corefile"]); customisation != "" {
		blockers = append(blockers, "the CoreDNS Corefile is customised ("+customisation+"); move it to the coredns ModuleInfo config first")
	}

	service := &corev1.Service{}
	if err := remoteClient.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: "kube-dns"}, service); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, pkgerrors.Wrap(err, "failed to get the kube-dns Service")
		}
	} else if want := indexedIP(serviceCIDR, 10); service.Spec.ClusterIP != want {
		blockers = append(blockers, fmt.Sprintf("kube-dns has ClusterIP %s, the platform chart assigns %s", service.Spec.ClusterIP, want))
	}
	return blockers, nil
}

// globalHandoffBlockers holds the global cluster to a stricter standard: the
// network of the management cluster must be healthy before kube-ovn rolls.
func globalHandoffBlockers(ctx context.Context, remoteClient client.Client) ([]string, error) {
	var blockers []string
	nodes := &corev1.NodeList{}
	if err := remoteClient.List(ctx, nodes); err != nil {
		return nil, pkgerrors.Wrap(err, "failed to list Nodes")
	}
	for i := range nodes.Items {
		if !nodeReady(&nodes.Items[i]) {
			blockers = append(blockers, fmt.Sprintf("node %s is not ready", nodes.Items[i].Name))
		}
	}
	deployment := &appsv1.Deployment{}
	if err := remoteClient.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: ovnCentralName}, deployment); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, pkgerrors.Wrap(err, "failed to get ovn-central")
		}
	} else {
		replicas := int32(1)
		if deployment.Spec.Replicas != nil {
			replicas = *deployment.Spec.Replicas
		}
		if deployment.Status.ReadyReplicas < replicas {
			blockers = append(blockers, fmt.Sprintf("ovn-central has %d of %d replicas ready", deployment.Status.ReadyReplicas, replicas))
		}
	}
	return blockers, nil
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// handOverCoreAddons writes the network facts the platform renders kube-ovn
// from, then the cni-type label, in one patch so that cluster-transformer never
// sees the label without them. The interface this provider renders goes into
// the network-device annotation unless the cluster already names one.
func (r *clusterReconciler) handOverCoreAddons(ctx context.Context, cluster *clusterv1.Cluster) error {
	patchBase := cluster.DeepCopy()
	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	if cluster.Labels == nil {
		cluster.Labels = map[string]string{}
	}
	if joinCIDR := cluster.Annotations[legacyKubeOvnJoinCIDRAnnotation]; joinCIDR != "" {
		cluster.Annotations[kubeOvnJoinCIDRAnnotation] = joinCIDR
	}
	cluster.Annotations[kubeOvnNetworkDeviceAnnotation] = handoffNetworkDevice(cluster)
	cluster.Annotations[networkTypeAnnotation] = networkTypeKubeOvn
	cluster.Labels[cniTypeLabel] = networkTypeKubeOvn
	if err := r.Client.Patch(ctx, cluster, client.MergeFrom(patchBase)); err != nil {
		return pkgerrors.Wrap(err, "failed to hand kube-ovn and CoreDNS over to the platform")
	}
	return nil
}

// indexedIP returns the index-th address of cidr, as cluster-transformer does
// when it assigns the kube-dns ClusterIP (the tenth one).
func indexedIP(cidr string, index int64) string {
	_, network, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return ""
	}
	base := network.IP
	if v4 := base.To4(); v4 != nil {
		base = v4
	}
	sum := new(big.Int).Add(new(big.Int).SetBytes(base), big.NewInt(index))
	bytes := sum.Bytes()
	ip := make(net.IP, len(base))
	copy(ip[len(ip)-len(bytes):], bytes)
	if !network.Contains(ip) {
		return ""
	}
	return ip.String()
}

// corefileCustomisation describes how a Corefile departs from the kubeadm
// default, or returns "" when it does not.
func corefileCustomisation(corefile string) string {
	depth := 0
	var serverBlocks []string
	var extraPlugins []string
	for _, line := range strings.Split(corefile, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		switch depth {
		case 0:
			if strings.HasSuffix(line, "{") {
				serverBlocks = append(serverBlocks, strings.TrimSpace(strings.TrimSuffix(line, "{")))
			}
		case 1:
			if fields[0] != "}" && !slices.Contains(coreDNSDefaultPlugins, fields[0]) {
				extraPlugins = append(extraPlugins, fields[0])
			}
		}
		depth += strings.Count(line, "{") - strings.Count(line, "}")
	}
	var parts []string
	if len(serverBlocks) != 1 || serverBlocks[0] != ".:53" {
		parts = append(parts, "server blocks "+strings.Join(serverBlocks, ", "))
	}
	if len(extraPlugins) > 0 {
		parts = append(parts, "plugins "+strings.Join(extraPlugins, ", "))
	}
	return strings.Join(parts, "; ")
}

// nestedValueString reads a string from AppRelease values, which are JSON, so
// nested sections are maps.
func nestedValueString(data map[string]interface{}, path ...string) string {
	var current interface{} = data
	for _, key := range path {
		section, ok := current.(map[string]interface{})
		if !ok {
			return ""
		}
		current = section[key]
	}
	if current == nil {
		return ""
	}
	if s, ok := current.(string); ok {
		return s
	}
	return fmt.Sprint(current)
}
