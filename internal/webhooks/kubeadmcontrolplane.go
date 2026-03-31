package webhooks

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
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

	poolRef := template.Spec.Template.Spec.ResourcePoolRef
	if poolRef == nil {
		return allErrs
	}
	pool := &infrav1.VSphereResourcePool{}
	if err := webhook.Client.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.Invalid(templatePath.Child("name"), template.Name, "referenced resource pool does not exist"))
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
	if pool.Spec.ConsumerRef != nil && !services.ConsumerRefsEqual(pool.Spec.ConsumerRef, self) {
		allErrs = append(allErrs, field.Forbidden(templatePath, fmt.Sprintf("resource pool %s/%s is bound to %s %s/%s", pool.Namespace, pool.Name, pool.Spec.ConsumerRef.Kind, pool.Spec.ConsumerRef.Namespace, pool.Spec.ConsumerRef.Name)))
	}

	if err := rejectOtherObjectsReferencingPool(ctx, webhook.Client, poolRef, self); err != nil {
		allErrs = append(allErrs, field.Forbidden(templatePath, err.Error()))
	}

	return allErrs
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
		if err := c.Get(ctx, client.ObjectKey{Namespace: kcp.Namespace, Name: kcp.Spec.MachineTemplate.InfrastructureRef.Name}, template); err == nil {
			if services.ConsumerRefsEqual(template.Spec.Template.Spec.ResourcePoolRef, poolRef) {
				return errors.Errorf("resource pool %s/%s is already referenced by KubeadmControlPlane %s/%s", poolRef.Namespace, poolRef.Name, kcp.Namespace, kcp.Name)
			}
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
		if err := c.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: md.Spec.Template.Spec.InfrastructureRef.Name}, template); err == nil {
			if services.ConsumerRefsEqual(template.Spec.Template.Spec.ResourcePoolRef, poolRef) {
				return errors.Errorf("resource pool %s/%s is already referenced by MachineDeployment %s/%s", poolRef.Namespace, poolRef.Name, md.Namespace, md.Name)
			}
		}
	}
	return nil
}
