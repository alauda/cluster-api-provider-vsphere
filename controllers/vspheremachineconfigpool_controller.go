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
	"reflect"
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
		return nil, errors.Wrapf(err, "failed to get Cluster %s/%s referenced by VSphereMachineConfigPool %s/%s",
			clusterKey.Namespace, clusterKey.Name, pool.Namespace, pool.Name)
	}

	// Step 2: Get VSphereCluster
	if cluster.Spec.InfrastructureRef == nil {
		conditions.MarkFalse(pool, infrav1.ClusterRefReadyCondition,
			infrav1.VSphereClusterNotFoundReason, clusterv1.ConditionSeverityWarning,
			"Cluster %s/%s has nil InfrastructureRef", cluster.Namespace, cluster.Name)
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
		return nil, errors.Wrapf(err, "failed to get VSphereCluster %s/%s", vsphereClusterKey.Namespace, vsphereClusterKey.Name)
	}

	conditions.MarkTrue(pool, infrav1.ClusterRefReadyCondition)

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
			return nil, errors.Wrap(err, "failed to get credentials from IdentityRef")
		}
		params.username = creds.Username
		params.password = creds.Password
	} else if params.username == "" || params.password == "" {
		conditions.MarkFalse(pool, infrav1.VCenterAvailableCondition,
			infrav1.IdentityCredentialsUnavailableReason, clusterv1.ConditionSeverityWarning,
			"VSphereCluster has no IdentityRef and controller manager credentials are not configured")
		return nil, errors.New("VSphereCluster has no IdentityRef and controller manager credentials are not configured")
	} else {
		log.V(4).Info("VSphereCluster has no IdentityRef, falling back to controller manager credentials")
	}

	conditions.MarkTrue(pool, infrav1.VCenterAvailableCondition)

	return params, nil
}

