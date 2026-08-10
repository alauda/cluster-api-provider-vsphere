package webhooks

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	ctrlclientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

func zeroMaxSurgeRolloutStrategy() *controlplanev1.RolloutStrategy {
	return &controlplanev1.RolloutStrategy{
		Type:          controlplanev1.RollingUpdateStrategyType,
		RollingUpdate: &controlplanev1.RollingUpdate{MaxSurge: ptr.To(intstr.FromInt32(0))},
	}
}

func zeroMaxSurgeMDStrategy() *clusterv1.MachineDeploymentStrategy {
	return &clusterv1.MachineDeploymentStrategy{
		Type: clusterv1.RollingUpdateMachineDeploymentStrategyType,
		RollingUpdate: &clusterv1.MachineRollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromInt32(0)),
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
		},
	}
}

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
					MachineConfigPoolRef: &corev1.ObjectReference{Name: "pool-a", Namespace: "default"},
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
			Replicas:        ptr.To(int32(3)),
			RolloutStrategy: zeroMaxSurgeRolloutStrategy(),
		},
	}

	t.Run("allow unbound pool", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec:       infrav1.VSphereMachineConfigPoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Configs: []infrav1.MachineConfigSlot{{Hostname: "slot-1"}}},
		}
		webhook := &KubeadmControlPlane{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(template, kcp, pool).Build()}
		_, err := webhook.ValidateCreate(context.Background(), kcp)
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("reject pool bound to another consumer", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec:       infrav1.VSphereMachineConfigPoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Configs: []infrav1.MachineConfigSlot{{Hostname: "slot-1"}}},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConsumerRef: &corev1.ObjectReference{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "MachineDeployment",
					Namespace:  "default",
					Name:       "md-a",
				},
			},
		}
		webhook := &KubeadmControlPlane{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(template, kcp, pool).Build()}
		_, err := webhook.ValidateCreate(context.Background(), kcp)
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("allow pool already bound to self", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec:       infrav1.VSphereMachineConfigPoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Configs: []infrav1.MachineConfigSlot{{Hostname: "slot-1"}}},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConsumerRef: &corev1.ObjectReference{
					APIVersion: controlplanev1.GroupVersion.String(),
					Kind:       "KubeadmControlPlane",
					Namespace:  "default",
					Name:       "cp-a",
				},
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
					MachineConfigPoolRef: &corev1.ObjectReference{Name: "pool-a", Namespace: "default"},
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
			Strategy: zeroMaxSurgeMDStrategy(),
		},
	}

	t.Run("allow unbound pool", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec:       infrav1.VSphereMachineConfigPoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Configs: []infrav1.MachineConfigSlot{{Hostname: "slot-1"}}},
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

		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec:       infrav1.VSphereMachineConfigPoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Configs: []infrav1.MachineConfigSlot{{Hostname: "slot-1"}}},
		}
		webhook := &MachineDeployment{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(template, md, otherTemplate, otherMD, pool).Build()}
		_, err := webhook.ValidateCreate(context.Background(), md)
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("allow pool already bound to self", func(t *testing.T) {
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
			Spec:       infrav1.VSphereMachineConfigPoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Configs: []infrav1.MachineConfigSlot{{Hostname: "slot-1"}}},
			Status: infrav1.VSphereMachineConfigPoolStatus{
				ConsumerRef: &corev1.ObjectReference{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "MachineDeployment",
					Namespace:  "default",
					Name:       "md-a",
				},
			},
		}
		webhook := &MachineDeployment{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(template, md, pool).Build()}
		_, err := webhook.ValidateCreate(context.Background(), md)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func TestKubeadmControlPlaneMaxSurge(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)

	newTemplate := func(withPool bool) *infrav1.VSphereMachineTemplate {
		tmpl := &infrav1.VSphereMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cp-template", Namespace: "default"},
		}
		if withPool {
			tmpl.Spec.Template.Spec.MachineConfigPoolRef = &corev1.ObjectReference{Name: "pool-a", Namespace: "default"}
		}
		return tmpl
	}
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec:       infrav1.VSphereMachineConfigPoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Configs: []infrav1.MachineConfigSlot{{Hostname: "slot-1"}}},
	}
	// replicas is pinned to 3 so these cases exercise only the maxSurge rule, not the replicas >= 3 rule.
	newKCP := func(strategy *controlplanev1.RolloutStrategy) *controlplanev1.KubeadmControlPlane {
		return &controlplanev1.KubeadmControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp-a", Namespace: "default"},
			Spec: controlplanev1.KubeadmControlPlaneSpec{
				MachineTemplate: controlplanev1.KubeadmControlPlaneMachineTemplate{InfrastructureRef: corev1.ObjectReference{Name: "cp-template"}},
				Replicas:        ptr.To(int32(3)),
				RolloutStrategy: strategy,
			},
		}
	}
	surge := func(n int32) *controlplanev1.RolloutStrategy {
		return &controlplanev1.RolloutStrategy{RollingUpdate: &controlplanev1.RollingUpdate{MaxSurge: ptr.To(intstr.FromInt32(n))}}
	}

	tests := []struct {
		name     string
		withPool bool
		strategy *controlplanev1.RolloutStrategy
		wantErr  bool
	}{
		{name: "pool-bound with maxSurge 0 is allowed", withPool: true, strategy: surge(0), wantErr: false},
		{name: "pool-bound with maxSurge 1 is rejected", withPool: true, strategy: surge(1), wantErr: true},
		{name: "pool-bound with default (nil) maxSurge is rejected", withPool: true, strategy: nil, wantErr: true},
		{name: "template without pool ignores maxSurge", withPool: false, strategy: surge(1), wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			kcp := newKCP(tt.strategy)
			webhook := &KubeadmControlPlane{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(newTemplate(tt.withPool), kcp, pool).Build()}
			_, err := webhook.ValidateCreate(context.Background(), kcp)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("maxSurge must be 0"))
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
		})
	}
}

func TestKubeadmControlPlaneReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)

	newTemplate := func(withPool bool) *infrav1.VSphereMachineTemplate {
		tmpl := &infrav1.VSphereMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cp-template", Namespace: "default"},
		}
		if withPool {
			tmpl.Spec.Template.Spec.MachineConfigPoolRef = &corev1.ObjectReference{Name: "pool-a", Namespace: "default"}
		}
		return tmpl
	}
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec:       infrav1.VSphereMachineConfigPoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Configs: []infrav1.MachineConfigSlot{{Hostname: "slot-1"}}},
	}
	// maxSurge is pinned to 0 so these cases exercise only the replicas >= 3 rule, not the maxSurge rule.
	newKCP := func(replicas *int32) *controlplanev1.KubeadmControlPlane {
		return &controlplanev1.KubeadmControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "cp-a", Namespace: "default"},
			Spec: controlplanev1.KubeadmControlPlaneSpec{
				MachineTemplate: controlplanev1.KubeadmControlPlaneMachineTemplate{InfrastructureRef: corev1.ObjectReference{Name: "cp-template"}},
				Replicas:        replicas,
				RolloutStrategy: zeroMaxSurgeRolloutStrategy(),
			},
		}
	}

	tests := []struct {
		name     string
		withPool bool
		replicas *int32
		wantErr  bool
	}{
		{name: "pool-bound with 3 replicas is allowed", withPool: true, replicas: ptr.To(int32(3)), wantErr: false},
		{name: "pool-bound with 5 replicas is allowed", withPool: true, replicas: ptr.To(int32(5)), wantErr: false},
		{name: "pool-bound with 1 replica is rejected", withPool: true, replicas: ptr.To(int32(1)), wantErr: true},
		{name: "pool-bound with 2 replicas is rejected", withPool: true, replicas: ptr.To(int32(2)), wantErr: true},
		{name: "pool-bound with nil (default) replicas is rejected", withPool: true, replicas: nil, wantErr: true},
		{name: "template without pool ignores replicas", withPool: false, replicas: ptr.To(int32(1)), wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			kcp := newKCP(tt.replicas)
			webhook := &KubeadmControlPlane{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(newTemplate(tt.withPool), kcp, pool).Build()}
			_, err := webhook.ValidateCreate(context.Background(), kcp)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("must have at least 3 replicas"))
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
		})
	}
}

