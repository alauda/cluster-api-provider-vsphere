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
	"time"

	"github.com/pkg/errors"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/finalizers"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
)

const (
	// ResourcePoolFinalizer allows the reconciler to clean up resources.
	ResourcePoolFinalizer = "vsphereresourcepool.infrastructure.cluster.x-k8s.io"

	// DefaultReleaseDelayHours is the default time to wait before reclaiming a slot.
	DefaultReleaseDelayHours = 24

	reclaimTaskStateRunning   = "Running"
	reclaimTaskStateFailed    = "Failed"
	reclaimTaskStateCompleted = "Completed"
)

// AddVSphereResourcePoolControllerToManager adds a VSphereResourcePool controller to the manager.
func AddVSphereResourcePoolControllerToManager(ctx context.Context, controllerManagerCtx *capvcontext.ControllerManagerContext, mgr manager.Manager, options controller.Options) error {
	reconciler := resourcePoolReconciler{
		Client:                   controllerManagerCtx.Client,
		ControllerManagerContext: controllerManagerCtx,
		Recorder:                 mgr.GetEventRecorderFor("vsphereresourcepool-controller"),
	}
	predicateLog := ctrl.LoggerFrom(ctx).WithValues("controller", "vsphereresourcepool")

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.VSphereResourcePool{}).
		WithOptions(options).
		WithEventFilter(predicates.ResourceHasFilterLabel(mgr.GetScheme(), predicateLog, controllerManagerCtx.WatchFilterValue)).
		Complete(reconciler)
}

type resourcePoolReconciler struct {
	Client                   client.Client
	ControllerManagerContext *capvcontext.ControllerManagerContext
	Recorder                 record.EventRecorder
}

func (r resourcePoolReconciler) Reconcile(ctx context.Context, req reconcile.Request) (_ reconcile.Result, reterr error) {
	pool := &infrav1.VSphereResourcePool{}
	if err := r.Client.Get(ctx, req.NamespacedName, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if finalizerAdded, err := finalizers.EnsureFinalizer(ctx, r.Client, pool, ResourcePoolFinalizer); err != nil || finalizerAdded {
		return ctrl.Result{}, err
	}

	patchHelper, err := patch.NewHelper(pool, r.Client)
	if err != nil {
		return reconcile.Result{}, err
	}

	defer func() {
		if err := patchHelper.Patch(ctx, pool); err != nil {
			if reterr == nil {
				reterr = errors.Wrapf(err, "failed to patch VSphereResourcePool %s/%s", pool.Namespace, pool.Name)
			}
		}
	}()

	if !pool.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, pool)
	}

	return r.reconcileNormal(ctx, pool)
}

