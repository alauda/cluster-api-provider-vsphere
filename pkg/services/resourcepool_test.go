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
	"strings"
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
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool",
				Namespace: "default",
			},
			Spec: infrav1.VSphereResourcePoolSpec{
				Resources: []infrav1.ResourceSlot{
					{Hostname: "host-1"},
				},
			},
			Status: infrav1.VSphereResourcePoolStatus{
				ResourceStatuses: []infrav1.ResourceSlotStatus{
					{
						Hostname: "host-1",
						State:    "InUse",
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

		slot, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, "", nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(slot.Hostname).To(Equal("host-1"))
	})

	t.Run("should prefer Released slot over Available", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-reuse",
				Namespace: "default",
			},
			Spec: infrav1.VSphereResourcePoolSpec{
				Resources: []infrav1.ResourceSlot{
					{Hostname: "host-available"},
					{Hostname: "host-released"},
				},
			},
			Status: infrav1.VSphereResourcePoolStatus{
				ResourceStatuses: []infrav1.ResourceSlotStatus{
					{Hostname: "host-available", State: "Available"},
					{Hostname: "host-released", State: "Released"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		slot, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, "", nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(slot.Hostname).To(Equal("host-released"))

		// Verify status update
		updatedPool := &infrav1.VSphereResourcePool{}
		_ = c.Get(ctx, client.ObjectKeyFromObject(pool), updatedPool)
		g.Expect(updatedPool.Status.ResourceStatuses[1].State).To(Equal("InUse"))
		g.Expect(updatedPool.Status.ResourceStatuses[1].MachineRef.Name).To(Equal(machine.Name))
	})

	t.Run("should return error when no slots available", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-full",
				Namespace: "default",
			},
			Spec: infrav1.VSphereResourcePoolSpec{
				Resources: []infrav1.ResourceSlot{{Hostname: "host-1"}},
			},
			Status: infrav1.VSphereResourcePoolStatus{
				ResourceStatuses: []infrav1.ResourceSlotStatus{
					{Hostname: "host-1", State: "InUse", MachineRef: &corev1.ObjectReference{Name: "other"}},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		_, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, "", nil)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("no available slots"))
	})

	t.Run("should select slot matching desired datacenter", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-datacenter",
				Namespace: "default",
			},
			Spec: infrav1.VSphereResourcePoolSpec{
				Datacenter: "dc-default",
				Resources: []infrav1.ResourceSlot{
					{Hostname: "host-default"},
					{Hostname: "host-target", Datacenter: "dc-target"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		slot, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, "dc-target", nil)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(slot.Hostname).To(Equal("host-target"))
	})

	t.Run("should return error when no slot matches desired datacenter", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-no-match",
				Namespace: "default",
			},
			Spec: infrav1.VSphereResourcePoolSpec{
				Datacenter: "dc-default",
				Resources: []infrav1.ResourceSlot{
					{Hostname: "host-default"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		_, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, "dc-target", nil)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(`matching datacenter "dc-target"`))
	})

	t.Run("should require slot to satisfy both template and failure domain datacenters", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-template-and-failure-domain",
				Namespace: "default",
			},
			Spec: infrav1.VSphereResourcePoolSpec{
				Resources: []infrav1.ResourceSlot{
					{Hostname: "host-a", Datacenter: "dc-template"},
					{Hostname: "host-b", Datacenter: "dc-fd"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		_, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, "dc-template", []string{"dc-fd"})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring(`matching datacenter "dc-template" and failure domain datacenters`))
	})

	t.Run("should select slot matching failure domain datacenters when template datacenter is empty", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-failure-domain-match",
				Namespace: "default",
			},
			Spec: infrav1.VSphereResourcePoolSpec{
				Datacenter: "dc-pool",
				Resources: []infrav1.ResourceSlot{
					{Hostname: "host-a"},
					{Hostname: "host-b", Datacenter: "dc-fd"},
				},
			},
			Status: infrav1.VSphereResourcePoolStatus{
				ResourceStatuses: []infrav1.ResourceSlotStatus{
					{Hostname: "host-a", State: "Available"},
					{Hostname: "host-b", State: "Released"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		slot, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, "", []string{"dc-fd"})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(slot.Hostname).To(Equal("host-b"))
		g.Expect(slot.Datacenter).To(Equal("dc-fd"))
	})

	t.Run("should return error when no slot matches failure domain datacenters", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-failure-domain-no-match",
				Namespace: "default",
			},
			Spec: infrav1.VSphereResourcePoolSpec{
				Datacenter: "dc-pool",
				Resources: []infrav1.ResourceSlot{
					{Hostname: "host-a"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		_, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, "", []string{"dc-fd"})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("matching failure domain datacenters"))
	})

	t.Run("should backfill the resolved datacenter onto the selected slot", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-backfill-datacenter",
				Namespace: "default",
			},
			Spec: infrav1.VSphereResourcePoolSpec{
				Datacenter: "dc-pool",
				Resources: []infrav1.ResourceSlot{
					{Hostname: "host-a"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

		slot, err := AllocateSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, machine, "", []string{"dc-pool"})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(slot.Datacenter).To(Equal("dc-pool"))
	})
}

