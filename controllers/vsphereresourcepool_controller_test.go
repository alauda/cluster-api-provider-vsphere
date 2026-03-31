package controllers

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
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

func TestResourcePoolReconcileReleasedSlotWithoutReclaimableDiskBecomesAvailable(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	releasedAt := metav1.NewTime(time.Now().Add(-25 * time.Hour))
	pool := &infrav1.VSphereResourcePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool",
			Namespace:  "default",
			Finalizers: []string{ResourcePoolFinalizer},
		},
		Spec: infrav1.VSphereResourcePoolSpec{
			Resources: []infrav1.ResourceSlot{{
				Hostname: "host-1",
				PersistentDisks: []infrav1.PersistentDisk{{
					Name:       "data-0",
					MountPath:  "/var/cpaas",
					SizeGiB:    20,
					FSFormat:   "ext4",
					UnitNumber: ptrTo[int32](0),
				}},
			}},
		},
		Status: infrav1.VSphereResourcePoolStatus{
			ResourceStatuses: []infrav1.ResourceSlotStatus{{
				Hostname:         "host-1",
				State:            "Released",
				LastReleasedTime: &releasedAt,
				MachineRef: &corev1.ObjectReference{
					Name:      "worker-1",
					Namespace: "default",
				},
				ReclaimStatus: &infrav1.ResourceSlotReclaimStatus{
					State: reclaimTaskStateCompleted,
				},
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(pool).
		WithObjects(pool).
		Build()

	r := resourcePoolReconciler{
		Client: fakeClient,
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(pool),
	})
	g.Expect(err).NotTo(HaveOccurred())

	updated := &infrav1.VSphereResourcePool{}
	err = fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pool), updated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updated.Status.ResourceStatuses).To(HaveLen(1))
	g.Expect(updated.Status.ResourceStatuses[0].State).To(Equal("Available"))
	g.Expect(updated.Status.ResourceStatuses[0].MachineRef).To(BeNil())
	g.Expect(updated.Status.ResourceStatuses[0].LastReleasedTime).To(BeNil())
	g.Expect(updated.Status.ResourceStatuses[0].ReclaimStatus).NotTo(BeNil())
	g.Expect(updated.Status.ResourceStatuses[0].ReclaimStatus.TaskRef).To(BeEmpty())
	g.Expect(updated.Status.ResourceStatuses[0].ReclaimStatus.VolumePath).To(BeEmpty())
	g.Expect(updated.Status.ResourceStatuses[0].ReclaimStatus.RetryAfter).To(BeNil())
}

func TestResolveSlotDatacenter(t *testing.T) {
	g := NewWithT(t)

	pool := &infrav1.VSphereResourcePool{
		Spec: infrav1.VSphereResourcePoolSpec{
			Datacenter: "Datacenter1",
		},
	}

	slotWithOwnDatacenter := &infrav1.ResourceSlot{
		Hostname:   "host-1",
		Datacenter: "Datacenter2",
	}

	slotWithoutDatacenter := &infrav1.ResourceSlot{
		Hostname: "host-2",
	}

	g.Expect(services.ResolveResourcePoolDatacenter(pool, slotWithOwnDatacenter)).To(Equal("Datacenter2"))
	g.Expect(services.ResolveResourcePoolDatacenter(pool, slotWithoutDatacenter)).To(Equal("Datacenter1"))
	g.Expect(services.ResolveResourcePoolDatacenter(&infrav1.VSphereResourcePool{}, slotWithoutDatacenter)).To(BeEmpty())
	g.Expect(services.ResolveResourcePoolDatacenter(nil, slotWithoutDatacenter)).To(BeEmpty())
}

func ptrTo[T any](v T) *T {
	return &v
}