func (r resourcePoolReconciler) reconcileNormal(ctx context.Context, pool *infrav1.VSphereResourcePool) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	if pool.Status.ResourceStatuses == nil {
		pool.Status.ResourceStatuses = []infrav1.ResourceSlotStatus{}
	}

	statusMap := make(map[string]infrav1.ResourceSlotStatus)
	for _, s := range pool.Status.ResourceStatuses {
		statusMap[s.Hostname] = s
	}

	newStatuses := []infrav1.ResourceSlotStatus{}
	delayHours := DefaultReleaseDelayHours
	if pool.Spec.ReleaseDelayHours != nil {
		delayHours = *pool.Spec.ReleaseDelayHours
	}

	requeueAfter := time.Duration(0)
	specChanged := false

	if bound, err := r.reconcileConsumerBinding(ctx, pool); err != nil {
		return reconcile.Result{}, err
	} else if bound {
		specChanged = true
	}

	for i := range pool.Spec.Resources {
		slot := &pool.Spec.Resources[i]
		status, ok := statusMap[slot.Hostname]
		if !ok {
			status = infrav1.ResourceSlotStatus{
				Hostname: slot.Hostname,
				State:    "Available",
			}
		}

		// 1. Reclamation Check (Released -> Available)
		if status.State == "Released" && status.LastReleasedTime != nil {
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
				specUpdated, wait, err := r.reconcileReclaimTask(ctx, pool, slot, &status)
				if err != nil {
					log.Error(err, "failed to reconcile reclaim task for slot", "hostname", slot.Hostname, "task", status.ReclaimStatus.TaskRef)
				}
				if specUpdated {
					specChanged = true
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
				reclaimed, wait, err := r.reclaimPhysicalResources(ctx, pool, slot, &status)
				if err != nil {
					log.Error(err, "failed to reclaim physical resources for slot", "hostname", slot.Hostname)
				}
				if reclaimed {
					status.State = "Available"
					status.MachineRef = nil
					status.LastReleasedTime = nil
					status.ReclaimStatus = &infrav1.ResourceSlotReclaimStatus{State: reclaimTaskStateCompleted}
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
		if status.State == "InUse" && status.MachineRef != nil {
			m := &infrav1.VSphereMachine{}
			err := r.Client.Get(ctx, client.ObjectKey{Namespace: status.MachineRef.Namespace, Name: status.MachineRef.Name}, m)
			if err != nil && apierrors.IsNotFound(err) {
				log.Info("Machine associated with slot no longer exists, moving slot to Released", "hostname", slot.Hostname, "machine", status.MachineRef.Name)
				status.State = "Released"
				now := metav1.Now()
				status.LastReleasedTime = &now
			}
		}

		newStatuses = append(newStatuses, status)
	}

	if specChanged {
		if err := r.Client.Update(ctx, pool); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{Requeue: true}, nil
	}

	pool.Status.ResourceStatuses = newStatuses

	return reconcile.Result{RequeueAfter: requeueAfter}, nil
}

func (r resourcePoolReconciler) reconcileConsumerBinding(ctx context.Context, pool *infrav1.VSphereResourcePool) (bool, error) {
	if pool.Spec.ConsumerRef == nil {
		return false, nil
	}

	target := objectForConsumer(pool.Spec.ConsumerRef)
	if target == nil {
		return false, errors.Errorf("unsupported consumer kind %q on VSphereResourcePool %s/%s", pool.Spec.ConsumerRef.Kind, pool.Namespace, pool.Name)
	}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: pool.Spec.ConsumerRef.Namespace, Name: pool.Spec.ConsumerRef.Name}, target); err == nil {
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}

	if !services.IsPoolFullyReusable(pool) {
		return false, nil
	}
	pool.Spec.ConsumerRef = nil
	return true, nil
}

func objectForConsumer(ref *corev1.ObjectReference) client.Object {
	if ref == nil {
		return nil
	}
	switch ref.Kind {
	case "KubeadmControlPlane":
		return &controlplanev1.KubeadmControlPlane{}
	case "MachineDeployment":
		return &clusterv1.MachineDeployment{}
	default:
		return nil
	}
}

