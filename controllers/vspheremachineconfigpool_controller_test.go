package controllers

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/vmware/govmomi/vim25/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta2conditions "sigs.k8s.io/cluster-api/util/conditions/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/internal/test/helpers/vcsim"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
)

func TestResolveVCenterParams(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)

	t.Run("cluster not found", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "missing-cluster"},
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
			},
		}
		r := machineConfigPoolReconciler{
			Client:                   fake.NewClientBuilder().WithScheme(scheme).Build(),
			ControllerManagerContext: &capvcontext.ControllerManagerContext{},
		}
		_, err := r.resolveVCenterParams(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("missing-cluster"))
		c := conditions.Get(pool, infrav1.ClusterRefReadyCondition)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(corev1.ConditionFalse))
		g.Expect(c.Reason).To(Equal(infrav1.ClusterNotFoundReason))
	})

	t.Run("VSphereCluster not found", func(t *testing.T) {
		g := NewWithT(t)
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "default"},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{Name: "my-vsphere-cluster"},
			},
		}
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "my-cluster"},
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
			},
		}
		r := machineConfigPoolReconciler{
			Client:                   fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build(),
			ControllerManagerContext: &capvcontext.ControllerManagerContext{},
		}
		_, err := r.resolveVCenterParams(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		c := conditions.Get(pool, infrav1.ClusterRefReadyCondition)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(corev1.ConditionFalse))
		g.Expect(c.Reason).To(Equal(infrav1.VSphereClusterNotFoundReason))
	})

	t.Run("nil InfrastructureRef", func(t *testing.T) {
		g := NewWithT(t)
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "default"},
		}
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "my-cluster"},
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
			},
		}
		r := machineConfigPoolReconciler{
			Client:                   fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build(),
			ControllerManagerContext: &capvcontext.ControllerManagerContext{},
		}
		_, err := r.resolveVCenterParams(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("nil InfrastructureRef"))
	})

	t.Run("nil IdentityRef falls back to global creds", func(t *testing.T) {
		g := NewWithT(t)
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "default"},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{Name: "my-vsphere-cluster"},
			},
		}
		vsphereCluster := &infrav1.VSphereCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "my-vsphere-cluster", Namespace: "default"},
			Spec:       infrav1.VSphereClusterSpec{Server: "vcenter.example.com", Thumbprint: "AA:BB"},
		}
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "my-cluster"},
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
			},
		}
		r := machineConfigPoolReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, vsphereCluster).Build(),
			ControllerManagerContext: &capvcontext.ControllerManagerContext{
				Username: "admin",
				Password: "pass",
			},
		}
		params, err := r.resolveVCenterParams(context.Background(), pool)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(params.server).To(Equal("vcenter.example.com"))
		g.Expect(params.thumbprint).To(Equal("AA:BB"))
		g.Expect(params.username).To(Equal("admin"))
		g.Expect(params.password).To(Equal("pass"))
		g.Expect(conditions.IsTrue(pool, infrav1.ClusterRefReadyCondition)).To(BeTrue())
		g.Expect(conditions.IsTrue(pool, infrav1.VCenterAvailableCondition)).To(BeTrue())
	})

	t.Run("nil IdentityRef and empty global creds", func(t *testing.T) {
		g := NewWithT(t)
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "default"},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{Name: "my-vsphere-cluster"},
			},
		}
		vsphereCluster := &infrav1.VSphereCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "my-vsphere-cluster", Namespace: "default"},
			Spec:       infrav1.VSphereClusterSpec{Server: "vcenter.example.com"},
		}
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "my-cluster"},
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
			},
		}
		r := machineConfigPoolReconciler{
			Client:                   fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, vsphereCluster).Build(),
			ControllerManagerContext: &capvcontext.ControllerManagerContext{},
		}
		_, err := r.resolveVCenterParams(context.Background(), pool)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("controller manager credentials are not configured"))
		c := conditions.Get(pool, infrav1.VCenterAvailableCondition)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(corev1.ConditionFalse))
	})

	t.Run("IdentityRef resolves credentials from secret", func(t *testing.T) {
		g := NewWithT(t)
		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "my-cluster", Namespace: "default"},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{Name: "my-vsphere-cluster"},
			},
		}
		vsphereCluster := &infrav1.VSphereCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "my-vsphere-cluster", Namespace: "default"},
			Spec: infrav1.VSphereClusterSpec{
				Server:     "vcenter.example.com",
				Thumbprint: "CC:DD",
				IdentityRef: &infrav1.VSphereIdentityReference{
					Kind: infrav1.SecretKind,
					Name: "vcenter-creds",
				},
			},
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "vcenter-creds", Namespace: "default"},
			Data: map[string][]byte{
				"username": []byte("secret-user"),
				"password": []byte("secret-pass"),
			},
		}
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "my-cluster"},
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
			},
		}
		r := machineConfigPoolReconciler{
			Client:                   fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, vsphereCluster, secret).Build(),
			ControllerManagerContext: &capvcontext.ControllerManagerContext{Namespace: "default"},
		}
		params, err := r.resolveVCenterParams(context.Background(), pool)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(params.server).To(Equal("vcenter.example.com"))
		g.Expect(params.thumbprint).To(Equal("CC:DD"))
		g.Expect(params.username).To(Equal("secret-user"))
		g.Expect(params.password).To(Equal("secret-pass"))
		g.Expect(conditions.IsTrue(pool, infrav1.ClusterRefReadyCondition)).To(BeTrue())
		g.Expect(conditions.IsTrue(pool, infrav1.VCenterAvailableCondition)).To(BeTrue())
	})
}

