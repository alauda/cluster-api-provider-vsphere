package controllers

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

func TestResourcePoolReconcileDeleteBlocksWhenMachineStillExists(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	machine := &infrav1.VSphereMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cp-0",
			Namespace: "default",
		},
	}
	pool := &infrav1.VSphereResourcePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool",
			Namespace:  "default",
			Finalizers: []string{ResourcePoolFinalizer},
		},
		Spec: infrav1.VSphereResourcePoolSpec{
			Resources: []infrav1.ResourceSlot{{Hostname: "host-1"}},
		},
		Status: infrav1.VSphereResourcePoolStatus{
			ResourceStatuses: []infrav1.ResourceSlotStatus{{
				Hostname: "host-1",
				State:    "InUse",
				MachineRef: &corev1.ObjectReference{
					Name:      "cp-0",
					Namespace: "default",
				},
			}},
		},
	}

	r := resourcePoolReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(pool, machine).
			Build(),
	}

	_, err := r.reconcileDelete(context.Background(), pool)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("blocking VSphereResourcePool deletion"))
	g.Expect(pool.Finalizers).To(ContainElement(ResourcePoolFinalizer))
}

func TestResourcePoolReconcileDeleteRemovesFinalizerAfterSafeReclaim(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	now := metav1.Now()
	pool := &infrav1.VSphereResourcePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool",
			Namespace:  "default",
			Finalizers: []string{ResourcePoolFinalizer},
		},
		Spec: infrav1.VSphereResourcePoolSpec{
			Resources: []infrav1.ResourceSlot{{Hostname: "host-1"}},
		},
		Status: infrav1.VSphereResourcePoolStatus{
			ResourceStatuses: []infrav1.ResourceSlotStatus{{
				Hostname: "host-1",
				State:    "InUse",
				MachineRef: &corev1.ObjectReference{
					Name:      "cp-0",
					Namespace: "default",
				},
				LastReleasedTime: &now,
			}},
		},
	}

	r := resourcePoolReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(pool).
			Build(),
	}

	result, err := r.reconcileDelete(context.Background(), pool)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(pool.Finalizers).NotTo(ContainElement(ResourcePoolFinalizer))
	g.Expect(pool.Status.ResourceStatuses[0].MachineRef).To(BeNil())
	g.Expect(pool.Status.ResourceStatuses[0].State).To(Equal("Available"))
}
