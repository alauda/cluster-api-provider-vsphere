package webhooks

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
)

// +kubebuilder:webhook:verbs=create;update,path=/validate-cluster-x-k8s-io-v1beta1-machinedeployment-capv,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=cluster.x-k8s.io,resources=machinedeployments,versions=v1beta1,name=validation.machinedeployment.capv.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1beta1
type MachineDeployment struct {
	Client client.Client
}

var _ webhook.CustomValidator = &MachineDeployment{}

func (webhook *MachineDeployment) SetupWebhookWithManager(mgr ctrl.Manager) error {
	webhook.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&clusterv1.MachineDeployment{}).
		WithValidator(webhook).
		Complete()
}

func (webhook *MachineDeployment) ValidateCreate(ctx context.Context, raw runtime.Object) (admission.Warnings, error) {
	obj, ok := raw.(*clusterv1.MachineDeployment)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a MachineDeployment but got a %T", raw))
	}
	return nil, AggregateObjErrors(obj.GroupVersionKind().GroupKind(), obj.Name, webhook.validatePoolRef(ctx, obj))
}

func (webhook *MachineDeployment) ValidateUpdate(ctx context.Context, _, newRaw runtime.Object) (admission.Warnings, error) {
	obj, ok := newRaw.(*clusterv1.MachineDeployment)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a MachineDeployment but got a %T", newRaw))
	}
	return nil, AggregateObjErrors(obj.GroupVersionKind().GroupKind(), obj.Name, webhook.validatePoolRef(ctx, obj))
}

func (webhook *MachineDeployment) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (webhook *MachineDeployment) validatePoolRef(ctx context.Context, obj *clusterv1.MachineDeployment) field.ErrorList {
	var allErrs field.ErrorList
	templatePath := field.NewPath("spec", "template", "spec", "infrastructureRef")
	if obj.Spec.Template.Spec.InfrastructureRef.Name == "" {
		return allErrs
	}
	template := &infrav1.VSphereMachineTemplate{}
	key := client.ObjectKey{Namespace: obj.Namespace, Name: obj.Spec.Template.Spec.InfrastructureRef.Name}
	if err := webhook.Client.Get(ctx, key, template); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(templatePath.Child("name"), obj.Spec.Template.Spec.InfrastructureRef.Name, "referenced VSphereMachineTemplate does not exist"))
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
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "MachineDeployment",
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

	// OnDelete replaces machines delete-first and has no surge, so it needs no fixed-IP constraint.
	if obj.Spec.Strategy == nil || obj.Spec.Strategy.Type != clusterv1.OnDeleteMachineDeploymentStrategyType {
		var maxSurge, maxUnavailable *intstr.IntOrString
		if obj.Spec.Strategy != nil && obj.Spec.Strategy.RollingUpdate != nil {
			maxSurge = obj.Spec.Strategy.RollingUpdate.MaxSurge
			maxUnavailable = obj.Spec.Strategy.RollingUpdate.MaxUnavailable
		}
		if err := requireZeroMaxSurge(maxSurge, field.NewPath("spec", "strategy", "rollingUpdate", "maxSurge")); err != nil {
			allErrs = append(allErrs, err)
		}
		if err := requirePositiveMaxUnavailable(maxUnavailable, field.NewPath("spec", "strategy", "rollingUpdate", "maxUnavailable")); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	return allErrs
}
