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

func TestVSphereMachineConfigPoolValidateCreate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)

	t.Run("valid pool", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("reject missing clusterRef name", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		pool.Spec.ClusterRef.Name = ""
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("reject network without primary networkName", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		pool.Spec.Configs[0].Network = &infrav1.MachineConfigSlotNetwork{
			Primary: infrav1.NetworkConfig{},
		}
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("primary"))
	})

	t.Run("reject invalid hostname for kubernetes node name", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		pool.Spec.Configs[0].Hostname = "Node_01"
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.configs[0].hostname"))
	})
}

func TestVSphereMachineConfigPoolValidateUpdate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)

	t.Run("rejects changing clusterRef while consumerRef is set", func(t *testing.T) {
		g := NewWithT(t)
		oldPool := newPool()
		oldPool.Status.ConsumerRef = &corev1.ObjectReference{
			Kind: "KubeadmControlPlane", Name: "cp-1",
		}
		newPool := oldPool.DeepCopy()
		newPool.Spec.ClusterRef.Name = "other-cluster"
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateUpdate(context.Background(), oldPool, newPool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("clusterRef"))
	})

	t.Run("allows changing clusterRef when consumerRef is nil", func(t *testing.T) {
		g := NewWithT(t)
		oldPool := newPool()
		newPool := oldPool.DeepCopy()
		newPool.Spec.ClusterRef.Name = "other-cluster"
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateUpdate(context.Background(), oldPool, newPool)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func newPool() *infrav1.VSphereMachineConfigPool {
	return &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Configs:  []infrav1.MachineConfigSlot{{Hostname: "slot-1"}},
		},
	}
}
