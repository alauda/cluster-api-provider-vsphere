/*
Copyright 2025.

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

package services

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

var validNetwork = &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{NetworkName: "net"}}

func TestAllocateSlot(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	ctx := context.Background()
	machine := &infrav1.VSphereMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-machine",
			Namespace: "default",
			UID:       "uid-1",
		},
	}

	t.Run("should reuse already assigned slot", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool",
				Namespace: "default",
			},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-1"},
				},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{
						Hostname: "host-1",
						State:    infrav1.MachineConfigSlotStateInUse,
						MachineRef: &corev1.ObjectReference{
							Name:      machine.Name,
							Namespace: machine.Namespace,
							UID:       machine.UID,
						},
					},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		slot, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, nil, "", nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(slot.Hostname).To(Equal("host-1"))
	})

	t.Run("should prefer Released slot over Available", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-reuse",
				Namespace: "default",
			},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-available"},
					{Hostname: "host-released"},
				},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{Hostname: "host-available", State: infrav1.MachineConfigSlotStateAvailable},
					{Hostname: "host-released", State: infrav1.MachineConfigSlotStateReleased},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		slot, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, nil, "", nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(slot.Hostname).To(Equal("host-released"))

		// Verify status update
		updatedPool := &infrav1.VSphereMachineConfigPool{}
		_ = c.Get(ctx, client.ObjectKeyFromObject(pool), updatedPool)
		g.Expect(updatedPool.Status.ConfigStatuses[1].State).To(Equal(infrav1.MachineConfigSlotStateInUse))
		g.Expect(updatedPool.Status.ConfigStatuses[1].MachineRef.Name).To(Equal(machine.Name))
	})

	t.Run("should return error when no slots available", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-full",
				Namespace: "default",
			},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{Hostname: "host-1", State: infrav1.MachineConfigSlotStateInUse, MachineRef: &corev1.ObjectReference{Name: "other"}},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		_, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, nil, "", nil)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("no available slots"))
	})

	t.Run("should select slot matching desired datacenter", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-datacenter",
				Namespace: "default",
			},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Datacenter: "dc-default",
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-default"},
					{Hostname: "host-target", Datacenter: "dc-target"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		slot, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, nil, "dc-target", nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(slot.Hostname).To(Equal("host-target"))
	})

	t.Run("should return error when no slot matches desired datacenter", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-no-match",
				Namespace: "default",
			},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Datacenter: "dc-default",
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-default"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		_, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, nil, "dc-target", nil)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(`matching datacenter "dc-target"`))
	})

	t.Run("should require slot to satisfy both template and failure domain datacenters", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-template-and-failure-domain",
				Namespace: "default",
			},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-a", Datacenter: "dc-template"},
					{Hostname: "host-b", Datacenter: "dc-fd"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		_, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, nil, "dc-template", []string{"dc-fd"})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(`matching datacenter "dc-template" and failure domain datacenters`))
	})

	t.Run("should select slot matching failure domain datacenters when template datacenter is empty", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-failure-domain-match",
				Namespace: "default",
			},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Datacenter: "dc-pool",
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-a"},
					{Hostname: "host-b", Datacenter: "dc-fd"},
				},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{Hostname: "host-a", State: infrav1.MachineConfigSlotStateAvailable},
					{Hostname: "host-b", State: infrav1.MachineConfigSlotStateReleased},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		slot, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, nil, "", []string{"dc-fd"})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(slot.Hostname).To(Equal("host-b"))
		g.Expect(slot.Datacenter).To(Equal("dc-fd"))
	})

	t.Run("should treat wildcard template datacenter as unspecified when matching failure domain datacenters", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-failure-domain-match-wildcard",
				Namespace: "default",
			},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Datacenter: "dc-pool",
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-a"},
					{Hostname: "host-b", Datacenter: "dc-fd"},
				},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{Hostname: "host-a", State: infrav1.MachineConfigSlotStateAvailable},
					{Hostname: "host-b", State: infrav1.MachineConfigSlotStateAvailable},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		slot, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, nil, "*", []string{"dc-fd"})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(slot.Hostname).To(Equal("host-b"))
		g.Expect(slot.Datacenter).To(Equal("dc-fd"))
	})

	t.Run("should return error when no slot matches failure domain datacenters", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-failure-domain-no-match",
				Namespace: "default",
			},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Datacenter: "dc-pool",
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-a"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		_, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, nil, "", []string{"dc-fd"})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("matching failure domain datacenters"))
	})

	t.Run("should backfill the resolved datacenter onto the selected slot", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-backfill-datacenter",
				Namespace: "default",
			},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Datacenter: "dc-pool",
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-a"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		slot, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, nil, "", []string{"dc-pool"})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(slot.Datacenter).To(Equal("dc-pool"))
	})
}

func TestReleaseSlot(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	ctx := context.Background()
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-release",
			Namespace: "default",
		},
		Status: infrav1.VSphereMachineConfigPoolStatus{
			ConfigStatuses: []infrav1.MachineConfigSlotStatus{
				{
					Hostname: "host-1",
					State:    infrav1.MachineConfigSlotStateInUse,
					MachineRef: &corev1.ObjectReference{
						Name:      "my-machine",
						Namespace: "default",
					},
				},
			},
			// An attached disk on the slot must flip to Available on release: the
			// VM is gone so the disk is detached but still backed and reusable.
			PersistentDiskStatuses: []infrav1.PersistentDiskStatus{{
				Hostname:   "host-1",
				Name:       "data-0",
				VolumePath: "[ds] host-1/data-0.vmdk",
				Phase:      infrav1.PersistentDiskPhaseAttached,
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

	err := ReleaseSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, &corev1.ObjectReference{Name: "my-machine", Namespace: "default"})
	g.Expect(err).NotTo(HaveOccurred())

	updatedPool := &infrav1.VSphereMachineConfigPool{}
	_ = c.Get(ctx, client.ObjectKeyFromObject(pool), updatedPool)
	g.Expect(updatedPool.Status.ConfigStatuses[0].State).To(Equal(infrav1.MachineConfigSlotStateReleased))
	g.Expect(updatedPool.Status.ConfigStatuses[0].LastReleasedTime).NotTo(BeNil())
	g.Expect(updatedPool.Status.PersistentDiskStatuses[0].Phase).To(Equal(infrav1.PersistentDiskPhaseAvailable))
}

func TestGetSlotForMachine(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	ctx := context.Background()
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-lookup",
			Namespace: "default",
		},
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Configs: []infrav1.MachineConfigSlot{
				{Hostname: "host-a"},
				{Hostname: "host-b"},
			},
		},
		Status: infrav1.VSphereMachineConfigPoolStatus{
			ConfigStatuses: []infrav1.MachineConfigSlotStatus{
				{
					Hostname: "host-b",
					State:    infrav1.MachineConfigSlotStateInUse,
					MachineRef: &corev1.ObjectReference{
						Name:      "my-machine",
						Namespace: "default",
					},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

	slot, err := GetSlotForMachine(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, &corev1.ObjectReference{Name: "my-machine", Namespace: "default"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(slot).NotTo(BeNil())
	g.Expect(slot.Hostname).To(Equal("host-b"))
}

func TestResolveMachineConfigPoolDatacenter(t *testing.T) {
	g := NewWithT(t)

	pool := &infrav1.VSphereMachineConfigPool{
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Datacenter: "dc-pool",
		},
	}

	slotWithOwnDatacenter := &infrav1.MachineConfigSlot{
		Hostname:   "host-1",
		Datacenter: "dc-slot",
	}
	slotWithoutDatacenter := &infrav1.MachineConfigSlot{
		Hostname: "host-2",
	}

	g.Expect(ResolveMachineConfigPoolDatacenter(pool, slotWithOwnDatacenter)).To(Equal("dc-slot"))
	g.Expect(ResolveMachineConfigPoolDatacenter(pool, slotWithoutDatacenter)).To(Equal("dc-pool"))
	g.Expect(ResolveMachineConfigPoolDatacenter(nil, slotWithoutDatacenter)).To(BeEmpty())
}

func TestDatacentersWithAvailableSlots(t *testing.T) {
	t.Run("empty pool list returns empty result", func(t *testing.T) {
		g := NewWithT(t)
		result := DatacentersWithAvailableSlots(nil)
		g.Expect(result).To(BeEmpty())
	})

	t.Run("available and released slots are counted", func(t *testing.T) {
		g := NewWithT(t)
		pools := []infrav1.VSphereMachineConfigPool{{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "cluster-1"},
				Datacenter: "dc-1",
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-1"},
					{Hostname: "host-2"},
					{Hostname: "host-3", Datacenter: "dc-2"},
				},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{Hostname: "host-1", State: infrav1.MachineConfigSlotStateInUse},
					{Hostname: "host-2", State: infrav1.MachineConfigSlotStateReleased},
					{Hostname: "host-3", State: infrav1.MachineConfigSlotStateAvailable},
				},
			},
		}}
		result := DatacentersWithAvailableSlots(pools)
		g.Expect(result).To(HaveKey("dc-1"))
		g.Expect(result).To(HaveKey("dc-2"))
	})

	t.Run("all slots in use excludes datacenter", func(t *testing.T) {
		g := NewWithT(t)
		pools := []infrav1.VSphereMachineConfigPool{{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "cluster-1"},
				Datacenter: "dc-1",
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-1"},
					{Hostname: "host-2"},
				},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{Hostname: "host-1", State: infrav1.MachineConfigSlotStateInUse},
					{Hostname: "host-2", State: infrav1.MachineConfigSlotStateInUse},
				},
			},
		}}
		result := DatacentersWithAvailableSlots(pools)
		g.Expect(result).NotTo(HaveKey("dc-1"))
	})

	t.Run("uninitialized slots count as available", func(t *testing.T) {
		g := NewWithT(t)
		pools := []infrav1.VSphereMachineConfigPool{{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "cluster-1"},
				Datacenter: "dc-1",
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
			},
			// No status entries
		}}
		result := DatacentersWithAvailableSlots(pools)
		g.Expect(result).To(HaveKey("dc-1"))
	})

	t.Run("slot-level datacenter overrides pool-level", func(t *testing.T) {
		g := NewWithT(t)
		pools := []infrav1.VSphereMachineConfigPool{{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "cluster-1"},
				Datacenter: "dc-pool",
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-1", Datacenter: "dc-slot"},
				},
			},
		}}
		result := DatacentersWithAvailableSlots(pools)
		g.Expect(result).To(HaveKey("dc-slot"))
		g.Expect(result).NotTo(HaveKey("dc-pool"))
	})

	t.Run("multiple pools aggregate available datacenters", func(t *testing.T) {
		g := NewWithT(t)
		pools := []infrav1.VSphereMachineConfigPool{
			{
				Spec: infrav1.VSphereMachineConfigPoolSpec{
					ClusterRef: corev1.ObjectReference{Name: "cluster-1"},
					Datacenter: "dc-1",
					Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
				},
				Status: infrav1.VSphereMachineConfigPoolStatus{
					ConfigStatuses: []infrav1.MachineConfigSlotStatus{
						{Hostname: "host-1", State: infrav1.MachineConfigSlotStateInUse},
					},
				},
			},
			{
				Spec: infrav1.VSphereMachineConfigPoolSpec{
					ClusterRef: corev1.ObjectReference{Name: "cluster-1"},
					Datacenter: "dc-1",
					Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-2"}},
				},
				// host-2 has no status -> available
			},
		}
		result := DatacentersWithAvailableSlots(pools)
		g.Expect(result).To(HaveKey("dc-1"))
	})
}

func TestIsPoolFullyReusable(t *testing.T) {
	g := NewWithT(t)
	pool := &infrav1.VSphereMachineConfigPool{
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Configs:    []infrav1.MachineConfigSlot{{Hostname: "slot-1"}},
		},
		Status: infrav1.VSphereMachineConfigPoolStatus{
			ConfigStatuses: []infrav1.MachineConfigSlotStatus{{Hostname: "slot-1", State: infrav1.MachineConfigSlotStateAvailable}},
		},
	}
	g.Expect(IsPoolFullyReusable(pool)).To(BeTrue())

	released := pool.DeepCopy()
	now := metav1.Now()
	released.Status.ConfigStatuses[0].State = infrav1.MachineConfigSlotStateReleased
	released.Status.ConfigStatuses[0].LastReleasedTime = &now
	g.Expect(IsPoolFullyReusable(released)).To(BeFalse())

	// A reclaim task in flight (tracked per-disk in status) keeps the pool bound.
	withTask := pool.DeepCopy()
	withTask.Spec.Configs[0].PersistentDisks = []infrav1.PersistentDisk{{Name: "disk-1"}}
	withTask.Status.PersistentDiskStatuses = []infrav1.PersistentDiskStatus{
		{Hostname: "slot-1", Name: "disk-1", Phase: infrav1.PersistentDiskPhaseReclaiming, TaskRef: "task-1"},
	}
	g.Expect(IsPoolFullyReusable(withTask)).To(BeFalse())

	// A disk whose observed VolumePath still lives on spec (un-seeded legacy).
	withDisk := pool.DeepCopy()
	withDisk.Spec.Configs[0].PersistentDisks = []infrav1.PersistentDisk{{Name: "disk-1", VolumePath: "[ds] vm/disk.vmdk"}}
	g.Expect(IsPoolFullyReusable(withDisk)).To(BeFalse())

	// A failed reclaim awaiting retry keeps the pool bound.
	withRetry := pool.DeepCopy()
	retryAfter := metav1.Now()
	withRetry.Spec.Configs[0].PersistentDisks = []infrav1.PersistentDisk{{Name: "disk-1"}}
	withRetry.Status.PersistentDiskStatuses = []infrav1.PersistentDiskStatus{
		{Hostname: "slot-1", Name: "disk-1", Phase: infrav1.PersistentDiskPhaseError, RetryAfter: &retryAfter},
	}
	g.Expect(IsPoolFullyReusable(withRetry)).To(BeFalse())
}

func TestFindMachineConfigPoolForMachine(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	ctx := context.Background()
	poolA := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-a",
			Namespace: "default",
		},
		Status: infrav1.VSphereMachineConfigPoolStatus{
			ConfigStatuses: []infrav1.MachineConfigSlotStatus{
				{
					Hostname: "host-a",
					State:    infrav1.MachineConfigSlotStateInUse,
					MachineRef: &corev1.ObjectReference{
						Name:      "other-machine",
						Namespace: "default",
					},
				},
			},
		},
	}
	poolB := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-b",
			Namespace: "default",
		},
		Status: infrav1.VSphereMachineConfigPoolStatus{
			ConfigStatuses: []infrav1.MachineConfigSlotStatus{
				{
					Hostname: "host-b",
					State:    infrav1.MachineConfigSlotStateInUse,
					MachineRef: &corev1.ObjectReference{
						Name:      "my-machine",
						Namespace: "default",
						UID:       "machine-uid",
					},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(poolA, poolB).WithStatusSubresource(poolA, poolB).Build()

	poolRef, err := FindMachineConfigPoolForMachine(ctx, c, "default", &corev1.ObjectReference{Name: "my-machine", Namespace: "default", UID: "machine-uid"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(poolRef).NotTo(BeNil())
	g.Expect(poolRef.Name).To(Equal("pool-b"))
}

func toInt32Ptr(v int32) *int32 {
	return &v
}

func TestPersistSlotChangesPersistsUnitNumber(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	ctx := context.Background()
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-persist",
			Namespace: "default",
		},
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Configs: []infrav1.MachineConfigSlot{
				{
					Hostname: "host-1",
					Network:  validNetwork,
					PersistentDisks: []infrav1.PersistentDisk{
						{Name: "disk-1"},
					},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

	unitNumber := int32(3)
	err := PersistSlotChanges(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, &infrav1.MachineConfigSlot{
		Hostname: "host-1",
		PersistentDisks: []infrav1.PersistentDisk{
			{
				Name:       "disk-1",
				UnitNumber: &unitNumber,
				VolumePath: "[datastore1] disk-1/disk-1.vmdk",
				DiskUUID:   "disk-uuid",
			},
		},
	}, "machine-1", "machine-1-uid")
	g.Expect(err).NotTo(HaveOccurred())

	updatedPool := &infrav1.VSphereMachineConfigPool{}
	_ = c.Get(ctx, client.ObjectKeyFromObject(pool), updatedPool)

	// Observed state is recorded in status, not written back to spec.
	rec, _ := infrav1.FindDiskStatus(updatedPool, "host-1", "disk-1")
	g.Expect(rec).NotTo(BeNil())
	g.Expect(rec.UnitNumber).NotTo(BeNil())
	g.Expect(*rec.UnitNumber).To(Equal(unitNumber))
	g.Expect(rec.VolumePath).To(Equal("[datastore1] disk-1/disk-1.vmdk"))
	g.Expect(rec.DiskUUID).To(Equal("disk-uuid"))
	g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseAttached))
	g.Expect(rec.OwnerMachineName).To(Equal("machine-1"))
	g.Expect(updatedPool.Spec.Configs[0].PersistentDisks[0].VolumePath).To(BeEmpty())
	g.Expect(updatedPool.Spec.Configs[0].PersistentDisks[0].UnitNumber).To(BeNil())
}

func TestResolveMachineConsumerRef(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)

	ctx := context.Background()

	t.Run("resolves kubeadmcontrolplane owner", func(t *testing.T) {
		kcp := &controlplanev1.KubeadmControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp-a", Namespace: "default", UID: "kcp-uid"},
		}
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "machine-a",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: controlplanev1.GroupVersion.String(),
					Kind:       "KubeadmControlPlane",
					Name:       "cp-a",
					UID:        "kcp-uid",
				}},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp, machine).Build()
		ref, err := ResolveMachineConsumerRef(ctx, c, machine)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ref).NotTo(BeNil())
		g.Expect(ref.Kind).To(Equal("KubeadmControlPlane"))
		g.Expect(ref.Name).To(Equal("cp-a"))
	})

	t.Run("resolves machinedeployment owner through machineset", func(t *testing.T) {
		md := &clusterv1.MachineDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "md-a", Namespace: "default", UID: "md-uid"},
		}
		ms := &clusterv1.MachineSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ms-a",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "MachineDeployment",
					Name:       "md-a",
					UID:        "md-uid",
				}},
			},
		}
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "machine-b",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "MachineSet",
					Name:       "ms-a",
				}},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(md, ms, machine).Build()
		ref, err := ResolveMachineConsumerRef(ctx, c, machine)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ref).NotTo(BeNil())
		g.Expect(ref.Kind).To(Equal("MachineDeployment"))
		g.Expect(ref.Name).To(Equal("md-a"))
	})

	t.Run("fails closed when owner cannot be resolved", func(t *testing.T) {
		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "machine-c", Namespace: "default"},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build()
		ref, err := ResolveMachineConsumerRef(ctx, c, machine)
		g.Expect(err).To(HaveOccurred())
		g.Expect(ref).To(BeNil())
	})
}

func TestApplyDiskBackfill(t *testing.T) {
	int32Ptr := func(v int32) *int32 { return &v }

	newPool := func(disks ...infrav1.PersistentDisk) *infrav1.VSphereMachineConfigPool {
		return &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Configs: []infrav1.MachineConfigSlot{{
					Hostname:        "host-1",
					PersistentDisks: disks,
				}},
			},
		}
	}

	t.Run("records observed state in status, leaving spec untouched", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool(infrav1.PersistentDisk{Name: "disk-a", SizeGiB: 20})
		updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
			Hostname: "host-1",
			PersistentDisks: []infrav1.PersistentDisk{
				{Name: "disk-a", SizeGiB: 20, VolumePath: "[ds] vm/disk.vmdk", DiskUUID: "uuid-1", UnitNumber: int32Ptr(1)},
			},
		}, "machine-1", "machine-1-uid")
		g.Expect(updated).To(BeTrue())
		rec, _ := infrav1.FindDiskStatus(pool, "host-1", "disk-a")
		g.Expect(rec).NotTo(BeNil())
		g.Expect(rec.VolumePath).To(Equal("[ds] vm/disk.vmdk"))
		g.Expect(rec.DiskUUID).To(Equal("uuid-1"))
		g.Expect(*rec.UnitNumber).To(Equal(int32(1)))
		g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseAttached))
		g.Expect(rec.OwnerMachineName).To(Equal("machine-1"))
		g.Expect(rec.OwnerMachineUID).To(Equal("machine-1-uid"))
		// spec is frozen — never written back.
		g.Expect(pool.Spec.Configs[0].PersistentDisks[0].VolumePath).To(BeEmpty())
		g.Expect(pool.Spec.Configs[0].PersistentDisks[0].UnitNumber).To(BeNil())
	})

	t.Run("records duplicate-size disks by name", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool(
			infrav1.PersistentDisk{Name: "disk-a", SizeGiB: 20},
			infrav1.PersistentDisk{Name: "disk-b", SizeGiB: 20},
		)
		updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
			Hostname: "host-1",
			PersistentDisks: []infrav1.PersistentDisk{
				{Name: "disk-a", SizeGiB: 20, VolumePath: "[ds] vm/disk-a.vmdk", DiskUUID: "uuid-a", UnitNumber: int32Ptr(0)},
				{Name: "disk-b", SizeGiB: 20, VolumePath: "[ds] vm/disk-b.vmdk", DiskUUID: "uuid-b", UnitNumber: int32Ptr(1)},
			},
		}, "machine-1", "machine-1-uid")
		g.Expect(updated).To(BeTrue())
		recA, _ := infrav1.FindDiskStatus(pool, "host-1", "disk-a")
		recB, _ := infrav1.FindDiskStatus(pool, "host-1", "disk-b")
		g.Expect(recA.VolumePath).To(Equal("[ds] vm/disk-a.vmdk"))
		g.Expect(*recA.UnitNumber).To(Equal(int32(0)))
		g.Expect(recB.VolumePath).To(Equal("[ds] vm/disk-b.vmdk"))
		g.Expect(*recB.UnitNumber).To(Equal(int32(1)))
	})

	t.Run("returns false when the status record is already up to date", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool(infrav1.PersistentDisk{Name: "disk-a", SizeGiB: 20})
		pool.Status.PersistentDiskStatuses = []infrav1.PersistentDiskStatus{{
			Hostname: "host-1", Name: "disk-a",
			VolumePath: "[ds] vm/disk.vmdk", DiskUUID: "uuid-1", UnitNumber: int32Ptr(1),
			Phase: infrav1.PersistentDiskPhaseAttached, OwnerMachineName: "machine-1", OwnerMachineUID: "machine-1-uid",
		}}
		updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
			Hostname: "host-1",
			PersistentDisks: []infrav1.PersistentDisk{
				{Name: "disk-a", SizeGiB: 20, VolumePath: "[ds] vm/disk.vmdk", DiskUUID: "uuid-1", UnitNumber: int32Ptr(1)},
			},
		}, "machine-1", "machine-1-uid")
		g.Expect(updated).To(BeFalse())
	})

	t.Run("skips unprovisioned disk with no existing record", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool(infrav1.PersistentDisk{Name: "disk-a", SizeGiB: 20})
		updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
			Hostname: "host-1",
			PersistentDisks: []infrav1.PersistentDisk{
				{Name: "disk-a", SizeGiB: 20}, // no VolumePath yet
			},
		}, "machine-1", "machine-1-uid")
		g.Expect(updated).To(BeFalse())
		g.Expect(pool.Status.PersistentDiskStatuses).To(BeEmpty())
	})

	t.Run("seeds Creating for a fallback disk with only a unit number", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool(infrav1.PersistentDisk{Name: "disk-a", SizeGiB: 20})
		updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
			Hostname: "host-1",
			PersistentDisks: []infrav1.PersistentDisk{
				{Name: "disk-a", SizeGiB: 20, UnitNumber: int32Ptr(2)}, // clone assigned a unit, vmdk not observed yet
			},
		}, "machine-1", "machine-1-uid")
		g.Expect(updated).To(BeTrue())
		rec, _ := infrav1.FindDiskStatus(pool, "host-1", "disk-a")
		g.Expect(rec).NotTo(BeNil())
		g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseCreating))
		g.Expect(rec.VolumePath).To(BeEmpty())
		g.Expect(*rec.UnitNumber).To(Equal(int32(2)))
	})

	t.Run("Creating flips to Attached once VolumePath is observed", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool(infrav1.PersistentDisk{Name: "disk-a", SizeGiB: 20})
		pool.Status.PersistentDiskStatuses = []infrav1.PersistentDiskStatus{{
			Hostname: "host-1", Name: "disk-a", UnitNumber: int32Ptr(2),
			Phase: infrav1.PersistentDiskPhaseCreating, OwnerMachineName: "machine-1", OwnerMachineUID: "machine-1-uid",
		}}
		updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
			Hostname: "host-1",
			PersistentDisks: []infrav1.PersistentDisk{
				{Name: "disk-a", SizeGiB: 20, VolumePath: "[ds] vm/disk-a.vmdk", DiskUUID: "uuid-a", UnitNumber: int32Ptr(2)},
			},
		}, "machine-1", "machine-1-uid")
		g.Expect(updated).To(BeTrue())
		rec, _ := infrav1.FindDiskStatus(pool, "host-1", "disk-a")
		g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseAttached))
		g.Expect(rec.VolumePath).To(Equal("[ds] vm/disk-a.vmdk"))
	})

	t.Run("does not mark an unverified expected path as Attached", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool(infrav1.PersistentDisk{Name: "disk-a", SizeGiB: 20})
		updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
			Hostname: "host-1",
			PersistentDisks: []infrav1.PersistentDisk{
				// createDataDisks sets the expected path before CloneVM completes;
				// no UUID means the backing has not been observed yet.
				{Name: "disk-a", SizeGiB: 20, VolumePath: "[ds] host-1/disk-a.vmdk", UnitNumber: int32Ptr(2)},
			},
		}, "machine-1", "machine-1-uid")
		g.Expect(updated).To(BeTrue())
		rec, _ := infrav1.FindDiskStatus(pool, "host-1", "disk-a")
		g.Expect(rec).NotTo(BeNil())
		g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseCreating))
		g.Expect(rec.VolumePath).To(BeEmpty())
		g.Expect(rec.DiskUUID).To(BeEmpty())
	})

	t.Run("does not downgrade an active disk to Creating", func(t *testing.T) {
		g := NewWithT(t)
		for _, phase := range []infrav1.PersistentDiskPhase{
			infrav1.PersistentDiskPhaseAttached,
			infrav1.PersistentDiskPhaseAvailable,
			infrav1.PersistentDiskPhaseReclaiming,
		} {
			pool := newPool(infrav1.PersistentDisk{Name: "disk-a", SizeGiB: 20})
			pool.Status.PersistentDiskStatuses = []infrav1.PersistentDiskStatus{{
				Hostname: "host-1", Name: "disk-a", VolumePath: "[ds] vm/disk-a.vmdk", UnitNumber: int32Ptr(2),
				Phase: phase, OwnerMachineName: "machine-1", OwnerMachineUID: "machine-1-uid",
			}}
			updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
				Hostname: "host-1",
				PersistentDisks: []infrav1.PersistentDisk{
					{Name: "disk-a", SizeGiB: 20, UnitNumber: int32Ptr(2)}, // no VolumePath (lost hydrate)
				},
			}, "machine-1", "machine-1-uid")
			g.Expect(updated).To(BeFalse(), "phase %s must not be downgraded", phase)
			rec, _ := infrav1.FindDiskStatus(pool, "host-1", "disk-a")
			g.Expect(rec.Phase).To(Equal(phase))
		}
	})

	t.Run("a reused Reclaimed tombstone can be reseeded to Creating", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool(infrav1.PersistentDisk{Name: "disk-a", SizeGiB: 20})
		pool.Status.PersistentDiskStatuses = []infrav1.PersistentDiskStatus{{
			Hostname: "host-1", Name: "disk-a", Phase: infrav1.PersistentDiskPhaseReclaimed,
		}}
		updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
			Hostname: "host-1",
			PersistentDisks: []infrav1.PersistentDisk{
				{Name: "disk-a", SizeGiB: 20, UnitNumber: int32Ptr(2)},
			},
		}, "machine-2", "machine-2-uid")
		g.Expect(updated).To(BeTrue())
		rec, _ := infrav1.FindDiskStatus(pool, "host-1", "disk-a")
		g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseCreating))
		g.Expect(rec.OwnerMachineName).To(Equal("machine-2"))
	})

	t.Run("nil pool or slot returns false", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(ApplyDiskBackfill(nil, &infrav1.MachineConfigSlot{}, "", "")).To(BeFalse())
		g.Expect(ApplyDiskBackfill(&infrav1.VSphereMachineConfigPool{}, nil, "", "")).To(BeFalse())
	})

	t.Run("records ephemeral disk observed unit without a VolumePath gate", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
			Hostname: "host-1",
			EphemeralDisks: []infrav1.EphemeralDisk{
				{Name: "cache-1", SizeGiB: 20, UnitNumber: int32Ptr(3)},
			},
		}, "machine-1", "machine-1-uid")
		g.Expect(updated).To(BeTrue())
		rec, _ := infrav1.FindEphemeralDiskStatus(pool, "host-1", "cache-1")
		g.Expect(rec).NotTo(BeNil())
		g.Expect(*rec.UnitNumber).To(Equal(int32(3)))
	})

	t.Run("skips ephemeral disk before its unit is known", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
			Hostname: "host-1",
			EphemeralDisks: []infrav1.EphemeralDisk{
				{Name: "cache-1", SizeGiB: 20}, // unit not assigned yet
			},
		}, "machine-1", "machine-1-uid")
		g.Expect(updated).To(BeFalse())
		g.Expect(pool.Status.EphemeralDiskStatuses).To(BeEmpty())
	})

	t.Run("returns false when the ephemeral unit is already recorded", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool()
		pool.Status.EphemeralDiskStatuses = []infrav1.EphemeralDiskStatus{
			{Hostname: "host-1", Name: "cache-1", UnitNumber: int32Ptr(3)},
		}
		updated := ApplyDiskBackfill(pool, &infrav1.MachineConfigSlot{
			Hostname: "host-1",
			EphemeralDisks: []infrav1.EphemeralDisk{
				{Name: "cache-1", SizeGiB: 20, UnitNumber: int32Ptr(3)},
			},
		}, "machine-1", "machine-1-uid")
		g.Expect(updated).To(BeFalse())
	})
}

func TestObjectForConsumerRef(t *testing.T) {
	g := NewWithT(t)

	g.Expect(ObjectForConsumerRef(nil)).To(BeNil())

	kcp := ObjectForConsumerRef(&corev1.ObjectReference{Kind: "KubeadmControlPlane"})
	g.Expect(kcp).NotTo(BeNil())

	md := ObjectForConsumerRef(&corev1.ObjectReference{Kind: "MachineDeployment"})
	g.Expect(md).NotTo(BeNil())

	unknown := ObjectForConsumerRef(&corev1.ObjectReference{Kind: "Pod"})
	g.Expect(unknown).To(BeNil())
}

func TestValidateSlotFields(t *testing.T) {
	int32Ptr := func(v int32) *int32 { return &v }
	validNetwork := &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{NetworkName: "net"}}

	t.Run("rejects empty configs", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{},
		}
		errs := ValidateSlotFields(pool)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Field).To(Equal("spec.configs"))
	})

	t.Run("rejects slot without network", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
			},
		}
		errs := ValidateSlotFields(pool)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Field).To(Equal("spec.configs[0].network"))
	})

	t.Run("rejects slot without primary networkName", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "host-1",
					Network:  &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{}},
				}},
			},
		}
		errs := ValidateSlotFields(pool)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Field).To(Equal("spec.configs[0].network.primary.networkName"))
	})

	t.Run("rejects additional network without networkName", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "host-1",
					Network: &infrav1.MachineConfigSlotNetwork{
						Primary:    infrav1.NetworkConfig{NetworkName: "net"},
						Additional: []infrav1.NetworkConfig{{}},
					},
				}},
			},
		}
		errs := ValidateSlotFields(pool)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Field).To(Equal("spec.configs[0].network.additional[0].networkName"))
	})

	t.Run("valid pool has no errors", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "host-1",
					Network:  validNetwork,
					PersistentDisks: []infrav1.PersistentDisk{
						{Name: "etcd", SizeGiB: 20, UnitNumber: int32Ptr(0), MountPath: "/var/lib/etcd"},
						{Name: "data", SizeGiB: 50, UnitNumber: int32Ptr(1), MountPath: "/var/lib/data"},
					},
				}},
			},
		}
		g.Expect(ValidateSlotFields(pool)).To(BeEmpty())
	})

	t.Run("flags bad size, reserved and out-of-range unit numbers", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "host-1",
					Network:  validNetwork,
					PersistentDisks: []infrav1.PersistentDisk{
						{Name: "a", SizeGiB: 0, UnitNumber: int32Ptr(7)},
						{Name: "b", SizeGiB: 10, UnitNumber: int32Ptr(16)},
					},
				}},
			},
		}
		errs := ValidateSlotFields(pool)
		g.Expect(errs).To(HaveLen(3)) // sizeGiB<1, unit 7 reserved, unit 16 out of range
	})

	t.Run("flags intra-slot duplicate name and mountPath", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "host-1",
					Network:  validNetwork,
					PersistentDisks: []infrav1.PersistentDisk{
						{Name: "dup", SizeGiB: 10, UnitNumber: int32Ptr(1), MountPath: "/data"},
						{Name: "dup", SizeGiB: 10, UnitNumber: int32Ptr(1), MountPath: "/data"},
					},
				}},
			},
		}
		errs := ValidateSlotFields(pool)
		g.Expect(errs).To(HaveLen(2)) // duplicate name, mountPath
	})

	t.Run("flags normalized duplicate mountPath", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "host-1",
					Network:  validNetwork,
					PersistentDisks: []infrav1.PersistentDisk{
						{Name: "data-1", SizeGiB: 10, UnitNumber: int32Ptr(1), MountPath: "/data"},
						{Name: "data-2", SizeGiB: 10, UnitNumber: int32Ptr(2), MountPath: "/data/"},
					},
				}},
			},
		}
		errs := ValidateSlotFields(pool)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Field).To(Equal("spec.configs[0].persistentDisks[1].mountPath"))
	})

	t.Run("rejects invalid mountPath values", func(t *testing.T) {
		testCases := []struct {
			name      string
			mountPath string
		}{
			{name: "relative", mountPath: "data"},
			{name: "tab", mountPath: "/data\tlogs"},
			{name: "newline", mountPath: "/data\nlogs"},
			{name: "root", mountPath: "/"},
		}
		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				g := NewWithT(t)
				pool := &infrav1.VSphereMachineConfigPool{
					Spec: infrav1.VSphereMachineConfigPoolSpec{
						Configs: []infrav1.MachineConfigSlot{{
							Hostname: "host-1",
							Network:  validNetwork,
							PersistentDisks: []infrav1.PersistentDisk{
								{Name: "data", SizeGiB: 10, UnitNumber: int32Ptr(1), MountPath: tc.mountPath},
							},
						}},
					},
				}
				errs := ValidateSlotFields(pool)
				g.Expect(errs).To(HaveLen(1))
				g.Expect(errs[0].Field).To(Equal("spec.configs[0].persistentDisks[0].mountPath"))
			})
		}
	})

	t.Run("valid ephemeral disks alongside persistent disks have no errors", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "host-1",
					Network:  validNetwork,
					PersistentDisks: []infrav1.PersistentDisk{
						{Name: "etcd", SizeGiB: 20, UnitNumber: int32Ptr(0), MountPath: "/var/lib/etcd"},
					},
					EphemeralDisks: []infrav1.EphemeralDisk{
						{Name: "cache", SizeGiB: 50, MountPath: "/var/lib/containerd"},
					},
				}},
			},
		}
		g.Expect(ValidateSlotFields(pool)).To(BeEmpty())
	})

	t.Run("flags ephemeral disk bad size and empty name", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "host-1",
					Network:  validNetwork,
					EphemeralDisks: []infrav1.EphemeralDisk{
						{Name: "", SizeGiB: 0},
					},
				}},
			},
		}
		errs := ValidateSlotFields(pool)
		g.Expect(errs).To(HaveLen(2)) // required name, sizeGiB<1
	})

	t.Run("flags name and mountPath colliding across persistent and ephemeral disks", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "host-1",
					Network:  validNetwork,
					PersistentDisks: []infrav1.PersistentDisk{
						{Name: "shared", SizeGiB: 10, UnitNumber: int32Ptr(1), MountPath: "/data"},
					},
					EphemeralDisks: []infrav1.EphemeralDisk{
						{Name: "shared", SizeGiB: 10, MountPath: "/data"},
					},
				}},
			},
		}
		errs := ValidateSlotFields(pool)
		g.Expect(errs).To(HaveLen(2)) // duplicate name and mountPath across the two lists
	})
}

func TestCrossPoolUniquenessConflicts(t *testing.T) {
	poolWith := func(name, clusterName string, slots ...infrav1.MachineConfigSlot) infrav1.VSphereMachineConfigPool {
		return infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: clusterName},
				Configs:    slots,
			},
		}
	}
	slot := func(host, ip string) infrav1.MachineConfigSlot {
		s := infrav1.MachineConfigSlot{Hostname: host}
		if ip != "" {
			s.Network = &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{IP: ip}}
		}
		return s
	}

	t.Run("no clusterRef yields no conflicts", func(t *testing.T) {
		g := NewWithT(t)
		pool := poolWith("p1", "", slot("h1", "10.0.0.1"))
		other := poolWith("p2", "", slot("h1", "10.0.0.1"))
		g.Expect(CrossPoolUniquenessConflicts(&pool, []infrav1.VSphereMachineConfigPool{other})).To(BeEmpty())
	})

	t.Run("hostname and IP collide within the same cluster", func(t *testing.T) {
		g := NewWithT(t)
		pool := poolWith("p1", "cluster-a", slot("h1", "10.0.0.1"), slot("h2", "10.0.0.2"))
		other := poolWith("p2", "cluster-a", slot("h1", "10.0.0.9"), slot("hx", "10.0.0.2"))
		conflicts := CrossPoolUniquenessConflicts(&pool, []infrav1.VSphereMachineConfigPool{other})
		g.Expect(conflicts).To(HaveLen(2))
		g.Expect(conflicts[0]).To(Equal(CrossPoolConflict{ConfigIndex: 0, Field: "hostname", Value: "h1", OtherPool: "p2"}))
		g.Expect(conflicts[1]).To(Equal(CrossPoolConflict{ConfigIndex: 1, Field: "ip", Value: "10.0.0.2", OtherPool: "p2"}))
	})

	t.Run("different cluster or namespace does not conflict", func(t *testing.T) {
		g := NewWithT(t)
		pool := poolWith("p1", "cluster-a", slot("h1", "10.0.0.1"))
		otherCluster := poolWith("p2", "cluster-b", slot("h1", "10.0.0.1"))
		otherNs := poolWith("p3", "cluster-a", slot("h1", "10.0.0.1"))
		otherNs.Namespace = "elsewhere"
		g.Expect(CrossPoolUniquenessConflicts(&pool, []infrav1.VSphereMachineConfigPool{otherCluster, otherNs})).To(BeEmpty())
	})

	t.Run("the pool does not conflict with itself", func(t *testing.T) {
		g := NewWithT(t)
		pool := poolWith("p1", "cluster-a", slot("h1", "10.0.0.1"))
		g.Expect(CrossPoolUniquenessConflicts(&pool, []infrav1.VSphereMachineConfigPool{pool})).To(BeEmpty())
	})
}

func TestValidateAllocatedSlotsImmutable(t *testing.T) {
	int32Ptr := func(v int32) *int32 { return &v }
	allocatedPool := func() *infrav1.VSphereMachineConfigPool {
		return &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "h1",
					Network:  &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{IP: "10.0.0.1", IPv6: "fd00::1"}},
					PersistentDisks: []infrav1.PersistentDisk{
						{Name: "etcd", SizeGiB: 20, UnitNumber: int32Ptr(1), MountPath: "/var/lib/etcd"},
					},
				}},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{Hostname: "h1", State: infrav1.MachineConfigSlotStateInUse},
				},
			},
		}
	}

	t.Run("no allocated slots means no immutability errors", func(t *testing.T) {
		g := NewWithT(t)
		oldPool := allocatedPool()
		oldPool.Status.ConfigStatuses[0].State = infrav1.MachineConfigSlotStateAvailable
		newPool := oldPool.DeepCopy()
		newPool.Spec.Configs[0].Network.Primary.IP = "10.0.0.99"
		g.Expect(ValidateAllocatedSlotsImmutable(oldPool, newPool)).To(BeEmpty())
	})

	t.Run("unchanged allocated slot passes", func(t *testing.T) {
		g := NewWithT(t)
		oldPool := allocatedPool()
		g.Expect(ValidateAllocatedSlotsImmutable(oldPool, oldPool.DeepCopy())).To(BeEmpty())
	})

	t.Run("allows normalized equivalent mountPath on allocated disk", func(t *testing.T) {
		g := NewWithT(t)
		oldPool := allocatedPool()
		newPool := oldPool.DeepCopy()
		newPool.Spec.Configs[0].PersistentDisks[0].MountPath = "/var/lib/etcd/"
		g.Expect(ValidateAllocatedSlotsImmutable(oldPool, newPool)).To(BeEmpty())
	})

	t.Run("rejects removing an allocated slot", func(t *testing.T) {
		g := NewWithT(t)
		oldPool := allocatedPool()
		newPool := oldPool.DeepCopy()
		newPool.Spec.Configs = nil
		errs := ValidateAllocatedSlotsImmutable(oldPool, newPool)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Detail).To(ContainSubstring("cannot remove allocated slot"))
	})

	t.Run("rejects changing IP, disk size and mountPath but allows unit number", func(t *testing.T) {
		g := NewWithT(t)
		oldPool := allocatedPool()
		newPool := oldPool.DeepCopy()
		newPool.Spec.Configs[0].Network.Primary.IP = "10.0.0.2"
		newPool.Spec.Configs[0].PersistentDisks[0].SizeGiB = 40
		newPool.Spec.Configs[0].PersistentDisks[0].MountPath = "/other"
		newPool.Spec.Configs[0].PersistentDisks[0].UnitNumber = int32Ptr(3)
		errs := ValidateAllocatedSlotsImmutable(oldPool, newPool)
		g.Expect(errs).To(HaveLen(3))
	})

	t.Run("rejects removing an allocated disk", func(t *testing.T) {
		g := NewWithT(t)
		oldPool := allocatedPool()
		newPool := oldPool.DeepCopy()
		newPool.Spec.Configs[0].PersistentDisks = nil
		errs := ValidateAllocatedSlotsImmutable(oldPool, newPool)
		g.Expect(errs).To(HaveLen(1))
		g.Expect(errs[0].Detail).To(ContainSubstring("cannot remove persistent disk"))
	})

	t.Run("allows nil to value unitNumber backfill and adding a disk", func(t *testing.T) {
		g := NewWithT(t)
		oldPool := allocatedPool()
		oldPool.Spec.Configs[0].PersistentDisks[0].UnitNumber = nil
		newPool := oldPool.DeepCopy()
		newPool.Spec.Configs[0].PersistentDisks[0].UnitNumber = int32Ptr(2)
		newPool.Spec.Configs[0].PersistentDisks = append(newPool.Spec.Configs[0].PersistentDisks,
			infrav1.PersistentDisk{Name: "data", SizeGiB: 30, UnitNumber: int32Ptr(4)})
		g.Expect(ValidateAllocatedSlotsImmutable(oldPool, newPool)).To(BeEmpty())
	})
}

func TestValidateHostnameUniqueness(t *testing.T) {
	g := NewWithT(t)
	pool := &infrav1.VSphereMachineConfigPool{
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			Configs: []infrav1.MachineConfigSlot{
				{Hostname: "host-1"},
				{Hostname: "host-2"},
				{Hostname: "host-1"},
			},
		},
	}
	errs := ValidateHostnameUniqueness(pool)
	g.Expect(errs).To(HaveLen(1))
	g.Expect(errs[0].Field).To(ContainSubstring("hostname"))
}

func TestValidateIPUniqueness(t *testing.T) {
	g := NewWithT(t)
	pool := &infrav1.VSphereMachineConfigPool{
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			Configs: []infrav1.MachineConfigSlot{
				{Hostname: "host-1", Network: &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{IP: "10.0.0.1", IPv6: "fd00::1"}}},
				{Hostname: "host-2", Network: &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{IP: "10.0.0.1"}}},
				{Hostname: "host-3", Network: &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{IP: "10.0.0.3"}}},
			},
		},
	}
	errs := ValidateIPUniqueness(pool)
	g.Expect(errs).To(HaveLen(1)) // 10.0.0.1 duplicated once
}

func TestHydrateSlotFromStatus(t *testing.T) {
	poolWith := func(records ...infrav1.PersistentDiskStatus) *infrav1.VSphereMachineConfigPool {
		return &infrav1.VSphereMachineConfigPool{
			Status: infrav1.VSphereMachineConfigPoolStatus{PersistentDiskStatuses: records},
		}
	}

	t.Run("status observed values win over empty spec", func(t *testing.T) {
		g := NewWithT(t)
		pool := poolWith(infrav1.PersistentDiskStatus{
			Hostname: "host-1", Name: "disk-a",
			VolumePath: "[ds] vm/disk-a.vmdk", DiskUUID: "uuid-a", UnitNumber: toInt32Ptr(2),
		})
		slot := &infrav1.MachineConfigSlot{
			Hostname:        "host-1",
			PersistentDisks: []infrav1.PersistentDisk{{Name: "disk-a", SizeGiB: 20}},
		}
		HydrateSlotFromStatus(pool, slot)
		g.Expect(slot.PersistentDisks[0].VolumePath).To(Equal("[ds] vm/disk-a.vmdk"))
		g.Expect(slot.PersistentDisks[0].DiskUUID).To(Equal("uuid-a"))
		g.Expect(*slot.PersistentDisks[0].UnitNumber).To(Equal(int32(2)))
	})

	t.Run("observed UnitNumber overlays spec value", func(t *testing.T) {
		g := NewWithT(t)
		pool := poolWith(infrav1.PersistentDiskStatus{
			Hostname: "host-1", Name: "disk-a",
			VolumePath: "[ds] vm/disk-a.vmdk", DiskUUID: "uuid-a", UnitNumber: toInt32Ptr(9),
		})
		slot := &infrav1.MachineConfigSlot{
			Hostname:        "host-1",
			PersistentDisks: []infrav1.PersistentDisk{{Name: "disk-a", SizeGiB: 20, UnitNumber: toInt32Ptr(3)}},
		}
		HydrateSlotFromStatus(pool, slot)
		g.Expect(*slot.PersistentDisks[0].UnitNumber).To(Equal(int32(9)), "latest observed unit number wins")
		g.Expect(slot.PersistentDisks[0].VolumePath).To(Equal("[ds] vm/disk-a.vmdk"))
	})

	t.Run("no status record leaves the slot untouched", func(t *testing.T) {
		g := NewWithT(t)
		pool := poolWith()
		slot := &infrav1.MachineConfigSlot{
			Hostname:        "host-1",
			PersistentDisks: []infrav1.PersistentDisk{{Name: "disk-a", SizeGiB: 20, VolumePath: "[ds] legacy/disk.vmdk"}},
		}
		HydrateSlotFromStatus(pool, slot)
		g.Expect(slot.PersistentDisks[0].VolumePath).To(Equal("[ds] legacy/disk.vmdk"))
		g.Expect(slot.PersistentDisks[0].UnitNumber).To(BeNil())
	})

	t.Run("ephemeral observed unit is overlaid onto the slot", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Status: infrav1.VSphereMachineConfigPoolStatus{
				EphemeralDiskStatuses: []infrav1.EphemeralDiskStatus{
					{Hostname: "host-1", Name: "cache-1", UnitNumber: toInt32Ptr(4)},
				},
			},
		}
		slot := &infrav1.MachineConfigSlot{
			Hostname:       "host-1",
			EphemeralDisks: []infrav1.EphemeralDisk{{Name: "cache-1", SizeGiB: 20}},
		}
		HydrateSlotFromStatus(pool, slot)
		g.Expect(slot.EphemeralDisks[0].UnitNumber).NotTo(BeNil())
		g.Expect(*slot.EphemeralDisks[0].UnitNumber).To(Equal(int32(4)))
	})

	t.Run("no ephemeral status record leaves the unit nil", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{}
		slot := &infrav1.MachineConfigSlot{
			Hostname:       "host-1",
			EphemeralDisks: []infrav1.EphemeralDisk{{Name: "cache-1", SizeGiB: 20}},
		}
		HydrateSlotFromStatus(pool, slot)
		g.Expect(slot.EphemeralDisks[0].UnitNumber).To(BeNil())
	})
}

func TestSeedPersistentDiskStatuses(t *testing.T) {
	t.Run("seeds records from frozen spec values and freezes spec", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "host-1",
					Network:  validNetwork,
					PersistentDisks: []infrav1.PersistentDisk{
						{Name: "disk-a", SizeGiB: 20, VolumePath: "[ds] vm/disk-a.vmdk", DiskUUID: "uuid-a", UnitNumber: toInt32Ptr(0)},
					},
				}},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{{
					Hostname: "host-1", State: infrav1.MachineConfigSlotStateInUse,
					MachineRef: &corev1.ObjectReference{Name: "worker-1", UID: "worker-1-uid"},
				}},
			},
		}
		changed := SeedPersistentDiskStatuses(pool)
		g.Expect(changed).To(BeTrue())
		rec, _ := infrav1.FindDiskStatus(pool, "host-1", "disk-a")
		g.Expect(rec).NotTo(BeNil())
		g.Expect(rec.VolumePath).To(Equal("[ds] vm/disk-a.vmdk"))
		g.Expect(rec.DiskUUID).To(Equal("uuid-a"))
		g.Expect(*rec.UnitNumber).To(Equal(int32(0)))
		g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseAttached))
		g.Expect(rec.OwnerMachineName).To(Equal("worker-1"))
		g.Expect(rec.OwnerMachineUID).To(Equal("worker-1-uid"))
		// spec is left intact (frozen), removed only in the next release.
		g.Expect(pool.Spec.Configs[0].PersistentDisks[0].VolumePath).To(Equal("[ds] vm/disk-a.vmdk"))

		// Idempotent: a second pass changes nothing.
		g.Expect(SeedPersistentDiskStatuses(pool)).To(BeFalse())
	})

	t.Run("folds a legacy in-flight reclaimStatus into the disk record and clears it", func(t *testing.T) {
		g := NewWithT(t)
		retryAfter := metav1.Now()
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "host-1",
					Network:  validNetwork,
					PersistentDisks: []infrav1.PersistentDisk{
						{Name: "disk-a", SizeGiB: 20, VolumePath: "[ds] vm/disk-a.vmdk"},
					},
				}},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{{
					Hostname: "host-1", State: infrav1.MachineConfigSlotStateReleased,
					ReclaimStatus: &infrav1.MachineConfigSlotReclaimStatus{
						State:      infrav1.MachineConfigSlotReclaimStateFailed,
						VolumePath: "[ds] vm/disk-a.vmdk",
						RetryAfter: &retryAfter,
						LastError:  "boom",
					},
				}},
			},
		}
		g.Expect(SeedPersistentDiskStatuses(pool)).To(BeTrue())
		rec, _ := infrav1.FindDiskStatus(pool, "host-1", "disk-a")
		g.Expect(rec).NotTo(BeNil())
		g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseError))
		g.Expect(rec.LastError).To(Equal("boom"))
		g.Expect(rec.RetryAfter).NotTo(BeNil())
		// Legacy per-slot ReclaimStatus is cleared once folded.
		g.Expect(pool.Status.ConfigStatuses[0].ReclaimStatus).To(BeNil())
	})

	t.Run("does not overwrite an existing record", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Configs: []infrav1.MachineConfigSlot{{
					Hostname:        "host-1",
					PersistentDisks: []infrav1.PersistentDisk{{Name: "disk-a", SizeGiB: 20, VolumePath: "[ds] spec/disk.vmdk"}},
				}},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				PersistentDiskStatuses: []infrav1.PersistentDiskStatus{{
					Hostname: "host-1", Name: "disk-a", VolumePath: "[ds] status/disk.vmdk", Phase: infrav1.PersistentDiskPhaseAttached,
				}},
			},
		}
		g.Expect(SeedPersistentDiskStatuses(pool)).To(BeFalse())
		rec, _ := infrav1.FindDiskStatus(pool, "host-1", "disk-a")
		g.Expect(rec.VolumePath).To(Equal("[ds] status/disk.vmdk"))
	})
}