func (r resourcePoolReconciler) reclaimPhysicalResources(ctx context.Context, pool *infrav1.VSphereResourcePool, slot *infrav1.ResourceSlot, status *infrav1.ResourceSlotStatus) (bool, time.Duration, error) {
	log := ctrl.LoggerFrom(ctx)
	slotDatacenter := services.ResolveResourcePoolDatacenter(pool, slot)

	if slotDatacenter == "" {
		return false, 0, errors.Errorf("datacenter must be specified on slot %q or VSphereResourcePool %s/%s for resource reclamation", slot.Hostname, pool.Namespace, pool.Name)
	}

	params := session.NewParams().
		WithUserInfo(r.ControllerManagerContext.Username, r.ControllerManagerContext.Password).
		WithServer(pool.Spec.Server).
		WithThumbprint(pool.Spec.Thumbprint).
		WithDatacenter(slotDatacenter)

	s, err := session.GetOrCreate(ctx, params)
	if err != nil {
		return false, 0, err
	}

	for i := range slot.PersistentDisks {
		pd := &slot.PersistentDisks[i]
		if pd.VolumePath != "" {
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
				return false, 0, errors.Wrapf(err, "failed to find datacenter %s for reclamation", dcName)
			}

			task, err := m.DeleteDatastoreFile(ctx, pd.VolumePath, dc)
			if err != nil {
				log.Error(err, "Failed to start datastore file deletion", "hostname", slot.Hostname, "disk", pd.Name, "path", pd.VolumePath)
				if !soap.IsSoapFault(err) {
					return false, 0, err
				}
				return false, 0, err
			}
			status.ReclaimStatus = &infrav1.ResourceSlotReclaimStatus{
				TaskRef:    task.Reference().Value,
				State:      reclaimTaskStateRunning,
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

func (r resourcePoolReconciler) reconcileReclaimTask(ctx context.Context, pool *infrav1.VSphereResourcePool, slot *infrav1.ResourceSlot, status *infrav1.ResourceSlotStatus) (bool, time.Duration, error) {
	log := ctrl.LoggerFrom(ctx)
	slotDatacenter := services.ResolveResourcePoolDatacenter(pool, slot)

	params := session.NewParams().
		WithUserInfo(r.ControllerManagerContext.Username, r.ControllerManagerContext.Password).
		WithServer(pool.Spec.Server).
		WithThumbprint(pool.Spec.Thumbprint).
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
		status.ReclaimStatus = &infrav1.ResourceSlotReclaimStatus{State: reclaimTaskStateCompleted}
		return specUpdated, 1 * time.Second, nil
	case types.TaskInfoStateError:
		errMessage := "reclaim task failed"
		if task.Info.Error != nil {
			errMessage = task.Info.Error.LocalizedMessage
		}
		log.Error(errors.New(errMessage), "Reclaim task failed")
		retryAfter := metav1.NewTime(time.Now().Add(1 * time.Minute))
		status.ReclaimStatus = &infrav1.ResourceSlotReclaimStatus{
			State:      reclaimTaskStateFailed,
			RetryAfter: &retryAfter,
			LastError:  errMessage,
		}
		return false, time.Until(retryAfter.Time), nil
	default:
		return false, 0, errors.Errorf("unknown reclaim task state %q", task.Info.State)
	}
}

func (r resourcePoolReconciler) reconcileDelete(ctx context.Context, pool *infrav1.VSphereResourcePool) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	statusMap := make(map[string]infrav1.ResourceSlotStatus, len(pool.Status.ResourceStatuses))
	for _, status := range pool.Status.ResourceStatuses {
		statusMap[status.Hostname] = status
	}

	newStatuses := make([]infrav1.ResourceSlotStatus, 0, len(pool.Spec.Resources))
	var blockingMachines []string
	requeueAfter := time.Duration(0)

	for i := range pool.Spec.Resources {
		slot := &pool.Spec.Resources[i]
		status, ok := statusMap[slot.Hostname]
		if !ok {
			status = infrav1.ResourceSlotStatus{
				Hostname: slot.Hostname,
				State:    "Available",
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
				if status.State == "InUse" {
					status.State = "Released"
					now := metav1.Now()
					status.LastReleasedTime = &now
				}
			default:
				return reconcile.Result{}, err
			}
		}

		if status.State == "Released" {
			if status.ReclaimStatus != nil && status.ReclaimStatus.TaskRef != "" {
				log.Info("Deleting pool: released slot has in-flight reclaim task",
					"hostname", slot.Hostname,
					"task", status.ReclaimStatus.TaskRef,
				)
				_, wait, err := r.reconcileReclaimTask(ctx, pool, slot, &status)
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
				reclaimed, wait, err := r.reclaimPhysicalResources(ctx, pool, slot, &status)
				if err != nil {
					return reconcile.Result{}, err
				}
				if reclaimed {
					log.Info("Deleting pool: slot reclaim completed without pending vSphere tasks",
						"hostname", slot.Hostname,
					)
					status.State = "Available"
					status.LastReleasedTime = nil
					status.ReclaimStatus = &infrav1.ResourceSlotReclaimStatus{State: reclaimTaskStateCompleted}
				}
				if wait > 0 && (requeueAfter == 0 || wait < requeueAfter) {
					requeueAfter = wait
				}
			} else {
				log.Info("Deleting pool: released slot has no reclaimable persistent disks, marking available",
					"hostname", slot.Hostname,
					"persistentDiskCount", len(slot.PersistentDisks),
				)
				status.State = "Available"
				status.LastReleasedTime = nil
				status.ReclaimStatus = &infrav1.ResourceSlotReclaimStatus{State: reclaimTaskStateCompleted}
			}
		}

		newStatuses = append(newStatuses, status)
	}

	pool.Status.ResourceStatuses = newStatuses

	if len(blockingMachines) > 0 {
		return reconcile.Result{}, errors.Errorf("blocking VSphereResourcePool deletion: currently in use by VSphereMachines %v", blockingMachines)
	}

	if requeueAfter > 0 {
		return reconcile.Result{RequeueAfter: requeueAfter}, nil
	}

	ctrlutil.RemoveFinalizer(pool, ResourcePoolFinalizer)
	return reconcile.Result{}, nil
}

func hasReclaimablePersistentDisk(slot *infrav1.ResourceSlot) bool {
	for i := range slot.PersistentDisks {
		if slot.PersistentDisks[i].VolumePath != "" {
			return true
		}
	}
	return false
}