func TestMachineConfigPoolReconcileDeleteBlocksWhenMachineStillExists(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	machine := &infrav1.VSphereMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cp-0",
			Namespace: "default",
		},
	}
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool",
			Namespace:  "default",
			Finalizers: []string{MachineConfigPoolFinalizer},
		},
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
		},
		Status: infrav1.VSphereMachineConfigPoolStatus{
			ConfigStatuses: []infrav1.MachineConfigSlotStatus{{
				Hostname: "host-1",
				State:    infrav1.MachineConfigSlotStateInUse,
				MachineRef: &corev1.ObjectReference{
					Name:      "cp-0",
					Namespace: "default",
				},
			}},
		},
	}

	r := machineConfigPoolReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(pool, machine).
			Build(),
	}

	_, err := r.reconcileDelete(context.Background(), pool)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("blocking VSphereMachineConfigPool deletion"))
	g.Expect(pool.Finalizers).To(ContainElement(MachineConfigPoolFinalizer))
}

func TestMachineConfigPoolReconcileDeleteRemovesFinalizerAfterSafeReclaim(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	now := metav1.Now()
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool",
			Namespace:  "default",
			Finalizers: []string{MachineConfigPoolFinalizer},
		},
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
		},
		Status: infrav1.VSphereMachineConfigPoolStatus{
			ConfigStatuses: []infrav1.MachineConfigSlotStatus{{
				Hostname: "host-1",
				State:    infrav1.MachineConfigSlotStateInUse,
				MachineRef: &corev1.ObjectReference{
					Name:      "cp-0",
					Namespace: "default",
				},
				LastReleasedTime: &now,
			}},
		},
	}

	r := machineConfigPoolReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(pool).
			Build(),
	}

	result, err := r.reconcileDelete(context.Background(), pool)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.IsZero()).To(BeTrue())
	g.Expect(pool.Finalizers).NotTo(ContainElement(MachineConfigPoolFinalizer))
	g.Expect(pool.Status.ConfigStatuses[0].MachineRef).To(BeNil())
	g.Expect(pool.Status.ConfigStatuses[0].State).To(Equal(infrav1.MachineConfigSlotStateAvailable))
}

