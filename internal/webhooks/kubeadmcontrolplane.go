package webhooks

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
)

// +kubebuilder:webhook:verbs=create;update,path=/validate-controlplane-cluster-x-k8s-io-v1beta1-kubeadmcontrolplane-capv,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=controlplane.cluster.x-k8s.io,resources=kubeadmcontrolplanes,versions=v1beta1,name=validation.kubeadmcontrolplane.capv.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1beta1
type KubeadmControlPlane struct {
	Client client.Client
}

var _ webhook.CustomValidator = &KubeadmControlPlane{}

func (webhook *KubeadmControlPlane) SetupWebhookWithManager(mgr ctrl.Manager) error {
	webhook.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&controlplanev1.KubeadmControlPlane{}).
		WithValidator(webhook).
		Complete()
}

func (webhook *KubeadmControlPlane) ValidateCreate(ctx context.Context, raw runtime.Object) (admission.Warnings, error) {
	obj, ok := raw.(*controlplanev1.KubeadmControlPlane)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a KubeadmControlPlane but got a %T", raw))
	}
	return nil, AggregateObjErrors(obj.GroupVersionKind().GroupKind(), obj.Name, webhook.validatePoolRef(ctx, obj))
}

func (webhook *KubeadmControlPlane) ValidateUpdate(ctx context.Context, _, newRaw runtime.Object) (admission.Warnings, error) {
	obj, ok := newRaw.(*controlplanev1.KubeadmControlPlane)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a KubeadmControlPlane but got a %T", newRaw))
	}
	return nil, AggregateObjErrors(obj.GroupVersionKind().GroupKind(), obj.Name, webhook.validatePoolRef(ctx, obj))
}

func (webhook *KubeadmControlPlane) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (webhook *KubeadmControlPlane) validatePoolRef(ctx context.Context, obj *controlplanev1.KubeadmControlPlane) field.ErrorList {
	var allErrs field.ErrorList
	template := &infrav1.VSphereMachineTemplate{}
	templatePath := field.NewPath("spec", "machineTemplate", "infrastructureRef")
	key := client.ObjectKey{Namespace: obj.Namespace, Name: obj.Spec.MachineTemplate.InfrastructureRef.Name}
	if err := webhook.Client.Get(ctx, key, template); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(templatePath.Child("name"), obj.Spec.MachineTemplate.InfrastructureRef.Name, "referenced VSphereMachineTemplate does not exist"))
		} else {
			allErrs = append(allErrs, field.InternalError(templatePath, err))
		}
		return allErrs
	}

	poolRef := template.Spec.Template.Spec.MachineConfigPoolRef
	if poolRef == nil {
		return allErrs
	}
	pool := &infrav1.VSphereMachineConfigPool{}
	if err := webhook.Client.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(templatePath.Child("name"), template.Name, "referenced machine config pool does not exist"))
		} else {
			allErrs = append(allErrs, field.InternalError(templatePath, err))
		}
		return allErrs
	}

	self := &corev1.ObjectReference{
		APIVersion: controlplanev1.GroupVersion.String(),
		Kind:       "KubeadmControlPlane",
		Namespace:  obj.Namespace,
		Name:       obj.Name,
		UID:        obj.UID,
	}
	if pool.Status.ConsumerRef != nil && !services.ConsumerRefsEqual(pool.Status.ConsumerRef, self) {
		allErrs = append(allErrs, field.Forbidden(templatePath, fmt.Sprintf("machine config pool %s/%s is bound to %s %s/%s", pool.Namespace, pool.Name, pool.Status.ConsumerRef.Kind, pool.Status.ConsumerRef.Namespace, pool.Status.ConsumerRef.Name)))
	}

	if err := rejectOtherObjectsReferencingPool(ctx, webhook.Client, poolRef, self); err != nil {
		allErrs = append(allErrs, field.Forbidden(templatePath, err.Error()))
	}

	var maxSurge *intstr.IntOrString
	if obj.Spec.RolloutStrategy != nil && obj.Spec.RolloutStrategy.RollingUpdate != nil {
		maxSurge = obj.Spec.RolloutStrategy.RollingUpdate.MaxSurge
	}
	if err := requireZeroMaxSurge(maxSurge, field.NewPath("spec", "rolloutStrategy", "rollingUpdate", "maxSurge")); err != nil {
		allErrs = append(allErrs, err)
	}

	// A fixed-IP rollout uses maxSurge 0 (scale-in), which Cluster API only permits for control planes with
	// at least 3 replicas. Enforce it here so the rejection carries a clear, self-explanatory reason instead
	// of Cluster API's generic scale-in error; single-replica control planes are not supported on fixed IP.
	if obj.Spec.Replicas == nil || *obj.Spec.Replicas < 3 {
		val := "1 (default)"
		if obj.Spec.Replicas != nil {
			val = fmt.Sprintf("%d", *obj.Spec.Replicas)
		}
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "replicas"), val,
			"a KubeadmControlPlane whose infrastructure template references a machineConfigPoolRef (fixed IP) must have at least 3 replicas: "+
				"the fixed-IP rollout uses maxSurge 0 (scale-in), which Cluster API only permits for control planes with 3 or more replicas; single-replica control planes are not supported"))
	}

	return allErrs
}

