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
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta2conditions "sigs.k8s.io/cluster-api/util/conditions/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
)

const (
	handoffPodCIDR     = "10.3.0.0/16"
	handoffServiceCIDR = "10.4.0.0/16"
	handoffJoinCIDR    = "100.64.0.0/16"
	handoffKubeOvn     = "v4.5.3"
)

// kubeadmCorefile is the Corefile kubeadm installs.
const kubeadmCorefile = `.:53 {
    errors
    health {
       lameduck 5s
    }
    ready
    kubernetes cluster.local in-addr.arpa ip6.arpa {
       pods insecure
       fallthrough in-addr.arpa ip6.arpa
       ttl 30
    }
    prometheus :9153
    forward . /etc/resolv.conf {
       max_concurrent 1000
    }
    cache 30 {
       disable success cluster.local
       disable denial cluster.local
    }
    loop
    reload
    loadbalance
}
`

func handoffTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := newSystemComponentTestScheme(t)
	for _, gvk := range []schema.GroupVersionKind{productBaseGVK, clusterModuleGVK, modulePluginGVK, appReleaseGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return scheme
}

func handoffProductBase(spec, status string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec":   map[string]interface{}{"version": spec},
		"status": map[string]interface{}{"version": status},
	}}
	obj.SetGroupVersionKind(productBaseGVK)
	obj.SetName(productBaseName)
	return obj
}

func handoffClusterModule(name, version string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"version": version},
	}}
	obj.SetGroupVersionKind(clusterModuleGVK)
	obj.SetName(name)
	return obj
}

func handoffKubeOvnModulePlugin(clusterVersion, version string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"targetClusterVersions": map[string]interface{}{clusterVersion: map[string]interface{}{"version": version}},
		},
	}}
	obj.SetGroupVersionKind(modulePluginGVK)
	obj.SetName(kubeOvnModuleName)
	return obj
}

func handoffCluster(initialized bool) *clusterv1.Cluster {
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-cluster",
			Annotations: map[string]string{
				registryAddressAnnotation:       "registry.example.com",
				networkTypeAnnotation:           networkTypeKubeOvn,
				legacyKubeOvnJoinCIDRAnnotation: handoffJoinCIDR,
			},
		},
		Spec: clusterv1.ClusterSpec{
			ClusterNetwork: &clusterv1.ClusterNetwork{
				Pods:     &clusterv1.NetworkRanges{CIDRBlocks: []string{handoffPodCIDR}},
				Services: &clusterv1.NetworkRanges{CIDRBlocks: []string{handoffServiceCIDR}},
			},
			InfrastructureRef: &corev1.ObjectReference{APIVersion: infrav1.GroupVersion.String(), Kind: "VSphereCluster", Name: "test-cluster"},
			ControlPlaneRef:   &corev1.ObjectReference{APIVersion: controlplanev1.GroupVersion.String(), Kind: kubeadmControlPlaneKind, Name: "test-control-plane"},
		},
	}
	if initialized {
		conditions.MarkTrue(cluster, clusterv1.ControlPlaneInitializedCondition)
	}
	return cluster
}

func handedOffCluster() *clusterv1.Cluster {
	cluster := handoffCluster(true)
	cluster.Labels = map[string]string{cniTypeLabel: networkTypeKubeOvn}
	return cluster
}

func handoffClusterContext(cluster *clusterv1.Cluster) *capvcontext.ClusterContext {
	return &capvcontext.ClusterContext{
		Cluster:        cluster,
		VSphereCluster: &infrav1.VSphereCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-cluster"}},
	}
}

func handoffKCP(replicas int32, skipPhases ...string) *controlplanev1.KubeadmControlPlane {
	kcp := &controlplanev1.KubeadmControlPlane{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-control-plane"},
		Spec:       controlplanev1.KubeadmControlPlaneSpec{Version: "v1.35.0"},
		Status:     controlplanev1.KubeadmControlPlaneStatus{Replicas: replicas, UpdatedReplicas: replicas},
	}
	if len(skipPhases) > 0 {
		kcp.Spec.KubeadmConfigSpec.InitConfiguration = &bootstrapv1.InitConfiguration{SkipPhases: skipPhases}
	}
	return kcp
}

func handoffControlPlaneMachine() *clusterv1.Machine {
	return &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default",
		Name:      "test-control-plane-abcde",
		Labels:    map[string]string{clusterv1.ClusterNameLabel: "test-cluster", clusterv1.MachineControlPlaneLabel: ""},
	}}
}

// providerKubeOvnAppRelease is the kube-ovn AppRelease this provider renders
// for the handoff fixtures, synced and healthy.
func providerKubeOvnAppRelease(chartName, version string) *unstructured.Unstructured {
	app := buildKubeOvnAppRelease(handoffCluster(true), "registry.example.com", chartName, version,
		handoffPodCIDR, handoffServiceCIDR, handoffJoinCIDR, "", nil, []string{"cp-0"})
	app.SetGeneration(2)
	app.Object["status"] = map[string]interface{}{
		"observedGeneration": int64(2),
		"charts":             map[string]interface{}{chartName: map[string]interface{}{"revision": version, "installedRevision": version}},
		"conditions": []interface{}{
			map[string]interface{}{"type": "Sync", "status": "True"},
			map[string]interface{}{"type": "Health", "status": "True"},
		},
	}
	return app
}