// TestMachineConfigPoolReconcileDeleteMigratedPoolConverges exercises the full
// reconcileDelete path for a pool upgraded from before the status migration: the
// disk's observed VolumePath is still frozen on spec and status starts empty.
// Before the Reclaimed tombstone, each reclaim removed the status record and the
// next reconcile re-seeded it from spec, so the slot never left Released and the
// finalizer was never removed — pool deletion hung forever. This asserts it
// converges instead.
func TestMachineConfigPoolReconcileDeleteMigratedPoolConverges(t *testing.T) {
	g := NewWithT(t)
	simr, err := vcsim.NewBuilder().Build()
	g.Expect(err).NotTo(HaveOccurred())
	defer simr.Destroy()

	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)

	password, _ := simr.ServerURL().User.Password()
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
		Spec:       clusterv1.ClusterSpec{InfrastructureRef: &corev1.ObjectReference{Name: "test-cluster"}},
	}
	vsphereCluster := &infrav1.VSphereCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "default"},
		Spec:       infrav1.VSphereClusterSpec{Server: simr.ServerURL().Host},
	}

	now := metav1.Now()
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool",
			Namespace:  "default",
			Finalizers: []string{MachineConfigPoolFinalizer},
		},
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Datacenter: "DC0",
			Configs: []infrav1.MachineConfigSlot{{
				Hostname: "host-1",
				PersistentDisks: []infrav1.PersistentDisk{{
					Name: "data-0", SizeGiB: 20,
					VolumePath: "[LocalDS_0] host-1/data-0.vmdk", // frozen on spec (migrated)
				}},
			}},
		},
		Status: infrav1.VSphereMachineConfigPoolStatus{
			ConfigStatuses: []infrav1.MachineConfigSlotStatus{{
				Hostname:         "host-1",
				State:            infrav1.MachineConfigSlotStateReleased,
				LastReleasedTime: &now,
			}},
		},
	}

	r := machineConfigPoolReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cluster, vsphereCluster, pool).Build(),
		ControllerManagerContext: &capvcontext.ControllerManagerContext{
			Username: simr.ServerURL().User.Username(),
			Password: password,
		},
	}

	// Reclaim is asynchronous, so delete requeues while the task runs; loop until
	// it settles. An unfixed build never settles (re-seed loop), so this would
	// spin to the cap and the assertions below would fail.
	done := false
	for i := 0; i < 8 && !done; i++ {
		res, rerr := r.reconcileDelete(context.Background(), pool)
		g.Expect(rerr).NotTo(HaveOccurred())
		done = res.IsZero()
	}
	g.Expect(done).To(BeTrue())
	g.Expect(pool.Finalizers).NotTo(ContainElement(MachineConfigPoolFinalizer))
	g.Expect(pool.Status.ConfigStatuses[0].State).To(Equal(infrav1.MachineConfigSlotStateAvailable))
	rec, _ := infrav1.FindDiskStatus(pool, "host-1", "data-0")
	g.Expect(rec).NotTo(BeNil())
	g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseReclaimed))
}

