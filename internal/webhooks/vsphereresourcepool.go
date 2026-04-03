package webhooks

import (
	"context"
	"fmt"

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

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1beta1-vsphereresourcepool,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=vsphereresourcepools,versions=v1beta1,name=validation.vsphereresourcepool.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1beta1

type VSphereResourcePool struct {
	Client client.Client
}

var _ webhook.CustomValidator = &VSphereResourcePool{}

func (webhook *VSphereResourcePool) SetupWebhookWithManager(mgr ctrl.Manager) error {
	webhook.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&infrav1.VSphereResourcePool{}).
		WithValidator(webhook).
		Complete()
}

func (webhook *VSphereResourcePool) ValidateCreate(ctx context.Context, raw runtime.Object) (admission.Warnings, error) {
	obj, ok := raw.(*infrav1.VSphereResourcePool)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereResourcePool but got a %T", raw))
	}
	return nil, AggregateObjErrors(obj.GroupVersionKind().GroupKind(), obj.Name, webhook.validateConsumerRef(ctx, nil, obj))
}

func (webhook *VSphereResourcePool) ValidateUpdate(ctx context.Context, oldRaw runtime.Object, newRaw runtime.Object) (admission.Warnings, error) {
	oldObj, ok := oldRaw.(*infrav1.VSphereResourcePool)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereResourcePool but got a %T", oldRaw))
	}
	newObj, ok := newRaw.(*infrav1.VSphereResourcePool)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereResourcePool but got a %T", newRaw))
	}
	return nil, AggregateObjErrors(newObj.GroupVersionKind().GroupKind(), newObj.Name, webhook.validateConsumerRef(ctx, oldObj, newObj))
}

func (webhook *VSphereResourcePool) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (webhook *VSphereResourcePool) validateConsumerRef(ctx context.Context, oldObj, newObj *infrav1.VSphereResourcePool) field.ErrorList {
	var allErrs field.ErrorList
	consumerPath := field.NewPath("spec", "consumerRef")
	ref := newObj.Spec.ConsumerRef

	if oldObj != nil && !services.ConsumerRefsEqual(oldObj.Spec.ConsumerRef, newObj.Spec.ConsumerRef) && oldObj.Spec.ConsumerRef != nil && !services.IsPoolFullyReusable(oldObj) {
		allErrs = append(allErrs, field.Forbidden(consumerPath, "cannot change consumerRef until the pool is fully reusable"))
	}
	if oldObj != nil && oldObj.Spec.ConsumerRef != nil && newObj.Spec.ConsumerRef != nil && !services.ConsumerRefsEqual(oldObj.Spec.ConsumerRef, newObj.Spec.ConsumerRef) {
		allErrs = append(allErrs, field.Forbidden(consumerPath, "cannot rebind directly to a different consumer; wait until the pool is unbound"))
	}
	if oldObj != nil && oldObj.Spec.ConsumerRef != nil && newObj.Spec.ConsumerRef == nil {
		target := services.ObjectForConsumerRef(oldObj.Spec.ConsumerRef)
		if target != nil {
			key := client.ObjectKey{Namespace: oldObj.Namespace, Name: oldObj.Spec.ConsumerRef.Name}
			if err := webhook.Client.Get(ctx, key, target); err == nil {
				allErrs = append(allErrs, field.Forbidden(consumerPath, "cannot clear consumerRef while the referenced consumer still exists"))
			} else if err != nil && !apierrors.IsNotFound(err) {
				allErrs = append(allErrs, field.InternalError(consumerPath, err))
			}
		}
	}
	if ref == nil {
		return allErrs
	}

	switch ref.Kind {
	case "KubeadmControlPlane":
		if ref.APIVersion != controlplanev1.GroupVersion.String() {
			allErrs = append(allErrs, field.Invalid(consumerPath.Child("apiVersion"), ref.APIVersion, "must match KubeadmControlPlane apiVersion"))
		}
	case "MachineDeployment":
		if ref.APIVersion != clusterv1.GroupVersion.String() {
			allErrs = append(allErrs, field.Invalid(consumerPath.Child("apiVersion"), ref.APIVersion, "must match MachineDeployment apiVersion"))
		}
	default:
		allErrs = append(allErrs, field.NotSupported(consumerPath.Child("kind"), ref.Kind, []string{"KubeadmControlPlane", "MachineDeployment"}))
	}

	if ref.Namespace != "" && ref.Namespace != newObj.Namespace {
		allErrs = append(allErrs, field.Invalid(consumerPath.Child("namespace"), ref.Namespace, "must match pool namespace"))
	}
	if ref.Name == "" {
		allErrs = append(allErrs, field.Required(consumerPath.Child("name"), "must be set"))
	}

	if target := services.ObjectForConsumerRef(ref); target != nil {
		key := client.ObjectKey{Namespace: newObj.Namespace, Name: ref.Name}
		if err := webhook.Client.Get(ctx, key, target); err != nil {
			if apierrors.IsNotFound(err) {
				allErrs = append(allErrs, field.Invalid(consumerPath, ref.Name, "referenced consumer does not exist"))
			} else {
				allErrs = append(allErrs, field.InternalError(consumerPath, err))
			}
		}
	}

	pools := &infrav1.VSphereResourcePoolList{}
	if err := webhook.Client.List(ctx, pools, client.InNamespace(newObj.Namespace)); err != nil {
		allErrs = append(allErrs, field.InternalError(consumerPath, err))
		return allErrs
	}
	for i := range pools.Items {
		existing := &pools.Items[i]
		if existing.Name == newObj.Name {
			continue
		}
		if services.ConsumerRefsEqual(existing.Spec.ConsumerRef, ref) {
			allErrs = append(allErrs, field.Invalid(consumerPath, ref.Name, fmt.Sprintf("already bound by VSphereResourcePool %s", existing.Name)))
			break
		}
	}

	return allErrs
}