func setAppReleaseValue(app *unstructured.Unstructured, value string, path ...string) {
	if err := unstructured.SetNestedField(app.Object, value, append([]string{"spec", "values"}, path...)...); err != nil {
		panic(err)
	}
}

func kubeadmCoreDNSObjects(corefile, clusterIP string) []client.Object {
	return []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: systemNamespace, Name: "coredns"}},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: systemNamespace, Name: "coredns"},
			Data:       map[string]string{"Corefile": corefile},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: systemNamespace, Name: "kube-dns"},
			Spec:       corev1.ServiceSpec{ClusterIP: clusterIP},
		},
	}
}

func coreAddonsReason(vsphereCluster *infrav1.VSphereCluster) (string, string) {
	v1beta2 := v1beta2conditions.Get(vsphereCluster, infrav1.VSphereClusterCoreAddonsHandedOffV1Beta2Condition)
	if v1beta2 == nil {
		return conditions.GetReason(vsphereCluster, infrav1.CoreAddonsHandedOffCondition), ""
	}
	return conditions.GetReason(vsphereCluster, infrav1.CoreAddonsHandedOffCondition), v1beta2.Reason
}

func TestReconcileKubeadmControlPlaneCoreDNSSkip(t *testing.T) {
	ctx := context.Background()
	scheme := handoffTestScheme(t)
	otherAPIVersion := handoffCluster(false)
	otherAPIVersion.Spec.ControlPlaneRef.APIVersion = "controlplane.cluster.x-k8s.io/v1beta2"
	calico := handoffCluster(false)
	delete(calico.Annotations, networkTypeAnnotation)
	for _, tt := range []struct {
		name      string
		cluster   *clusterv1.Cluster
		kcp       *controlplanev1.KubeadmControlPlane
		objects   []client.Object
		wantReady bool
		wantSkip  bool
		wantErr   bool
	}{
		{
			name:      "new cluster on v4.5 skips the kubeadm CoreDNS addon",
			kcp:       handoffKCP(0),
			objects:   []client.Object{handoffProductBase("v4.5.0", "v4.5.0")},
			wantReady: true,
			wantSkip:  true,
		},
		{
			name:      "new cluster on a v4.5 pre-release skips the kubeadm CoreDNS addon",
			kcp:       handoffKCP(0),
			objects:   []client.Object{handoffProductBase("v4.5.0-beta.3", "v4.5.0-beta.3")},
			wantReady: true,
			wantSkip:  true,
		},
		{
			name:      "the status version wins while the platform is upgrading",
			kcp:       handoffKCP(0),
			objects:   []client.Object{handoffProductBase("v4.5.0", "v4.4.3")},
			wantReady: true,
		},
		{
			name:      "new cluster below v4.5 keeps the kubeadm CoreDNS addon",
			kcp:       handoffKCP(0),
			objects:   []client.Object{handoffProductBase("v4.4.3", "v4.4.3")},
			wantReady: true,
		},
		{
			name:    "infrastructure waits for the KubeadmControlPlane on v4.5",
			objects: []client.Object{handoffProductBase("v4.5.0", "v4.5.0")},
		},
		{
			name:    "infrastructure waits when the platform version cannot be read",
			kcp:     handoffKCP(0),
			wantErr: true,
		},
		{
			name:      "a cluster with control-plane machines is never patched",
			kcp:       handoffKCP(0),
			objects:   []client.Object{handoffControlPlaneMachine(), handoffProductBase("v4.5.0", "v4.5.0")},
			wantReady: true,
		},
		{
			name:      "a KCP with replicas is never patched",
			kcp:       handoffKCP(1),
			objects:   []client.Object{handoffProductBase("v4.5.0", "v4.5.0")},
			wantReady: true,
		},
		{
			name:      "an initialized control plane is left alone without reading the platform version",
			cluster:   handoffCluster(true),
			kcp:       handoffKCP(0),
			wantReady: true,
		},
		{
			name:      "clusters without the kube-ovn network type keep the kubeadm CoreDNS addon",
			cluster:   calico,
			kcp:       handoffKCP(0),
			objects:   []client.Object{handoffProductBase("v4.5.0", "v4.5.0")},
			wantReady: true,
		},
		{
			name:      "a control plane this provider does not look up is left alone",
			cluster:   otherAPIVersion,
			kcp:       handoffKCP(0),
			objects:   []client.Object{handoffProductBase("v4.5.0", "v4.5.0")},
			wantReady: true,
		},
		{
			name:      "an already patched KCP stays as it is",
			kcp:       handoffKCP(0, coreDNSAddonPhase),
			wantReady: true,
			wantSkip:  true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cluster := tt.cluster
			if cluster == nil {
				cluster = handoffCluster(false)
			}
			objects := append([]client.Object{cluster}, tt.objects...)
			if tt.kcp != nil {
				objects = append(objects, tt.kcp)
			}
			c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			r := &clusterReconciler{Client: c}

			ready, err := r.reconcileKubeadmControlPlaneCoreDNSSkip(ctx, handoffClusterContext(cluster))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if ready != tt.wantReady {
				t.Fatalf("ready = %v, want %v", ready, tt.wantReady)
			}
			if tt.kcp == nil {
				return
			}
			got := &controlplanev1.KubeadmControlPlane{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(tt.kcp), got); err != nil {
				t.Fatal(err)
			}
			var skipPhases []string
			if got.Spec.KubeadmConfigSpec.InitConfiguration != nil {
				skipPhases = got.Spec.KubeadmConfigSpec.InitConfiguration.SkipPhases
			}
			if tt.wantSkip {
				if !reflect.DeepEqual(skipPhases, []string{coreDNSAddonPhase}) {
					t.Fatalf("skipPhases = %v, want [%s]", skipPhases, coreDNSAddonPhase)
				}
				if got.Annotations[controlplanev1.SkipCoreDNSAnnotation] == "" && tt.kcp.Spec.KubeadmConfigSpec.InitConfiguration == nil {
					t.Fatal("the skip-coredns annotation is missing")
				}
			} else if len(skipPhases) != 0 {
				t.Fatalf("skipPhases = %v, want none", skipPhases)
			}
		})
	}
}