// TestReclaimSlotDisks drives reclaimSlotDisks against a vCenter simulator to
// cover the per-disk reclaim state machine that replaced the per-slot
// ReclaimStatus: a slot with no backing reclaims immediately, an attached disk
// is refused (Error + retry), an unattached disk's backing is deleted and its
// record tombstoned as Reclaimed, and a migrated pool whose observed VolumePath
// is still frozen on spec converges instead of re-reclaiming forever.
func TestReclaimSlotDisks(t *testing.T) {
	simr, err := vcsim.NewBuilder().Build()
	if err != nil {
		t.Fatalf("unable to create simulator: %s", err)
	}
	defer simr.Destroy()

	vcp := &vcenterParams{
		server:   simr.ServerURL().Host,
		username: simr.ServerURL().User.Username(),
	}
	vcp.password, _ = simr.ServerURL().User.Password()

	newPool := func(disks []infrav1.PersistentDisk, diskStatuses []infrav1.PersistentDiskStatus) *infrav1.VSphereMachineConfigPool {
		return &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				Datacenter: "DC0",
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1", PersistentDisks: disks}},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{PersistentDiskStatuses: diskStatuses},
		}
	}

	t.Run("slot with no reclaimable backing reclaims immediately", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool([]infrav1.PersistentDisk{{Name: "data-0", SizeGiB: 20}}, nil)
		r := machineConfigPoolReconciler{}

		reclaimed, _, err := r.reclaimSlotDisks(context.Background(), pool, &pool.Spec.Configs[0], vcp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(reclaimed).To(BeTrue())
		g.Expect(pool.Status.PersistentDiskStatuses).To(BeEmpty())
	})

	t.Run("attached disk is refused and moves to Error with retry", func(t *testing.T) {
		g := NewWithT(t)
		attachedPath := firstAttachedDiskPath(context.Background(), g, vcp)
		pool := newPool(
			[]infrav1.PersistentDisk{{Name: "data-0", SizeGiB: 20}},
			[]infrav1.PersistentDiskStatus{{
				Hostname: "host-1", Name: "data-0", VolumePath: attachedPath,
				Phase: infrav1.PersistentDiskPhaseAttached,
			}},
		)
		r := machineConfigPoolReconciler{}

		reclaimed, wait, err := r.reclaimSlotDisks(context.Background(), pool, &pool.Spec.Configs[0], vcp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(reclaimed).To(BeFalse())
		g.Expect(wait).To(BeNumerically(">", 0))
		rec, _ := infrav1.FindDiskStatus(pool, "host-1", "data-0")
		g.Expect(rec).NotTo(BeNil())
		g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseError))
		g.Expect(rec.RetryAfter).NotTo(BeNil())
		g.Expect(rec.LastError).To(ContainSubstring("still attached"))
	})

	t.Run("unattached disk backing is deleted and its record tombstoned", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool(
			[]infrav1.PersistentDisk{{Name: "data-0", SizeGiB: 20}},
			[]infrav1.PersistentDiskStatus{{
				Hostname: "host-1", Name: "data-0",
				VolumePath: "[LocalDS_0] host-1/data-0.vmdk",
				Phase:      infrav1.PersistentDiskPhaseAvailable,
			}},
		)
		r := machineConfigPoolReconciler{}

		// Reclamation is asynchronous (Reclaiming -> polled to completion), so
		// drive it across a few reconciles until the slot has no backing left.
		var reclaimed bool
		for i := 0; i < 5 && !reclaimed; i++ {
			reclaimed, _, err = r.reclaimSlotDisks(context.Background(), pool, &pool.Spec.Configs[0], vcp)
			g.Expect(err).NotTo(HaveOccurred())
		}
		g.Expect(reclaimed).To(BeTrue())
		// The record is kept as a Reclaimed tombstone (not removed) with its
		// observed backing cleared, so a later reconcile does not re-reclaim it.
		rec, _ := infrav1.FindDiskStatus(pool, "host-1", "data-0")
		g.Expect(rec).NotTo(BeNil())
		g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseReclaimed))
		g.Expect(rec.VolumePath).To(BeEmpty())
		g.Expect(rec.TaskRef).To(BeEmpty())
	})

	// Regression: a pool upgraded from before the status migration still carries
	// the disk's observed VolumePath frozen on spec. Reclaim must converge —
	// before the tombstone, success removed the status record, the next seed
	// re-created it from spec's frozen VolumePath, and the disk was reclaimed
	// forever (finalizer never removed, released slot never freed).
	t.Run("migrated pool with frozen spec VolumePath converges", func(t *testing.T) {
		g := NewWithT(t)
		pool := newPool(
			[]infrav1.PersistentDisk{{
				Name: "data-0", SizeGiB: 20,
				VolumePath: "[LocalDS_0] host-1/data-0.vmdk", // frozen on spec (legacy)
			}},
			nil, // status starts empty, as on a freshly upgraded object
		)
		r := machineConfigPoolReconciler{}

		// Mirror the reconciler entry: seed from spec, then reclaim. Repeat to
		// prove it does not loop — an unfixed build never reaches reclaimed=true.
		var reclaimed bool
		for i := 0; i < 6 && !reclaimed; i++ {
			services.SeedPersistentDiskStatuses(pool)
			reclaimed, _, err = r.reclaimSlotDisks(context.Background(), pool, &pool.Spec.Configs[0], vcp)
			g.Expect(err).NotTo(HaveOccurred())
		}
		g.Expect(reclaimed).To(BeTrue())

		rec, _ := infrav1.FindDiskStatus(pool, "host-1", "data-0")
		g.Expect(rec).NotTo(BeNil())
		g.Expect(rec.Phase).To(Equal(infrav1.PersistentDiskPhaseReclaimed))

		// A further seed+reclaim must not resurrect the disk: the tombstone stops
		// the re-seed and the slot stays reclaimed.
		services.SeedPersistentDiskStatuses(pool)
		g.Expect(services.HasReclaimablePersistentDiskBacking(pool, &pool.Spec.Configs[0])).To(BeFalse())
		reclaimedAgain, _, err := r.reclaimSlotDisks(context.Background(), pool, &pool.Spec.Configs[0], vcp)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(reclaimedAgain).To(BeTrue())
	})
}

