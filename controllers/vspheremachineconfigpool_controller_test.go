package controllers

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
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

func TestMachineConfigPoolReconcileReleasedSlotWithoutReclaimableDiskBecomesAvailable(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)

	releasedAt := metav1.NewTime(time.Now().Add(-25 * time.Hour))
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "pool",
			Namespace:  "default",
			Finalizers: []string{MachineConfigPoolFinalizer},
		},
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Configs: []infrav1.MachineConfigSlot{{
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
		Status: infrav1.VSphereMachineConfigPoolStatus{
			ConfigStatuses: []infrav1.MachineConfigSlotStatus{{
				Hostname:         "host-1",
				State:            infrav1.MachineConfigSlotStateReleased,
				LastReleasedTime: &releasedAt,
				MachineRef: &corev1.ObjectReference{
					Name:      "worker-1",
					Namespace: "default",
				},
				ReclaimStatus: &infrav1.MachineConfigSlotReclaimStatus{
					State: infrav1.MachineConfigSlotReclaimStateCompleted,
				},
			}},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(pool).
		WithObjects(pool).
		Build()

	r := machineConfigPoolReconciler{
		Client: fakeClient,
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(pool),
	})
	g.Expect(err).NotTo(HaveOccurred())

	updated := &infrav1.VSphereMachineConfigPool{}
	err = fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pool), updated)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updated.Status.ConfigStatuses).To(HaveLen(1))
	g.Expect(updated.Status.ConfigStatuses[0].State).To(Equal(infrav1.MachineConfigSlotStateAvailable))
	g.Expect(updated.Status.ConfigStatuses[0].MachineRef).To(BeNil())
	g.Expect(updated.Status.ConfigStatuses[0].LastReleasedTime).To(BeNil())
	g.Expect(updated.Status.ConfigStatuses[0].ReclaimStatus).NotTo(BeNil())
	g.Expect(updated.Status.ConfigStatuses[0].ReclaimStatus.TaskRef).To(BeEmpty())
	g.Expect(updated.Status.ConfigStatuses[0].ReclaimStatus.VolumePath).To(BeEmpty())
	g.Expect(updated.Status.ConfigStatuses[0].ReclaimStatus.RetryAfter).To(BeNil())
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

func ptrTo[T any](v T) *T {
	return &v
}
