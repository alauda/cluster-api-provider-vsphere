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

	t.Run("reject missing network", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		pool.Spec.Configs[0].Network = nil
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("spec.configs[0].network"))
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

	t.Run("reject additional network without networkName", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		pool.Spec.Configs[0].Network = &infrav1.MachineConfigSlotNetwork{
			Primary:    infrav1.NetworkConfig{NetworkName: "net"},
			Additional: []infrav1.NetworkConfig{{}},
		}
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("additional"))
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

func TestVSphereMachineConfigPoolValidateSharedValidators(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	int32Ptr := func(v int32) *int32 { return &v }

	t.Run("rejects duplicate hostname within the pool", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		pool.Spec.Configs = append(pool.Spec.Configs, infrav1.MachineConfigSlot{
			Hostname: "slot-1",
			Network:  &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{NetworkName: "net-2"}},
		})
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("hostname"))
	})

	t.Run("rejects reserved unit number via shared field validator", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		pool.Spec.Configs[0].PersistentDisks = []infrav1.PersistentDisk{
			{Name: "etcd", SizeGiB: 20, UnitNumber: int32Ptr(7)},
		}
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("reserved"))
	})
}

func TestVSphereMachineConfigPoolValidateCrossPool(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)

	existing := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"},
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Configs:    []infrav1.MachineConfigSlot{{Hostname: "slot-1"}},
		},
	}

	t.Run("rejects hostname already used by another pool in the same cluster", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool() // hostname slot-1, clusterRef test-cluster
		pool.Name = "pool"
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("also used by pool other"))
	})

	t.Run("allows the same hostname across different clusters", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		pool.Name = "pool"
		pool.Spec.ClusterRef.Name = "cluster-b"
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()}
		_, err := webhook.ValidateCreate(context.Background(), pool)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func TestVSphereMachineConfigPoolValidateImmutable(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)

	oldPool := func() *infrav1.VSphereMachineConfigPool {
		p := newPool()
		p.Spec.Configs[0].Network = &infrav1.MachineConfigSlotNetwork{
			Primary: infrav1.NetworkConfig{NetworkName: "net", IP: "10.0.0.1"},
		}
		p.Status.ConfigStatuses = []infrav1.MachineConfigSlotStatus{
			{Hostname: "slot-1", State: infrav1.MachineConfigSlotStateInUse},
		}
		return p
	}

	t.Run("rejects changing the primary IP of an allocated slot", func(t *testing.T) {
		g := NewWithT(t)
		op := oldPool()
		np := op.DeepCopy()
		np.Spec.Configs[0].Network.Primary.IP = "10.0.0.2"
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateUpdate(context.Background(), op, np)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("immutable"))
	})
}

func TestVSphereMachineConfigPoolValidateDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)

	t.Run("rejects delete while a slot is in use", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		pool.Status.ConfigStatuses = []infrav1.MachineConfigSlotStatus{
			{Hostname: "slot-1", State: infrav1.MachineConfigSlotStateInUse},
		}
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateDelete(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("still in use"))
	})

	t.Run("allows delete when only released or available slots remain", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		pool.Status.ConfigStatuses = []infrav1.MachineConfigSlotStatus{
			{Hostname: "slot-1", State: infrav1.MachineConfigSlotStateReleased},
		}
		webhook := &VSphereMachineConfigPool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateDelete(context.Background(), pool)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func newPool() *infrav1.VSphereMachineConfigPool {
	return &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Configs: []infrav1.MachineConfigSlot{{
				Hostname: "slot-1",
				Network:  &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{NetworkName: "net"}},
			}},
		},
	}
}
