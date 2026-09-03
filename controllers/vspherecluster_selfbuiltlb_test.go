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
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta2conditions "sigs.k8s.io/cluster-api/util/conditions/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlclientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
)

func selfBuiltLBScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	for _, gvk := range []schema.GroupVersionKind{moduleInfoGVK, moduleConfigGVK, clusterModuleGVK, modulePluginGVK, platformClusterGVK, clusterRegistryClusterGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return scheme
}

func selfBuiltLBCluster() *infrav1.VSphereCluster {
	return &infrav1.VSphereCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
		Spec: infrav1.VSphereClusterSpec{
			ControlPlaneEndpoint: infrav1.APIEndpoint{Host: "10.0.0.10", Port: 6443},
			ControlPlaneLoadBalancer: &infrav1.ControlPlaneLoadBalancer{
				Type:      infrav1.ControlPlaneLoadBalancerTypeInternal,
				Host:      "10.0.0.10",
				Port:      6443,
				VRID:      42,
				Interface: "ens192",
			},
		},
	}
}

func TestReconcileControlPlaneEndpoint(t *testing.T) {
	t.Run("does nothing without a load balancer", func(t *testing.T) {
		g := NewWithT(t)
		vsphereCluster := selfBuiltLBCluster()
		vsphereCluster.Spec.ControlPlaneLoadBalancer = nil
		vsphereCluster.Spec.ControlPlaneEndpoint = infrav1.APIEndpoint{}
		g.Expect(reconcileControlPlaneEndpoint(&capvcontext.ClusterContext{VSphereCluster: vsphereCluster})).To(Succeed())
		g.Expect(vsphereCluster.Spec.ControlPlaneEndpoint.Host).To(BeEmpty())
	})

	t.Run("backfills an empty endpoint", func(t *testing.T) {
		g := NewWithT(t)
		vsphereCluster := selfBuiltLBCluster()
		vsphereCluster.Spec.ControlPlaneEndpoint = infrav1.APIEndpoint{}
		g.Expect(reconcileControlPlaneEndpoint(&capvcontext.ClusterContext{VSphereCluster: vsphereCluster})).To(Succeed())
		g.Expect(vsphereCluster.Spec.ControlPlaneEndpoint.Host).To(Equal("10.0.0.10"))
		g.Expect(vsphereCluster.Spec.ControlPlaneEndpoint.Port).To(Equal(int32(6443)))
	})

	t.Run("fails on a diverging endpoint", func(t *testing.T) {
		g := NewWithT(t)
		vsphereCluster := selfBuiltLBCluster()
		vsphereCluster.Spec.ControlPlaneEndpoint.Port = 8443
		err := reconcileControlPlaneEndpoint(&capvcontext.ClusterContext{VSphereCluster: vsphereCluster})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("does not match"))
	})
}

func TestEnsureSelfBuiltLBClearsConditionWhenNotInternal(t *testing.T) {
	g := NewWithT(t)
	vsphereCluster := selfBuiltLBCluster()
	vsphereCluster.Spec.ControlPlaneLoadBalancer.Type = infrav1.ControlPlaneLoadBalancerTypeExternal
	conditions.MarkTrue(vsphereCluster, infrav1.SelfBuiltLoadBalancerReadyCondition)
	v1beta2conditions.Set(vsphereCluster, metav1.Condition{
		Type:   infrav1.VSphereClusterSelfBuiltLoadBalancerReadyV1Beta2Condition,
		Status: metav1.ConditionTrue,
		Reason: infrav1.SelfBuiltLoadBalancerReadyReason,
	})

	reconciler := &clusterReconciler{Client: ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).Build()}
	result, err := reconciler.ensureSelfBuiltLB(context.Background(), &capvcontext.ClusterContext{VSphereCluster: vsphereCluster})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(conditions.Get(vsphereCluster, infrav1.SelfBuiltLoadBalancerReadyCondition)).To(BeNil())
	g.Expect(v1beta2conditions.Get(vsphereCluster, infrav1.VSphereClusterSelfBuiltLoadBalancerReadyV1Beta2Condition)).To(BeNil())
}

