package webhooks

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
)

// +kubebuilder:webhook:verbs=create;update;delete,path=/validate-infrastructure-cluster-x-k8s-io-v1beta1-vspheremachineconfigpool,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=vspheremachineconfigpools,versions=v1beta1,name=validation.vspheremachineconfigpool.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1beta1

type VSphereMachineConfigPool struct {
	Client client.Client
}

var _ webhook.CustomValidator = &VSphereMachineConfigPool{}

func (webhook *VSphereMachineConfigPool) SetupWebhookWithManager(mgr ctrl.Manager) error {
	webhook.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&infrav1.VSphereMachineConfigPool{}).
		WithValidator(webhook).
		Complete()
}

func (webhook *VSphereMachineConfigPool) ValidateCreate(ctx context.Context, raw runtime.Object) (admission.Warnings, error) {
	obj, ok := raw.(*infrav1.VSphereMachineConfigPool)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereMachineConfigPool but got a %T", raw))
	}
	return nil, AggregateObjErrors(obj.GroupVersionKind().GroupKind(), obj.Name, webhook.validate(ctx, nil, obj))
}

func (webhook *VSphereMachineConfigPool) ValidateUpdate(ctx context.Context, oldRaw runtime.Object, newRaw runtime.Object) (admission.Warnings, error) {
	oldObj, ok := oldRaw.(*infrav1.VSphereMachineConfigPool)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereMachineConfigPool but got a %T", oldRaw))
	}
	newObj, ok := newRaw.(*infrav1.VSphereMachineConfigPool)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereMachineConfigPool but got a %T", newRaw))
	}
	return nil, AggregateObjErrors(newObj.GroupVersionKind().GroupKind(), newObj.Name, webhook.validate(ctx, oldObj, newObj))
}

// ValidateDelete fails fast when the pool still has InUse slots, giving the user
// an immediate rejection instead of an object stuck in Terminating behind the
// controller finalizer (which blocks until reclaim completes). Released slots are
// allowed to delete — their reclaim is handled by the finalizer.
func (webhook *VSphereMachineConfigPool) ValidateDelete(_ context.Context, raw runtime.Object) (admission.Warnings, error) {
	obj, ok := raw.(*infrav1.VSphereMachineConfigPool)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereMachineConfigPool but got a %T", raw))
	}
	var inUse []string
	for i := range obj.Status.ConfigStatuses {
		if obj.Status.ConfigStatuses[i].State == infrav1.MachineConfigSlotStateInUse {
			inUse = append(inUse, obj.Status.ConfigStatuses[i].Hostname)
		}
	}
	if len(inUse) > 0 {
		return nil, apierrors.NewForbidden(
			infrav1.GroupVersion.WithResource("vspheremachineconfigpools").GroupResource(),
			obj.Name,
			fmt.Errorf("cannot delete pool while %d slot(s) are still in use: %v", len(inUse), inUse),
		)
	}
	return nil, nil
}

func (webhook *VSphereMachineConfigPool) validate(ctx context.Context, oldObj, newObj *infrav1.VSphereMachineConfigPool) field.ErrorList {
	allErrs := webhook.validateClusterRef(oldObj, newObj)
	allErrs = append(allErrs, webhook.validateSlotHostnames(newObj)...)

	// Shared structural + intra-pool uniqueness validators. These also drive the
	// P1-2 MembersValid / MembersUnique conditions; wiring them here promotes the
	// same checks to a hard admission gate so the two never drift.
	allErrs = append(allErrs, services.ValidateSlotFields(newObj)...)
	allErrs = append(allErrs, services.ValidateHostnameUniqueness(newObj)...)
	allErrs = append(allErrs, services.ValidateIPUniqueness(newObj)...)

	// Cross-pool uniqueness (same Cluster, same namespace).
	allErrs = append(allErrs, webhook.validateCrossPoolUniqueness(ctx, newObj)...)

	// Allocated-slot immutability (update only).
	if oldObj != nil {
		allErrs = append(allErrs, services.ValidateAllocatedSlotsImmutable(oldObj, newObj)...)
	}
	return allErrs
}

// validateCrossPoolUniqueness rejects hostnames or primary IPs that collide with
// another pool bound to the same Cluster in the same namespace. On a listing
// error it fails open (returns no error) and lets the P1-2 MembersUnique
// condition act as the backstop — the same posture the reconciler takes — so a
// transient apiserver hiccup does not block otherwise-valid pool writes,
// including the controller's own disk backfill.
func (webhook *VSphereMachineConfigPool) validateCrossPoolUniqueness(ctx context.Context, pool *infrav1.VSphereMachineConfigPool) field.ErrorList {
	if pool == nil || pool.Spec.ClusterRef.Name == "" || webhook.Client == nil {
		return nil
	}
	others := &infrav1.VSphereMachineConfigPoolList{}
	if err := webhook.Client.List(ctx, others, client.InNamespace(pool.Namespace)); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to list pools for cross-pool uniqueness check; deferring to MembersUnique condition")
		return nil
	}

	var allErrs field.ErrorList
	for _, c := range services.CrossPoolUniquenessConflicts(pool, others.Items) {
		p := field.NewPath("spec", "configs").Index(c.ConfigIndex)
		detail := fmt.Sprintf("%s (also used by pool %s)", c.Value, c.OtherPool)
		switch c.Field {
		case "hostname":
			allErrs = append(allErrs, field.Duplicate(p.Child("hostname"), detail))
		case "ip":
			allErrs = append(allErrs, field.Duplicate(p.Child("network", "primary", "ip"), detail))
		case "ipv6":
			allErrs = append(allErrs, field.Duplicate(p.Child("network", "primary", "ipv6"), detail))
		}
	}
	return allErrs
}

func (webhook *VSphereMachineConfigPool) validateClusterRef(oldObj, newObj *infrav1.VSphereMachineConfigPool) field.ErrorList {
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

func (webhook *VSphereMachineConfigPool) validateSlotHostnames(pool *infrav1.VSphereMachineConfigPool) field.ErrorList {
	var allErrs field.ErrorList
	if pool == nil {
		return allErrs
	}

	for i := range pool.Spec.Configs {
		hostnamePath := field.NewPath("spec", "configs").Index(i).Child("hostname")
		hostname := pool.Spec.Configs[i].Hostname
		for _, err := range validation.IsDNS1123Subdomain(hostname) {
			allErrs = append(allErrs, field.Invalid(hostnamePath, hostname, err))
		}
	}

	return allErrs
}