func TestReleaseSlot(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	ctx := context.Background()
	pool := &infrav1.VSphereResourcePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-release",
			Namespace: "default",
		},
		Status: infrav1.VSphereResourcePoolStatus{
			ResourceStatuses: []infrav1.ResourceSlotStatus{
				{
					Hostname: "host-1",
					State:    "InUse",
					MachineRef: &corev1.ObjectReference{
						Name:      "my-machine",
						Namespace: "default",
					},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).WithStatusSubresource(pool).Build()

	err := ReleaseSlot(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, &corev1.ObjectReference{Name: "my-machine", Namespace: "default"})
	g.Expect(err).NotTo(HaveOccurred())

	updatedPool := &infrav1.VSphereResourcePool{}
	_ = c.Get(ctx, client.ObjectKeyFromObject(pool), updatedPool)
	g.Expect(updatedPool.Status.ResourceStatuses[0].State).To(Equal("Released"))
	g.Expect(updatedPool.Status.ResourceStatuses[0].LastReleasedTime).NotTo(BeNil())
}

func TestGetSlotForMachine(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	ctx := context.Background()
	pool := &infrav1.VSphereResourcePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-lookup",
			Namespace: "default",
		},
		Spec: infrav1.VSphereResourcePoolSpec{
			Resources: []infrav1.ResourceSlot{
				{Hostname: "host-a"},
				{Hostname: "host-b"},
			},
		},
		Status: infrav1.VSphereResourcePoolStatus{
			ResourceStatuses: []infrav1.ResourceSlotStatus{
				{
					Hostname: "host-b",
					State:    "InUse",
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

func TestResolveResourcePoolDatacenter(t *testing.T) {
	g := NewWithT(t)

	pool := &infrav1.VSphereResourcePool{
		Spec: infrav1.VSphereResourcePoolSpec{
			Datacenter: "dc-pool",
		},
	}

	slotWithOwnDatacenter := &infrav1.ResourceSlot{
		Hostname:   "host-1",
		Datacenter: "dc-slot",
	}
	slotWithoutDatacenter := &infrav1.ResourceSlot{
		Hostname: "host-2",
	}

	g.Expect(ResolveResourcePoolDatacenter(pool, slotWithOwnDatacenter)).To(Equal("dc-slot"))
	g.Expect(ResolveResourcePoolDatacenter(pool, slotWithoutDatacenter)).To(Equal("dc-pool"))
	g.Expect(ResolveResourcePoolDatacenter(nil, slotWithoutDatacenter)).To(BeEmpty())
}

func TestTryBindPoolToConsumer(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	ctx := context.Background()
	pool := &infrav1.VSphereResourcePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec:       infrav1.VSphereResourcePoolSpec{Resources: []infrav1.ResourceSlot{{Hostname: "slot-1"}}},
	}

	t.Run("binds unbound pool", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool.DeepCopy()).Build()
		err := TryBindPoolToConsumer(ctx, c, &corev1.ObjectReference{Name: "pool-a", Namespace: "default"}, &corev1.ObjectReference{
			APIVersion: controlplanev1.GroupVersion.String(),
			Kind:       "KubeadmControlPlane",
			Namespace:  "default",
			Name:       "cp-a",
		})
		g.Expect(err).NotTo(HaveOccurred())
		updated := &infrav1.VSphereResourcePool{}
		g.Expect(c.Get(ctx, client.ObjectKey{Name: "pool-a", Namespace: "default"}, updated)).To(Succeed())
		g.Expect(updated.Spec.ConsumerRef).NotTo(BeNil())
		g.Expect(updated.Spec.ConsumerRef.Name).To(Equal("cp-a"))
	})

	t.Run("rejects binding to different consumer", func(t *testing.T) {
		p := pool.DeepCopy()
		p.Spec.ConsumerRef = &corev1.ObjectReference{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "MachineDeployment",
			Namespace:  "default",
			Name:       "md-a",
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p).Build()
		err := TryBindPoolToConsumer(ctx, c, &corev1.ObjectReference{Name: "pool-a", Namespace: "default"}, &corev1.ObjectReference{
			APIVersion: controlplanev1.GroupVersion.String(),
			Kind:       "KubeadmControlPlane",
			Namespace:  "default",
			Name:       "cp-a",
		})
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("allows idempotent bind to same consumer", func(t *testing.T) {
		p := pool.DeepCopy()
		p.Spec.ConsumerRef = &corev1.ObjectReference{
			APIVersion: controlplanev1.GroupVersion.String(),
			Kind:       "KubeadmControlPlane",
			Namespace:  "default",
			Name:       "cp-a",
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p).Build()
		err := TryBindPoolToConsumer(ctx, c, &corev1.ObjectReference{Name: "pool-a", Namespace: "default"}, &corev1.ObjectReference{
			APIVersion: controlplanev1.GroupVersion.String(),
			Kind:       "KubeadmControlPlane",
			Namespace:  "default",
			Name:       "cp-a",
		})
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func TestIsPoolFullyReusable(t *testing.T) {
	g := NewWithT(t)
	pool := &infrav1.VSphereResourcePool{
		Spec: infrav1.VSphereResourcePoolSpec{
			Resources: []infrav1.ResourceSlot{{Hostname: "slot-1"}},
		},
		Status: infrav1.VSphereResourcePoolStatus{
			ResourceStatuses: []infrav1.ResourceSlotStatus{{Hostname: "slot-1", State: "Available"}},
		},
	}
	g.Expect(IsPoolFullyReusable(pool)).To(BeTrue())

	released := pool.DeepCopy()
	now := metav1.Now()
	released.Status.ResourceStatuses[0].State = "Released"
	released.Status.ResourceStatuses[0].LastReleasedTime = &now
	g.Expect(IsPoolFullyReusable(released)).To(BeFalse())

	withTask := pool.DeepCopy()
	withTask.Status.ResourceStatuses[0].ReclaimStatus = &infrav1.ResourceSlotReclaimStatus{TaskRef: "task-1", State: "Running"}
	g.Expect(IsPoolFullyReusable(withTask)).To(BeFalse())

	withDisk := pool.DeepCopy()
	withDisk.Spec.Resources[0].PersistentDisks = []infrav1.PersistentDisk{{Name: "disk-1", VolumePath: "[ds] vm/disk.vmdk"}}
	g.Expect(IsPoolFullyReusable(withDisk)).To(BeFalse())

	withRetry := pool.DeepCopy()
	retryAfter := metav1.Now()
	withRetry.Status.ResourceStatuses[0].ReclaimStatus = &infrav1.ResourceSlotReclaimStatus{RetryAfter: &retryAfter, State: "Failed"}
	g.Expect(IsPoolFullyReusable(withRetry)).To(BeFalse())
}

func TestEnsurePersistentDiskBootstrapData(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	ctx := context.Background()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"format": []byte("cloud-config"),
			"value": []byte(`## template: jinja
#cloud-config

runcmd:
- echo ok
`),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	err := ensurePersistentDiskBootstrapData(ctx, c, &corev1.ObjectReference{
		Kind:      "Secret",
		Name:      secret.Name,
		Namespace: secret.Namespace,
	}, []infrav1.PersistentDisk{{
		Name:       "data-1",
		UnitNumber: toInt32Ptr(0),
		MountPath:  "/var/cpaas",
	}})
	g.Expect(err).NotTo(HaveOccurred())

	updatedSecret := &corev1.Secret{}
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(secret), updatedSecret)).To(Succeed())
	value := string(updatedSecret.Data["value"])
	g.Expect(value).To(ContainSubstring("write_files:"))
	g.Expect(value).To(ContainSubstring("/etc/capv/persistent-disks.tsv"))
	g.Expect(value).To(ContainSubstring("capv-persistent-disk-reconcile.service"))

	// Ensure merge is idempotent.
	err = ensurePersistentDiskBootstrapData(ctx, c, &corev1.ObjectReference{
		Kind:      "Secret",
		Name:      secret.Name,
		Namespace: secret.Namespace,
	}, []infrav1.PersistentDisk{{
		Name:       "data-1",
		UnitNumber: toInt32Ptr(0),
		MountPath:  "/var/cpaas",
	}})
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(secret), updatedSecret)).To(Succeed())
	value = string(updatedSecret.Data["value"])
	g.Expect(strings.Count(value, "/etc/capv/persistent-disks.tsv")).To(Equal(1))
}

func TestFindResourcePoolForMachine(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	ctx := context.Background()
	poolA := &infrav1.VSphereResourcePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-a",
			Namespace: "default",
		},
		Status: infrav1.VSphereResourcePoolStatus{
			ResourceStatuses: []infrav1.ResourceSlotStatus{
				{
					Hostname: "host-a",
					State:    "InUse",
					MachineRef: &corev1.ObjectReference{
						Name:      "other-machine",
						Namespace: "default",
					},
				},
			},
		},
	}
	poolB := &infrav1.VSphereResourcePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-b",
			Namespace: "default",
		},
		Status: infrav1.VSphereResourcePoolStatus{
			ResourceStatuses: []infrav1.ResourceSlotStatus{
				{
					Hostname: "host-b",
					State:    "InUse",
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

	poolRef, err := FindResourcePoolForMachine(ctx, c, "default", &corev1.ObjectReference{Name: "my-machine", Namespace: "default", UID: "machine-uid"})
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
	pool := &infrav1.VSphereResourcePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool-persist",
			Namespace: "default",
		},
		Spec: infrav1.VSphereResourcePoolSpec{
			Resources: []infrav1.ResourceSlot{
				{
					Hostname: "host-1",
					PersistentDisks: []infrav1.PersistentDisk{
						{Name: "disk-1"},
					},
				},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()

	unitNumber := int32(3)
	err := PersistSlotChanges(ctx, c, &corev1.ObjectReference{Name: pool.Name, Namespace: pool.Namespace}, &infrav1.ResourceSlot{
		Hostname: "host-1",
		PersistentDisks: []infrav1.PersistentDisk{
			{
				Name:       "disk-1",
				UnitNumber: &unitNumber,
				VolumePath: "[datastore1] disk-1/disk-1.vmdk",
				DiskUUID:   "disk-uuid",
			},
		},
	})
	g.Expect(err).NotTo(HaveOccurred())

	updatedPool := &infrav1.VSphereResourcePool{}
	_ = c.Get(ctx, client.ObjectKeyFromObject(pool), updatedPool)
	g.Expect(updatedPool.Spec.Resources[0].PersistentDisks[0].UnitNumber).NotTo(BeNil())
	g.Expect(*updatedPool.Spec.Resources[0].PersistentDisks[0].UnitNumber).To(Equal(unitNumber))
	g.Expect(updatedPool.Spec.Resources[0].PersistentDisks[0].VolumePath).To(Equal("[datastore1] disk-1/disk-1.vmdk"))
	g.Expect(updatedPool.Spec.Resources[0].PersistentDisks[0].DiskUUID).To(Equal("disk-uuid"))
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