func TestValidateVIPNotClaimedBySlot(t *testing.T) {
	pool := func(ip string) *infrav1.VSphereMachineConfigPool {
		return &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "cp-1",
					Network:  &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{NetworkName: "net", IP: ip}},
				}},
			},
		}
	}

	t.Run("accepts a VIP no slot claims", func(t *testing.T) {
		g := NewWithT(t)
		reconciler := &clusterReconciler{Client: ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).WithObjects(pool("10.0.0.20/24")).Build()}
		g.Expect(reconciler.validateVIPNotClaimedBySlot(context.Background(), selfBuiltLBCluster())).To(Succeed())
	})

	t.Run("rejects a VIP a slot already owns", func(t *testing.T) {
		g := NewWithT(t)
		reconciler := &clusterReconciler{Client: ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).WithObjects(pool("10.0.0.10/24")).Build()}
		err := reconciler.validateVIPNotClaimedBySlot(context.Background(), selfBuiltLBCluster())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(`slot "cp-1"`))
	})
}

func TestAliveModuleInfoName(t *testing.T) {
	g := NewWithT(t)
	name := aliveModuleInfoName("test-cluster")
	g.Expect(name).To(HavePrefix("test-cluster-"))
	g.Expect(strings.TrimPrefix(name, "test-cluster-")).To(HaveLen(32))
	// Stable across calls, and distinct per cluster.
	g.Expect(aliveModuleInfoName("test-cluster")).To(Equal(name))
	g.Expect(aliveModuleInfoName("other-cluster")).NotTo(Equal(name))
}

func TestAliveModuleInfoConfigPorts(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		wantHTTP    int64
		wantHTTPS   int64
		wantExtra   string
	}{
		{name: "ordinary cluster", clusterName: "workload", wantHTTP: 11780, wantHTTPS: 11781, wantExtra: ""},
		{name: "global cluster", clusterName: "global", wantHTTP: 80, wantHTTPS: 443, wantExtra: "11443 2379"},
		{name: "global cluster case insensitive", clusterName: "GLOBAL", wantHTTP: 80, wantHTTPS: 443, wantExtra: "11443 2379"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := aliveModuleInfoConfigForCluster(tt.clusterName, selfBuiltLBCluster().Spec.ControlPlaneLoadBalancer)
			if config["apiserverPort"] != int64(6443) || config["httpPort"] != tt.wantHTTP || config["httpsPort"] != tt.wantHTTPS || config["extraPorts"] != tt.wantExtra {
				t.Fatalf("ports = api:%v http:%v https:%v extra:%q, want api:6443 http:%d https:%d extra:%q", config["apiserverPort"], config["httpPort"], config["httpsPort"], config["extraPorts"], tt.wantHTTP, tt.wantHTTPS, tt.wantExtra)
			}
		})
	}
}

