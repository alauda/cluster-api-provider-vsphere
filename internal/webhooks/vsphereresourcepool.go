package webhooks

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
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

func (webhook *VSphereResourcePool) ValidateCreate(_ context.Context, raw runtime.Object) (admission.Warnings, error) {
	obj, ok := raw.(*infrav1.VSphereResourcePool)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereResourcePool but got a %T", raw))
	}
	return nil, AggregateObjErrors(obj.GroupVersionKind().GroupKind(), obj.Name, webhook.validateClusterRef(nil, obj))
}

func (webhook *VSphereResourcePool) ValidateUpdate(_ context.Context, oldRaw runtime.Object, newRaw runtime.Object) (admission.Warnings, error) {
	oldObj, ok := oldRaw.(*infrav1.VSphereResourcePool)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereResourcePool but got a %T", oldRaw))
	}
	newObj, ok := newRaw.(*infrav1.VSphereResourcePool)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereResourcePool but got a %T", newRaw))
	}
	return nil, AggregateObjErrors(newObj.GroupVersionKind().GroupKind(), newObj.Name, webhook.validateClusterRef(oldObj, newObj))
}

func (webhook *VSphereResourcePool) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (webhook *VSphereResourcePool) validateClusterRef(oldObj, newObj *infrav1.VSphereResourcePool) field.ErrorList {
	var allErrs field.ErrorList
	clusterRefPath := field.NewPath("spec", "clusterRef")
	ref := newObj.Spec.ClusterRef

	if ref.Name == "" {
		allErrs = append(allErrs, field.Required(clusterRefPath.Child("name"), "must be set"))
	}
	if ref.APIVersion != "" && ref.APIVersion != clusterv1.GroupVersion.String() {
		allErrs = append(allErrs, field.Invalid(clusterRefPath.Child("apiVersion"), ref.APIVersion, fmt.Sprintf("must be %s", clusterv1.GroupVersion.String())))
	}
	if ref.Kind != "" && ref.Kind != "Cluster" {
		allErrs = append(allErrs, field.Invalid(clusterRefPath.Child("kind"), ref.Kind, "must be Cluster"))
	}
	if ref.Namespace != "" && ref.Namespace != newObj.Namespace {
		allErrs = append(allErrs, field.Invalid(clusterRefPath.Child("namespace"), ref.Namespace, "must match pool namespace"))
	}

	// ClusterRef can only be changed when consumerRef (in status) is nil
	if oldObj != nil && oldObj.Spec.ClusterRef.Name != newObj.Spec.ClusterRef.Name && oldObj.Status.ConsumerRef != nil {
		allErrs = append(allErrs, field.Forbidden(clusterRefPath, "cannot change clusterRef while consumerRef is set"))
	}

	return allErrs
}