// firstAttachedDiskPath returns the datastore path of the first virtual disk
// attached to any VM in datacenter DC0 of the simulator, so a test can assert
// reclaimSlotDisks refuses to delete a disk that is still attached.
func firstAttachedDiskPath(ctx context.Context, g *WithT, vcp *vcenterParams) string {
	s, err := session.GetOrCreate(ctx, session.NewParams().
		WithUserInfo(vcp.username, vcp.password).
		WithServer(vcp.server).
		WithThumbprint(vcp.thumbprint).
		WithDatacenter("DC0"))
	g.Expect(err).NotTo(HaveOccurred())

	vms, err := s.Finder.VirtualMachineList(ctx, "*")
	g.Expect(err).NotTo(HaveOccurred())
	for _, vm := range vms {
		devices, err := vm.Device(ctx)
		g.Expect(err).NotTo(HaveOccurred())
		for _, d := range devices.SelectByType(&types.VirtualDisk{}) {
			if b, ok := d.GetVirtualDevice().Backing.(types.BaseVirtualDeviceFileBackingInfo); ok {
				if path := b.GetVirtualDeviceFileBackingInfo().FileName; path != "" {
					return path
				}
			}
		}
	}
	g.Expect(false).To(BeTrue(), "simulator has no attached virtual disk to test against")
	return ""
}

func TestUpdateSlotCounters(t *testing.T) {
	g := NewWithT(t)

	pool := &infrav1.VSphereMachineConfigPool{
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			Configs: []infrav1.MachineConfigSlot{
				{Hostname: "host-1"},
				{Hostname: "host-2"},
				{Hostname: "host-3"},
				{Hostname: "host-4"},
			},
		},
	}
	statuses := []infrav1.MachineConfigSlotStatus{
		{Hostname: "host-1", State: infrav1.MachineConfigSlotStateAvailable},
		{Hostname: "host-2", State: infrav1.MachineConfigSlotStateInUse},
		{Hostname: "host-3", State: infrav1.MachineConfigSlotStateInUse},
		{Hostname: "host-4", State: infrav1.MachineConfigSlotStateReleased},
	}

	updateSlotCounters(pool, statuses)

	// Total counts every declared slot; Available and Allocated count only
	// their respective states; a Released slot counts toward neither.
	g.Expect(pool.Status.Total).To(Equal(int32(4)))
	g.Expect(pool.Status.Available).To(Equal(int32(1)))
	g.Expect(pool.Status.Allocated).To(Equal(int32(2)))
}