func TestAliveVersionFor(t *testing.T) {
	modulePlugin := func(latest string, targets map[string]interface{}) *unstructured.Unstructured {
		status := map[string]interface{}{}
		if latest != "" {
			status["latestVersion"] = latest
		}
		if targets != nil {
			status["targetClusterVersions"] = targets
		}
		return &unstructured.Unstructured{Object: map[string]interface{}{"status": status}}
	}
	targetStatus := func(version string) map[string]interface{} {
		return map[string]interface{}{
			"version":        version,
			"readyForDeploy": true,
		}
	}

	t.Run("the provider override wins", func(t *testing.T) {
		g := NewWithT(t)
		version, err := aliveVersionFor(modulePlugin("v4.1.0", map[string]interface{}{"v1.31": targetStatus("v4.0.0")}), "v1.31", "v3.9.0")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(version).To(Equal("v3.9.0"))
	})

	t.Run("the cluster version mapping beats latestVersion", func(t *testing.T) {
		g := NewWithT(t)
		version, err := aliveVersionFor(modulePlugin("v4.1.0", map[string]interface{}{"v1.31": targetStatus("v4.0.0")}), "v1.31", "")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(version).To(Equal("v4.0.0"))
	})

	t.Run("falls back to latestVersion when the cluster version is unmapped", func(t *testing.T) {
		g := NewWithT(t)
		version, err := aliveVersionFor(modulePlugin("v4.1.0", map[string]interface{}{"v1.30": targetStatus("v4.0.0")}), "v1.31", "")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(version).To(Equal("v4.1.0"))
	})

	t.Run("rejects a mapped target without a version", func(t *testing.T) {
		g := NewWithT(t)
		_, err := aliveVersionFor(modulePlugin("v4.1.0", map[string]interface{}{"v1.31": map[string]interface{}{"readyForDeploy": true}}), "v1.31", "")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(`targetClusterVersions["v1.31"]: missing version`))
	})

	t.Run("fails when nothing resolves", func(t *testing.T) {
		g := NewWithT(t)
		_, err := aliveVersionFor(modulePlugin("", nil), "v1.31", "")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("cannot resolve an alive version"))
	})
}

