/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhooks

import (
	"context"
	"fmt"
	"net/netip"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	infrautilv1 "sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

// +kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1beta1-vspherecluster,mutating=false,failurePolicy=fail,matchPolicy=Equivalent,groups=infrastructure.cluster.x-k8s.io,resources=vsphereclusters,versions=v1beta1,name=validation.vspherecluster.infrastructure.cluster.x-k8s.io,sideEffects=None,admissionReviewVersions=v1beta1

// VSphereCluster validates the control plane load balancer declaration.
type VSphereCluster struct {
	Client client.Client
}

var _ webhook.CustomValidator = &VSphereCluster{}

func (webhook *VSphereCluster) SetupWebhookWithManager(mgr ctrl.Manager) error {
	webhook.Client = mgr.GetClient()
	return ctrl.NewWebhookManagedBy(mgr).
		For(&infrav1.VSphereCluster{}).
		WithValidator(webhook).
		Complete()
}

func (webhook *VSphereCluster) ValidateCreate(ctx context.Context, raw runtime.Object) (admission.Warnings, error) {
	obj, ok := raw.(*infrav1.VSphereCluster)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereCluster but got a %T", raw))
	}
	warnings, allErrs := webhook.validate(ctx, nil, obj)
	return warnings, AggregateObjErrors(obj.GroupVersionKind().GroupKind(), obj.Name, allErrs)
}

func (webhook *VSphereCluster) ValidateUpdate(ctx context.Context, oldRaw runtime.Object, newRaw runtime.Object) (admission.Warnings, error) {
	oldObj, ok := oldRaw.(*infrav1.VSphereCluster)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereCluster but got a %T", oldRaw))
	}
	newObj, ok := newRaw.(*infrav1.VSphereCluster)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a VSphereCluster but got a %T", newRaw))
	}
	warnings, allErrs := webhook.validate(ctx, oldObj, newObj)
	return warnings, AggregateObjErrors(newObj.GroupVersionKind().GroupKind(), newObj.Name, allErrs)
}

func (webhook *VSphereCluster) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (webhook *VSphereCluster) validate(ctx context.Context, oldObj, newObj *infrav1.VSphereCluster) (admission.Warnings, field.ErrorList) {
	var warnings admission.Warnings
	var allErrs field.ErrorList

	// The control plane endpoint is a creation-time decision, so the field never
	// changes after CREATE - not even from absent to set. Everything downstream of
	// it (controlPlaneEndpoint, the apiserver serving certificate SANs, the guest
	// bootstrap VIP, the alive ModuleInfo) is derived while the first control plane
	// node bootstraps and cannot be re-derived in place.
	if oldObj != nil {
		allErrs = append(allErrs, validateControlPlaneLoadBalancerImmutable(oldObj.Spec.ControlPlaneLoadBalancer, newObj.Spec.ControlPlaneLoadBalancer)...)
	}

	lbWarnings, lbErrs := webhook.validateControlPlaneLoadBalancer(ctx, newObj)
	warnings = append(warnings, lbWarnings...)
	allErrs = append(allErrs, lbErrs...)

	return warnings, allErrs
}

// validateControlPlaneLoadBalancerImmutable freezes the field for the life of the
// cluster: it is set at CREATE or never.
func validateControlPlaneLoadBalancerImmutable(oldLB, newLB *infrav1.ControlPlaneLoadBalancer) field.ErrorList {
	lbPath := field.NewPath("spec", "controlPlaneLoadBalancer")

	// An absent field is already a complete endpoint configuration: it means the same
	// as type=external, an endpoint the user provides with no provider-managed VIP
	// (see ControlPlaneLoadBalancer.IsInternal, which reads nil as not-internal). So
	// filling it in later is a change like any other, not a harmless record of the
	// status quo. The endpoint consistency check below pins host/port to
	// spec.controlPlaneEndpoint but not the type, so without this a running cluster
	// could be switched to type=internal at its current address and have alive
	// installed on top of a VIP it does not own.
	if oldLB == nil {
		if newLB == nil {
			return nil
		}
		return field.ErrorList{field.Forbidden(lbPath, "cannot be set after the cluster is created: an absent controlPlaneLoadBalancer already means an external load balancer, and the control plane endpoint is fixed at creation; recreate the cluster to change it")}
	}

	if newLB == nil {
		return field.ErrorList{field.Forbidden(lbPath, "cannot be removed once set")}
	}

	var allErrs field.ErrorList
	if oldLB.Type != newLB.Type {
		allErrs = append(allErrs, field.Forbidden(lbPath.Child("type"), immutableDetail(string(oldLB.Type))))
	}
	if oldLB.Host != newLB.Host {
		allErrs = append(allErrs, field.Forbidden(lbPath.Child("host"), immutableDetail(oldLB.Host)))
	}
	if oldLB.Port != newLB.Port {
		allErrs = append(allErrs, field.Forbidden(lbPath.Child("port"), immutableDetail(fmt.Sprintf("%d", oldLB.Port))))
	}
	if oldLB.VRID != newLB.VRID {
		allErrs = append(allErrs, field.Forbidden(lbPath.Child("vrid"), immutableDetail(fmt.Sprintf("%d", oldLB.VRID))))
	}
	if oldLB.Interface != newLB.Interface {
		allErrs = append(allErrs, field.Forbidden(lbPath.Child("interface"), immutableDetail(oldLB.Interface)))
	}
	return allErrs
}