// KCP creates the first machine once the infrastructure is ready, so a new
// v4.5 cluster must not be reported ready before the skip is in the KCP.
func TestHoldForCoreDNSSkip(t *testing.T) {
	r := &clusterReconciler{}
	vsphereCluster := &infrav1.VSphereCluster{}

	if !r.holdForCoreDNSSkip(vsphereCluster, false) {
		t.Fatal("the infrastructure was not held back before the KubeadmControlPlane skips the kubeadm CoreDNS addon")
	}
	if v1beta1, v1beta2 := coreAddonsReason(vsphereCluster); v1beta1 != infrav1.CoreDNSSkipPhasePendingReason || v1beta2 != infrav1.CoreDNSSkipPhasePendingReason {
		t.Fatalf("reasons = %q, %q, want %q", v1beta1, v1beta2, infrav1.CoreDNSSkipPhasePendingReason)
	}

	if r.holdForCoreDNSSkip(vsphereCluster, true) {
		t.Fatal("the infrastructure was held back once the KubeadmControlPlane skips the kubeadm CoreDNS addon")
	}
	if v1beta1, v1beta2 := coreAddonsReason(vsphereCluster); v1beta1 != infrav1.CoreAddonsHandoffPendingReason || v1beta2 != infrav1.CoreAddonsHandoffPendingReason {
		t.Fatalf("reasons = %q, %q, want %q once the skip is in place", v1beta1, v1beta2, infrav1.CoreAddonsHandoffPendingReason)
	}

	untouched := &infrav1.VSphereCluster{}
	if r.holdForCoreDNSSkip(untouched, true) || conditions.Get(untouched, infrav1.CoreAddonsHandedOffCondition) != nil {
		t.Fatal("a cluster that never waited for the skip got a handoff condition")
	}
}