func TestEnsureAliveModuleInfo(t *testing.T) {
	lb := selfBuiltLBCluster().Spec.ControlPlaneLoadBalancer
	pluginLabels := map[string]string{"cpaas.io/module-catalog": "network"}

	t.Run("creates the ModuleInfo", func(t *testing.T) {
		g := NewWithT(t)
		reconciler := &clusterReconciler{Client: ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).Build()}
		moduleInfo, changed, err := reconciler.ensureAliveModuleInfo(context.Background(), "test-cluster", lb, "v4.1.0", pluginLabels)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed).To(BeTrue())
		g.Expect(moduleInfo.GetName()).To(Equal(aliveModuleInfoName("test-cluster")))
		g.Expect(moduleInfo.GetLabels()).To(HaveKeyWithValue(selfBuiltLBManagedLabel, "true"))
		g.Expect(moduleInfo.GetLabels()).To(HaveKeyWithValue("cpaas.io/cluster-name", "test-cluster"))
		g.Expect(moduleInfo.GetLabels()).To(HaveKeyWithValue("cpaas.io/module-catalog", "network"))
		g.Expect(moduleInfo.GetAnnotations()).To(HaveKeyWithValue("cpaas.io/display-name", "alive"))

		version, _, err := unstructured.NestedString(moduleInfo.Object, "spec", "version")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(version).To(Equal("v4.1.0"))
		config, _, err := unstructured.NestedMap(moduleInfo.Object, "spec", "config")
		g.Expect(err).NotTo(HaveOccurred())
		// The ports and the VRID are numbers, like the ones cluster-transformer
		// writes for a platform-managed alive; extraPorts is a string there too.
		g.Expect(config).To(Equal(map[string]interface{}{
			"vip":           "10.0.0.10",
			"vrid":          int64(42),
			"apiserverPort": int64(6443),
			"httpPort":      int64(11780),
			"httpsPort":     int64(11781),
			"extraPorts":    "",
			"interface":     "ens192",
		}))
		// masterIPs stays out: alive derives the backends from the workload Nodes.
		g.Expect(config).NotTo(HaveKey("masterIPs"))
	})

	t.Run("is a no-op when nothing changed", func(t *testing.T) {
		g := NewWithT(t)
		reconciler := &clusterReconciler{Client: ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).Build()}
		_, _, err := reconciler.ensureAliveModuleInfo(context.Background(), "test-cluster", lb, "v4.1.0", pluginLabels)
		g.Expect(err).NotTo(HaveOccurred())
		_, changed, err := reconciler.ensureAliveModuleInfo(context.Background(), "test-cluster", lb, "v4.1.0", pluginLabels)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed).To(BeFalse())
	})

	t.Run("leaves an existing provider-managed ModuleInfo to the platform", func(t *testing.T) {
		g := NewWithT(t)
		existing := newUnstructured(moduleInfoGVK)
		existing.SetName(aliveModuleInfoName("test-cluster"))
		existing.SetLabels(map[string]string{selfBuiltLBManagedLabel: "true"})
		g.Expect(unstructured.SetNestedMap(existing.Object, map[string]interface{}{
			"vip":           "10.0.0.10",
			"somethingElse": "keep-me",
		}, "spec", "config")).To(Succeed())
		g.Expect(unstructured.SetNestedField(existing.Object, "v4.0.0", "spec", "version")).To(Succeed())

		reconciler := &clusterReconciler{Client: ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).WithObjects(existing).Build()}
		moduleInfo, changed, err := reconciler.ensureAliveModuleInfo(context.Background(), "test-cluster", lb, "v4.1.0", pluginLabels)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed).To(BeFalse())
		version, _, _ := unstructured.NestedString(moduleInfo.Object, "spec", "version")
		g.Expect(version).To(Equal("v4.0.0"))
		config, _, _ := unstructured.NestedMap(moduleInfo.Object, "spec", "config")
		g.Expect(config).To(HaveKeyWithValue("somethingElse", "keep-me"))
		g.Expect(config).NotTo(HaveKey("vrid"))
	})

	t.Run("leaves the annotations and the product label to the platform webhook", func(t *testing.T) {
		g := NewWithT(t)
		existing := newUnstructured(moduleInfoGVK)
		existing.SetName(aliveModuleInfoName("test-cluster"))
		labels := aliveModuleInfoIdentityLabels("test-cluster")
		labels["cpaas.io/product"] = "set-by-cluster-transformer"
		existing.SetLabels(labels)
		existing.SetAnnotations(map[string]string{"cpaas.io/display-name": "set-by-cluster-transformer"})
		g.Expect(unstructured.SetNestedField(existing.Object, "v4.1.0", "spec", "version")).To(Succeed())
		g.Expect(unstructured.SetNestedMap(existing.Object, aliveModuleInfoConfig(lb), "spec", "config")).To(Succeed())

		reconciler := &clusterReconciler{Client: ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).WithObjects(existing).Build()}
		moduleInfo, changed, err := reconciler.ensureAliveModuleInfo(context.Background(), "test-cluster", lb, "v4.1.0", map[string]string{"cpaas.io/product": "ACP"})
		g.Expect(err).NotTo(HaveOccurred())
		// Reasserting either would make this controller and cluster-transformer's
		// ModuleInfo webhook flip the object back and forth forever.
		g.Expect(changed).To(BeFalse())
		g.Expect(moduleInfo.GetLabels()).To(HaveKeyWithValue("cpaas.io/product", "set-by-cluster-transformer"))
		g.Expect(moduleInfo.GetAnnotations()).To(HaveKeyWithValue("cpaas.io/display-name", "set-by-cluster-transformer"))
	})

	t.Run("leaves an existing platform ModuleInfo unchanged", func(t *testing.T) {
		g := NewWithT(t)
		existing := newUnstructured(moduleInfoGVK)
		existing.SetName(aliveModuleInfoName("test-cluster"))

		reconciler := &clusterReconciler{Client: ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).WithObjects(existing).Build()}
		moduleInfo, changed, err := reconciler.ensureAliveModuleInfo(context.Background(), "test-cluster", lb, "v4.1.0", pluginLabels)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed).To(BeFalse())
		g.Expect(moduleInfo.GetLabels()).NotTo(HaveKey(selfBuiltLBManagedLabel))
	})
}