func TestMachineDeploymentMaxSurge(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)

	newTemplate := func(withPool bool) *infrav1.VSphereMachineTemplate {
		tmpl := &infrav1.VSphereMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "md-template", Namespace: "default"},
		}
		if withPool {
			tmpl.Spec.Template.Spec.MachineConfigPoolRef = &corev1.ObjectReference{Name: "pool-a", Namespace: "default"}
		}
		return tmpl
	}
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec:       infrav1.VSphereMachineConfigPoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Configs: []infrav1.MachineConfigSlot{{Hostname: "slot-1"}}},
	}
	newMD := func(strategy *clusterv1.MachineDeploymentStrategy) *clusterv1.MachineDeployment {
		return &clusterv1.MachineDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "md-a", Namespace: "default"},
			Spec: clusterv1.MachineDeploymentSpec{
				Template: clusterv1.MachineTemplateSpec{Spec: clusterv1.MachineSpec{InfrastructureRef: corev1.ObjectReference{Name: "md-template"}}},
				Strategy: strategy,
			},
		}
	}
	// maxUnavailable is pinned to 1 so these cases exercise only the maxSurge rule, not the maxUnavailable rule.
	surge := func(n int32) *clusterv1.MachineDeploymentStrategy {
		return &clusterv1.MachineDeploymentStrategy{RollingUpdate: &clusterv1.MachineRollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromInt32(n)),
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
		}}
	}

	tests := []struct {
		name     string
		withPool bool
		strategy *clusterv1.MachineDeploymentStrategy
		wantErr  bool
	}{
		{name: "pool-bound with maxSurge 0 is allowed", withPool: true, strategy: surge(0), wantErr: false},
		{name: "pool-bound with maxSurge 1 is rejected", withPool: true, strategy: surge(1), wantErr: true},
		{name: "pool-bound with default (nil) strategy is rejected", withPool: true, strategy: nil, wantErr: true},
		{name: "pool-bound with OnDelete strategy is allowed", withPool: true, strategy: &clusterv1.MachineDeploymentStrategy{Type: clusterv1.OnDeleteMachineDeploymentStrategyType}, wantErr: false},
		{name: "template without pool ignores maxSurge", withPool: false, strategy: surge(1), wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			md := newMD(tt.strategy)
			webhook := &MachineDeployment{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(newTemplate(tt.withPool), md, pool).Build()}
			_, err := webhook.ValidateCreate(context.Background(), md)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("maxSurge must be 0"))
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
		})
	}
}

func TestMachineDeploymentMaxUnavailable(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)

	newTemplate := func(withPool bool) *infrav1.VSphereMachineTemplate {
		tmpl := &infrav1.VSphereMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "md-template", Namespace: "default"},
		}
		if withPool {
			tmpl.Spec.Template.Spec.MachineConfigPoolRef = &corev1.ObjectReference{Name: "pool-a", Namespace: "default"}
		}
		return tmpl
	}
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec:       infrav1.VSphereMachineConfigPoolSpec{ClusterRef: corev1.ObjectReference{Name: "test-cluster"}, Configs: []infrav1.MachineConfigSlot{{Hostname: "slot-1"}}},
	}
	newMD := func(strategy *clusterv1.MachineDeploymentStrategy) *clusterv1.MachineDeployment {
		return &clusterv1.MachineDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "md-a", Namespace: "default"},
			Spec: clusterv1.MachineDeploymentSpec{
				Template: clusterv1.MachineTemplateSpec{Spec: clusterv1.MachineSpec{InfrastructureRef: corev1.ObjectReference{Name: "md-template"}}},
				Strategy: strategy,
			},
		}
	}
	// maxSurge is pinned to 0 so these cases exercise only the maxUnavailable rule, not the maxSurge rule.
	unavail := func(u *intstr.IntOrString) *clusterv1.MachineDeploymentStrategy {
		return &clusterv1.MachineDeploymentStrategy{RollingUpdate: &clusterv1.MachineRollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromInt32(0)),
			MaxUnavailable: u,
		}}
	}

	tests := []struct {
		name     string
		withPool bool
		strategy *clusterv1.MachineDeploymentStrategy
		wantErr  bool
	}{
		{name: "pool-bound with maxUnavailable 1 is allowed", withPool: true, strategy: unavail(ptr.To(intstr.FromInt32(1))), wantErr: false},
		{name: "pool-bound with maxUnavailable 2 is allowed", withPool: true, strategy: unavail(ptr.To(intstr.FromInt32(2))), wantErr: false},
		{name: "pool-bound with maxUnavailable 0 is rejected", withPool: true, strategy: unavail(ptr.To(intstr.FromInt32(0))), wantErr: true},
		{name: "pool-bound with nil (default 0) maxUnavailable is rejected", withPool: true, strategy: unavail(nil), wantErr: true},
		{name: "template without pool ignores maxUnavailable", withPool: false, strategy: unavail(ptr.To(intstr.FromInt32(0))), wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			md := newMD(tt.strategy)
			webhook := &MachineDeployment{Client: ctrlclientfake.NewClientBuilder().WithScheme(scheme).WithObjects(newTemplate(tt.withPool), md, pool).Build()}
			_, err := webhook.ValidateCreate(context.Background(), md)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("maxUnavailable must be at least 1"))
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
		})
	}
}