func TestReconcilePoolHealthConditions(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	newReconciler := func(objs ...client.Object) machineConfigPoolReconciler {
		return machineConfigPoolReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		}
	}

	t.Run("healthy pool with a free slot", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "c1"},
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "host-1"},
					{Hostname: "host-2"},
				},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{Hostname: "host-1", State: infrav1.MachineConfigSlotStateInUse},
					{Hostname: "host-2", State: infrav1.MachineConfigSlotStateAvailable},
				},
			},
		}
		updateSlotCounters(pool, pool.Status.ConfigStatuses)
		r := newReconciler(pool)
		r.reconcilePoolHealthConditions(context.Background(), pool)

		g.Expect(conditions.IsTrue(pool, infrav1.MachineConfigPoolMembersValidCondition)).To(BeTrue())
		g.Expect(conditions.IsTrue(pool, infrav1.MachineConfigPoolMembersUniqueCondition)).To(BeTrue())
		g.Expect(conditions.IsTrue(pool, infrav1.MachineConfigPoolSlotAvailableCondition)).To(BeTrue())
		g.Expect(conditions.IsTrue(pool, infrav1.MachineConfigPoolPersistentDisksReadyCondition)).To(BeTrue())
		// v1beta2 mirror is written too.
		g.Expect(v1beta2conditions.Get(pool, infrav1.VSphereMachineConfigPoolMembersValidV1Beta2Condition).Status).To(Equal(metav1.ConditionTrue))
	})

	t.Run("duplicate hostname flags MembersUnique with DuplicateHostname", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "c1"},
				Configs: []infrav1.MachineConfigSlot{
					{Hostname: "dup"},
					{Hostname: "dup"},
				},
			},
		}
		updateSlotCounters(pool, pool.Status.ConfigStatuses)
		r := newReconciler(pool)
		r.reconcilePoolHealthConditions(context.Background(), pool)

		g.Expect(conditions.IsFalse(pool, infrav1.MachineConfigPoolMembersUniqueCondition)).To(BeTrue())
		g.Expect(conditions.GetReason(pool, infrav1.MachineConfigPoolMembersUniqueCondition)).To(Equal(infrav1.MachineConfigPoolDuplicateHostnameReason))
	})

	t.Run("full pool with a released slot flags SlotAvailable WaitingForReclaim", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "c1"},
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{Hostname: "host-1", State: infrav1.MachineConfigSlotStateReleased},
				},
			},
		}
		updateSlotCounters(pool, pool.Status.ConfigStatuses)
		r := newReconciler(pool)
		r.reconcilePoolHealthConditions(context.Background(), pool)

		g.Expect(conditions.IsFalse(pool, infrav1.MachineConfigPoolSlotAvailableCondition)).To(BeTrue())
		g.Expect(conditions.GetReason(pool, infrav1.MachineConfigPoolSlotAvailableCondition)).To(Equal(infrav1.MachineConfigPoolWaitingForReclaimReason))
	})

	t.Run("failed reclaim flags PersistentDisksReady", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "c1"},
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "host-1"}},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{{
					Hostname: "host-1",
					State:    infrav1.MachineConfigSlotStateReleased,
				}},
				PersistentDiskStatuses: []infrav1.PersistentDiskStatus{{
					Hostname:  "host-1",
					Name:      "data",
					Phase:     infrav1.PersistentDiskPhaseError,
					LastError: "persistent disk is still attached: vm-1",
				}},
			},
		}
		updateSlotCounters(pool, pool.Status.ConfigStatuses)
		r := newReconciler(pool)
		r.reconcilePoolHealthConditions(context.Background(), pool)

		g.Expect(conditions.IsFalse(pool, infrav1.MachineConfigPoolPersistentDisksReadyCondition)).To(BeTrue())
		g.Expect(conditions.GetReason(pool, infrav1.MachineConfigPoolPersistentDisksReadyCondition)).To(Equal(infrav1.MachineConfigPoolDiskStillAttachedReason))
	})

	t.Run("in-use slot with unprovisioned disk flags PersistentDisksReady DisksProvisioning", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "c1"},
				Configs: []infrav1.MachineConfigSlot{{
					Hostname:        "host-1",
					PersistentDisks: []infrav1.PersistentDisk{{Name: "data", SizeGiB: 10}},
				}},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{Hostname: "host-1", State: infrav1.MachineConfigSlotStateInUse},
				},
			},
		}
		updateSlotCounters(pool, pool.Status.ConfigStatuses)
		r := newReconciler(pool)
		r.reconcilePoolHealthConditions(context.Background(), pool)

		g.Expect(conditions.IsFalse(pool, infrav1.MachineConfigPoolPersistentDisksReadyCondition)).To(BeTrue())
		g.Expect(conditions.GetReason(pool, infrav1.MachineConfigPoolPersistentDisksReadyCondition)).To(Equal(infrav1.MachineConfigPoolDisksProvisioningReason))
	})

	t.Run("in-use slot with provisioned disk keeps PersistentDisksReady true", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "c1"},
				Configs: []infrav1.MachineConfigSlot{{
					Hostname:        "host-1",
					PersistentDisks: []infrav1.PersistentDisk{{Name: "data", SizeGiB: 10, VolumePath: "[ds] host-1/data.vmdk"}},
				}},
			},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConfigStatuses: []infrav1.MachineConfigSlotStatus{
					{Hostname: "host-1", State: infrav1.MachineConfigSlotStateInUse},
				},
			},
		}
		updateSlotCounters(pool, pool.Status.ConfigStatuses)
		r := newReconciler(pool)
		r.reconcilePoolHealthConditions(context.Background(), pool)

		g.Expect(conditions.IsTrue(pool, infrav1.MachineConfigPoolPersistentDisksReadyCondition)).To(BeTrue())
	})

	t.Run("cross-pool hostname collision flags MembersUnique", func(t *testing.T) {
		g := NewWithT(t)
		other := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "c1"},
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "shared"}},
			},
		}
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "c1"},
				Configs:    []infrav1.MachineConfigSlot{{Hostname: "shared"}},
			},
		}
		updateSlotCounters(pool, pool.Status.ConfigStatuses)
		r := newReconciler(pool, other)
		r.reconcilePoolHealthConditions(context.Background(), pool)

		g.Expect(conditions.IsFalse(pool, infrav1.MachineConfigPoolMembersUniqueCondition)).To(BeTrue())
		g.Expect(conditions.GetReason(pool, infrav1.MachineConfigPoolMembersUniqueCondition)).To(Equal(infrav1.MachineConfigPoolDuplicateHostnameReason))
	})
}