func TestModuleInfoReadiness(t *testing.T) {
	moduleInfo := func(version, phase string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"status": map[string]interface{}{"version": version, "phase": phase},
		}}
	}

	tests := []struct {
		name        string
		moduleInfo  *unstructured.Unstructured
		wantMessage string
	}{
		{"ready", moduleInfo("v4.1.0", "Running"), ""},
		{"version not observed yet", moduleInfo("v4.0.0", "Running"), `waiting for alive ModuleInfo to observe version "v4.1.0"`},
		// The phase lags behind a healthy installation, so it must not gate; the
		// AppRelease, the alive pods and the VIP probe decide instead.
		{"phase not Running does not block", moduleInfo("v4.1.0", "Installing"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(moduleInfoReadiness(tt.moduleInfo, "v4.1.0")).To(Equal(tt.wantMessage))
		})
	}
}

func TestKubeProxyConfigWithVIPExcluded(t *testing.T) {
	t.Run("leaves a non IPVS kube-proxy alone", func(t *testing.T) {
		g := NewWithT(t)
		updated, changed, err := kubeProxyConfigWithVIPExcluded("mode: iptables\n", "10.0.0.10")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed).To(BeFalse())
		g.Expect(updated).To(BeEmpty())
	})

	t.Run("sets strictARP and excludes the VIP", func(t *testing.T) {
		g := NewWithT(t)
		updated, changed, err := kubeProxyConfigWithVIPExcluded("mode: ipvs\nipvs:\n  scheduler: rr\n", "10.0.0.10")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed).To(BeTrue())

		config := map[string]interface{}{}
		g.Expect(yaml.Unmarshal([]byte(updated), &config)).To(Succeed())
		strictARP, _, _ := unstructured.NestedBool(config, "ipvs", "strictARP")
		g.Expect(strictARP).To(BeTrue())
		excludeCIDRs, _, _ := unstructured.NestedStringSlice(config, "ipvs", "excludeCIDRs")
		g.Expect(excludeCIDRs).To(ConsistOf("10.0.0.10/32"))
		// Unrelated settings survive the round trip.
		scheduler, _, _ := unstructured.NestedString(config, "ipvs", "scheduler")
		g.Expect(scheduler).To(Equal("rr"))
	})

	t.Run("is idempotent", func(t *testing.T) {
		g := NewWithT(t)
		updated, _, err := kubeProxyConfigWithVIPExcluded("mode: ipvs\n", "10.0.0.10")
		g.Expect(err).NotTo(HaveOccurred())
		again, changed, err := kubeProxyConfigWithVIPExcluded(updated, "10.0.0.10")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed).To(BeFalse())
		g.Expect(again).To(Equal(updated))
	})

	t.Run("keeps existing exclusions", func(t *testing.T) {
		g := NewWithT(t)
		updated, changed, err := kubeProxyConfigWithVIPExcluded("mode: ipvs\nipvs:\n  excludeCIDRs:\n  - 10.0.1.0/24\n", "10.0.0.10")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed).To(BeTrue())
		config := map[string]interface{}{}
		g.Expect(yaml.Unmarshal([]byte(updated), &config)).To(Succeed())
		excludeCIDRs, _, _ := unstructured.NestedStringSlice(config, "ipvs", "excludeCIDRs")
		g.Expect(excludeCIDRs).To(ConsistOf("10.0.1.0/24", "10.0.0.10/32"))
	})
}

