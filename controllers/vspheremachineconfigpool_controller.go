/*
Copyright 2025.

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

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta2conditions "sigs.k8s.io/cluster-api/util/conditions/v1beta2"
	"sigs.k8s.io/cluster-api/util/finalizers"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/identity"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/govmomi"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/standby"
)

const (
	// MachineConfigPoolFinalizer allows the reconciler to clean up resources.
	MachineConfigPoolFinalizer = "vspheremachineconfigpool.infrastructure.cluster.x-k8s.io"

	// DefaultReleaseDelayHours is the default time to wait before reclaiming a slot.
	DefaultReleaseDelayHours = 24
)

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vspheremachineconfigpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vspheremachineconfigpools/status,verbs=get;update;patch

// AddVSphereMachineConfigPoolControllerToManager adds a VSphereMachineConfigPool controller to the manager.
func AddVSphereMachineConfigPoolControllerToManager(ctx context.Context, controllerManagerCtx *capvcontext.ControllerManagerContext, mgr manager.Manager, options controller.Options) error {
	reconciler := machineConfigPoolReconciler{
		Client:                   controllerManagerCtx.Client,
		ControllerManagerContext: controllerManagerCtx,
		Recorder:                 mgr.GetEventRecorderFor("vspheremachineconfigpool-controller"),
	}
	predicateLog := ctrl.LoggerFrom(ctx).WithValues("controller", "vspheremachineconfigpool")

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.VSphereMachineConfigPool{}).
		Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(reconciler.clusterToMachineConfigPools)).
		Watches(&infrav1.VSphereCluster{}, handler.EnqueueRequestsFromMapFunc(reconciler.vsphereClusterToMachineConfigPools)).
		WithOptions(options).
		WithEventFilter(predicates.ResourceHasFilterLabel(mgr.GetScheme(), predicateLog, controllerManagerCtx.WatchFilterValue)).
		Complete(standby.WrapClusterNamedReconcilerWithConfigMapDetector(
			mgr.GetAPIReader(),
			"vspheremachineconfigpool",
			func() client.Object { return &infrav1.VSphereMachineConfigPool{} },
			clusterNameFromMachineConfigPool,
			reconciler,
		))
}

func clusterNameFromMachineConfigPool(obj client.Object) string {
	pool := obj.(*infrav1.VSphereMachineConfigPool)
	if pool.Spec.ClusterRef.Name != "" {
		return pool.Spec.ClusterRef.Name
	}
	return standby.ClusterNameFromLabel(obj)
}

// clusterToMachineConfigPools maps a Cluster to the VSphereMachineConfigPools that reference it.
func (r machineConfigPoolReconciler) clusterToMachineConfigPools(ctx context.Context, o client.Object) []reconcile.Request {
	pools := &infrav1.VSphereMachineConfigPoolList{}
	if err := r.Client.List(ctx, pools, client.InNamespace(o.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range pools.Items {
		if pools.Items[i].Spec.ClusterRef.Name == o.GetName() {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&pools.Items[i]),
			})
		}
	}
	return requests
}

// vsphereClusterToMachineConfigPools maps a VSphereCluster to the VSphereMachineConfigPools
// whose ClusterRef points to a Cluster that uses this VSphereCluster as infrastructure.
func (r machineConfigPoolReconciler) vsphereClusterToMachineConfigPools(ctx context.Context, o client.Object) []reconcile.Request {
	// Find the Cluster that owns this VSphereCluster
	clusters := &clusterv1.ClusterList{}
	if err := r.Client.List(ctx, clusters, client.InNamespace(o.GetNamespace())); err != nil {
		return nil
	}
	var ownerClusterName string
	for i := range clusters.Items {
		if clusters.Items[i].Spec.InfrastructureRef != nil && clusters.Items[i].Spec.InfrastructureRef.Name == o.GetName() {
			ownerClusterName = clusters.Items[i].Name
			break
		}
	}
	if ownerClusterName == "" {
		return nil
	}
	return r.clusterToMachineConfigPools(ctx, &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: ownerClusterName, Namespace: o.GetNamespace()},
	})
}

type machineConfigPoolReconciler struct {
	Client                   client.Client
	ControllerManagerContext *capvcontext.ControllerManagerContext
	Recorder                 record.EventRecorder
}

func (r machineConfigPoolReconciler) Reconcile(ctx context.Context, req reconcile.Request) (_ reconcile.Result, reterr error) {
	pool := &infrav1.VSphereMachineConfigPool{}
	if err := r.Client.Get(ctx, req.NamespacedName, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if finalizerAdded, err := finalizers.EnsureFinalizer(ctx, r.Client, pool, MachineConfigPoolFinalizer); err != nil || finalizerAdded {
		return ctrl.Result{}, err
	}

	before := pool.DeepCopy()

	defer func() {
		if err := r.persistPool(ctx, pool, before); err != nil {
			if reterr == nil {
				reterr = errors.Wrapf(err, "failed to persist VSphereMachineConfigPool %s/%s", pool.Namespace, pool.Name)
			}
		}
	}()

	if !pool.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, pool)
	}

	return r.reconcileNormal(ctx, pool)
}

// persistPool writes any spec/finalizer and status changes accumulated on pool
// (relative to before) via Update and Status().Update. Conflicts are returned
// as-is so controller-runtime reschedules reconcile against a fresh object.
// Both writers on this CRD (this controller and vimmachine.persistMachineConfigPoolChanges)
// use Update, so the RV optimistic lock prevents silent overwrites.
func (r machineConfigPoolReconciler) persistPool(ctx context.Context, pool, before *infrav1.VSphereMachineConfigPool) error {
	// Always recompute the Ready summary from the health sub-conditions so it is
	// consistent regardless of which reconcile path returned. SlotAvailable is a
	// capacity signal and is deliberately excluded from Ready.
	setPoolReadySummary(pool)

	specDirty := !reflect.DeepEqual(pool.Spec, before.Spec) ||
		!reflect.DeepEqual(pool.Finalizers, before.Finalizers)
	if specDirty {
		if err := r.Client.Update(ctx, pool); err != nil {
			return err
		}
	}

	// Skip status update if the object is already gone (finalizer removed above,
	// and GC already deleted it).
	if pool.UID == "" {
		return nil
	}

	if !reflect.DeepEqual(pool.Status, before.Status) {
		if err := r.Client.Status().Update(ctx, pool); err != nil {
			return err
		}
	}
	return nil
}

// vcenterParams holds the resolved vCenter connection parameters from the ClusterRef credential chain.
type vcenterParams struct {
	server     string
	thumbprint string
	username   string
	password   string
}

// resolveVCenterParams resolves vCenter connection parameters via the ClusterRef credential chain:
// pool.Spec.ClusterRef → Cluster (same namespace) → VSphereCluster → IdentityRef → credentials.
// It sets ClusterRefReadyCondition and VCenterAvailableCondition on the pool.
func (r machineConfigPoolReconciler) resolveVCenterParams(ctx context.Context, pool *infrav1.VSphereMachineConfigPool) (*vcenterParams, error) {
	log := ctrl.LoggerFrom(ctx)

	// Step 1: Get Cluster (same namespace as pool)
	cluster := &clusterv1.Cluster{}
	clusterKey := client.ObjectKey{
		Namespace: pool.Namespace,
		Name:      pool.Spec.ClusterRef.Name,
	}
	if err := r.Client.Get(ctx, clusterKey, cluster); err != nil {
		conditions.MarkFalse(pool, infrav1.ClusterRefReadyCondition,
			infrav1.ClusterNotFoundReason, clusterv1.ConditionSeverityWarning,
			"Cluster %s/%s not found: %v", clusterKey.Namespace, clusterKey.Name, err)
		setPoolClusterRefReadyV1Beta2False(pool, fmt.Sprintf("Cluster %s/%s not found: %v", clusterKey.Namespace, clusterKey.Name, err))
		return nil, errors.Wrapf(err, "failed to get Cluster %s/%s referenced by VSphereMachineConfigPool %s/%s",
			clusterKey.Namespace, clusterKey.Name, pool.Namespace, pool.Name)
	}

	// Step 2: Get VSphereCluster
	if cluster.Spec.InfrastructureRef == nil {
		conditions.MarkFalse(pool, infrav1.ClusterRefReadyCondition,
			infrav1.VSphereClusterNotFoundReason, clusterv1.ConditionSeverityWarning,
			"Cluster %s/%s has nil InfrastructureRef", cluster.Namespace, cluster.Name)
		setPoolClusterRefReadyV1Beta2False(pool, fmt.Sprintf("Cluster %s/%s has nil InfrastructureRef", cluster.Namespace, cluster.Name))
		return nil, errors.Errorf("Cluster %s/%s has nil InfrastructureRef", cluster.Namespace, cluster.Name)
	}
	vsphereCluster := &infrav1.VSphereCluster{}
	vsphereClusterKey := client.ObjectKey{
		Namespace: cluster.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(ctx, vsphereClusterKey, vsphereCluster); err != nil {
		conditions.MarkFalse(pool, infrav1.ClusterRefReadyCondition,
			infrav1.VSphereClusterNotFoundReason, clusterv1.ConditionSeverityWarning,
			"VSphereCluster %s/%s not found: %v", vsphereClusterKey.Namespace, vsphereClusterKey.Name, err)
		setPoolClusterRefReadyV1Beta2False(pool, fmt.Sprintf("VSphereCluster %s/%s not found: %v", vsphereClusterKey.Namespace, vsphereClusterKey.Name, err))
		return nil, errors.Wrapf(err, "failed to get VSphereCluster %s/%s", vsphereClusterKey.Namespace, vsphereClusterKey.Name)
	}

	conditions.MarkTrue(pool, infrav1.ClusterRefReadyCondition)
	v1beta2conditions.Set(pool, metav1.Condition{
		Type:   infrav1.VSphereMachineConfigPoolClusterRefReadyV1Beta2Condition,
		Status: metav1.ConditionTrue,
		Reason: infrav1.VSphereMachineConfigPoolConditionSatisfiedV1Beta2Reason,
	})

	// Step 3: Resolve credentials
	params := &vcenterParams{
		server:     vsphereCluster.Spec.Server,
		thumbprint: vsphereCluster.Spec.Thumbprint,
		username:   r.ControllerManagerContext.Username,
		password:   r.ControllerManagerContext.Password,
	}

	if vsphereCluster.Spec.IdentityRef != nil {
		creds, err := identity.GetCredentials(ctx, r.Client, vsphereCluster, r.ControllerManagerContext.Namespace)
		if err != nil {
			conditions.MarkFalse(pool, infrav1.VCenterAvailableCondition,
				infrav1.IdentityCredentialsUnavailableReason, clusterv1.ConditionSeverityWarning,
				"Failed to resolve credentials from IdentityRef: %v", err)
			setPoolVCenterAvailableV1Beta2False(pool, fmt.Sprintf("Failed to resolve credentials from IdentityRef: %v", err))
			return nil, errors.Wrap(err, "failed to get credentials from IdentityRef")
		}
		params.username = creds.Username
		params.password = creds.Password
	} else if params.username == "" || params.password == "" {
		conditions.MarkFalse(pool, infrav1.VCenterAvailableCondition,
			infrav1.IdentityCredentialsUnavailableReason, clusterv1.ConditionSeverityWarning,
			"VSphereCluster has no IdentityRef and controller manager credentials are not configured")
		setPoolVCenterAvailableV1Beta2False(pool, "VSphereCluster has no IdentityRef and controller manager credentials are not configured")
		return nil, errors.New("VSphereCluster has no IdentityRef and controller manager credentials are not configured")
	} else {
		log.V(4).Info("VSphereCluster has no IdentityRef, falling back to controller manager credentials")
	}

	conditions.MarkTrue(pool, infrav1.VCenterAvailableCondition)
	v1beta2conditions.Set(pool, metav1.Condition{
		Type:   infrav1.VSphereMachineConfigPoolVCenterAvailableV1Beta2Condition,
		Status: metav1.ConditionTrue,
		Reason: infrav1.VSphereMachineConfigPoolConditionSatisfiedV1Beta2Reason,
	})

	return params, nil
}

// setPoolClusterRefReadyV1Beta2False sets the v1beta2 ClusterRefReady condition False.
func setPoolClusterRefReadyV1Beta2False(pool *infrav1.VSphereMachineConfigPool, msg string) {
	v1beta2conditions.Set(pool, metav1.Condition{
		Type:    infrav1.VSphereMachineConfigPoolClusterRefReadyV1Beta2Condition,
		Status:  metav1.ConditionFalse,
		Reason:  infrav1.VSphereMachineConfigPoolClusterRefNotReadyV1Beta2Reason,
		Message: msg,
	})
}

// setPoolVCenterAvailableV1Beta2False sets the v1beta2 VCenterAvailable condition False.
func setPoolVCenterAvailableV1Beta2False(pool *infrav1.VSphereMachineConfigPool, msg string) {
	v1beta2conditions.Set(pool, metav1.Condition{
		Type:    infrav1.VSphereMachineConfigPoolVCenterAvailableV1Beta2Condition,
		Status:  metav1.ConditionFalse,
		Reason:  infrav1.VSphereMachineConfigPoolVCenterUnavailableV1Beta2Reason,
		Message: msg,
	})
}

func (r machineConfigPoolReconciler) reconcileNormal(ctx context.Context, pool *infrav1.VSphereMachineConfigPool) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Release-1 migration: fold any observed disk state that still lives on spec
	// (or in a legacy per-slot ReclaimStatus) into pool.Status.PersistentDiskStatuses.
	// This is pure status bookkeeping, so it runs before vCenter resolution and
	// still makes progress when vCenter is unreachable.
	services.SeedPersistentDiskStatuses(pool)

	// Resolve vCenter params via ClusterRef chain before any vCenter operations
	vcp, err := r.resolveVCenterParams(ctx, pool)
	if err != nil {
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}

	statusMap := make(map[string]infrav1.MachineConfigSlotStatus)
	for _, s := range pool.Status.ConfigStatuses {
		statusMap[s.Hostname] = s
	}

	newStatuses := []infrav1.MachineConfigSlotStatus{}
	delayHours := DefaultReleaseDelayHours
	if pool.Spec.ReleaseDelayHours != nil {
		delayHours = *pool.Spec.ReleaseDelayHours
	}

	requeueAfter := time.Duration(0)

	r.reconcileConsumerBinding(ctx, pool)

	for i := range pool.Spec.Configs {
		slot := &pool.Spec.Configs[i]
		status, ok := statusMap[slot.Hostname]
		if !ok {
			status = infrav1.MachineConfigSlotStatus{
				Hostname: slot.Hostname,
				State:    infrav1.MachineConfigSlotStateAvailable,
			}
		}

		// 1. Reclamation Check (Released -> Available). Per-disk reclaim progress
		// (task polling, retry backoff) is driven inside reclaimSlotDisks via the
		// disks' status records; here we only gate on the release window.
		if status.State == infrav1.MachineConfigSlotStateReleased && status.LastReleasedTime != nil {
			deadline := status.LastReleasedTime.Add(time.Duration(delayHours) * time.Hour)
			if time.Now().After(deadline) {
				log.Info("Reclaiming stale slot", "hostname", slot.Hostname, "persistentDiskCount", len(slot.PersistentDisks))
				reclaimed, wait, err := r.reclaimSlotDisks(ctx, pool, slot, vcp)
				if err != nil {
					log.Error(err, "failed to reclaim persistent disks for slot", "hostname", slot.Hostname)
				}
				if reclaimed {
					status.State = infrav1.MachineConfigSlotStateAvailable
					status.MachineRef = nil
					status.LastReleasedTime = nil
				}
				if wait > 0 && (requeueAfter == 0 || wait < requeueAfter) {
					requeueAfter = wait
				}
			} else {
				log.Info("Released slot reclaim delayed until release window elapses",
					"hostname", slot.Hostname,
					"wait", time.Until(deadline),
				)
				wait := time.Until(deadline)
				if requeueAfter == 0 || wait < requeueAfter {
					requeueAfter = wait
				}
			}
		}

		// 2. Orphan Check (InUse but Machine is gone)
		if status.State == infrav1.MachineConfigSlotStateInUse && status.MachineRef != nil {
			m := &infrav1.VSphereMachine{}
			err := r.Client.Get(ctx, client.ObjectKey{Namespace: status.MachineRef.Namespace, Name: status.MachineRef.Name}, m)
			if err != nil && apierrors.IsNotFound(err) {
				log.Info("Machine associated with slot no longer exists, moving slot to Released", "hostname", slot.Hostname, "machine", status.MachineRef.Name)
				status.State = infrav1.MachineConfigSlotStateReleased
				now := metav1.Now()
				status.LastReleasedTime = &now
			}
		}

		newStatuses = append(newStatuses, status)
	}

	if !reflect.DeepEqual(pool.Status.ConfigStatuses, newStatuses) {
		pool.Status.ConfigStatuses = newStatuses
	}

	updateSlotCounters(pool, newStatuses)
	r.reconcilePoolHealthConditions(ctx, pool)

	return reconcile.Result{RequeueAfter: requeueAfter}, nil
}

// updateSlotCounters recomputes the pool-level slot counters (Total/Available/Allocated)
// from the per-slot statuses. Total is the number of declared slots; Available counts
// slots free for allocation; Allocated counts slots bound to a machine. Released slots
// (awaiting reclaim) count toward Total but neither Available nor Allocated.
func updateSlotCounters(pool *infrav1.VSphereMachineConfigPool, statuses []infrav1.MachineConfigSlotStatus) {
	var available, allocated int32
	for i := range statuses {
		switch statuses[i].State {
		case infrav1.MachineConfigSlotStateAvailable:
			available++
		case infrav1.MachineConfigSlotStateInUse:
			allocated++
		}
	}
	pool.Status.Total = int32(len(pool.Spec.Configs))
	pool.Status.Available = available
	pool.Status.Allocated = allocated
}

// markPoolConditionTrue sets the given pool health condition True in both the
// v1beta1 and v1beta2 condition sets.
func markPoolConditionTrue(pool *infrav1.VSphereMachineConfigPool, v1b1 clusterv1.ConditionType, v1b2 string) {
	conditions.MarkTrue(pool, v1b1)
	v1beta2conditions.Set(pool, metav1.Condition{
		Type:   v1b2,
		Status: metav1.ConditionTrue,
		Reason: infrav1.VSphereMachineConfigPoolConditionSatisfiedV1Beta2Reason,
	})
}

// markPoolConditionFalse sets the given pool health condition False in both the
// v1beta1 and v1beta2 condition sets.
func markPoolConditionFalse(pool *infrav1.VSphereMachineConfigPool, v1b1 clusterv1.ConditionType, v1b2 string, v1b1Reason string, severity clusterv1.ConditionSeverity, v1b2Reason, msg string) {
	conditions.MarkFalse(pool, v1b1, v1b1Reason, severity, "%s", msg)
	v1beta2conditions.Set(pool, metav1.Condition{
		Type:    v1b2,
		Status:  metav1.ConditionFalse,
		Reason:  v1b2Reason,
		Message: msg,
	})
}

// reconcilePoolHealthConditions computes and dual-writes the pool-level health
// conditions (MembersValid, MembersUnique, SlotAvailable, PersistentDisksReady).
// The Ready summary is computed separately in persistPool. SlotAvailable relies
// on the counters set by updateSlotCounters, so it must run after them.
func (r machineConfigPoolReconciler) reconcilePoolHealthConditions(ctx context.Context, pool *infrav1.VSphereMachineConfigPool) {
	// MembersValid: per-slot structural field validity.
	if fieldErrs := services.ValidateSlotFields(pool); len(fieldErrs) == 0 {
		markPoolConditionTrue(pool, infrav1.MachineConfigPoolMembersValidCondition, infrav1.VSphereMachineConfigPoolMembersValidV1Beta2Condition)
	} else {
		markPoolConditionFalse(pool, infrav1.MachineConfigPoolMembersValidCondition, infrav1.VSphereMachineConfigPoolMembersValidV1Beta2Condition,
			infrav1.MachineConfigPoolInvalidMemberConfigReason, clusterv1.ConditionSeverityWarning,
			infrav1.MachineConfigPoolInvalidMemberConfigReason, fieldErrs.ToAggregate().Error())
	}

	// MembersUnique: hostname takes precedence over IP; within-pool and cross-pool
	// (same clusterRef, same namespace) collisions both count.
	hostnameErrs := services.ValidateHostnameUniqueness(pool)
	ipErrs := services.ValidateIPUniqueness(pool)
	dupHostname, dupIP, crossMsg := r.crossPoolConflicts(ctx, pool)
	switch {
	case len(hostnameErrs) > 0 || dupHostname:
		msg := crossMsg
		if len(hostnameErrs) > 0 {
			msg = hostnameErrs.ToAggregate().Error()
		}
		markPoolConditionFalse(pool, infrav1.MachineConfigPoolMembersUniqueCondition, infrav1.VSphereMachineConfigPoolMembersUniqueV1Beta2Condition,
			infrav1.MachineConfigPoolDuplicateHostnameReason, clusterv1.ConditionSeverityWarning,
			infrav1.MachineConfigPoolDuplicateHostnameReason, msg)
	case len(ipErrs) > 0 || dupIP:
		msg := crossMsg
		if len(ipErrs) > 0 {
			msg = ipErrs.ToAggregate().Error()
		}
		markPoolConditionFalse(pool, infrav1.MachineConfigPoolMembersUniqueCondition, infrav1.VSphereMachineConfigPoolMembersUniqueV1Beta2Condition,
			infrav1.MachineConfigPoolDuplicateIPAddressReason, clusterv1.ConditionSeverityWarning,
			infrav1.MachineConfigPoolDuplicateIPAddressReason, msg)
	default:
		markPoolConditionTrue(pool, infrav1.MachineConfigPoolMembersUniqueCondition, infrav1.VSphereMachineConfigPoolMembersUniqueV1Beta2Condition)
	}

	// SlotAvailable: capacity signal (does not feed Ready).
	if pool.Status.Available > 0 {
		markPoolConditionTrue(pool, infrav1.MachineConfigPoolSlotAvailableCondition, infrav1.VSphereMachineConfigPoolSlotAvailableV1Beta2Condition)
	} else {
		reason := infrav1.MachineConfigPoolAllSlotsInUseReason
		for i := range pool.Status.ConfigStatuses {
			if pool.Status.ConfigStatuses[i].State == infrav1.MachineConfigSlotStateReleased {
				reason = infrav1.MachineConfigPoolWaitingForReclaimReason
				break
			}
		}
		markPoolConditionFalse(pool, infrav1.MachineConfigPoolSlotAvailableCondition, infrav1.VSphereMachineConfigPoolSlotAvailableV1Beta2Condition,
			reason, clusterv1.ConditionSeverityInfo, reason, "no slots are currently available for allocation")
	}

	// PersistentDisksReady: every persistent disk must be in a settled healthy
	// state — idle on an available slot, or fully provisioned on an in-use slot.
	// A disk in the Error phase (failed reclaim) is a hard failure; an in-use slot
	// whose disks are not yet provisioned (no backfilled VolumePath) is still
	// preparing and also counts as not ready. Disks reclaiming normally do not
	// pull this down.
	var failedDisk *infrav1.PersistentDiskStatus
	for i := range pool.Status.PersistentDiskStatuses {
		d := &pool.Status.PersistentDiskStatuses[i]
		if d.Phase == infrav1.PersistentDiskPhaseError {
			failedDisk = d
			break
		}
	}
	switch {
	case failedDisk != nil:
		reason := infrav1.MachineConfigPoolReclaimFailedReason
		if strings.Contains(failedDisk.LastError, "still attached") {
			reason = infrav1.MachineConfigPoolDiskStillAttachedReason
		}
		msg := failedDisk.LastError
		if msg == "" {
			msg = fmt.Sprintf("persistent disk %q reclaim failed for slot %s", failedDisk.Name, failedDisk.Hostname)
		}
		markPoolConditionFalse(pool, infrav1.MachineConfigPoolPersistentDisksReadyCondition, infrav1.VSphereMachineConfigPoolPersistentDisksReadyV1Beta2Condition,
			reason, clusterv1.ConditionSeverityWarning, reason, msg)
	default:
		if host, disk, preparing := poolUnprovisionedInUseDisk(pool); preparing {
			markPoolConditionFalse(pool, infrav1.MachineConfigPoolPersistentDisksReadyCondition, infrav1.VSphereMachineConfigPoolPersistentDisksReadyV1Beta2Condition,
				infrav1.MachineConfigPoolDisksProvisioningReason, clusterv1.ConditionSeverityInfo,
				infrav1.MachineConfigPoolDisksProvisioningReason,
				fmt.Sprintf("persistent disk %q for in-use slot %s is still being provisioned", disk, host))
		} else {
			markPoolConditionTrue(pool, infrav1.MachineConfigPoolPersistentDisksReadyCondition, infrav1.VSphereMachineConfigPoolPersistentDisksReadyV1Beta2Condition)
		}
	}
}

// poolUnprovisionedInUseDisk returns the first in-use slot that still has a
// persistent disk without a backfilled VolumePath (i.e. not yet created in
// vCenter). The observed VolumePath lives in status; spec's frozen value is a
// fallback for un-seeded legacy objects. Available and reclaiming slots are
// ignored — only in-use slots are expected to have provisioned disks.
func poolUnprovisionedInUseDisk(pool *infrav1.VSphereMachineConfigPool) (hostname, disk string, preparing bool) {
	inUse := map[string]struct{}{}
	for i := range pool.Status.ConfigStatuses {
		if pool.Status.ConfigStatuses[i].State == infrav1.MachineConfigSlotStateInUse {
			inUse[pool.Status.ConfigStatuses[i].Hostname] = struct{}{}
		}
	}
	for i := range pool.Spec.Configs {
		cfg := &pool.Spec.Configs[i]
		if _, ok := inUse[cfg.Hostname]; !ok {
			continue
		}
		for j := range cfg.PersistentDisks {
			if services.ObservedVolumePath(pool, cfg.Hostname, cfg.PersistentDisks[j].Name, cfg.PersistentDisks[j].VolumePath) == "" {
				return cfg.Hostname, cfg.PersistentDisks[j].Name, true
			}
		}
	}
	return "", "", false
}

// crossPoolConflicts reports whether this pool's hostnames or primary IPs collide
// with any other pool bound to the same Cluster in the same namespace. Within-pool
// duplicates are handled by the shared validators; this covers only cross-pool.
func (r machineConfigPoolReconciler) crossPoolConflicts(ctx context.Context, pool *infrav1.VSphereMachineConfigPool) (dupHostname bool, dupIP bool, msg string) {
	if pool.Spec.ClusterRef.Name == "" {
		return false, false, ""
	}
	others := &infrav1.VSphereMachineConfigPoolList{}
	if err := r.Client.List(ctx, others, client.InNamespace(pool.Namespace)); err != nil {
		// Treat a listing failure as "no conflict observed" — MembersUnique is a
		// best-effort backstop for the webhook, not a hard gate.
		ctrl.LoggerFrom(ctx).Error(err, "failed to list pools for cross-pool uniqueness check")
		return false, false, ""
	}

	// Hostname collisions take precedence over IP for the condition reason.
	conflicts := services.CrossPoolUniquenessConflicts(pool, others.Items)
	for _, c := range conflicts {
		if c.Field == "hostname" {
			return true, dupIP, "hostname " + c.Value + " also used by pool " + c.OtherPool
		}
	}
	for _, c := range conflicts {
		dupIP = true
		msg = "primary IP " + c.Value + " also used by pool " + c.OtherPool
	}
	return dupHostname, dupIP, msg
}

// setPoolReadySummary computes the pool's Ready condition (v1beta1 and v1beta2)
// from the health sub-conditions. SlotAvailable is excluded — a fully-allocated
// fixed-IP pool is a healthy state.
func setPoolReadySummary(pool *infrav1.VSphereMachineConfigPool) {
	conditions.SetSummary(pool, conditions.WithConditions(
		infrav1.ClusterRefReadyCondition,
		infrav1.VCenterAvailableCondition,
		infrav1.MachineConfigPoolMembersValidCondition,
		infrav1.MachineConfigPoolMembersUniqueCondition,
		infrav1.MachineConfigPoolPersistentDisksReadyCondition,
	))

	_ = v1beta2conditions.SetSummaryCondition(pool, pool, infrav1.VSphereMachineConfigPoolReadyV1Beta2Condition,
		v1beta2conditions.ForConditionTypes{
			infrav1.VSphereMachineConfigPoolClusterRefReadyV1Beta2Condition,
			infrav1.VSphereMachineConfigPoolVCenterAvailableV1Beta2Condition,
			infrav1.VSphereMachineConfigPoolMembersValidV1Beta2Condition,
			infrav1.VSphereMachineConfigPoolMembersUniqueV1Beta2Condition,
			infrav1.VSphereMachineConfigPoolPersistentDisksReadyV1Beta2Condition,
		},
		// These may not be set yet on an early return (before vCenter resolves or
		// before the first health pass); ignore them rather than forcing Unknown.
		v1beta2conditions.IgnoreTypesIfMissing{
			infrav1.VSphereMachineConfigPoolVCenterAvailableV1Beta2Condition,
			infrav1.VSphereMachineConfigPoolMembersValidV1Beta2Condition,
			infrav1.VSphereMachineConfigPoolMembersUniqueV1Beta2Condition,
			infrav1.VSphereMachineConfigPoolPersistentDisksReadyV1Beta2Condition,
		},
		v1beta2conditions.CustomMergeStrategy{
			MergeStrategy: v1beta2conditions.DefaultMergeStrategy(
				v1beta2conditions.ComputeReasonFunc(v1beta2conditions.GetDefaultComputeMergeReasonFunc(
					infrav1.VSphereMachineConfigPoolNotReadyV1Beta2Reason,
					infrav1.VSphereMachineConfigPoolReadyUnknownV1Beta2Reason,
					infrav1.VSphereMachineConfigPoolReadyV1Beta2Reason,
				)),
			),
		},
	)
}

func (r machineConfigPoolReconciler) reconcileConsumerBinding(ctx context.Context, pool *infrav1.VSphereMachineConfigPool) {
	if pool.Status.ConsumerRef == nil {
		return
	}
	if !services.IsPoolFullyReusable(pool) {
		return
	}

	previous := pool.Status.ConsumerRef
	ctrl.LoggerFrom(ctx).Info("Pool is fully reusable, clearing consumer binding",
		"previousConsumerKind", previous.Kind,
		"previousConsumerNamespace", previous.Namespace,
		"previousConsumerName", previous.Name,
	)
	r.Recorder.Eventf(pool, corev1.EventTypeNormal, "ConsumerUnbound",
		"Pool is fully reusable, cleared consumer binding to %s %s/%s",
		previous.Kind, previous.Namespace, previous.Name)
	pool.Status.ConsumerRef = nil
}

// reclaimSlotDisks drives reclamation of a released slot's persistent disks,
// tracking per-disk progress in pool.Status.PersistentDiskStatuses (Phase,
// TaskRef, RetryAfter, LastError). It processes every disk of the slot, deleting
// or polling as needed, and returns reclaimed=true only once the slot has no
// remaining reclaimable backing. wait is the shortest requeue any disk requested.
func (r machineConfigPoolReconciler) reclaimSlotDisks(ctx context.Context, pool *infrav1.VSphereMachineConfigPool, slot *infrav1.MachineConfigSlot, vcp *vcenterParams) (bool, time.Duration, error) {
	log := ctrl.LoggerFrom(ctx)
	slotDatacenter := services.ResolveMachineConfigPoolDatacenter(pool, slot)
	if slotDatacenter == "" {
		return false, 0, errors.Errorf("datacenter must be specified on slot %q or VSphereMachineConfigPool %s/%s for resource reclamation", slot.Hostname, pool.Namespace, pool.Name)
	}

	params := session.NewParams().
		WithUserInfo(vcp.username, vcp.password).
		WithServer(vcp.server).
		WithThumbprint(vcp.thumbprint).
		WithDatacenter(slotDatacenter)
	s, err := session.GetOrCreate(ctx, params)
	if err != nil {
		return false, 0, err
	}

	requeue := time.Duration(0)
	accumulate := func(w time.Duration) {
		if w > 0 && (requeue == 0 || w < requeue) {
			requeue = w
		}
	}

	for i := range slot.PersistentDisks {
		pd := &slot.PersistentDisks[i]
		rec, _ := infrav1.FindDiskStatus(pool, slot.Hostname, pd.Name)
		volumePath := services.ObservedVolumePath(pool, slot.Hostname, pd.Name, pd.VolumePath)

		// Already reclaimed: the backing is gone and the record is kept as a
		// tombstone so seed does not re-create it from spec's frozen VolumePath and
		// restart reclaim. Nothing more to do.
		if rec != nil && rec.Phase == infrav1.PersistentDiskPhaseReclaimed {
			continue
		}

		// In-flight reclaim task: poll it.
		if rec != nil && rec.TaskRef != "" {
			wait, err := r.pollReclaimTask(ctx, s, pool, slot, pd.Name, rec)
			if err != nil {
				return false, 0, err
			}
			accumulate(wait)
			continue
		}

		if volumePath == "" {
			// Nothing to reclaim; drop any stale record.
			if rec != nil {
				infrav1.RemoveDiskStatus(pool, slot.Hostname, pd.Name)
			}
			continue
		}

		// Backoff after a prior failure.
		if rec != nil && rec.Phase == infrav1.PersistentDiskPhaseError && rec.RetryAfter != nil && time.Now().Before(rec.RetryAfter.Time) {
			accumulate(time.Until(rec.RetryAfter.Time))
			continue
		}

		// Safety: never reclaim a disk still attached to a VM.
		attachments, err := govmomi.FindAttachedPersistentDisks(ctx, s, slotDatacenter, []infrav1.PersistentDisk{{Name: pd.Name, VolumePath: volumePath}})
		if err != nil {
			return false, 0, errors.Wrapf(err, "failed to check persistent disk attachments before reclaiming %s", volumePath)
		}
		if len(attachments) > 0 {
			attachmentText := formatPersistentDiskAttachments(attachments)
			log.Info("Waiting to reclaim persistent disk backing because it is still attached", "hostname", slot.Hostname, "disk", pd.Name, "path", volumePath, "attachments", attachmentText)
			if r.Recorder != nil {
				r.Recorder.Eventf(pool, corev1.EventTypeWarning, "PersistentDiskStillAttached", "Waiting to reclaim persistent disk backing %s because it is still attached: %v", volumePath, attachmentText)
			}
			retryAfter := metav1.NewTime(time.Now().Add(30 * time.Second))
			rs := diskRecordBase(pool, slot.Hostname, pd, volumePath)
			rs.Phase = infrav1.PersistentDiskPhaseError
			rs.RetryAfter = &retryAfter
			rs.LastError = fmt.Sprintf("persistent disk is still attached: %v", attachmentText)
			infrav1.UpsertDiskStatus(pool, rs)
			accumulate(time.Until(retryAfter.Time))
			continue
		}

		// Start deletion.
		log.Info("Deleting persistent disk backing for released slot", "hostname", slot.Hostname, "disk", pd.Name, "path", volumePath)
		m := object.NewFileManager(s.Client.Client)
		dc, err := s.Finder.Datacenter(ctx, slotDatacenter)
		if err != nil {
			return false, 0, errors.Wrapf(err, "failed to find datacenter %s for reclamation", slotDatacenter)
		}
		task, err := m.DeleteDatastoreFile(ctx, volumePath, dc)
		if err != nil {
			if types.IsFileNotFound(err) {
				log.Info("Datastore file already gone, treating as reclaimed", "hostname", slot.Hostname, "disk", pd.Name, "path", volumePath)
				infrav1.TombstoneDiskStatus(pool, slot.Hostname, pd.Name)
				accumulate(1 * time.Second)
				continue
			}
			log.Error(err, "Failed to start datastore file deletion", "hostname", slot.Hostname, "disk", pd.Name, "path", volumePath)
			return false, 0, err
		}
		log.Info("Started datastore file deletion task", "hostname", slot.Hostname, "disk", pd.Name, "path", volumePath, "task", task.Reference().Value)
		rs := diskRecordBase(pool, slot.Hostname, pd, volumePath)
		rs.Phase = infrav1.PersistentDiskPhaseReclaiming
		rs.TaskRef = task.Reference().Value
		infrav1.UpsertDiskStatus(pool, rs)
		accumulate(15 * time.Second)
	}

	return !services.HasReclaimablePersistentDiskBacking(pool, slot), requeue, nil
}

// pollReclaimTask checks the in-flight vCenter delete task recorded on rec and
// advances the disk's status: on success it tombstones the record as Reclaimed,
// on failure it moves the disk to Error with a retry window.
func (r machineConfigPoolReconciler) pollReclaimTask(ctx context.Context, s *session.Session, pool *infrav1.VSphereMachineConfigPool, slot *infrav1.MachineConfigSlot, diskName string, rec *infrav1.PersistentDiskStatus) (time.Duration, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("hostname", slot.Hostname, "disk", diskName, "task", rec.TaskRef)

	task := &mo.Task{}
	taskRef := types.ManagedObjectReference{Type: "Task", Value: rec.TaskRef}
	if err := s.RetrieveOne(ctx, taskRef, []string{"info"}, task); err != nil {
		return 0, err
	}

	switch task.Info.State {
	case types.TaskInfoStateQueued, types.TaskInfoStateRunning:
		log.Info("Reclaim task still in progress", "taskState", task.Info.State)
		return 15 * time.Second, nil
	case types.TaskInfoStateSuccess:
		log.Info("Reclaim task completed, marking disk reclaimed")
		infrav1.TombstoneDiskStatus(pool, slot.Hostname, diskName)
		return 1 * time.Second, nil
	case types.TaskInfoStateError:
		if task.Info.Error != nil {
			if _, ok := task.Info.Error.Fault.(*types.FileNotFound); ok {
				log.Info("Reclaim task reported file not found, treating as reclaimed")
				infrav1.TombstoneDiskStatus(pool, slot.Hostname, diskName)
				return 1 * time.Second, nil
			}
		}
		errMessage := "reclaim task failed"
		if task.Info.Error != nil {
			errMessage = task.Info.Error.LocalizedMessage
		}
		log.Error(errors.New(errMessage), "Reclaim task failed")
		retryAfter := metav1.NewTime(time.Now().Add(1 * time.Minute))
		updated := *rec
		updated.Phase = infrav1.PersistentDiskPhaseError
		updated.TaskRef = ""
		updated.RetryAfter = &retryAfter
		updated.LastError = errMessage
		infrav1.UpsertDiskStatus(pool, updated)
		return time.Until(retryAfter.Time), nil
	default:
		return 0, errors.Errorf("unknown reclaim task state %q", task.Info.State)
	}
}

// diskRecordBase returns a status record for (hostname, disk) carrying forward
// the observed identity fields (DiskUUID/UnitNumber) and ownership from any
// existing record, so a reclaim-state update does not lose them.
func diskRecordBase(pool *infrav1.VSphereMachineConfigPool, hostname string, pd *infrav1.PersistentDisk, volumePath string) infrav1.PersistentDiskStatus {
	rec := infrav1.PersistentDiskStatus{Hostname: hostname, Name: pd.Name, VolumePath: volumePath}
	if existing, _ := infrav1.FindDiskStatus(pool, hostname, pd.Name); existing != nil {
		rec.DiskUUID = existing.DiskUUID
		rec.UnitNumber = existing.UnitNumber
		rec.OwnerMachineName = existing.OwnerMachineName
		rec.OwnerMachineUID = existing.OwnerMachineUID
	}
	return rec
}

func (r machineConfigPoolReconciler) reconcileDelete(ctx context.Context, pool *infrav1.VSphereMachineConfigPool) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Release-1 migration: fold any legacy spec/ReclaimStatus disk state into
	// pool.Status.PersistentDiskStatuses before reclaiming against it.
	services.SeedPersistentDiskStatuses(pool)

	// Check if any vCenter operations are needed (any slot with reclaimable disks).
	needsVCenter := false
	for i := range pool.Spec.Configs {
		slot := &pool.Spec.Configs[i]
		if services.HasReclaimablePersistentDiskBacking(pool, slot) {
			needsVCenter = true
			break
		}
	}

	var vcp *vcenterParams
	if needsVCenter {
		var err error
		vcp, err = r.resolveVCenterParams(ctx, pool)
		if err != nil {
			log.Error(err, "Cannot resolve vCenter credentials for deletion reclaim, will retry")
			return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}

	statusMap := make(map[string]infrav1.MachineConfigSlotStatus, len(pool.Status.ConfigStatuses))
	for _, status := range pool.Status.ConfigStatuses {
		statusMap[status.Hostname] = status
	}

	newStatuses := make([]infrav1.MachineConfigSlotStatus, 0, len(pool.Spec.Configs))
	var blockingMachines []string
	requeueAfter := time.Duration(0)

	for i := range pool.Spec.Configs {
		slot := &pool.Spec.Configs[i]
		status, ok := statusMap[slot.Hostname]
		if !ok {
			status = infrav1.MachineConfigSlotStatus{
				Hostname: slot.Hostname,
				State:    infrav1.MachineConfigSlotStateAvailable,
			}
		}

		machinePresent := false
		if status.MachineRef != nil {
			machine := &infrav1.VSphereMachine{}
			err := r.Client.Get(ctx, client.ObjectKey{Namespace: status.MachineRef.Namespace, Name: status.MachineRef.Name}, machine)
			switch {
			case err == nil:
				machinePresent = true
				blockingMachines = append(blockingMachines, status.MachineRef.Namespace+"/"+status.MachineRef.Name)
			case apierrors.IsNotFound(err):
				log.Info("Deleting pool: machine for slot no longer exists, continuing reclaim", "hostname", slot.Hostname, "machine", status.MachineRef.Name)
				status.MachineRef = nil
				if status.State == infrav1.MachineConfigSlotStateInUse {
					status.State = infrav1.MachineConfigSlotStateReleased
					now := metav1.Now()
					status.LastReleasedTime = &now
				}
			default:
				return reconcile.Result{}, err
			}
		}

		// A slot with no live machine is eligible for reclaim regardless of its
		// previous state. In particular, an Available slot can still carry a
		// persistent-disk VolumePath after an interrupted or legacy reconcile.
		if !machinePresent && status.State == infrav1.MachineConfigSlotStateInUse {
			status.State = infrav1.MachineConfigSlotStateReleased
			if status.LastReleasedTime == nil {
				now := metav1.Now()
				status.LastReleasedTime = &now
			}
		}

		if !machinePresent && status.State != infrav1.MachineConfigSlotStateInUse {
			if services.HasReclaimablePersistentDiskBacking(pool, slot) {
				log.Info("Deleting pool: reclaiming slot's persistent disks",
					"hostname", slot.Hostname,
					"persistentDiskCount", len(slot.PersistentDisks),
				)
				reclaimed, wait, err := r.reclaimSlotDisks(ctx, pool, slot, vcp)
				if err != nil {
					return reconcile.Result{}, err
				}
				if reclaimed {
					log.Info("Deleting pool: slot reclaim completed", "hostname", slot.Hostname)
					status.State = infrav1.MachineConfigSlotStateAvailable
					status.LastReleasedTime = nil
				}
				if wait > 0 && (requeueAfter == 0 || wait < requeueAfter) {
					requeueAfter = wait
				}
			} else {
				log.Info("Deleting pool: slot has no reclaimable persistent disks, marking available",
					"hostname", slot.Hostname,
					"persistentDiskCount", len(slot.PersistentDisks),
				)
				status.State = infrav1.MachineConfigSlotStateAvailable
				status.LastReleasedTime = nil
			}
		}

		newStatuses = append(newStatuses, status)
	}

	pool.Status.ConfigStatuses = newStatuses
	updateSlotCounters(pool, newStatuses)

	if len(blockingMachines) > 0 {
		return reconcile.Result{}, errors.Errorf("blocking VSphereMachineConfigPool deletion: currently in use by VSphereMachines %v", blockingMachines)
	}

	if requeueAfter > 0 {
		return reconcile.Result{RequeueAfter: requeueAfter}, nil
	}

	ctrlutil.RemoveFinalizer(pool, MachineConfigPoolFinalizer)
	return reconcile.Result{}, nil
}