func TestReconcileCoreAddonsHandoff(t *testing.T) {
	ctx := context.Background()
	scheme := handoffTestScheme(t)

	withCoreDNS := func(objects ...client.Object) []client.Object {
		return append(objects, kubeadmCoreDNSObjects(kubeadmCorefile, "10.4.0.10")...)
	}
	stockKubeOvn := func(version string) []client.Object {
		return withCoreDNS(providerKubeOvnAppRelease(kubeOvnChartNameV44, version))
	}
	modified := func(mutate func(app *unstructured.Unstructured)) []client.Object {
		app := providerKubeOvnAppRelease(kubeOvnChartNameV44, handoffKubeOvn)
		mutate(app)
		return withCoreDNS(app)
	}
	rollingKCP := handoffKCP(3)
	rollingKCP.Status.UpdatedReplicas = 2
	mdReplicas := int32(2)
	rollingMD := &clusterv1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-md", Labels: map[string]string{clusterv1.ClusterNameLabel: "test-cluster"}},
		Spec:       clusterv1.MachineDeploymentSpec{ClusterName: "test-cluster", Replicas: &mdReplicas},
		Status:     clusterv1.MachineDeploymentStatus{Replicas: 2, UpdatedReplicas: 1},
	}
	v45 := []client.Object{handoffClusterModule("test-cluster", "v4.5.0"), handoffProductBase("v4.5.0", "v4.5.0")}
	notReadyNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "global-cp-0"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}}

	for _, tt := range []struct {
		name              string
		clusterName       string
		labels            map[string]string
		networkDevice     string
		managed           []client.Object
		remote            []client.Object
		wantHandedOff     bool
		wantNetworkDevice string
		wantReason        string
		wantMessage       string
	}{
		{
			name:          "a labelled cluster is handed over",
			labels:        map[string]string{cniTypeLabel: networkTypeKubeOvn},
			wantHandedOff: true,
		},
		{
			name:       "waits for the ClusterModule",
			wantReason: infrav1.CoreAddonsHandoffPendingReason,
		},
		{
			name:       "a cluster below v4.5 stays with the provider",
			managed:    []client.Object{handoffClusterModule("test-cluster", "v4.4.3"), handoffProductBase("v4.5.0", "v4.5.0")},
			wantReason: infrav1.CoreAddonsHandoffNotApplicableReason,
		},
		{
			name:        "waits while the platform rolls out a new version",
			managed:     []client.Object{handoffClusterModule("test-cluster", "v4.5.0"), handoffProductBase("v4.6.0", "v4.5.0")},
			wantReason:  infrav1.CoreAddonsHandoffPendingReason,
			wantMessage: "to be on one version",
		},
		{
			name:              "a new cluster is handed over on the interface this provider renders",
			managed:           v45,
			wantHandedOff:     true,
			wantNetworkDevice: kubeOvnInterface,
		},
		{
			name:              "a new cluster that names its interface keeps it",
			networkDevice:     "eth1",
			managed:           v45,
			wantHandedOff:     true,
			wantNetworkDevice: "eth1",
		},
		{
			name:              "an upgraded cluster is handed over",
			managed:           v45,
			remote:            stockKubeOvn(handoffKubeOvn),
			wantHandedOff:     true,
			wantNetworkDevice: kubeOvnInterface,
		},
		{
			// The platform upgrade leaves kube-ovn alone on clusters it does not
			// manage yet; cluster-transformer upgrades it on the handoff.
			name:              "kube-ovn one minor behind is handed over for the platform to upgrade it",
			managed:           v45,
			remote:            stockKubeOvn("v4.4.6"),
			wantHandedOff:     true,
			wantNetworkDevice: kubeOvnInterface,
		},
		{
			name:              "kube-ovn several minors behind is handed over too",
			managed:           v45,
			remote:            stockKubeOvn("v4.4.0"),
			wantHandedOff:     true,
			wantNetworkDevice: kubeOvnInterface,
		},
		{
			name:        "a kube-ovn newer than the platform target blocks the handoff",
			managed:     v45,
			remote:      stockKubeOvn("v4.5.7"),
			wantReason:  infrav1.CoreAddonsHandoffBlockedReason,
			wantMessage: "kube-ovn is at v4.5.7, the platform installs v4.5.3 on v4.5.0",
		},
		{
			// A cluster upgraded from v4.3, or whose kube-ovn stayed below v4.4,
			// still has the chart at its pre-v4.4 address; the platform moves the
			// same release to acp/chart-kube-ovn when it upgrades it.
			name:              "a kube-ovn on the legacy chart is handed over for the platform to upgrade it",
			managed:           v45,
			remote:            withCoreDNS(providerKubeOvnAppRelease(kubeOvnLegacyChartName, "v4.3.9")),
			wantHandedOff:     true,
			wantNetworkDevice: kubeOvnInterface,
		},
		{
			name:        "a kube-ovn AppRelease on another chart blocks the handoff",
			managed:     v45,
			remote:      withCoreDNS(providerKubeOvnAppRelease("acp/chart-other-cni", handoffKubeOvn)),
			wantReason:  infrav1.CoreAddonsHandoffBlockedReason,
			wantMessage: "uses acp/chart-other-cni",
		},
		{
			// The platform would move kube-ovn to the interface the cluster names.
			name:          "a cluster naming another interface than kube-ovn runs on blocks the handoff",
			networkDevice: "eth1",
			managed:       v45,
			remote:        stockKubeOvn(handoffKubeOvn),
			wantReason:    infrav1.CoreAddonsHandoffBlockedReason,
			wantMessage:   `kube-ovn networking.IFACE is "eth0", the platform renders "eth1"`,
		},
		{
			name:        "taking over an existing kube-ovn waits for the control plane to roll out",
			managed:     append([]client.Object{rollingKCP}, v45...),
			remote:      stockKubeOvn("v4.4.6"),
			wantReason:  infrav1.CoreAddonsHandoffPendingReason,
			wantMessage: "KubeadmControlPlane test-control-plane is rolling out",
		},
		{
			name:        "taking over an existing kube-ovn waits for the workers to roll out",
			managed:     append([]client.Object{handoffKCP(1), rollingMD}, v45...),
			remote:      stockKubeOvn("v4.4.6"),
			wantReason:  infrav1.CoreAddonsHandoffPendingReason,
			wantMessage: "MachineDeployment test-md is rolling out",
		},
		{
			// Right after a spec change the Sync and Health conditions still
			// describe the previous revision.
			name:    "a kube-ovn upgrade sentry has not applied yet is waited for",
			managed: v45,
			remote: modified(func(app *unstructured.Unstructured) {
				app.SetGeneration(3)
				_ = unstructured.SetNestedField(app.Object, "v4.4.6", "status", "charts", kubeOvnChartNameV44, "installedRevision")
			}),
			wantReason:  infrav1.CoreAddonsHandoffPendingReason,
			wantMessage: "waiting for the kube-ovn AppRelease to be synced and healthy",
		},
		{
			name:    "a kube-ovn revision sentry has not installed yet is waited for",
			managed: v45,
			remote: modified(func(app *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(app.Object, "v4.4.6", "status", "charts", kubeOvnChartNameV44, "installedRevision")
			}),
			wantReason:  infrav1.CoreAddonsHandoffPendingReason,
			wantMessage: "waiting for the kube-ovn AppRelease to be synced and healthy",
		},
		{
			name:    "an unhealthy kube-ovn AppRelease is waited for",
			managed: v45,
			remote: modified(func(app *unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(app.Object, []interface{}{
					map[string]interface{}{"type": "Sync", "status": "True"},
					map[string]interface{}{"type": "Health", "status": "False"},
				}, "status", "conditions")
			}),
			wantReason:  infrav1.CoreAddonsHandoffPendingReason,
			wantMessage: "waiting for the kube-ovn AppRelease to be synced and healthy",
		},
		{
			name:    "a customised Corefile blocks the handoff",
			managed: v45,
			remote: append([]client.Object{providerKubeOvnAppRelease(kubeOvnChartNameV44, handoffKubeOvn)},
				kubeadmCoreDNSObjects(strings.Replace(kubeadmCorefile, "    loop\n", "    hosts {\n       10.0.0.1 registry.example.com\n       fallthrough\n    }\n    loop\n", 1), "10.4.0.10")...),
			wantReason:  infrav1.CoreAddonsHandoffBlockedReason,
			wantMessage: "plugins hosts",
		},
		{
			name:        "a kube-dns address the chart would change blocks the handoff",
			managed:     v45,
			remote:      append([]client.Object{providerKubeOvnAppRelease(kubeOvnChartNameV44, handoffKubeOvn)}, kubeadmCoreDNSObjects(kubeadmCorefile, "10.4.0.20")...),
			wantReason:  infrav1.CoreAddonsHandoffBlockedReason,
			wantMessage: "kube-dns has ClusterIP 10.4.0.20",
		},
		{
			name:        "a kube-ovn network the platform would render differently blocks the handoff",
			managed:     v45,
			remote:      modified(func(app *unstructured.Unstructured) { setAppReleaseValue(app, "10.99.0.0/16", "ipv4", "POD_CIDR") }),
			wantReason:  infrav1.CoreAddonsHandoffBlockedReason,
			wantMessage: "ipv4.POD_CIDR",
		},
		{
			name:    "a kube-ovn VLAN interface the platform would render differently blocks the handoff",
			managed: v45,
			remote: modified(func(app *unstructured.Unstructured) {
				setAppReleaseValue(app, "bond0", "networking", "vlan", "VLAN_INTERFACE_NAME")
			}),
			wantReason:  infrav1.CoreAddonsHandoffBlockedReason,
			wantMessage: "networking.vlan.VLAN_INTERFACE_NAME",
		},
		{
			name:        "global waits for every node to be ready",
			clusterName: globalClusterName,
			managed:     []client.Object{handoffClusterModule(globalClusterName, "v4.5.0"), handoffProductBase("v4.5.0", "v4.5.0")},
			remote:      withCoreDNS(providerKubeOvnAppRelease(kubeOvnChartNameV44, handoffKubeOvn), notReadyNode),
			wantReason:  infrav1.CoreAddonsHandoffBlockedReason,
			wantMessage: "node global-cp-0 is not ready",
		},
		{
			// Its nodes cannot be ready before the platform installs kube-ovn,
			// which it does only after the handoff.
			name:              "a new global is handed over before its nodes are ready",
			clusterName:       globalClusterName,
			managed:           []client.Object{handoffClusterModule(globalClusterName, "v4.5.0"), handoffProductBase("v4.5.0", "v4.5.0")},
			remote:            []client.Object{notReadyNode.DeepCopy()},
			wantHandedOff:     true,
			wantNetworkDevice: kubeOvnInterface,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cluster := handoffCluster(true)
			if tt.clusterName != "" {
				cluster.Name = tt.clusterName
			}
			cluster.Labels = tt.labels
			if tt.networkDevice != "" {
				cluster.Annotations[kubeOvnNetworkDeviceAnnotation] = tt.networkDevice
			}
			objects := append([]client.Object{cluster, handoffKubeOvnModulePlugin("v4.5.0", handoffKubeOvn)}, tt.managed...)
			c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			remote := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.remote...).Build()
			r := &clusterReconciler{Client: c}
			clusterCtx := handoffClusterContext(cluster)

			handedOff, err := r.reconcileCoreAddonsHandoff(ctx, clusterCtx, remote)
			if err != nil {
				t.Fatalf("reconcileCoreAddonsHandoff() error = %v", err)
			}
			vsphereCluster := clusterCtx.VSphereCluster
			if handedOff != tt.wantHandedOff {
				t.Fatalf("handedOff = %v, want %v (%+v)", handedOff, tt.wantHandedOff, conditions.Get(vsphereCluster, infrav1.CoreAddonsHandedOffCondition))
			}

			got := &clusterv1.Cluster{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), got); err != nil {
				t.Fatal(err)
			}
			if tt.wantHandedOff {
				if !conditions.IsTrue(vsphereCluster, infrav1.CoreAddonsHandedOffCondition) {
					t.Fatalf("CoreAddonsHandedOff is not true: %+v", conditions.Get(vsphereCluster, infrav1.CoreAddonsHandedOffCondition))
				}
				if v1beta2 := v1beta2conditions.Get(vsphereCluster, infrav1.VSphereClusterCoreAddonsHandedOffV1Beta2Condition); v1beta2 == nil ||
					v1beta2.Status != metav1.ConditionTrue || v1beta2.Reason != infrav1.CoreAddonsHandedOffReason {
					t.Fatalf("v1beta2 CoreAddonsHandedOff = %+v, want True/%s", v1beta2, infrav1.CoreAddonsHandedOffReason)
				}
				if got.Labels[cniTypeLabel] != networkTypeKubeOvn {
					t.Fatalf("cni-type label = %q, want kube-ovn", got.Labels[cniTypeLabel])
				}
				if tt.labels == nil && (got.Annotations[networkTypeAnnotation] != networkTypeKubeOvn ||
					got.Annotations[kubeOvnJoinCIDRAnnotation] != handoffJoinCIDR ||
					got.Annotations[kubeOvnNetworkDeviceAnnotation] != tt.wantNetworkDevice) {
					t.Fatalf("handoff annotations = %v", got.Annotations)
				}
				return
			}
			if _, ok := got.Labels[cniTypeLabel]; ok {
				t.Fatal("the cluster was labelled although the handoff did not happen")
			}
			if v1beta1, v1beta2 := coreAddonsReason(vsphereCluster); v1beta1 != tt.wantReason || v1beta2 != tt.wantReason {
				t.Fatalf("reasons = %q, %q, want %q", v1beta1, v1beta2, tt.wantReason)
			}
			if message := conditions.GetMessage(vsphereCluster, infrav1.CoreAddonsHandedOffCondition); !strings.Contains(message, tt.wantMessage) {
				t.Fatalf("message = %q, want it to contain %q", message, tt.wantMessage)
			}
		})
	}
}

