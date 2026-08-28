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

package webhooks

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrlclientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

func newSelfBuiltLBCluster() *infrav1.VSphereCluster {
	return &infrav1.VSphereCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
		Spec: infrav1.VSphereClusterSpec{
			ControlPlaneEndpoint: infrav1.APIEndpoint{Host: "10.0.0.10", Port: 6443},
			ControlPlaneLoadBalancer: &infrav1.ControlPlaneLoadBalancer{
				Type: infrav1.ControlPlaneLoadBalancerTypeInternal,
				Host: "10.0.0.10",
				Port: 6443,
				VRID: 42,
			},
		},
	}
}

func newVSphereClusterWebhook(objects ...runtime.Object) *VSphereCluster {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	return &VSphereCluster{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()}
}

func TestVSphereClusterValidateCreate(t *testing.T) {
	t.Run("accepts a well formed internal load balancer", func(t *testing.T) {
		g := NewWithT(t)
		_, err := newVSphereClusterWebhook().ValidateCreate(context.Background(), newSelfBuiltLBCluster())
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("accepts a cluster without a load balancer", func(t *testing.T) {
		g := NewWithT(t)
		obj := newSelfBuiltLBCluster()
		obj.Spec.ControlPlaneLoadBalancer = nil
		_, err := newVSphereClusterWebhook().ValidateCreate(context.Background(), obj)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("accepts an empty endpoint, which the controller backfills", func(t *testing.T) {
		g := NewWithT(t)
		obj := newSelfBuiltLBCluster()
		obj.Spec.ControlPlaneEndpoint = infrav1.APIEndpoint{}
		_, err := newVSphereClusterWebhook().ValidateCreate(context.Background(), obj)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("rejects an endpoint that disagrees with the load balancer", func(t *testing.T) {
		g := NewWithT(t)
		obj := newSelfBuiltLBCluster()
		obj.Spec.ControlPlaneEndpoint.Host = "10.0.0.11"
		_, err := newVSphereClusterWebhook().ValidateCreate(context.Background(), obj)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.controlPlaneEndpoint.host"))
	})

	t.Run("rejects an unknown type", func(t *testing.T) {
		g := NewWithT(t)
		obj := newSelfBuiltLBCluster()
		obj.Spec.ControlPlaneLoadBalancer.Type = "keepalived"
		_, err := newVSphereClusterWebhook().ValidateCreate(context.Background(), obj)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.controlPlaneLoadBalancer.type"))
	})

	t.Run("rejects a non IPv4 internal host", func(t *testing.T) {
		g := NewWithT(t)
		obj := newSelfBuiltLBCluster()
		obj.Spec.ControlPlaneLoadBalancer.Host = "fd00::1"
		obj.Spec.ControlPlaneEndpoint.Host = "fd00::1"
		_, err := newVSphereClusterWebhook().ValidateCreate(context.Background(), obj)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("must be an IPv4 address"))
	})

	t.Run("rejects a hostname as the internal host", func(t *testing.T) {
		g := NewWithT(t)
		obj := newSelfBuiltLBCluster()
		obj.Spec.ControlPlaneLoadBalancer.Host = "apiserver.example.com"
		obj.Spec.ControlPlaneEndpoint.Host = "apiserver.example.com"
		_, err := newVSphereClusterWebhook().ValidateCreate(context.Background(), obj)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("must be a valid IP address"))
	})

	t.Run("rejects a vrid outside 1-255", func(t *testing.T) {
		g := NewWithT(t)
		obj := newSelfBuiltLBCluster()
		obj.Spec.ControlPlaneLoadBalancer.VRID = 0
		_, err := newVSphereClusterWebhook().ValidateCreate(context.Background(), obj)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.controlPlaneLoadBalancer.vrid"))
	})

	t.Run("rejects a port outside 1-65535", func(t *testing.T) {
		g := NewWithT(t)
		obj := newSelfBuiltLBCluster()
		obj.Spec.ControlPlaneLoadBalancer.Port = 0
		obj.Spec.ControlPlaneEndpoint.Port = 0
		_, err := newVSphereClusterWebhook().ValidateCreate(context.Background(), obj)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.controlPlaneLoadBalancer.port"))
	})

	t.Run("warns about vrid and interface on an external load balancer", func(t *testing.T) {
		g := NewWithT(t)
		obj := newSelfBuiltLBCluster()
		obj.Spec.ControlPlaneLoadBalancer.Type = infrav1.ControlPlaneLoadBalancerTypeExternal
		obj.Spec.ControlPlaneLoadBalancer.Interface = "ens192"
		warnings, err := newVSphereClusterWebhook().ValidateCreate(context.Background(), obj)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(warnings).To(HaveLen(2))
	})

	t.Run("rejects a VIP already claimed by a config pool slot", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "cp-1",
					Network: &infrav1.MachineConfigSlotNetwork{
						// Slot addresses are CIDR formatted; the VIP is not.
						Primary: infrav1.NetworkConfig{NetworkName: "net", IP: "10.0.0.10/24"},
					},
				}},
			},
		}
		_, err := newVSphereClusterWebhook(pool).ValidateCreate(context.Background(), newSelfBuiltLBCluster())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(`conflicts with the address of slot "cp-1"`))
	})
}

