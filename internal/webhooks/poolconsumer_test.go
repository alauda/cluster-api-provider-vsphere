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
	ctrlclientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

func TestKubeadmControlPlaneValidatePoolRef(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)

	template := &infrav1.VSphereMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-template", Namespace: "default"},
		Spec: infrav1.VSphereMachineTemplateSpec{
			Template: infrav1.VSphereMachineTemplateResource{
				Spec: infrav1.VSphereMachineSpec{
					ResourcePoolRef: &corev1.ObjectReference{Name: "pool-a", Namespace: "default"},
				},
			},
		},
	}
	kcp := &controlplanev1.KubeadmControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-a", Namespace: "default"},
		Spec: controlplanev1.KubeadmControlPlaneSpec{
			MachineTemplate: controlplanev1.KubeadmControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{Name: "cp-template"},
			},
		},
	}

	t.Run("allow unbound pool", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec:       infrav1.VSphereResourcePoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Resources: []infrav1.ResourceSlot{{Hostname: "slot-1"}}},
		}
		webhook := &KubeadmControlPlane{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(template, kcp, pool).Build()}
		_, err := webhook.ValidateCreate(context.Background(), kcp)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("reject pool bound to another consumer", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec: infrav1.VSphereResourcePoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				ConsumerRef: &corev1.ObjectReference{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "MachineDeployment",
					Namespace:  "default",
					Name:       "md-a",
				},
				Resources: []infrav1.ResourceSlot{{Hostname: "slot-1"}},
			},
		}
		webhook := &KubeadmControlPlane{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(template, kcp, pool).Build()}
		_, err := webhook.ValidateCreate(context.Background(), kcp)
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("allow pool already bound to self", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec: infrav1.VSphereResourcePoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				ConsumerRef: &corev1.ObjectReference{
					APIVersion: controlplanev1.GroupVersion.String(),
					Kind:       "KubeadmControlPlane",
					Namespace:  "default",
					Name:       "cp-a",
				},
				Resources: []infrav1.ResourceSlot{{Hostname: "slot-1"}},
			},
		}
		webhook := &KubeadmControlPlane{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(template, kcp, pool).Build()}
		_, err := webhook.ValidateCreate(context.Background(), kcp)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func TestMachineDeploymentValidatePoolRef(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)

	template := &infrav1.VSphereMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "md-template", Namespace: "default"},
		Spec: infrav1.VSphereMachineTemplateSpec{
			Template: infrav1.VSphereMachineTemplateResource{
				Spec: infrav1.VSphereMachineSpec{
					ResourcePoolRef: &corev1.ObjectReference{Name: "pool-a", Namespace: "default"},
				},
			},
		},
	}
	md := &clusterv1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "md-a", Namespace: "default"},
		Spec: clusterv1.MachineDeploymentSpec{
			Template: clusterv1.MachineTemplateSpec{
				Spec: clusterv1.MachineSpec{
					InfrastructureRef: corev1.ObjectReference{Name: "md-template"},
				},
			},
		},
	}

	t.Run("allow unbound pool", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec:       infrav1.VSphereResourcePoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Resources: []infrav1.ResourceSlot{{Hostname: "slot-1"}}},
		}
		webhook := &MachineDeployment{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(template, md, pool).Build()}
		_, err := webhook.ValidateCreate(context.Background(), md)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("reject duplicate static reference from another deployment", func(t *testing.T) {
		otherTemplate := template.DeepCopy()
		otherTemplate.Name = "md-template-b"
		otherMD := md.DeepCopy()
		otherMD.Name = "md-b"
		otherMD.Spec.Template.Spec.InfrastructureRef.Name = "md-template-b"

		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec:       infrav1.VSphereResourcePoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Resources: []infrav1.ResourceSlot{{Hostname: "slot-1"}}},
		}
		webhook := &MachineDeployment{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(template, md, otherTemplate, otherMD, pool).Build()}
		_, err := webhook.ValidateCreate(context.Background(), md)
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("allow pool already bound to self", func(t *testing.T) {
		pool := &infrav1.VSphereResourcePool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec: infrav1.VSphereResourcePoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				ConsumerRef: &corev1.ObjectReference{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "MachineDeployment",
					Namespace:  "default",
					Name:       "md-a",
				},
				Resources: []infrav1.ResourceSlot{{Hostname: "slot-1"}},
			},
		}
		webhook := &MachineDeployment{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(template, md, pool).Build()}
		_, err := webhook.ValidateCreate(context.Background(), md)
		g.Expect(err).NotTo(HaveOccurred())
	})
}