func TestEnsureKubeProxyIPVSForVIP(t *testing.T) {
	kubeProxyConfigMap := func(mode string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: alivePodNamespace, Name: "kube-proxy"},
			Data:       map[string]string{"config.conf": "mode: " + mode + "\n"},
		}
	}
	rolledOutDaemonSet := func() *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: alivePodNamespace, Name: "kube-proxy", Generation: 1},
			Status: appsv1.DaemonSetStatus{
				ObservedGeneration:     1,
				DesiredNumberScheduled: 3,
				UpdatedNumberScheduled: 3,
				NumberAvailable:        3,
			},
		}
	}

	t.Run("is done when kube-proxy is absent", func(t *testing.T) {
		g := NewWithT(t)
		reconciler := &clusterReconciler{}
		done, _, err := reconciler.ensureKubeProxyIPVSForVIP(context.Background(), kubernetesfake.NewSimpleClientset(), "10.0.0.10")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue())
	})

	t.Run("is done when kube-proxy is not in IPVS mode", func(t *testing.T) {
		g := NewWithT(t)
		clientset := kubernetesfake.NewSimpleClientset(kubeProxyConfigMap("iptables"), rolledOutDaemonSet())
		reconciler := &clusterReconciler{}
		done, _, err := reconciler.ensureKubeProxyIPVSForVIP(context.Background(), clientset, "10.0.0.10")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue())
	})

	t.Run("writes the config and rolls the DaemonSet before reporting done", func(t *testing.T) {
		g := NewWithT(t)
		clientset := kubernetesfake.NewSimpleClientset(kubeProxyConfigMap("ipvs"), rolledOutDaemonSet())
		reconciler := &clusterReconciler{}

		done, message, err := reconciler.ensureKubeProxyIPVSForVIP(context.Background(), clientset, "10.0.0.10")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeFalse())
		g.Expect(message).To(ContainSubstring("rolling kube-proxy"))

		configMap, err := clientset.CoreV1().ConfigMaps(alivePodNamespace).Get(context.Background(), "kube-proxy", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(configMap.Data["config.conf"]).To(ContainSubstring("strictARP: true"))
		g.Expect(configMap.Data["config.conf"]).To(ContainSubstring("10.0.0.10/32"))

		daemonSet, err := clientset.AppsV1().DaemonSets(alivePodNamespace).Get(context.Background(), "kube-proxy", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(daemonSet.Spec.Template.Annotations).To(HaveKey(kubeProxyConfigHashAnnotation))

		// Second pass: the config is settled and the DaemonSet reports a finished rollout.
		done, _, err = reconciler.ensureKubeProxyIPVSForVIP(context.Background(), clientset, "10.0.0.10")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeTrue())
	})

	t.Run("waits for an unfinished rollout", func(t *testing.T) {
		g := NewWithT(t)
		daemonSet := rolledOutDaemonSet()
		daemonSet.Status.UpdatedNumberScheduled = 1
		clientset := kubernetesfake.NewSimpleClientset(kubeProxyConfigMap("ipvs"), daemonSet)
		reconciler := &clusterReconciler{}

		// First pass writes the config and stamps the DaemonSet.
		_, _, err := reconciler.ensureKubeProxyIPVSForVIP(context.Background(), clientset, "10.0.0.10")
		g.Expect(err).NotTo(HaveOccurred())

		done, message, err := reconciler.ensureKubeProxyIPVSForVIP(context.Background(), clientset, "10.0.0.10")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(done).To(BeFalse())
		g.Expect(message).To(ContainSubstring("rollout to complete"))
	})
}

func TestAlivePodsReadyOnNodes(t *testing.T) {
	alivePod := func(name, node string, ready bool) *corev1.Pod {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: alivePodNamespace, Name: name, Labels: map[string]string{"app": alivePodAppLabel}},
			Spec:       corev1.PodSpec{NodeName: node},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
			},
		}
	}

	t.Run("ready when every control plane Node runs a Ready pod", func(t *testing.T) {
		g := NewWithT(t)
		clientset := kubernetesfake.NewSimpleClientset(alivePod("alive-1", "cp-1", true), alivePod("alive-2", "cp-2", true))
		ready, _, err := alivePodsReadyOnNodes(context.Background(), clientset, []string{"cp-1", "cp-2"})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ready).To(BeTrue())
	})

	t.Run("names the Nodes still missing a Ready pod", func(t *testing.T) {
		g := NewWithT(t)
		clientset := kubernetesfake.NewSimpleClientset(alivePod("alive-1", "cp-1", true), alivePod("alive-2", "cp-2", false))
		ready, message, err := alivePodsReadyOnNodes(context.Background(), clientset, []string{"cp-1", "cp-2", "cp-3"})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ready).To(BeFalse())
		g.Expect(message).To(ContainSubstring("cp-2, cp-3"))
	})
}