func TestReconcileCoreAddonsHandoffLeavesOtherNetworksAlone(t *testing.T) {
	ctx := context.Background()
	scheme := handoffTestScheme(t)
	cluster := handoffCluster(true)
	delete(cluster.Annotations, networkTypeAnnotation)
	c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster,
		handoffClusterModule("test-cluster", "v4.5.0"), handoffProductBase("v4.5.0", "v4.5.0")).Build()
	r := &clusterReconciler{Client: c}
	clusterCtx := handoffClusterContext(cluster)

	handedOff, err := r.reconcileCoreAddonsHandoff(ctx, clusterCtx, clientfake.NewClientBuilder().WithScheme(scheme).Build())
	if err != nil || handedOff {
		t.Fatalf("reconcileCoreAddonsHandoff() = %v, %v; want no handoff", handedOff, err)
	}
	got := &clusterv1.Cluster{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Labels[cniTypeLabel]; ok || conditions.Get(clusterCtx.VSphereCluster, infrav1.CoreAddonsHandedOffCondition) != nil {
		t.Fatalf("a cluster without kube-ovn was handed over: labels %v", got.Labels)
	}
}

// After the handoff cluster-transformer owns the AppRelease, and the self-built
// load balancer that comes next still waits for the CNI.
func TestReconcileHandedOffKubeOvnOnlyReports(t *testing.T) {
	ctx := context.Background()
	scheme := handoffTestScheme(t)
	unhealthy := providerKubeOvnAppRelease(kubeOvnChartNameV44, handoffKubeOvn)
	_ = unstructured.SetNestedSlice(unhealthy.Object, []interface{}{
		map[string]interface{}{"type": "Sync", "status": "True"},
		map[string]interface{}{"type": "Health", "status": "False", "reason": "Unhealthy"},
	}, "status", "conditions")
	for _, tt := range []struct {
		name        string
		app         *unstructured.Unstructured
		wantStatus  corev1.ConditionStatus
		wantRequeue bool
		wantMessage string
	}{
		{name: "waits for the platform to install kube-ovn", wantStatus: corev1.ConditionFalse, wantRequeue: true, wantMessage: "waiting for the platform to install kube-ovn"},
		{name: "a healthy platform kube-ovn lets the reconcile go on", app: providerKubeOvnAppRelease(kubeOvnChartNameV44, handoffKubeOvn), wantStatus: corev1.ConditionTrue},
		{name: "an unhealthy platform kube-ovn is waited for", app: unhealthy, wantStatus: corev1.ConditionFalse, wantRequeue: true, wantMessage: "Unhealthy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			builder := clientfake.NewClientBuilder().WithScheme(scheme)
			if tt.app != nil {
				builder = builder.WithObjects(tt.app)
			}
			remote := builder.Build()
			r := &clusterReconciler{Client: clientfake.NewClientBuilder().WithScheme(scheme).Build()}
			clusterCtx := handoffClusterContext(handedOffCluster())

			result, err := r.reconcileHandedOffKubeOvn(ctx, clusterCtx, remote)
			if err != nil {
				t.Fatalf("reconcileHandedOffKubeOvn() error = %v", err)
			}
			if requeued := !result.IsZero(); requeued != tt.wantRequeue {
				t.Fatalf("requeued = %v, want %v", requeued, tt.wantRequeue)
			}
			condition := conditions.Get(clusterCtx.VSphereCluster, infrav1.KubeOvnAppReleaseReadyCondition)
			if condition == nil || condition.Status != tt.wantStatus || !strings.Contains(condition.Message, tt.wantMessage) {
				t.Fatalf("KubeOvnAppReleaseReady = %+v, want %s with %q", condition, tt.wantStatus, tt.wantMessage)
			}
			if tt.app == nil {
				return
			}
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(appReleaseGVK)
			if err := remote.Get(ctx, client.ObjectKeyFromObject(tt.app), got); err != nil {
				t.Fatal(err)
			}
			if got.GetResourceVersion() != tt.app.GetResourceVersion() && tt.app.GetResourceVersion() != "" {
				t.Fatalf("the provider wrote the platform's kube-ovn AppRelease")
			}
			if !reflect.DeepEqual(got.Object["spec"], tt.app.Object["spec"]) {
				t.Fatalf("the provider changed the platform's kube-ovn AppRelease: %+v", got.Object["spec"])
			}
		})
	}
}

