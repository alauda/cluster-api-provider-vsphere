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

func TestVSphereResourcePoolValidateCreate(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)

	t.Run("valid pool", func(t *testing.T) {
		pool := newPool()
		webhook := &VSphereResourcePool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("reject missing clusterRef name", func(t *testing.T) {
		pool := newPool()
		pool.Spec.ClusterRef.Name = ""
		webhook := &VSphereResourcePool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
	})
}

func TestVSphereResourcePoolValidateUpdate(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)

	t.Run("rejects changing clusterRef while consumerRef is set", func(t *testing.T) {
		oldPool := newPool()
		oldPool.Status.ConsumerRef = &corev1.ObjectReference{
			Kind: "KubeadmControlPlane", Name: "cp-1",
		}
		newPool := oldPool.DeepCopy()
		newPool.Spec.ClusterRef.Name = "other-cluster"
		webhook := &VSphereResourcePool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateUpdate(context.Background(), oldPool, newPool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("clusterRef"))
	})

	t.Run("allows changing clusterRef when consumerRef is nil", func(t *testing.T) {
		oldPool := newPool()
		newPool := oldPool.DeepCopy()
		newPool.Spec.ClusterRef.Name = "other-cluster"
		webhook := &VSphereResourcePool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateUpdate(context.Background(), oldPool, newPool)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func newPool() *infrav1.VSphereResourcePool {
	return &infrav1.VSphereResourcePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
		Spec: infrav1.VSphereResourcePoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Resources:  []infrav1.ResourceSlot{{Hostname: "slot-1"}},
		},
	}
}
