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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
)

func TestReconcileKubeadmControlPlaneSystemComponents(t *testing.T) {
	tests := []struct {
		name           string
		coreDNSTag     string
		wantRepository string
	}{
		{
			name:           "ACP 4.3 CoreDNS tag keeps legacy repository on Kubernetes 1.35",
			coreDNSTag:     "1.14.2-v4.3.11",
			wantRepository: "registry.example.com/tkestack",
		},
		{
			name:           "ACP 4.4 CoreDNS tag selects new repository",
			coreDNSTag:     "1.14.2-v4.4.0",
			wantRepository: "registry.example.com/acp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := newSystemComponentTestScheme(t)
			cluster, kcp := systemComponentTestClusterAndKCP("v1.35.0", tt.coreDNSTag)
			managementClient := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp).Build()
			r := &clusterReconciler{Client: managementClient}
			clusterCtx := &capvcontext.ClusterContext{Cluster: cluster}

			if err := r.reconcileKubeadmControlPlaneSystemComponents(ctx, clusterCtx); err != nil {
				t.Fatalf("reconcile KCP system components: %v", err)
			}
			updatedKCP := &controlplanev1.KubeadmControlPlane{}
			if err := managementClient.Get(ctx, client.ObjectKeyFromObject(kcp), updatedKCP); err != nil {
				t.Fatalf("get KCP: %v", err)
			}
			if got := updatedKCP.Spec.KubeadmConfigSpec.ClusterConfiguration.DNS.ImageRepository; got != tt.wantRepository {
				t.Fatalf("CoreDNS repository = %q, want %q", got, tt.wantRepository)
			}
			for _, annotation := range []string{controlplanev1.SkipCoreDNSAnnotation, controlplanev1.SkipKubeProxyAnnotation} {
				if _, ok := updatedKCP.Annotations[annotation]; !ok {
					t.Fatalf("KCP annotation %q is missing", annotation)
				}
			}

			resourceVersion := updatedKCP.ResourceVersion
			if err := r.reconcileKubeadmControlPlaneSystemComponents(ctx, clusterCtx); err != nil {
				t.Fatalf("second KCP reconcile: %v", err)
			}
			if err := managementClient.Get(ctx, client.ObjectKeyFromObject(kcp), updatedKCP); err != nil {
				t.Fatalf("get KCP after second reconcile: %v", err)
			}
			if updatedKCP.ResourceVersion != resourceVersion {
				t.Fatalf("idempotent reconcile changed resourceVersion: %q -> %q", resourceVersion, updatedKCP.ResourceVersion)
			}
		})
	}
}

func TestReconcileKubeadmControlPlaneSystemComponentsSkipsMissingRegistry(t *testing.T) {
	ctx := context.Background()
	scheme := newSystemComponentTestScheme(t)
	cluster, kcp := systemComponentTestClusterAndKCP("v1.35.0", "")
	cluster.Annotations = nil
	kcp.Spec.KubeadmConfigSpec.ClusterConfiguration = nil
	managementClient := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp).Build()
	r := &clusterReconciler{Client: managementClient}

	if err := r.reconcileKubeadmControlPlaneSystemComponents(ctx, &capvcontext.ClusterContext{Cluster: cluster}); err != nil {
		t.Fatalf("reconcile KCP system components: %v", err)
	}
	got := &controlplanev1.KubeadmControlPlane{}
	if err := managementClient.Get(ctx, client.ObjectKeyFromObject(kcp), got); err != nil {
		t.Fatalf("get KCP: %v", err)
	}
	if got.Spec.KubeadmConfigSpec.ClusterConfiguration != nil {
		t.Fatalf("unexpected ClusterConfiguration without registry: %#v", got.Spec.KubeadmConfigSpec.ClusterConfiguration)
	}
	if len(got.Annotations) != 0 {
		t.Fatalf("unexpected annotations without registry: %v", got.Annotations)
	}
}