func immutableDetail(current string) string {
	return fmt.Sprintf("is immutable once controlPlaneLoadBalancer is set (current value: %q); recreate the cluster to change it", current)
}

func (webhook *VSphereCluster) validateControlPlaneLoadBalancer(ctx context.Context, obj *infrav1.VSphereCluster) (admission.Warnings, field.ErrorList) {
	lb := obj.Spec.ControlPlaneLoadBalancer
	if lb == nil {
		return nil, nil
	}

	var warnings admission.Warnings
	var allErrs field.ErrorList
	lbPath := field.NewPath("spec", "controlPlaneLoadBalancer")

	switch lb.Type {
	case infrav1.ControlPlaneLoadBalancerTypeInternal, infrav1.ControlPlaneLoadBalancerTypeExternal, "":
	default:
		allErrs = append(allErrs, field.NotSupported(lbPath.Child("type"), lb.Type,
			[]string{string(infrav1.ControlPlaneLoadBalancerTypeInternal), string(infrav1.ControlPlaneLoadBalancerTypeExternal)}))
	}

	if lb.Host == "" {
		allErrs = append(allErrs, field.Required(lbPath.Child("host"), "must be set"))
	}
	if lb.Port < 1 || lb.Port > 65535 {
		allErrs = append(allErrs, field.Invalid(lbPath.Child("port"), lb.Port, "must be in the range 1-65535"))
	}

	if lb.IsInternal() {
		if lb.Host != "" {
			addr, err := netip.ParseAddr(lb.Host)
			switch {
			case err != nil:
				allErrs = append(allErrs, field.Invalid(lbPath.Child("host"), lb.Host, "must be a valid IP address"))
			case !addr.Is4():
				// IPv6 / dual-stack self-built LB is out of scope for this design.
				allErrs = append(allErrs, field.Invalid(lbPath.Child("host"), lb.Host, "must be an IPv4 address"))
			}
		}
		if lb.VRID < 1 || lb.VRID > 255 {
			allErrs = append(allErrs, field.Invalid(lbPath.Child("vrid"), lb.VRID, "must be in the range 1-255"))
		}
		allErrs = append(allErrs, webhook.validateVIPNotUsedBySlot(ctx, obj)...)
	} else {
		if lb.VRID != 0 {
			warnings = append(warnings, fmt.Sprintf("spec.controlPlaneLoadBalancer.vrid is ignored when type is %q", lb.Type))
		}
		if lb.Interface != "" {
			warnings = append(warnings, fmt.Sprintf("spec.controlPlaneLoadBalancer.interface is ignored when type is %q", lb.Type))
		}
	}

	// The endpoint and the load balancer are two views of the same address: if they
	// diverge, kubeadm and the actual VIP fork and the cluster silently loses its
	// entry point. An empty endpoint is fine — the controller backfills it.
	endpoint := obj.Spec.ControlPlaneEndpoint
	if endpoint.Host != "" || endpoint.Port != 0 {
		endpointPath := field.NewPath("spec", "controlPlaneEndpoint")
		if endpoint.Host != lb.Host {
			allErrs = append(allErrs, field.Invalid(endpointPath.Child("host"), endpoint.Host,
				fmt.Sprintf("must match spec.controlPlaneLoadBalancer.host (%q)", lb.Host)))
		}
		if endpoint.Port != lb.Port {
			allErrs = append(allErrs, field.Invalid(endpointPath.Child("port"), endpoint.Port,
				fmt.Sprintf("must match spec.controlPlaneLoadBalancer.port (%d)", lb.Port)))
		}
	}

	return warnings, allErrs
}

// validateVIPNotUsedBySlot rejects a VIP that is already declared as a node
// address in one of the cluster's machine config pools. Such a VIP cannot work:
// keepalived would add it to an interface that already owns it statically.
//
// On a listing error it fails open and leaves the check to the reconciler's
// defensive validation, matching the posture of the pool webhook.
func (webhook *VSphereCluster) validateVIPNotUsedBySlot(ctx context.Context, obj *infrav1.VSphereCluster) field.ErrorList {
	lb := obj.Spec.ControlPlaneLoadBalancer
	if webhook.Client == nil || lb == nil || lb.Host == "" {
		return nil
	}

	clusterName := obj.Labels[clusterv1.ClusterNameLabel]
	if clusterName == "" {
		clusterName = obj.Name
	}

	pools := &infrav1.VSphereMachineConfigPoolList{}
	if err := webhook.Client.List(ctx, pools, client.InNamespace(obj.Namespace)); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to list VSphereMachineConfigPools for VIP conflict check; deferring to the reconciler")
		return nil
	}

	claim, found := infrautilv1.FindConfigPoolSlotClaimingIP(pools.Items, clusterName, lb.Host)
	if !found {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("spec", "controlPlaneLoadBalancer", "host"),
		lb.Host,
		fmt.Sprintf("conflicts with the address of slot %q in VSphereMachineConfigPool %q", claim.Hostname, claim.PoolName),
	)}
}