func TestVSphereClusterValidateUpdate(t *testing.T) {
	t.Run("allows an absent field to stay absent", func(t *testing.T) {
		g := NewWithT(t)
		oldObj := newSelfBuiltLBCluster()
		oldObj.Spec.ControlPlaneLoadBalancer = nil
		newObj := newSelfBuiltLBCluster()
		newObj.Spec.ControlPlaneLoadBalancer = nil
		_, err := newVSphereClusterWebhook().ValidateUpdate(context.Background(), oldObj, newObj)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("rejects setting an absent field", func(t *testing.T) {
		// An absent field already means an external load balancer, so a running
		// cluster can be neither annotated with one nor re-pointed at a VIP.
		for _, tt := range []struct {
			name string
			lb   *infrav1.ControlPlaneLoadBalancer
		}{
			{"internal", newSelfBuiltLBCluster().Spec.ControlPlaneLoadBalancer},
			{"external", &infrav1.ControlPlaneLoadBalancer{
				Type: infrav1.ControlPlaneLoadBalancerTypeExternal,
				Host: newSelfBuiltLBCluster().Spec.ControlPlaneEndpoint.Host,
				Port: newSelfBuiltLBCluster().Spec.ControlPlaneEndpoint.Port,
			}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				g := NewWithT(t)
				oldObj := newSelfBuiltLBCluster()
				oldObj.Spec.ControlPlaneLoadBalancer = nil
				newObj := newSelfBuiltLBCluster()
				newObj.Spec.ControlPlaneLoadBalancer = tt.lb
				_, err := newVSphereClusterWebhook().ValidateUpdate(context.Background(), oldObj, newObj)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("cannot be set after the cluster is created"))
			})
		}
	})

	t.Run("allows an unchanged load balancer", func(t *testing.T) {
		g := NewWithT(t)
		_, err := newVSphereClusterWebhook().ValidateUpdate(context.Background(), newSelfBuiltLBCluster(), newSelfBuiltLBCluster())
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("rejects removing the load balancer", func(t *testing.T) {
		g := NewWithT(t)
		newObj := newSelfBuiltLBCluster()
		newObj.Spec.ControlPlaneLoadBalancer = nil
		_, err := newVSphereClusterWebhook().ValidateUpdate(context.Background(), newSelfBuiltLBCluster(), newObj)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("cannot be removed once set"))
	})

	t.Run("freezes every field once the load balancer is set", func(t *testing.T) {
		for _, tt := range []struct {
			name  string
			field string
			apply func(lb *infrav1.ControlPlaneLoadBalancer)
		}{
			{"type", "type", func(lb *infrav1.ControlPlaneLoadBalancer) {
				lb.Type = infrav1.ControlPlaneLoadBalancerTypeExternal
			}},
			{"host", "host", func(lb *infrav1.ControlPlaneLoadBalancer) { lb.Host = "10.0.0.11" }},
			{"port", "port", func(lb *infrav1.ControlPlaneLoadBalancer) { lb.Port = 8443 }},
			{"vrid", "vrid", func(lb *infrav1.ControlPlaneLoadBalancer) { lb.VRID = 43 }},
			{"interface", "interface", func(lb *infrav1.ControlPlaneLoadBalancer) { lb.Interface = "ens224" }},
		} {
			t.Run(tt.name, func(t *testing.T) {
				g := NewWithT(t)
				newObj := newSelfBuiltLBCluster()
				tt.apply(newObj.Spec.ControlPlaneLoadBalancer)
				// The endpoint follows the load balancer so the immutability error is
				// the only one reported.
				newObj.Spec.ControlPlaneEndpoint = infrav1.APIEndpoint{
					Host: newObj.Spec.ControlPlaneLoadBalancer.Host,
					Port: newObj.Spec.ControlPlaneLoadBalancer.Port,
				}
				_, err := newVSphereClusterWebhook().ValidateUpdate(context.Background(), newSelfBuiltLBCluster(), newObj)
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("spec.controlPlaneLoadBalancer." + tt.field))
				g.Expect(err.Error()).To(ContainSubstring("is immutable"))
			})
		}
	})
}