// sentry never updates an existing ServiceAccount, so the CoreDNS chart cannot
// give the kubeadm one the image pull secret when it adopts CoreDNS; nodes
// without registry credentials then cannot pull the chart image.
func TestCoreAddonsHandoffGivesCoreDNSServiceAccountThePullSecret(t *testing.T) {
	ctx := context.Background()
	scheme := handoffTestScheme(t)
	pullSecret := corev1.LocalObjectReference{Name: "global-registry-auth"}
	sentry := func(secrets ...corev1.LocalObjectReference) *corev1.ServiceAccount {
		return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: kubeOvnAppReleaseNamespace, Name: "sentry"}, ImagePullSecrets: secrets}
	}
	coreDNS := func(secrets ...corev1.LocalObjectReference) *corev1.ServiceAccount {
		return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: systemNamespace, Name: "coredns"}, ImagePullSecrets: secrets}
	}
	for _, tt := range []struct {
		name    string
		objects []client.Object
		want    []corev1.LocalObjectReference
	}{
		{name: "the kubeadm ServiceAccount gets the sentry pull secret", objects: []client.Object{sentry(pullSecret), coreDNS()}, want: []corev1.LocalObjectReference{pullSecret}},
		{name: "a ServiceAccount with the pull secret is left as it is", objects: []client.Object{sentry(pullSecret), coreDNS(pullSecret)}, want: []corev1.LocalObjectReference{pullSecret}},
		{name: "sentry without a pull secret changes nothing", objects: []client.Object{sentry(), coreDNS()}},
		{name: "a new cluster without the ServiceAccount gets none", objects: []client.Object{sentry(pullSecret)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cluster := handoffCluster(true)
			c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster,
				handoffClusterModule("test-cluster", "v4.5.0"), handoffProductBase("v4.5.0", "v4.5.0")).Build()
			remote := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...).Build()
			r := &clusterReconciler{Client: c}
			handedOff, err := r.reconcileCoreAddonsHandoff(ctx, handoffClusterContext(cluster), remote)
			if err != nil || !handedOff {
				t.Fatalf("reconcileCoreAddonsHandoff() = %v, %v; want a handoff", handedOff, err)
			}
			got := &corev1.ServiceAccount{}
			if err := remote.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: "coredns"}, got); err != nil {
				if tt.want != nil {
					t.Fatal(err)
				}
				return
			}
			if !reflect.DeepEqual(got.ImagePullSecrets, tt.want) {
				t.Fatalf("imagePullSecrets = %v, want %v", got.ImagePullSecrets, tt.want)
			}
		})
	}
}