func TestReconcileWorkloadSystemComponentRepositories(t *testing.T) {
	tests := []struct {
		name               string
		kubernetesVersion  string
		coreDNSTag         string
		wantKubeProxyImage string
		wantCoreDNSImage   string
	}{
		{
			name:               "legacy repositories",
			kubernetesVersion:  "v1.34.9",
			coreDNSTag:         "1.14.2-v4.3.11",
			wantKubeProxyImage: "registry.example.com/tkestack/kube-proxy:v1.34.9",
			wantCoreDNSImage:   "registry.example.com/tkestack/coredns:1.14.2-v4.3.11",
		},
		{
			name:               "new repositories",
			kubernetesVersion:  "v1.35.0",
			coreDNSTag:         "1.14.2-v4.4.0",
			wantKubeProxyImage: "registry.example.com/acp/k8s/kube-proxy:v1.35.0",
			wantCoreDNSImage:   "registry.example.com/acp/coredns:1.14.2-v4.4.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := newSystemComponentTestScheme(t)
			cluster, kcp := systemComponentTestClusterAndKCP(tt.kubernetesVersion, tt.coreDNSTag)
			remoteClient := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(
				&corev1.ServiceAccount{
					ObjectMeta:       metav1.ObjectMeta{Name: "sentry", Namespace: "cpaas-system"},
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: "global-registry-auth"}},
				},
				&appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system"},
					Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						Containers:       []corev1.Container{{Name: "kube-proxy", Image: "old.example.com/tkestack/kube-proxy:v1.33.0"}},
						ImagePullSecrets: []corev1.LocalObjectReference{{Name: "existing-kube-proxy-secret"}},
					}}},
				},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: "kube-system"},
					Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						Containers:       []corev1.Container{{Name: "coredns", Image: "old.example.com/tkestack/coredns:1.11.1"}},
						ImagePullSecrets: []corev1.LocalObjectReference{{Name: "existing-coredns-secret"}},
					}}},
				},
			).Build()
			r := &clusterReconciler{Client: remoteClient}

			sentryPullSecret, err := firstSentryImagePullSecret(ctx, remoteClient)
			if err != nil {
				t.Fatalf("get sentry imagePullSecret: %v", err)
			}
			if err := r.reconcileKubeProxyRepository(ctx, cluster, kcp, remoteClient, sentryPullSecret); err != nil {
				t.Fatalf("reconcile kube-proxy: %v", err)
			}
			if err := r.reconcileCoreDNSRepository(ctx, cluster, kcp, remoteClient, sentryPullSecret); err != nil {
				t.Fatalf("reconcile CoreDNS: %v", err)
			}

			daemonSet := &appsv1.DaemonSet{}
			if err := remoteClient.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "kube-proxy"}, daemonSet); err != nil {
				t.Fatalf("get kube-proxy: %v", err)
			}
			if got := daemonSet.Spec.Template.Spec.Containers[0].Image; got != tt.wantKubeProxyImage {
				t.Fatalf("kube-proxy image = %q, want %q", got, tt.wantKubeProxyImage)
			}
			if got := daemonSet.Spec.Template.Spec.ImagePullSecrets; len(got) != 2 || got[0].Name != "existing-kube-proxy-secret" || got[1].Name != "global-registry-auth" {
				t.Fatalf("kube-proxy imagePullSecrets = %v, want existing-kube-proxy-secret and global-registry-auth", got)
			}

			deployment := &appsv1.Deployment{}
			if err := remoteClient.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "coredns"}, deployment); err != nil {
				t.Fatalf("get CoreDNS: %v", err)
			}
			if got := deployment.Spec.Template.Spec.Containers[0].Image; got != tt.wantCoreDNSImage {
				t.Fatalf("CoreDNS image = %q, want %q", got, tt.wantCoreDNSImage)
			}
			if got := deployment.Spec.Template.Spec.ImagePullSecrets; len(got) != 2 || got[0].Name != "existing-coredns-secret" || got[1].Name != "global-registry-auth" {
				t.Fatalf("CoreDNS imagePullSecrets = %v, want existing-coredns-secret and global-registry-auth", got)
			}

			daemonSetResourceVersion := daemonSet.ResourceVersion
			deploymentResourceVersion := deployment.ResourceVersion
			if err := r.reconcileKubeProxyRepository(ctx, cluster, kcp, remoteClient, sentryPullSecret); err != nil {
				t.Fatalf("second kube-proxy reconcile: %v", err)
			}
			if err := r.reconcileCoreDNSRepository(ctx, cluster, kcp, remoteClient, sentryPullSecret); err != nil {
				t.Fatalf("second CoreDNS reconcile: %v", err)
			}
			if err := remoteClient.Get(ctx, client.ObjectKeyFromObject(daemonSet), daemonSet); err != nil {
				t.Fatalf("get kube-proxy after second reconcile: %v", err)
			}
			if err := remoteClient.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil {
				t.Fatalf("get CoreDNS after second reconcile: %v", err)
			}
			if daemonSet.ResourceVersion != daemonSetResourceVersion {
				t.Fatalf("idempotent kube-proxy reconcile changed resourceVersion: %q -> %q", daemonSetResourceVersion, daemonSet.ResourceVersion)
			}
			if deployment.ResourceVersion != deploymentResourceVersion {
				t.Fatalf("idempotent CoreDNS reconcile changed resourceVersion: %q -> %q", deploymentResourceVersion, deployment.ResourceVersion)
			}
		})
	}
}