// requireZeroMaxSurge enforces maxSurge: 0 for KCP/MachineDeployment objects whose infrastructure template
// is bound to a VSphereMachineConfigPool (fixed IP). A rolling update with surge creates the replacement
// machine before deleting the old one, which needs an extra free pool slot to obtain a fixed IP; a
// fully-allocated pool cannot provide it and the upgrade would stall waiting for a slot. maxSurge: 0 replaces
// machines delete-first so the freed slot is reused. A nil strategy/maxSurge means the API default (1).
func requireZeroMaxSurge(maxSurge *intstr.IntOrString, fldPath *field.Path) *field.Error {
	if maxSurge != nil && maxSurge.Type == intstr.Int && maxSurge.IntValue() == 0 {
		return nil
	}
	val := "1 (default)"
	if maxSurge != nil {
		val = maxSurge.String()
	}
	return field.Invalid(fldPath, val, "maxSurge must be 0 when the infrastructure template references a machineConfigPoolRef (fixed IP): "+
		"a rolling update with surge needs an extra free pool slot for the new machine; set maxSurge to 0 to replace machines delete-first and reuse the freed slot")
}

// requirePositiveMaxUnavailable enforces maxUnavailable >= 1 for a MachineDeployment whose infrastructure
// template is bound to a VSphereMachineConfigPool (fixed IP). With maxSurge pinned to 0, a rollout can only
// progress by first deleting an old machine to free its pool slot; maxUnavailable 0 (the Cluster API default)
// forbids that, so the rollout would stall. A nil maxUnavailable means the API default (0).
func requirePositiveMaxUnavailable(maxUnavailable *intstr.IntOrString, fldPath *field.Path) *field.Error {
	if maxUnavailable != nil && !(maxUnavailable.Type == intstr.Int && maxUnavailable.IntValue() == 0) {
		return nil
	}
	val := "0 (default)"
	if maxUnavailable != nil {
		val = maxUnavailable.String()
	}
	return field.Invalid(fldPath, val, "maxUnavailable must be at least 1 when the infrastructure template references a machineConfigPoolRef (fixed IP): "+
		"with maxSurge pinned to 0, a rollout must delete an old machine to free its pool slot before creating the replacement; maxUnavailable 0 forbids that and the rollout would stall")
}

func rejectOtherObjectsReferencingPool(ctx context.Context, c client.Client, poolRef, self *corev1.ObjectReference) error {
	kcps := &controlplanev1.KubeadmControlPlaneList{}
	if err := c.List(ctx, kcps, client.InNamespace(self.Namespace)); err != nil {
		return err
	}
	for i := range kcps.Items {
		kcp := &kcps.Items[i]
		if kcp.Name == self.Name && self.Kind == "KubeadmControlPlane" {
			continue
		}
		template := &infrav1.VSphereMachineTemplate{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: kcp.Namespace, Name: kcp.Spec.MachineTemplate.InfrastructureRef.Name}, template); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if services.ConsumerRefsEqual(template.Spec.Template.Spec.MachineConfigPoolRef, poolRef) {
			return errors.Errorf("machine config pool %s/%s is already referenced by KubeadmControlPlane %s/%s", poolRef.Namespace, poolRef.Name, kcp.Namespace, kcp.Name)
		}
	}

	mds := &clusterv1.MachineDeploymentList{}
	if err := c.List(ctx, mds, client.InNamespace(self.Namespace)); err != nil {
		return err
	}
	for i := range mds.Items {
		md := &mds.Items[i]
		if md.Spec.Template.Spec.InfrastructureRef.Name == "" {
			continue
		}
		if md.Name == self.Name && self.Kind == "MachineDeployment" {
			continue
		}
		template := &infrav1.VSphereMachineTemplate{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: md.Spec.Template.Spec.InfrastructureRef.Name}, template); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if services.ConsumerRefsEqual(template.Spec.Template.Spec.MachineConfigPoolRef, poolRef) {
			return errors.Errorf("machine config pool %s/%s is already referenced by MachineDeployment %s/%s", poolRef.Namespace, poolRef.Name, md.Namespace, md.Name)
		}
	}
	return nil
}