func TestSetPoolReadySummary(t *testing.T) {
	g := NewWithT(t)

	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
	}
	// All health conditions True, but SlotAvailable False — Ready must stay True
	// because SlotAvailable is a capacity signal excluded from the summary.
	conditions.MarkTrue(pool, infrav1.ClusterRefReadyCondition)
	conditions.MarkTrue(pool, infrav1.VCenterAvailableCondition)
	conditions.MarkTrue(pool, infrav1.MachineConfigPoolMembersValidCondition)
	conditions.MarkTrue(pool, infrav1.MachineConfigPoolMembersUniqueCondition)
	conditions.MarkTrue(pool, infrav1.MachineConfigPoolPersistentDisksReadyCondition)
	conditions.MarkFalse(pool, infrav1.MachineConfigPoolSlotAvailableCondition, infrav1.MachineConfigPoolAllSlotsInUseReason, clusterv1.ConditionSeverityInfo, "full")

	setPoolReadySummary(pool)
	g.Expect(conditions.IsTrue(pool, clusterv1.ReadyCondition)).To(BeTrue())

	// Flip a health condition False — Ready must become False.
	conditions.MarkFalse(pool, infrav1.MachineConfigPoolMembersValidCondition, infrav1.MachineConfigPoolInvalidMemberConfigReason, clusterv1.ConditionSeverityWarning, "bad")
	setPoolReadySummary(pool)
	g.Expect(conditions.IsFalse(pool, clusterv1.ReadyCondition)).To(BeTrue())
}

func TestResolveSlotDatacenter(t *testing.T) {
	g := NewWithT(t)

	pool := &infrav1.VSphereMachineConfigPool{
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Datacenter: "Datacenter1",
		},
	}

	slotWithOwnDatacenter := &infrav1.MachineConfigSlot{
		Hostname:   "host-1",
		Datacenter: "Datacenter2",
	}

	slotWithoutDatacenter := &infrav1.MachineConfigSlot{
		Hostname: "host-2",
	}

	g.Expect(services.ResolveMachineConfigPoolDatacenter(pool, slotWithOwnDatacenter)).To(Equal("Datacenter2"))
	g.Expect(services.ResolveMachineConfigPoolDatacenter(pool, slotWithoutDatacenter)).To(Equal("Datacenter1"))
	g.Expect(services.ResolveMachineConfigPoolDatacenter(&infrav1.VSphereMachineConfigPool{}, slotWithoutDatacenter)).To(BeEmpty())
	g.Expect(services.ResolveMachineConfigPoolDatacenter(nil, slotWithoutDatacenter)).To(BeEmpty())
}
