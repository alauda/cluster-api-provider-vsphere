package webhooks

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlclientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

func TestVSphereResourcePoolValidateCreate(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)

	kcp := &controlplanev1.KubeadmControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-1", Namespace: "default"},
	}
	md := &clusterv1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "md-1", Namespace: "default"},
	}

	tests := []struct {
		name    string
		pool    *infrav1.VSphereResourcePool
		objects []ctrlclient.Object
		wantErr bool
	}{
		{
			name: "allow empty consumer ref",
			pool: poolWithConsumerRef(nil),
		},
		{
			name: "allow kubeadmcontrolplane consumer ref",
			pool: poolWithConsumerRef(&corev1.ObjectReference{
				APIVersion: controlplanev1.GroupVersion.String(),
				Kind:       "KubeadmControlPlane",
				Namespace:  "default",
				Name:       "cp-1",
			}),
			objects: []ctrlclient.Object{kcp},
		},
		{
			name: "reject unsupported kind",
			pool: poolWithConsumerRef(&corev1.ObjectReference{
				APIVersion: "v1",
				Kind:       "Pod",
				Namespace:  "default",
				Name:       "foo",
			}),
			wantErr: true,
		},
		{
			name: "reject mismatched namespace",
			pool: poolWithConsumerRef(&corev1.ObjectReference{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "MachineDeployment",
				Namespace:  "other",
				Name:       "md-1",
			}),
			objects: []ctrlclient.Object{md},
			wantErr: true,
		},
		{
			name: "reject duplicate consumer binding",
			pool: poolWithConsumerRef(&corev1.ObjectReference{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "MachineDeployment",
				Namespace:  "default",
				Name:       "md-1",
			}),
			objects: []ctrlclient.Object{
				md,
				&infrav1.VSphereResourcePool{
					ObjectMeta: metav1.ObjectMeta{Name: "pool-other", Namespace: "default"},
					Spec: infrav1.VSphereResourcePoolSpec{
						ConsumerRef: &corev1.ObjectReference{
							APIVersion: clusterv1.GroupVersion.String(),
							Kind:       "MachineDeployment",
							Namespace:  "default",
							Name:       "md-1",
						},
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := ctrlclientfake.NewClientBuilder().WithScheme(scheme)
			if len(tt.objects) > 0 {
				builder = builder.WithObjects(toClientObjects(tt.objects)...)
			}
			webhook := &VSphereResourcePool{Client: builder.Build()}
			_, err := webhook.ValidateCreate(context.Background(), tt.pool)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
		})
	}
}

func TestVSphereResourcePoolValidateUpdate(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)

	kcp := &controlplanev1.KubeadmControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-1", Namespace: "default"},
	}

	t.Run("rejects direct rebind to another consumer", func(t *testing.T) {
		oldPool := poolWithConsumerRef(&corev1.ObjectReference{
			APIVersion: controlplanev1.GroupVersion.String(),
			Kind:       "KubeadmControlPlane",
			Namespace:  "default",
			Name:       "cp-1",
		})
		newPool := oldPool.DeepCopy()
		newPool.Spec.ConsumerRef = &corev1.ObjectReference{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "MachineDeployment",
			Namespace:  "default",
			Name:       "md-1",
		}
		webhook := &VSphereResourcePool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp).Build()}
		_, err := webhook.ValidateUpdate(context.Background(), oldPool, newPool)
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("rejects clearing consumer while consumer still exists", func(t *testing.T) {
		oldPool := poolWithConsumerRef(&corev1.ObjectReference{
			APIVersion: controlplanev1.GroupVersion.String(),
			Kind:       "KubeadmControlPlane",
			Namespace:  "default",
			Name:       "cp-1",
		})
		newPool := oldPool.DeepCopy()
		newPool.Spec.ConsumerRef = nil
		webhook := &VSphereResourcePool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(kcp).Build()}
		_, err := webhook.ValidateUpdate(context.Background(), oldPool, newPool)
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("allows clearing consumer after consumer is gone and pool is reusable", func(t *testing.T) {
		oldPool := poolWithConsumerRef(&corev1.ObjectReference{
			APIVersion: controlplanev1.GroupVersion.String(),
			Kind:       "KubeadmControlPlane",
			Namespace:  "default",
			Name:       "cp-1",
		})
		oldPool.Status = infrav1.VSphereResourcePoolStatus{
			ResourceStatuses: []infrav1.ResourceSlotStatus{{Hostname: "slot-1", State: "Available"}},
		}
		newPool := oldPool.DeepCopy()
		newPool.Spec.ConsumerRef = nil
		webhook := &VSphereResourcePool{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).Build()}
		_, err := webhook.ValidateUpdate(context.Background(), oldPool, newPool)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func toClientObjects(in []ctrlclient.Object) []ctrlclient.Object {
	out := make([]ctrlclient.Object, 0, len(in))
	for _, obj := range in {
		out = append(out, obj)
	}
	return out
}

func poolWithConsumerRef(ref *corev1.ObjectReference) *infrav1.VSphereResourcePool {
	return &infrav1.VSphereResourcePool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
		Spec: infrav1.VSphereResourcePoolSpec{
			ConsumerRef: ref,
			Resources:   []infrav1.ResourceSlot{{Hostname: "slot-1"}},
		},
	}
}