func TestReconcileKubeProxyRepositoryWaitsForKubeadmControlPlaneRollout(t *testing.T) {
	ctx := context.Background()
	scheme := newSystemComponentTestScheme(t)
	cluster, kcp := systemComponentTestClusterAndKCP("v1.35.0", "1.14.2-v4.4.0")
	kcp.Status.Replicas = 3
	kcp.Status.UpdatedReplicas = 2
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "kube-proxy", Image: "registry.example.com/tkestack/kube-proxy:v1.34.9",
		}}}}},
	}
	remoteClient := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(daemonSet).Build()
	r := &clusterReconciler{Client: remoteClient}

	if err := r.reconcileKubeProxyRepository(ctx, cluster, kcp, remoteClient, nil); err != nil {
		t.Fatalf("reconcile kube-proxy: %v", err)
	}
	got := &appsv1.DaemonSet{}
	if err := remoteClient.Get(ctx, client.ObjectKeyFromObject(daemonSet), got); err != nil {
		t.Fatalf("get kube-proxy: %v", err)
	}
	if got.Spec.Template.Spec.Containers[0].Image != daemonSet.Spec.Template.Spec.Containers[0].Image {
		t.Fatalf("kube-proxy changed before KCP rollout completed: %q", got.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestReconcileCoreDNSRepositoryWaitsForKubeadmControlPlaneRollout(t *testing.T) {
	ctx := context.Background()
	scheme := newSystemComponentTestScheme(t)
	cluster, kcp := systemComponentTestClusterAndKCP("v1.35.0", "1.14.2-v4.4.0")
	kcp.Status.Replicas = 3
	kcp.Status.UpdatedReplicas = 2
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: "kube-system"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "coredns", Image: "registry.example.com/tkestack/coredns:1.11.1",
		}}}}},
	}
	remoteClient := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
	r := &clusterReconciler{Client: remoteClient}

	if err := r.reconcileCoreDNSRepository(ctx, cluster, kcp, remoteClient, nil); err != nil {
		t.Fatalf("reconcile CoreDNS: %v", err)
	}
	got := &appsv1.Deployment{}
	if err := remoteClient.Get(ctx, client.ObjectKeyFromObject(deployment), got); err != nil {
		t.Fatalf("get CoreDNS: %v", err)
	}
	if got.Spec.Template.Spec.Containers[0].Image != deployment.Spec.Template.Spec.Containers[0].Image {
		t.Fatalf("CoreDNS changed before KCP rollout completed: %q", got.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestShouldEnqueueVSphereClusterForKubeadmControlPlaneUpdate(t *testing.T) {
	_, oldKCP := systemComponentTestClusterAndKCP("v1.34.9", "1.14.2-v4.3.11")
	if shouldEnqueueVSphereClusterForKubeadmControlPlaneUpdate(oldKCP, oldKCP.DeepCopy()) {
		t.Fatal("unchanged KCP should not enqueue")
	}

	tests := []struct {
		name   string
		mutate func(*controlplanev1.KubeadmControlPlane)
	}{
		{name: "Kubernetes version", mutate: func(kcp *controlplanev1.KubeadmControlPlane) { kcp.Spec.Version = "v1.35.0" }},
		{name: "CoreDNS tag", mutate: func(kcp *controlplanev1.KubeadmControlPlane) {
			kcp.Spec.KubeadmConfigSpec.ClusterConfiguration.DNS.ImageTag = "1.14.2-v4.4.0"
		}},
		{name: "replicas", mutate: func(kcp *controlplanev1.KubeadmControlPlane) { kcp.Status.Replicas++ }},
		{name: "updated replicas", mutate: func(kcp *controlplanev1.KubeadmControlPlane) { kcp.Status.UpdatedReplicas++ }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newKCP := oldKCP.DeepCopy()
			tt.mutate(newKCP)
			if !shouldEnqueueVSphereClusterForKubeadmControlPlaneUpdate(oldKCP, newKCP) {
				t.Fatal("expected KCP update to enqueue")
			}
		})
	}
}

func newSystemComponentTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for name, addToScheme := range map[string]func(*runtime.Scheme) error{
		"apps":           appsv1.AddToScheme,
		"core":           corev1.AddToScheme,
		"cluster":        clusterv1.AddToScheme,
		"controlplane":   controlplanev1.AddToScheme,
		"infrastructure": infrav1.AddToScheme,
	} {
		if err := addToScheme(scheme); err != nil {
			t.Fatalf("add %s scheme: %v", name, err)
		}
	}
	return scheme
}

func systemComponentTestClusterAndKCP(kubernetesVersion, coreDNSTag string) (*clusterv1.Cluster, *controlplanev1.KubeadmControlPlane) {
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
			Annotations: map[string]string{
				"cpaas.io/registry-address": "registry.example.com",
			},
		},
		Spec: clusterv1.ClusterSpec{ControlPlaneRef: &corev1.ObjectReference{
			APIVersion: controlplanev1.GroupVersion.String(),
			Kind:       "KubeadmControlPlane",
			Name:       "test-control-plane",
		}},
	}
	kcp := &controlplanev1.KubeadmControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "test-control-plane", Namespace: "default"},
		Spec: controlplanev1.KubeadmControlPlaneSpec{
			Version: kubernetesVersion,
			KubeadmConfigSpec: bootstrapv1.KubeadmConfigSpec{ClusterConfiguration: &bootstrapv1.ClusterConfiguration{
				DNS: bootstrapv1.DNS{ImageMeta: bootstrapv1.ImageMeta{ImageTag: coreDNSTag}},
			}},
		},
		Status: controlplanev1.KubeadmControlPlaneStatus{Replicas: 1, UpdatedReplicas: 1},
	}
	return cluster, kcp
}