func TestResolveAliveModuleRequeuesOnMissingProjection(t *testing.T) {
	g := NewWithT(t)
	reconciler := &clusterReconciler{
		Client:                   ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).Build(),
		ControllerManagerContext: &capvcontext.ControllerManagerContext{},
	}
	_, _, missing, err := reconciler.resolveAliveModule(context.Background(), "test-cluster", "test-namespace")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(missing).To(ContainSubstring("waiting for Cluster"))
}

func TestResolveAliveModule(t *testing.T) {
	newObject := func(gvk schema.GroupVersionKind, name string, object map[string]interface{}) client.Object {
		obj := newUnstructured(gvk)
		obj.SetName(name)
		for key, value := range object {
			obj.Object[key] = value
		}
		return obj
	}
	// The clusterregistry Cluster is namespaced, unlike the other platform objects
	// read here; storing it in a namespace means a lookup that forgets the namespace
	// fails to find it.
	newNamespacedObject := func(gvk schema.GroupVersionKind, namespace, name string) client.Object {
		obj := newUnstructured(gvk)
		obj.SetNamespace(namespace)
		obj.SetName(name)
		return obj
	}
	objects := func() []client.Object {
		return []client.Object{
			newObject(platformClusterGVK, "test-cluster", nil),
			newNamespacedObject(clusterRegistryClusterGVK, "test-namespace", "test-cluster"),
			newObject(clusterModuleGVK, "test-cluster", map[string]interface{}{
				"spec": map[string]interface{}{"version": "v1.31"},
			}),
			newObject(modulePluginGVK, aliveModuleName, map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":   aliveModuleName,
					"labels": map[string]interface{}{"cpaas.io/product": "ACP"},
				},
				"status": map[string]interface{}{"latestVersion": "v4.1.0"},
			}),
			newObject(moduleConfigGVK, "alive-v4.1.0", map[string]interface{}{
				"status": map[string]interface{}{"readyForDeploy": true},
			}),
		}
	}

	t.Run("resolves the version and the passthrough labels", func(t *testing.T) {
		g := NewWithT(t)
		reconciler := &clusterReconciler{
			Client:                   ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).WithObjects(objects()...).Build(),
			ControllerManagerContext: &capvcontext.ControllerManagerContext{},
		}
		version, pluginLabels, missing, err := reconciler.resolveAliveModule(context.Background(), "test-cluster", "test-namespace")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(missing).To(BeEmpty())
		g.Expect(version).To(Equal("v4.1.0"))
		g.Expect(pluginLabels).To(HaveKeyWithValue("cpaas.io/product", "ACP"))
	})

	t.Run("fails when the ModuleConfig is not ready for deploy", func(t *testing.T) {
		g := NewWithT(t)
		all := objects()
		all[len(all)-1] = newObject(moduleConfigGVK, "alive-v4.1.0", map[string]interface{}{
			"status": map[string]interface{}{"readyForDeploy": false},
		})
		reconciler := &clusterReconciler{
			Client:                   ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).WithObjects(all...).Build(),
			ControllerManagerContext: &capvcontext.ControllerManagerContext{},
		}
		_, _, _, err := reconciler.resolveAliveModule(context.Background(), "test-cluster", "test-namespace")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("not ready for deploy"))
	})

	t.Run("the version override still requires its ModuleConfig", func(t *testing.T) {
		g := NewWithT(t)
		reconciler := &clusterReconciler{
			Client:                   ctrlclientfake.NewClientBuilder().WithScheme(selfBuiltLBScheme()).WithObjects(objects()...).Build(),
			ControllerManagerContext: &capvcontext.ControllerManagerContext{PluginAliveVersion: "v3.9.0"},
		}
		_, _, _, err := reconciler.resolveAliveModule(context.Background(), "test-cluster", "test-namespace")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(`ModuleConfig "alive-v3.9.0"`))
	})
}