func TestPlatformInstallsKubeOvn(t *testing.T) {
	ctx := context.Background()
	scheme := handoffTestScheme(t)
	for _, tt := range []struct {
		name          string
		clusterModule client.Object
		want          bool
	}{
		{name: "the platform knows the cluster at v4.5", clusterModule: handoffClusterModule("test-cluster", "v4.5.0"), want: true},
		{name: "a cluster below v4.5 stays with the provider", clusterModule: handoffClusterModule("test-cluster", "v4.4.3")},
		// Nothing else would install kube-ovn.
		{name: "no ClusterModule yet"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			builder := clientfake.NewClientBuilder().WithScheme(scheme)
			if tt.clusterModule != nil {
				builder = builder.WithObjects(tt.clusterModule)
			}
			r := &clusterReconciler{Client: builder.Build()}
			got, err := r.platformInstallsKubeOvn(ctx, "test-cluster")
			if err != nil || got != tt.want {
				t.Fatalf("platformInstallsKubeOvn() = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestReconcileCoreDNSRepositoryAfterHandoffLeavesCoreDNS(t *testing.T) {
	ctx := context.Background()
	scheme := handoffTestScheme(t)
	kcp := handoffKCP(1)
	kcp.Spec.KubeadmConfigSpec.ClusterConfiguration = &bootstrapv1.ClusterConfiguration{DNS: bootstrapv1.DNS{ImageMeta: bootstrapv1.ImageMeta{ImageTag: "1.14.3-v4.5.0"}}}
	chartCoreDNS := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: systemNamespace, Name: "coredns"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "coredns", Image: "platform.registry/acp/coredns:1.14.3-v4.5.1"}},
		}}},
	}
	remote := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(chartCoreDNS).Build()
	r := &clusterReconciler{Client: clientfake.NewClientBuilder().WithScheme(scheme).Build()}

	if err := r.reconcileCoreDNSRepository(ctx, handedOffCluster(), kcp, remote, &corev1.LocalObjectReference{Name: "global-registry-auth"}); err != nil {
		t.Fatalf("reconcileCoreDNSRepository() error = %v", err)
	}
	got := &appsv1.Deployment{}
	if err := remote.Get(ctx, client.ObjectKeyFromObject(chartCoreDNS), got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Spec.Template.Spec, chartCoreDNS.Spec.Template.Spec) {
		t.Fatalf("the provider rewrote the platform's CoreDNS: %+v", got.Spec.Template.Spec)
	}
}