func (r machineConfigPoolReconciler) reconcileNormal(ctx context.Context, pool *infrav1.VSphereMachineConfigPool) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

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

		// 1. Reclamation Check (Released -> Available)
		if status.State == infrav1.MachineConfigSlotStateReleased && status.LastReleasedTime != nil {
			deadline := status.LastReleasedTime.Add(time.Duration(delayHours) * time.Hour)
			log.Info("Evaluating released slot for reclamation",
				"hostname", slot.Hostname,
				"delayHours", delayHours,
				"releasedAt", status.LastReleasedTime.Time,
				"deadline", deadline,
				"persistentDiskCount", len(slot.PersistentDisks),
				"hasReclaimTask", status.ReclaimStatus != nil && status.ReclaimStatus.TaskRef != "",
			)
			if status.ReclaimStatus != nil && status.ReclaimStatus.TaskRef != "" {
				_, wait, err := r.reconcileReclaimTask(ctx, pool, slot, &status, vcp)
				if err != nil {
					log.Error(err, "failed to reconcile reclaim task for slot", "hostname", slot.Hostname, "task", status.ReclaimStatus.TaskRef)
				}
				if wait > 0 && (requeueAfter == 0 || wait < requeueAfter) {
					requeueAfter = wait
				}
			} else if status.ReclaimStatus != nil && status.ReclaimStatus.RetryAfter != nil && time.Now().Before(status.ReclaimStatus.RetryAfter.Time) {
				log.Info("Released slot reclaim is waiting for retry window",
					"hostname", slot.Hostname,
					"retryAfter", status.ReclaimStatus.RetryAfter.Time,
					"lastReclaimError", status.ReclaimStatus.LastError,
				)
				wait := time.Until(status.ReclaimStatus.RetryAfter.Time)
				if requeueAfter == 0 || wait < requeueAfter {
					requeueAfter = wait
				}
			} else if time.Now().After(deadline) {
				log.Info("Reclaiming stale slot", "hostname", slot.Hostname)
				reclaimed, wait, err := r.reclaimPhysicalResources(ctx, pool, slot, &status, vcp)
				if err != nil {
					log.Error(err, "failed to reclaim physical resources for slot", "hostname", slot.Hostname)
				}
				if reclaimed {
					status.State = infrav1.MachineConfigSlotStateAvailable
					status.MachineRef = nil
					status.LastReleasedTime = nil
					status.ReclaimStatus = &infrav1.MachineConfigSlotReclaimStatus{State: infrav1.MachineConfigSlotReclaimStateCompleted}
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

	return reconcile.Result{RequeueAfter: requeueAfter}, nil
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

func (r machineConfigPoolReconciler) reclaimPhysicalResources(ctx context.Context, pool *infrav1.VSphereMachineConfigPool, slot *infrav1.MachineConfigSlot, status *infrav1.MachineConfigSlotStatus, vcp *vcenterParams) (bool, time.Duration, error) {
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

	for i := range slot.PersistentDisks {
		pd := &slot.PersistentDisks[i]
		if pd.VolumePath != "" {
			attachments, err := govmomi.FindAttachedPersistentDisks(ctx, s, slotDatacenter, []infrav1.PersistentDisk{*pd})
			if err != nil {
				return false, 0, errors.Wrapf(err, "failed to check persistent disk attachments before reclaiming %s", pd.VolumePath)
			}
			if len(attachments) > 0 {
				attachmentText := formatPersistentDiskAttachments(attachments)
				log.Info("Waiting to reclaim persistent disk backing because it is still attached", "hostname", slot.Hostname, "disk", pd.Name, "path", pd.VolumePath, "attachments", attachmentText)
				if r.Recorder != nil {
					r.Recorder.Eventf(pool, corev1.EventTypeWarning, "PersistentDiskStillAttached", "Waiting to reclaim persistent disk backing %s because it is still attached: %v", pd.VolumePath, attachmentText)
				}
				retryAfter := metav1.NewTime(time.Now().Add(30 * time.Second))
				status.ReclaimStatus = &infrav1.MachineConfigSlotReclaimStatus{
					State:      infrav1.MachineConfigSlotReclaimStateFailed,
					RetryAfter: &retryAfter,
					LastError:  errors.Errorf("persistent disk is still attached: %v", attachmentText).Error(),
					VolumePath: pd.VolumePath,
				}
				return false, time.Until(retryAfter.Time), nil
			}

			log.Info("Deleting persistent disk backing for released slot",
				"hostname", slot.Hostname,
				"disk", pd.Name,
				"path", pd.VolumePath,
				"diskUUID", pd.DiskUUID,
				"unitNumber", pd.UnitNumber,
			)
			m := object.NewFileManager(s.Client.Client)
			dc, err := s.Finder.Datacenter(ctx, slotDatacenter)
			if err != nil {
				return false, 0, errors.Wrapf(err, "failed to find datacenter %s for reclamation", slotDatacenter)
			}

			task, err := m.DeleteDatastoreFile(ctx, pd.VolumePath, dc)
			if err != nil {
				if types.IsFileNotFound(err) {
					log.Info("Datastore file already gone, treating as reclaimed",
						"hostname", slot.Hostname,
						"disk", pd.Name,
						"path", pd.VolumePath,
					)
					pd.VolumePath = ""
					pd.DiskUUID = ""
					status.ReclaimStatus = &infrav1.MachineConfigSlotReclaimStatus{State: infrav1.MachineConfigSlotReclaimStateCompleted}
					return false, 1 * time.Second, nil
				}
				log.Error(err, "Failed to start datastore file deletion", "hostname", slot.Hostname, "disk", pd.Name, "path", pd.VolumePath)
				return false, 0, err
			}
			status.ReclaimStatus = &infrav1.MachineConfigSlotReclaimStatus{
				TaskRef:    task.Reference().Value,
				State:      infrav1.MachineConfigSlotReclaimStateRunning,
				VolumePath: pd.VolumePath,
			}
			log.Info("Started datastore file deletion task",
				"hostname", slot.Hostname,
				"disk", pd.Name,
				"path", pd.VolumePath,
				"task", status.ReclaimStatus.TaskRef,
			)
			return false, 15 * time.Second, nil
		}
	}

	log.Info("Released slot has no reclaimable persistent disk backing",
		"hostname", slot.Hostname,
		"persistentDiskCount", len(slot.PersistentDisks),
	)
	return true, 0, nil
}

func (r machineConfigPoolReconciler) reconcileReclaimTask(ctx context.Context, pool *infrav1.VSphereMachineConfigPool, slot *infrav1.MachineConfigSlot, status *infrav1.MachineConfigSlotStatus, vcp *vcenterParams) (bool, time.Duration, error) {
	log := ctrl.LoggerFrom(ctx)
	slotDatacenter := services.ResolveMachineConfigPoolDatacenter(pool, slot)

	params := session.NewParams().
		WithUserInfo(vcp.username, vcp.password).
		WithServer(vcp.server).
		WithThumbprint(vcp.thumbprint).
		WithDatacenter(slotDatacenter)

	s, err := session.GetOrCreate(ctx, params)
	if err != nil {
		return false, 0, err
	}

	task := &mo.Task{}
	if status.ReclaimStatus == nil || status.ReclaimStatus.TaskRef == "" {
		return false, 0, nil
	}

	taskRef := types.ManagedObjectReference{Type: "Task", Value: status.ReclaimStatus.TaskRef}
	if err := s.RetrieveOne(ctx, taskRef, []string{"info"}, task); err != nil {
		return false, 0, err
	}

	log = log.WithValues("hostname", slot.Hostname, "task", status.ReclaimStatus.TaskRef, "taskState", task.Info.State, "path", status.ReclaimStatus.VolumePath)
	ctx = ctrl.LoggerInto(ctx, log)

	switch task.Info.State {
	case types.TaskInfoStateQueued, types.TaskInfoStateRunning:
		log.Info("Reclaim task still in progress")
		return false, 15 * time.Second, nil
	case types.TaskInfoStateSuccess:
		log.Info("Reclaim task completed, clearing reclaimed disk metadata from slot")
		specUpdated := false
		for i := range slot.PersistentDisks {
			if slot.PersistentDisks[i].VolumePath == status.ReclaimStatus.VolumePath {
				log.Info("Clearing reclaimed persistent disk metadata",
					"hostname", slot.Hostname,
					"disk", slot.PersistentDisks[i].Name,
					"path", slot.PersistentDisks[i].VolumePath,
					"diskUUID", slot.PersistentDisks[i].DiskUUID,
				)
				slot.PersistentDisks[i].VolumePath = ""
				slot.PersistentDisks[i].DiskUUID = ""
				specUpdated = true
				break
			}
		}
		status.ReclaimStatus = &infrav1.MachineConfigSlotReclaimStatus{State: infrav1.MachineConfigSlotReclaimStateCompleted}
		return specUpdated, 1 * time.Second, nil
	case types.TaskInfoStateError:
		if task.Info.Error != nil {
			if _, ok := task.Info.Error.Fault.(*types.FileNotFound); ok {
				log.Info("Reclaim task reported file not found, treating as reclaimed")
				specUpdated := false
				for i := range slot.PersistentDisks {
					if slot.PersistentDisks[i].VolumePath == status.ReclaimStatus.VolumePath {
						slot.PersistentDisks[i].VolumePath = ""
						slot.PersistentDisks[i].DiskUUID = ""
						specUpdated = true
						break
					}
				}
				status.ReclaimStatus = &infrav1.MachineConfigSlotReclaimStatus{State: infrav1.MachineConfigSlotReclaimStateCompleted}
				return specUpdated, 1 * time.Second, nil
			}
		}
		errMessage := "reclaim task failed"
		if task.Info.Error != nil {
			errMessage = task.Info.Error.LocalizedMessage
		}
		log.Error(errors.New(errMessage), "Reclaim task failed")
		retryAfter := metav1.NewTime(time.Now().Add(1 * time.Minute))
		status.ReclaimStatus = &infrav1.MachineConfigSlotReclaimStatus{
			State:      infrav1.MachineConfigSlotReclaimStateFailed,
			RetryAfter: &retryAfter,
			LastError:  errMessage,
		}
		return false, time.Until(retryAfter.Time), nil
	default:
		return false, 0, errors.Errorf("unknown reclaim task state %q", task.Info.State)
	}
}

func (r machineConfigPoolReconciler) reconcileDelete(ctx context.Context, pool *infrav1.VSphereMachineConfigPool) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Check if any vCenter operations are needed (released slots with reclaimable disks)
	needsVCenter := false
	for i := range pool.Spec.Configs {
		slot := &pool.Spec.Configs[i]
		for _, s := range pool.Status.ConfigStatuses {
			if s.Hostname == slot.Hostname && s.State == infrav1.MachineConfigSlotStateReleased {
				if (s.ReclaimStatus != nil && s.ReclaimStatus.TaskRef != "") || hasReclaimablePersistentDisk(slot) {
					needsVCenter = true
					break
				}
			}
		}
		if needsVCenter {
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

		if status.MachineRef != nil {
			machine := &infrav1.VSphereMachine{}
			err := r.Client.Get(ctx, client.ObjectKey{Namespace: status.MachineRef.Namespace, Name: status.MachineRef.Name}, machine)
			switch {
			case err == nil:
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

		if status.State == infrav1.MachineConfigSlotStateReleased {
			if status.ReclaimStatus != nil && status.ReclaimStatus.TaskRef != "" {
				log.Info("Deleting pool: released slot has in-flight reclaim task",
					"hostname", slot.Hostname,
					"task", status.ReclaimStatus.TaskRef,
				)
				_, wait, err := r.reconcileReclaimTask(ctx, pool, slot, &status, vcp)
				if err != nil {
					return reconcile.Result{}, err
				}
				if wait > 0 && (requeueAfter == 0 || wait < requeueAfter) {
					requeueAfter = wait
				}
			} else if hasReclaimablePersistentDisk(slot) {
				log.Info("Deleting pool: released slot has reclaimable persistent disks",
					"hostname", slot.Hostname,
					"persistentDiskCount", len(slot.PersistentDisks),
				)
				reclaimed, wait, err := r.reclaimPhysicalResources(ctx, pool, slot, &status, vcp)
				if err != nil {
					return reconcile.Result{}, err
				}
				if reclaimed {
					log.Info("Deleting pool: slot reclaim completed without pending vSphere tasks",
						"hostname", slot.Hostname,
					)
					status.State = infrav1.MachineConfigSlotStateAvailable
					status.LastReleasedTime = nil
					status.ReclaimStatus = &infrav1.MachineConfigSlotReclaimStatus{State: infrav1.MachineConfigSlotReclaimStateCompleted}
				}
				if wait > 0 && (requeueAfter == 0 || wait < requeueAfter) {
					requeueAfter = wait
				}
			} else {
				log.Info("Deleting pool: released slot has no reclaimable persistent disks, marking available",
					"hostname", slot.Hostname,
					"persistentDiskCount", len(slot.PersistentDisks),
				)
				status.State = infrav1.MachineConfigSlotStateAvailable
				status.LastReleasedTime = nil
				status.ReclaimStatus = &infrav1.MachineConfigSlotReclaimStatus{State: infrav1.MachineConfigSlotReclaimStateCompleted}
			}
		}

		newStatuses = append(newStatuses, status)
	}

	pool.Status.ConfigStatuses = newStatuses

	if len(blockingMachines) > 0 {
		return reconcile.Result{}, errors.Errorf("blocking VSphereMachineConfigPool deletion: currently in use by VSphereMachines %v", blockingMachines)
	}

	if requeueAfter > 0 {
		return reconcile.Result{RequeueAfter: requeueAfter}, nil
	}

	ctrlutil.RemoveFinalizer(pool, MachineConfigPoolFinalizer)
	return reconcile.Result{}, nil
}

func hasReclaimablePersistentDisk(slot *infrav1.MachineConfigSlot) bool {
	for i := range slot.PersistentDisks {
		if slot.PersistentDisks[i].VolumePath != "" {
			return true
		}
	}
	return false
}
