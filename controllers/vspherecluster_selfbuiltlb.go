/*
Copyright 2024 The Kubernetes Authors.

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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	pkgerrors "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta2conditions "sigs.k8s.io/cluster-api/util/conditions/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	infrautilv1 "sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

const (
	// aliveModuleName is the platform module that provides the self-built control
	// plane load balancer (keepalived + IPVS).
	aliveModuleName = "alive"
	// aliveModuleDisplayName is the display name used when deriving the ModuleInfo
	// name; it has to match what the platform would have generated.
	aliveModuleDisplayName = "alive"

	// aliveAppReleaseNamespace and aliveAppReleaseName locate the AppRelease that
	// cluster-transformer renders from the ModuleInfo, inside the workload cluster.
	aliveAppReleaseNamespace = "cpaas-system"
	aliveAppReleaseName      = "alive"

	// alivePodNamespace and alivePodAppLabel locate the alive static pods that hold
	// the VIP on each control plane node.
	alivePodNamespace = "kube-system"
	alivePodAppLabel  = "alive"

	// aliveHTTPPort and aliveHTTPSPort are alive's own listeners, kept distinct from
	// the apiserver port it fronts.
	aliveHTTPPort         = 11780
	aliveHTTPSPort        = 11781
	globalClusterName     = "global"
	globalAliveHTTPPort   = 80
	globalAliveHTTPSPort  = 443
	globalAliveExtraPorts = "11443 2379"

	// selfBuiltLBManagedLabel marks a ModuleInfo as owned by this provider. A
	// ModuleInfo without it is never adopted.
	selfBuiltLBManagedLabel = "infrastructure.cluster.x-k8s.io/self-built-lb-managed"

	// kubeProxyConfigHashAnnotation carries the hash of the kube-proxy config.conf
	// this provider wrote, so a config change rolls the DaemonSet. kube-proxy only
	// reads its configuration at startup.
	kubeProxyConfigHashAnnotation = "infrastructure.cluster.x-k8s.io/kube-proxy-config-hash"

	// kubeProxyConfigPatchedAtAnnotation records when this provider last wrote the
	// ConfigMap, so a rewrite rolls the DaemonSet even when the resulting config is
	// byte-identical to the one already stamped.
	kubeProxyConfigPatchedAtAnnotation = "infrastructure.cluster.x-k8s.io/kube-proxy-config-patched-at"

	selfBuiltLBRequeueAfter = 10 * time.Second

	// The deployment status of alive cannot prove that layer 4 traffic is stable, so
	// the VIP is probed end to end before the condition flips to True.
	vipProbeAttempts = 5
	vipProbeInterval = 2 * time.Second
	vipProbeTimeout  = 5 * time.Second
)

var (
	moduleInfoGVK = schema.GroupVersionKind{
		Group:   "cluster.alauda.io",
		Version: "v1alpha1",
		Kind:    "ModuleInfo",
	}
	moduleConfigGVK = schema.GroupVersionKind{
		Group:   "cluster.alauda.io",
		Version: "v1alpha1",
		Kind:    "ModuleConfig",
	}
	clusterModuleGVK = schema.GroupVersionKind{
		Group:   "cluster.alauda.io",
		Version: "v1alpha1",
		Kind:    "ClusterModule",
	}
	platformClusterGVK = schema.GroupVersionKind{
		Group:   "platform.tkestack.io",
		Version: "v1",
		Kind:    "Cluster",
	}
	clusterRegistryClusterGVK = schema.GroupVersionKind{
		Group:   "clusterregistry.k8s.io",
		Version: "v1alpha1",
		Kind:    "Cluster",
	}

	// aliveModulePluginPassthroughLabels are copied from ModulePlugin/alive onto the
	// ModuleInfo, matching what the platform's own installation path produces.
	aliveModulePluginPassthroughLabels = []string{
		"cpaas.io/module-catalog",
		"cpaas.io/product",
	}

	aliveAppReleaseReasons = appReleaseReasons{
		ready:       infrav1.SelfBuiltLoadBalancerReadyReason,
		reconciling: infrav1.SelfBuiltLoadBalancerReconcilingReason,
		notReady:    infrav1.SelfBuiltLoadBalancerNotReadyReason,
	}
)

// reconcileControlPlaneEndpoint keeps spec.controlPlaneEndpoint and
// spec.controlPlaneLoadBalancer describing the same address.
//
// It runs before anything else in reconcileNormal because the endpoint has to be
// settled before the cluster can be bootstrapped at all: the backfill has to land
// on the API server well before any workload client exists. The webhook already
// rejects an inconsistent pair; this is the defensive half of that check for
// objects that predate the webhook or were written while it was unavailable.
func reconcileControlPlaneEndpoint(clusterCtx *capvcontext.ClusterContext) error {
	vsphereCluster := clusterCtx.VSphereCluster
	lb := vsphereCluster.Spec.ControlPlaneLoadBalancer
	if lb == nil {
		return nil
	}

	endpoint := &vsphereCluster.Spec.ControlPlaneEndpoint
	if endpoint.Host == "" && endpoint.Port == 0 {
		endpoint.Host = lb.Host
		endpoint.Port = lb.Port
		return nil
	}

	if endpoint.Host != lb.Host || endpoint.Port != lb.Port {
		return fmt.Errorf("spec.controlPlaneEndpoint %s:%d does not match spec.controlPlaneLoadBalancer %s:%d",
			endpoint.Host, endpoint.Port, lb.Host, lb.Port)
	}
	return nil
}

// ensureSelfBuiltLB drives the provider-managed control plane load balancer: it
// creates the alive ModuleInfo in the management cluster and gates
// SelfBuiltLoadBalancerReady on the whole chain being live, up to and including an
// end-to-end probe of the VIP.
//
// It never gates VSphereCluster.Status.Ready, and it never blocks deletion.
func (r *clusterReconciler) ensureSelfBuiltLB(ctx context.Context, clusterCtx *capvcontext.ClusterContext) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	vsphereCluster := clusterCtx.VSphereCluster
	lb := vsphereCluster.Spec.ControlPlaneLoadBalancer

	if !lb.IsInternal() {
		conditions.Delete(vsphereCluster, infrav1.SelfBuiltLoadBalancerReadyCondition)
		v1beta2conditions.Delete(vsphereCluster, infrav1.VSphereClusterSelfBuiltLoadBalancerReadyV1Beta2Condition)
		return reconcile.Result{}, nil
	}

	cluster := clusterCtx.Cluster
	if cluster == nil {
		return reconcile.Result{}, nil
	}

	// Defensive input validation. The webhook rejects both of these, so reaching
	// them means the object was written without admission and the cluster cannot
	// work: fail loudly instead of installing alive on a VIP that is already taken.
	if err := reconcileControlPlaneEndpoint(clusterCtx); err != nil {
		r.setSelfBuiltLBCondition(vsphereCluster, corev1.ConditionFalse, infrav1.SelfBuiltLoadBalancerInvalidConfigurationReason, clusterv1.ConditionSeverityError, err.Error())
		return reconcile.Result{}, err
	}
	if err := r.validateVIPNotClaimedBySlot(ctx, vsphereCluster); err != nil {
		r.setSelfBuiltLBCondition(vsphereCluster, corev1.ConditionFalse, infrav1.SelfBuiltLoadBalancerInvalidConfigurationReason, clusterv1.ConditionSeverityError, err.Error())
		return reconcile.Result{}, err
	}

	clientset, restConfig, err := r.newRemoteClients(ctx, cluster)
	if err != nil {
		log.Error(err, "Skipping self-built load balancer reconcile because workload cluster client is unavailable")
		return r.selfBuiltLBRequeue(vsphereCluster, err.Error())
	}

	// alive's backend list is generated from the registered control plane Nodes, so
	// the ModuleInfo is only created once they are all there.
	controlPlaneNodeNames, ready, err := r.controlPlaneNodesRegistered(ctx, cluster, clientset)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !ready {
		return r.selfBuiltLBRequeue(vsphereCluster, "waiting for all control plane Nodes to register")
	}

	// kube-proxy in IPVS mode would proxy and announce the VIP itself, fighting
	// keepalived for it. Settle that before alive is installed.
	done, message, err := r.ensureKubeProxyIPVSForVIP(ctx, clientset, lb.Host)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !done {
		return r.selfBuiltLBRequeue(vsphereCluster, message)
	}

	version, pluginLabels, missing, err := r.resolveAliveModule(ctx, cluster.Name, cluster.Namespace)
	if err != nil {
		r.setSelfBuiltLBCondition(vsphereCluster, corev1.ConditionFalse, infrav1.SelfBuiltLoadBalancerInvalidConfigurationReason, clusterv1.ConditionSeverityError, err.Error())
		return reconcile.Result{}, err
	}
	if missing != "" {
		return r.selfBuiltLBRequeue(vsphereCluster, missing)
	}

	moduleInfo, changed, err := r.ensureAliveModuleInfo(ctx, cluster.Name, lb, version, pluginLabels)
	if err != nil {
		r.setSelfBuiltLBCondition(vsphereCluster, corev1.ConditionFalse, infrav1.SelfBuiltLoadBalancerNotReadyReason, clusterv1.ConditionSeverityWarning, err.Error())
		return reconcile.Result{}, err
	}
	if changed {
		return r.selfBuiltLBRequeue(vsphereCluster, "applied alive ModuleInfo, waiting for it to roll out")
	}

	// ModuleInfo status lags behind its spec, so wait for the observed version.
	observedVersion, _, _ := unstructured.NestedString(moduleInfo.Object, "spec", "version")
	if message := moduleInfoReadiness(moduleInfo, observedVersion); message != "" {
		return r.selfBuiltLBRequeue(vsphereCluster, message)
	}
	if phase := moduleInfoPhase(moduleInfo); phase != "Running" {
		log.V(4).Info("Alive ModuleInfo phase is not Running, checking the runtime instead", "ModuleInfo", moduleInfo.GetName(), "phase", phase)
	}

	dc, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return reconcile.Result{}, pkgerrors.Wrap(err, "failed to create dynamic client for workload cluster")
	}
	appRelease, err := dc.Resource(appReleaseGVR).Namespace(aliveAppReleaseNamespace).Get(ctx, aliveAppReleaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return r.selfBuiltLBRequeue(vsphereCluster, "waiting for cluster-transformer to render the alive AppRelease")
	}
	if err != nil {
		return reconcile.Result{}, pkgerrors.Wrap(err, "failed to get alive AppRelease")
	}
	if readiness := appReleaseReadiness(appRelease, aliveModuleName, aliveAppReleaseReasons); !readiness.ready {
		r.setSelfBuiltLBCondition(vsphereCluster, corev1.ConditionFalse, readiness.reason, clusterv1.ConditionSeverityInfo, readiness.message)
		return reconcile.Result{RequeueAfter: selfBuiltLBRequeueAfter}, nil
	}

	podsReady, message, err := alivePodsReadyOnNodes(ctx, clientset, controlPlaneNodeNames)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !podsReady {
		// A control plane node set that changed under alive (replacement, scale)
		// needs the AppRelease re-rendered; the identity string is what triggers it.
		if err := ensureControlPlaneNodeIdentity(ctx, clientset, dc, appRelease); err != nil {
			return reconcile.Result{}, err
		}
		return r.selfBuiltLBRequeue(vsphereCluster, message)
	}

	if err := probeVIP(ctx, restConfig, lb.Host, lb.Port); err != nil {
		return r.selfBuiltLBRequeue(vsphereCluster, err.Error())
	}

	r.setSelfBuiltLBCondition(vsphereCluster, corev1.ConditionTrue, infrav1.SelfBuiltLoadBalancerReadyReason, clusterv1.ConditionSeverityInfo, "")
	return reconcile.Result{}, nil
}

// selfBuiltLBRequeue marks the condition False with the reconciling reason and asks
// for the standard retry, matching the cadence of the kube-ovn path.
func (r *clusterReconciler) selfBuiltLBRequeue(vsphereCluster *infrav1.VSphereCluster, message string) (reconcile.Result, error) {
	r.setSelfBuiltLBCondition(vsphereCluster, corev1.ConditionFalse, infrav1.SelfBuiltLoadBalancerReconcilingReason, clusterv1.ConditionSeverityInfo, message)
	return reconcile.Result{RequeueAfter: selfBuiltLBRequeueAfter}, nil
}

func (r *clusterReconciler) setSelfBuiltLBCondition(vsphereCluster *infrav1.VSphereCluster, status corev1.ConditionStatus, reason string, severity clusterv1.ConditionSeverity, message string) {
	r.setDualCondition(vsphereCluster,
		infrav1.SelfBuiltLoadBalancerReadyCondition,
		infrav1.VSphereClusterSelfBuiltLoadBalancerReadyV1Beta2Condition,
		"self-built load balancer",
		status, reason, severity, message)
}

// validateVIPNotClaimedBySlot repeats the webhook's VIP conflict check against the
// live pool list, so a pool slot added after the cluster was admitted is still
// caught before alive is installed.
func (r *clusterReconciler) validateVIPNotClaimedBySlot(ctx context.Context, vsphereCluster *infrav1.VSphereCluster) error {
	lb := vsphereCluster.Spec.ControlPlaneLoadBalancer
	clusterName := vsphereCluster.Labels[clusterv1.ClusterNameLabel]
	if clusterName == "" {
		clusterName = vsphereCluster.Name
	}

	pools := &infrav1.VSphereMachineConfigPoolList{}
	if err := r.Client.List(ctx, pools, client.InNamespace(vsphereCluster.Namespace)); err != nil {
		return pkgerrors.Wrap(err, "failed to list VSphereMachineConfigPools for VIP conflict check")
	}
	if claim, found := infrautilv1.FindConfigPoolSlotClaimingIP(pools.Items, clusterName, lb.Host); found {
		return fmt.Errorf("control plane VIP %s conflicts with the address of slot %q in VSphereMachineConfigPool %q",
			lb.Host, claim.Hostname, claim.PoolName)
	}
	return nil
}

// resolveAliveModule picks the alive version to install and reads the labels the
// ModuleInfo has to carry.
//
// The two failure modes are deliberately different: a missing platform projection
// object (ClusterModule, platform Cluster, clusterregistry Cluster) is state that
// appears on its own and is reported through missing so the caller requeues, while
// a missing alive artifact means the offline package is incomplete and is returned
// as an error.
//
// clusterNamespace is the namespace of the Cluster: unlike the rest of the platform
// objects read here, the clusterregistry Cluster is namespaced.
func (r *clusterReconciler) resolveAliveModule(ctx context.Context, clusterName, clusterNamespace string) (version string, pluginLabels map[string]string, missing string, err error) {
	// Once the canonical ModuleInfo exists, its version is platform-owned. Do not
	// resolve ModulePlugin/ModuleConfig again and accidentally wait for or select a
	// newer artifact before the platform has upgraded the existing minfo.
	existing := newUnstructured(moduleInfoGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Name: aliveModuleInfoName(clusterName)}, existing); err == nil {
		version, found, err := unstructured.NestedString(existing.Object, "spec", "version")
		if err != nil {
			return "", nil, "", pkgerrors.Wrap(err, "failed to read existing alive ModuleInfo spec.version")
		}
		if !found || version == "" {
			return "", nil, "", fmt.Errorf("existing alive ModuleInfo %q has no spec.version", existing.GetName())
		}
		return version, nil, "", nil
	} else if !apierrors.IsNotFound(err) {
		return "", nil, "", pkgerrors.Wrap(err, "failed to get existing alive ModuleInfo")
	}

	for _, projection := range []struct {
		gvk schema.GroupVersionKind
		key client.ObjectKey
	}{
		{platformClusterGVK, client.ObjectKey{Name: clusterName}},
		{clusterRegistryClusterGVK, client.ObjectKey{Namespace: clusterNamespace, Name: clusterName}},
	} {
		if _, missing, err := r.getPlatformObject(ctx, projection.gvk, projection.key); err != nil || missing != "" {
			return "", nil, missing, err
		}
	}

	clusterModule, missing, err := r.getPlatformObject(ctx, clusterModuleGVK, client.ObjectKey{Name: clusterName})
	if err != nil || missing != "" {
		return "", nil, missing, err
	}
	clusterModuleVersion, _, err := unstructured.NestedString(clusterModule.Object, "spec", "version")
	if err != nil {
		return "", nil, "", pkgerrors.Wrapf(err, "failed to read ClusterModule %q spec.version", clusterName)
	}

	modulePlugin := newUnstructured(modulePluginGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Name: aliveModuleName}, modulePlugin); err != nil {
		return "", nil, "", pkgerrors.Wrapf(err, "failed to get ModulePlugin %q", aliveModuleName)
	}

	pluginLabels = map[string]string{}
	for _, key := range aliveModulePluginPassthroughLabels {
		if value, ok := modulePlugin.GetLabels()[key]; ok {
			pluginLabels[key] = value
		}
	}

	version, err = aliveVersionFor(modulePlugin, clusterModuleVersion, r.ControllerManagerContext.PluginAliveVersion)
	if err != nil {
		return "", nil, "", err
	}

	// The version override only selects a version; the artifact still has to exist
	// and be deployable.
	moduleConfigName := fmt.Sprintf("%s-%s", aliveModuleName, version)
	moduleConfig := newUnstructured(moduleConfigGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Name: moduleConfigName}, moduleConfig); err != nil {
		return "", nil, "", pkgerrors.Wrapf(err, "failed to get ModuleConfig %q", moduleConfigName)
	}
	readyForDeploy, _, err := unstructured.NestedBool(moduleConfig.Object, "status", "readyForDeploy")
	if err != nil {
		return "", nil, "", pkgerrors.Wrapf(err, "failed to read ModuleConfig %q status.readyForDeploy", moduleConfigName)
	}
	if !readyForDeploy {
		return "", nil, "", fmt.Errorf("ModuleConfig %q is not ready for deploy", moduleConfigName)
	}

	return version, pluginLabels, "", nil
}

// aliveVersionFor resolves the alive version: the provider-level override wins,
// then the version the ModulePlugin maps this cluster's version to, then the
// plugin's latest version.
func aliveVersionFor(modulePlugin *unstructured.Unstructured, clusterModuleVersion, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	if clusterModuleVersion != "" {
		targetVersions, found, err := unstructured.NestedMap(modulePlugin.Object, "status", "targetClusterVersions")
		if err != nil {
			return "", pkgerrors.Wrap(err, "failed to read ModulePlugin alive status.targetClusterVersions")
		}
		if found {
			target, ok := targetVersions[clusterModuleVersion]
			if ok {
				targetStatus, ok := target.(map[string]interface{})
				if !ok {
					return "", fmt.Errorf("invalid ModulePlugin alive status.targetClusterVersions[%q]: expected an object", clusterModuleVersion)
				}
				version, found, err := unstructured.NestedString(targetStatus, "version")
				if err != nil {
					return "", pkgerrors.Wrapf(err, "failed to read ModulePlugin alive status.targetClusterVersions[%q].version", clusterModuleVersion)
				}
				if !found || version == "" {
					return "", fmt.Errorf("invalid ModulePlugin alive status.targetClusterVersions[%q]: missing version", clusterModuleVersion)
				}
				return version, nil
			}
		}
	}

	version, _, err := unstructured.NestedString(modulePlugin.Object, "status", "latestVersion")
	if err != nil {
		return "", pkgerrors.Wrap(err, "failed to read ModulePlugin alive status.latestVersion")
	}
	if version == "" {
		return "", fmt.Errorf("cannot resolve an alive version: ModulePlugin %q has no latestVersion and no mapping for cluster version %q", aliveModuleName, clusterModuleVersion)
	}
	return version, nil
}

// getPlatformObject reads a platform object. A missing object (or a missing CRD, in
// an environment where the platform is not installed yet) is reported through
// missing rather than as an error, because the caller waits for the platform to
// converge instead of failing the cluster.
func (r *clusterReconciler) getPlatformObject(ctx context.Context, gvk schema.GroupVersionKind, key client.ObjectKey) (*unstructured.Unstructured, string, error) {
	obj := newUnstructured(gvk)
	if err := r.Client.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, fmt.Sprintf("waiting for %s %q", gvk.Kind, key), nil
		}
		return nil, "", pkgerrors.Wrapf(err, "failed to get %s %q", gvk.Kind, key)
	}
	return obj, "", nil
}

func newUnstructured(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	return obj
}

// aliveModuleInfoName reproduces the platform's ModuleInfo naming
// (GenerateModuleInfoName(clusterName, "alive", "alive")) so provider-created and
// platform-created objects never collide under different names.
func aliveModuleInfoName(clusterName string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s", clusterName, aliveModuleName, aliveModuleDisplayName)))
	return fmt.Sprintf("%s-%s", clusterName, hex.EncodeToString(sum[:])[:32])
}

// aliveModuleInfoIdentityLabels are the labels the platform keys off: the module
// type selects the plugin path in cluster-transformer's ModuleInfo webhook, and
// that webhook then resolves ModuleConfig "<module-name>-<spec.version>". They are
// reconciled on every pass.
func aliveModuleInfoIdentityLabels(clusterName string) map[string]string {
	return map[string]string{
		"cpaas.io/cluster-name": clusterName,
		"cpaas.io/module-name":  aliveModuleName,
		"cpaas.io/module-type":  "plugin",
		selfBuiltLBManagedLabel: "true",
	}
}

// aliveModuleInfoLabels adds the ModulePlugin labels the platform copies onto a
// ModuleInfo it creates itself. They are only written at creation: cluster-
// transformer's mutating webhook rewrites cpaas.io/product from the ModuleConfig,
// and reasserting our own value on every reconcile would just fight it.
func aliveModuleInfoLabels(clusterName string, pluginLabels map[string]string) map[string]string {
	labelSet := map[string]string{}
	for key, value := range pluginLabels {
		labelSet[key] = value
	}
	for key, value := range aliveModuleInfoIdentityLabels(clusterName) {
		labelSet[key] = value
	}
	return labelSet
}

// aliveModuleInfoConfig is the provider-owned part of ModuleInfo.spec.config.
//
// The ports and the VRID are numbers, matching what cluster-transformer writes
// for the platform-managed alive installation (its vrid comes from an *int32 and
// its ports from int fields); extraPorts is a string there and stays one here.
//
// masterIPs is deliberately absent: alive's values template derives the backend
// list from the workload cluster's control plane Node InternalIPs, and a second
// source of truth would only drift.
func aliveModuleInfoConfigForCluster(clusterName string, lb *infrav1.ControlPlaneLoadBalancer) map[string]interface{} {
	httpPort, httpsPort, extraPorts := alivePortsForCluster(clusterName)
	return map[string]interface{}{
		"vip":           lb.Host,
		"vrid":          int64(lb.VRID),
		"apiserverPort": int64(lb.Port),
		"httpPort":      int64(httpPort),
		"httpsPort":     int64(httpsPort),
		"extraPorts":    extraPorts,
		"interface":     lb.Interface,
	}
}

func aliveModuleInfoConfig(lb *infrav1.ControlPlaneLoadBalancer) map[string]interface{} {
	return aliveModuleInfoConfigForCluster("", lb)
}

func alivePortsForCluster(clusterName string) (int32, int32, string) {
	if strings.EqualFold(clusterName, globalClusterName) {
		return globalAliveHTTPPort, globalAliveHTTPSPort, globalAliveExtraPorts
	}
	return aliveHTTPPort, aliveHTTPSPort, ""
}

// ensureAliveModuleInfo creates the alive ModuleInfo and leaves an existing one
// unchanged. After creation, ModuleInfo version and configuration lifecycle belong
// to the platform, matching the platform-managed alive installation path.
func (r *clusterReconciler) ensureAliveModuleInfo(ctx context.Context, clusterName string, lb *infrav1.ControlPlaneLoadBalancer, version string, pluginLabels map[string]string) (*unstructured.Unstructured, bool, error) {
	name := aliveModuleInfoName(clusterName)
	desiredLabels := aliveModuleInfoLabels(clusterName, pluginLabels)
	desiredConfig := aliveModuleInfoConfigForCluster(clusterName, lb)

	moduleInfo := newUnstructured(moduleInfoGVK)
	err := r.Client.Get(ctx, client.ObjectKey{Name: name}, moduleInfo)
	if apierrors.IsNotFound(err) {
		moduleInfo = newUnstructured(moduleInfoGVK)
		moduleInfo.SetName(name)
		moduleInfo.SetLabels(desiredLabels)
		moduleInfo.SetAnnotations(map[string]string{"cpaas.io/display-name": aliveModuleDisplayName})
		if err := unstructured.SetNestedField(moduleInfo.Object, version, "spec", "version"); err != nil {
			return nil, false, err
		}
		if err := unstructured.SetNestedMap(moduleInfo.Object, desiredConfig, "spec", "config"); err != nil {
			return nil, false, err
		}
		if err := r.Client.Create(ctx, moduleInfo); err != nil {
			return nil, false, pkgerrors.Wrapf(err, "failed to create ModuleInfo %q", name)
		}
		ctrl.LoggerFrom(ctx).Info("Created alive ModuleInfo", "ModuleInfo", name, "version", version)
		return moduleInfo, true, nil
	}
	if err != nil {
		return nil, false, pkgerrors.Wrapf(err, "failed to get ModuleInfo %q", name)
	}

	ctrl.LoggerFrom(ctx).Info("Alive ModuleInfo already exists; leaving lifecycle management to the platform", "ModuleInfo", name)
	return moduleInfo, false, nil
}

// moduleInfoReadiness returns an empty string once the ModuleInfo has observed the
// requested version, or the reason it has not.
//
// Only the observed version gates: it is what tells us the platform has picked up
// our spec, and everything after this point (AppRelease conditions, alive pods, the
// VIP probe) measures the actual runtime. status.phase is reported separately and
// deliberately does not block, because it lags behind a healthy installation.
func moduleInfoReadiness(moduleInfo *unstructured.Unstructured, version string) string {
	observedVersion, _, err := unstructured.NestedString(moduleInfo.Object, "status", "version")
	if err != nil {
		return fmt.Sprintf("failed to read alive ModuleInfo status.version: %v", err)
	}
	if observedVersion != version {
		return fmt.Sprintf("waiting for alive ModuleInfo to observe version %q", version)
	}
	return ""
}

// moduleInfoPhase reports the ModuleInfo phase for logging.
func moduleInfoPhase(moduleInfo *unstructured.Unstructured) string {
	phase, _, _ := unstructured.NestedString(moduleInfo.Object, "status", "phase")
	return phase
}

// ensureKubeProxyIPVSForVIP stops kube-proxy from competing with keepalived for the
// VIP. In IPVS mode kube-proxy answers ARP for and programs virtual servers on
// addresses it manages, which would fight alive over the same address.
//
// It reports done only when the configuration is in place and the DaemonSet has
// finished rolling; kube-proxy reads its configuration once at startup, so the
// ConfigMap change is paired with a pod template annotation that rolls it.
func (r *clusterReconciler) ensureKubeProxyIPVSForVIP(ctx context.Context, clientset kubernetes.Interface, vip string) (bool, string, error) {
	configMap, err := clientset.CoreV1().ConfigMaps(alivePodNamespace).Get(ctx, "kube-proxy", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, "", nil
	}
	if err != nil {
		return false, "", pkgerrors.Wrap(err, "failed to get kube-proxy ConfigMap")
	}

	rawConfig, ok := configMap.Data["config.conf"]
	if !ok {
		return true, "", nil
	}

	updatedConfig, changed, err := kubeProxyConfigWithVIPExcluded(rawConfig, vip)
	if err != nil {
		return false, "", err
	}
	if updatedConfig == "" {
		// Not IPVS mode; kube-proxy does not touch the VIP.
		return true, "", nil
	}

	if changed {
		configMap.Data["config.conf"] = updatedConfig
		if _, err := clientset.CoreV1().ConfigMaps(alivePodNamespace).Update(ctx, configMap, metav1.UpdateOptions{}); err != nil {
			return false, "", pkgerrors.Wrap(err, "failed to update kube-proxy ConfigMap")
		}
		ctrl.LoggerFrom(ctx).Info("Excluded control plane VIP from kube-proxy IPVS", "vip", vip)
	}

	daemonSet, err := clientset.AppsV1().DaemonSets(alivePodNamespace).Get(ctx, "kube-proxy", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, "", nil
	}
	if err != nil {
		return false, "", pkgerrors.Wrap(err, "failed to get kube-proxy DaemonSet")
	}

	configHash := sha256.Sum256([]byte(updatedConfig))
	desiredHash := hex.EncodeToString(configHash[:])[:32]
	rollNeeded := daemonSet.Spec.Template.Annotations[kubeProxyConfigHashAnnotation] != desiredHash
	if rollNeeded || changed {
		if daemonSet.Spec.Template.Annotations == nil {
			daemonSet.Spec.Template.Annotations = map[string]string{}
		}
		daemonSet.Spec.Template.Annotations[kubeProxyConfigHashAnnotation] = desiredHash
		if changed {
			// The hash alone cannot roll pods that started from a configuration
			// somebody else reverted and we just wrote back: it is unchanged from
			// what we last stamped. Writing the ConfigMap therefore always rolls.
			daemonSet.Spec.Template.Annotations[kubeProxyConfigPatchedAtAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if _, err := clientset.AppsV1().DaemonSets(alivePodNamespace).Update(ctx, daemonSet, metav1.UpdateOptions{}); err != nil {
			return false, "", pkgerrors.Wrap(err, "failed to roll the kube-proxy DaemonSet after the config change")
		}
		return false, "rolling kube-proxy to pick up the IPVS configuration", nil
	}

	status := daemonSet.Status
	if status.ObservedGeneration != daemonSet.Generation ||
		status.UpdatedNumberScheduled != status.DesiredNumberScheduled ||
		status.NumberAvailable != status.DesiredNumberScheduled {
		return false, "waiting for the kube-proxy DaemonSet rollout to complete", nil
	}
	return true, "", nil
}

// kubeProxyConfigWithVIPExcluded returns the kube-proxy config.conf with strictARP
// enabled and the VIP excluded, plus whether that differs from the input. It
// returns an empty string when kube-proxy is not in IPVS mode and nothing has to
// change.
func kubeProxyConfigWithVIPExcluded(rawConfig, vip string) (string, bool, error) {
	config := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(rawConfig), &config); err != nil {
		return "", false, pkgerrors.Wrap(err, "failed to parse the kube-proxy config.conf")
	}

	mode, _, err := unstructured.NestedString(config, "mode")
	if err != nil {
		return "", false, pkgerrors.Wrap(err, "failed to read the kube-proxy mode")
	}
	if mode != "ipvs" {
		return "", false, nil
	}

	ipvs, _, err := unstructured.NestedMap(config, "ipvs")
	if err != nil {
		return "", false, pkgerrors.Wrap(err, "failed to read the kube-proxy ipvs configuration")
	}
	if ipvs == nil {
		ipvs = map[string]interface{}{}
	}

	changed := false
	if strictARP, _ := ipvs["strictARP"].(bool); !strictARP {
		ipvs["strictARP"] = true
		changed = true
	}

	vipCIDR := vip + "/32"
	excludeCIDRs, _, err := unstructured.NestedStringSlice(ipvs, "excludeCIDRs")
	if err != nil {
		return "", false, pkgerrors.Wrap(err, "failed to read the kube-proxy ipvs.excludeCIDRs")
	}
	if !containsString(excludeCIDRs, vipCIDR) {
		excludeCIDRs = append(excludeCIDRs, vipCIDR)
		changed = true
	}
	// Round-tripping through interface{} keeps SetNestedMap's deep-copy check happy.
	excluded := make([]interface{}, 0, len(excludeCIDRs))
	for _, cidr := range excludeCIDRs {
		excluded = append(excluded, cidr)
	}
	ipvs["excludeCIDRs"] = excluded

	if !changed {
		return rawConfig, false, nil
	}
	if err := unstructured.SetNestedMap(config, ipvs, "ipvs"); err != nil {
		return "", false, err
	}
	updated, err := yaml.Marshal(config)
	if err != nil {
		return "", false, pkgerrors.Wrap(err, "failed to render the kube-proxy config.conf")
	}
	return string(updated), true, nil
}

func containsString(values []string, value string) bool {
	for _, current := range values {
		if current == value {
			return true
		}
	}
	return false
}

// alivePodsReadyOnNodes reports whether every control plane node runs a Ready alive
// pod.
func alivePodsReadyOnNodes(ctx context.Context, clientset kubernetes.Interface, controlPlaneNodeNames []string) (bool, string, error) {
	pods, err := clientset.CoreV1().Pods(alivePodNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{"app": alivePodAppLabel}).String(),
	})
	if err != nil {
		return false, "", pkgerrors.Wrap(err, "failed to list alive pods")
	}

	readyNodes := map[string]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" || !podReady(pod) {
			continue
		}
		readyNodes[pod.Spec.NodeName] = true
	}

	missing := make([]string, 0, len(controlPlaneNodeNames))
	for _, node := range controlPlaneNodeNames {
		if !readyNodes[node] {
			missing = append(missing, node)
		}
	}
	if len(missing) > 0 {
		return false, fmt.Sprintf("waiting for a Ready alive pod on control plane Nodes %s", strings.Join(missing, ", ")), nil
	}
	return true, "", nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ensureControlPlaneNodeIdentity keeps global.controlPlaneNodeIdentity on the alive
// AppRelease in step with the current control plane node set. alive has to be
// reinstalled when the set changes (node replacement, scale), and the identity
// string is what makes the AppRelease change at all.
func ensureControlPlaneNodeIdentity(ctx context.Context, clientset kubernetes.Interface, dc dynamic.Interface, appRelease *unstructured.Unstructured) error {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			"node-role.kubernetes.io/control-plane": "",
		}).String(),
	})
	if err != nil {
		return pkgerrors.Wrap(err, "failed to list control plane Nodes")
	}

	identities := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		identities = append(identities, fmt.Sprintf("%s/%s/%s", node.Name, node.UID, node.Spec.ProviderID))
	}
	sort.Strings(identities)
	identity := strings.Join(identities, ",")

	current, _, err := unstructured.NestedString(appRelease.Object, "spec", "values", "global", "controlPlaneNodeIdentity")
	if err != nil {
		return pkgerrors.Wrap(err, "failed to read the alive AppRelease controlPlaneNodeIdentity")
	}
	if current == identity {
		return nil
	}

	patched := appRelease.DeepCopy()
	if err := unstructured.SetNestedField(patched.Object, identity, "spec", "values", "global", "controlPlaneNodeIdentity"); err != nil {
		return err
	}
	if _, err := dc.Resource(appReleaseGVR).Namespace(patched.GetNamespace()).Update(ctx, patched, metav1.UpdateOptions{}); err != nil {
		return pkgerrors.Wrap(err, "failed to update the alive AppRelease controlPlaneNodeIdentity")
	}
	ctrl.LoggerFrom(ctx).Info("Updated alive AppRelease control plane node identity")
	return nil
}

// probeVIP checks the apiserver end to end through the VIP.
//
// The deployment status of alive only proves the components are installed, not
// that layer 4 traffic works (a missing IPVS conntrack sysctl, for instance, leaves
// everything Ready while VIP:6443 times out intermittently), so several consecutive
// probes have to succeed. TLS verification is deliberately left on: it also proves
// the apiserver serving certificate covers the VIP.
func probeVIP(ctx context.Context, restConfig *rest.Config, vip string, port int32) error {
	config := rest.CopyConfig(restConfig)
	config.Host = "https://" + net.JoinHostPort(vip, strconv.Itoa(int(port)))
	config.Timeout = vipProbeTimeout

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return pkgerrors.Wrapf(err, "failed to build a client for the control plane VIP %s", config.Host)
	}

	for attempt := 1; attempt <= vipProbeAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(vipProbeInterval):
			}
		}
		if _, err := clientset.Discovery().ServerVersion(); err != nil {
			return fmt.Errorf("control plane VIP probe %d/%d against %s failed: %v", attempt, vipProbeAttempts, config.Host, err)
		}
	}
	return nil
}