func TestReconcileKCPSystemComponentsAfterHandoffKeepsOnlyTheSkipAnnotations(t *testing.T) {
	ctx := context.Background()
	scheme := handoffTestScheme(t)
	cluster := handedOffCluster()
	kcp := handoffKCP(1)
	kcp.Spec.KubeadmConfigSpec.ClusterConfiguration = &bootstrapv1.ClusterConfiguration{
		DNS: bootstrapv1.DNS{ImageMeta: bootstrapv1.ImageMeta{ImageRepository: "old.registry/tkestack", ImageTag: "1.14.2-v4.4.0"}},
	}
	c := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, kcp).Build()
	r := &clusterReconciler{Client: c}

	if err := r.reconcileKubeadmControlPlaneSystemComponents(ctx, handoffClusterContext(cluster)); err != nil {
		t.Fatalf("reconcileKubeadmControlPlaneSystemComponents() error = %v", err)
	}
	got := &controlplanev1.KubeadmControlPlane{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(kcp), got); err != nil {
		t.Fatal(err)
	}
	if repository := got.Spec.KubeadmConfigSpec.ClusterConfiguration.DNS.ImageRepository; repository != "old.registry/tkestack" {
		t.Fatalf("DNS image repository = %q, want it left alone after the handoff", repository)
	}
	for _, annotation := range []string{controlplanev1.SkipCoreDNSAnnotation, controlplanev1.SkipKubeProxyAnnotation} {
		if got.Annotations[annotation] == "" {
			t.Fatalf("annotation %s missing after the handoff", annotation)
		}
	}
}

func TestCorefileCustomisation(t *testing.T) {
	for _, tt := range []struct {
		name     string
		corefile string
		want     string
	}{
		{name: "the kubeadm Corefile", corefile: kubeadmCorefile},
		{name: "comments are ignored", corefile: strings.Replace(kubeadmCorefile, "    loop\n", "    loop # detect loops\n    # hosts would be a customisation\n", 1)},
		{name: "an extra plugin", corefile: strings.Replace(kubeadmCorefile, "    loop\n", "    rewrite name foo bar\n    loop\n", 1), want: "plugins rewrite"},
		{name: "an extra server block", corefile: kubeadmCorefile + "example.org:53 {\n    forward . 10.0.0.53\n}\n", want: "server blocks .:53, example.org:53"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := corefileCustomisation(tt.corefile); got != tt.want {
				t.Fatalf("corefileCustomisation() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIndexedIP(t *testing.T) {
	for cidr, want := range map[string]string{
		"10.96.0.0/12":  "10.96.0.10",
		"10.4.0.0/16":   "10.4.0.10",
		"fd00:10::/108": "fd00:10::a",
		"10.0.0.0/29":   "",
		"not-a-cidr":    "",
	} {
		if got := indexedIP(cidr, 10); got != want {
			t.Errorf("indexedIP(%q, 10) = %q, want %q", cidr, got, want)
		}
	}
}

func TestPlatformManagesCoreAddons(t *testing.T) {
	for version, want := range map[string]bool{
		"v4.5.0":        true,
		"4.5.0":         true,
		"v4.5.0-beta.1": true,
		"v4.6.2":        true,
		"v5.0.0":        true,
		"v4.4.9":        false,
		"":              false,
		"latest":        false,
	} {
		if got := platformManagesCoreAddons(version); got != want {
			t.Errorf("platformManagesCoreAddons(%q) = %v, want %v", version, got, want)
		}
	}
}

func TestKubeOvnUpgradableTo(t *testing.T) {
	for _, tt := range []struct {
		current, target string
		want            bool
	}{
		{"v4.5.3", "v4.5.3", true},
		{"v4.4.6", "v4.5.3", true},
		{"v4.2.6", "v4.5.3", true},
		{"v4.5.7", "v4.5.3", false},
		{"v3.9.0", "v4.5.3", false},
		{"", "v4.5.3", false},
	} {
		if got := kubeOvnUpgradableTo(tt.current, tt.target); got != tt.want {
			t.Errorf("kubeOvnUpgradableTo(%q, %q) = %v, want %v", tt.current, tt.target, got, tt.want)
		}
	}
}
