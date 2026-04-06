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

package services

import (
	"context"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	infrautil "sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

func ConsumerRefsEqual(a, b *corev1.ObjectReference) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.APIVersion == b.APIVersion &&
		a.Kind == b.Kind &&
		a.Namespace == b.Namespace &&
		a.Name == b.Name
}

// ObjectForConsumerRef returns an empty typed object for the given consumer
// reference kind, or nil if the kind is unsupported.
func ObjectForConsumerRef(ref *corev1.ObjectReference) client.Object {
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

func PoolRefsEqual(a, b *corev1.ObjectReference) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Namespace == b.Namespace && a.Name == b.Name
}

func ResolveMachineConsumerRef(ctx context.Context, c client.Client, machine *clusterv1.Machine) (*corev1.ObjectReference, error) {
	if machine == nil {
		return nil, errors.New("machine is nil")
	}

	if owner, err := infrautil.FetchControlPlaneOwnerObject(ctx, infrautil.FetchObjectInput{Client: c, Object: machine}); err == nil {
		kcp, ok := owner.(*controlplanev1.KubeadmControlPlane)
		if !ok {
			return nil, errors.Errorf("expected KubeadmControlPlane owner but got %T", owner)
		}
		return &corev1.ObjectReference{
			APIVersion: controlplanev1.GroupVersion.String(),
			Kind:       "KubeadmControlPlane",
			Namespace:  kcp.Namespace,
			Name:       kcp.Name,
			UID:        kcp.UID,
		}, nil
	} else if !apierrors.IsNotFound(err) {
		// util helpers currently encode missing-owner as generic errors, so only
		// return early for non-notfound client errors.
	}

	if owner, err := infrautil.FetchMachineDeploymentOwnerObject(ctx, infrautil.FetchObjectInput{Client: c, Object: machine}); err == nil {
		md, ok := owner.(*clusterv1.MachineDeployment)
		if !ok {
			return nil, errors.Errorf("expected MachineDeployment owner but got %T", owner)
		}
		return &corev1.ObjectReference{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "MachineDeployment",
			Namespace:  md.Namespace,
			Name:       md.Name,
			UID:        md.UID,
		}, nil
	}

	return nil, errors.Errorf("failed to resolve consumer for Machine %s/%s", machine.Namespace, machine.Name)
}

func ValidatePoolConsumer(pool *infrav1.VSphereResourcePool, consumerRef *corev1.ObjectReference) error {
	if pool == nil || pool.Status.ConsumerRef == nil || consumerRef == nil {
		return nil
	}
	if ConsumerRefsEqual(pool.Status.ConsumerRef, consumerRef) {
		return nil
	}
	return errors.Errorf("resource pool %s/%s is bound to %s %s/%s", pool.Namespace, pool.Name, pool.Status.ConsumerRef.Kind, pool.Status.ConsumerRef.Namespace, pool.Status.ConsumerRef.Name)
}

func IsPoolFullyReusable(pool *infrav1.VSphereResourcePool) bool {
	if pool == nil {
		return true
	}
	statusMap := make(map[string]infrav1.ResourceSlotStatus, len(pool.Status.ResourceStatuses))
	for _, status := range pool.Status.ResourceStatuses {
		statusMap[status.Hostname] = status
	}

	for i := range pool.Spec.Resources {
		slot := &pool.Spec.Resources[i]
		status, ok := statusMap[slot.Hostname]
		if ok {
			if status.State == "InUse" || status.State == "Released" {
				return false
			}
			if status.ReclaimStatus != nil {
				if status.ReclaimStatus.TaskRef != "" || status.ReclaimStatus.RetryAfter != nil {
					return false
				}
			}
		}
		if hasReclaimablePersistentDiskBacking(slot) {
			return false
		}
	}
	return true
}

func hasReclaimablePersistentDiskBacking(slot *infrav1.ResourceSlot) bool {
	for i := range slot.PersistentDisks {
		if slot.PersistentDisks[i].VolumePath != "" {
			return true
		}
	}
	return false
}

func normalizeDesiredDatacenter(datacenter string) string {
	if datacenter == "*" {
		return ""
	}
	return datacenter
}

func slotMatchesDatacenterConstraints(pool *infrav1.VSphereResourcePool, slot *infrav1.ResourceSlot, desiredDatacenter string, allowedDatacenters map[string]struct{}) bool {
	desiredDatacenter = normalizeDesiredDatacenter(desiredDatacenter)
	resolvedDatacenter := ResolveResourcePoolDatacenter(pool, slot)
	if desiredDatacenter != "" {
		if resolvedDatacenter != desiredDatacenter {
			return false
		}
	}
	if len(allowedDatacenters) > 0 {
		_, ok := allowedDatacenters[resolvedDatacenter]
		if !ok {
			return false
		}
	}
	return true
}

func findSlotByHostname(pool *infrav1.VSphereResourcePool, hostname string) *infrav1.ResourceSlot {
	for i := range pool.Spec.Resources {
		if pool.Spec.Resources[i].Hostname == hostname {
			return &pool.Spec.Resources[i]
		}
	}
	return nil
}

// ResolveResourcePoolDatacenter returns the effective datacenter for a slot.
// Slot-level Datacenter takes precedence over the pool-level default.
func ResolveResourcePoolDatacenter(pool *infrav1.VSphereResourcePool, slot *infrav1.ResourceSlot) string {
	if slot != nil && slot.Datacenter != "" {
		return slot.Datacenter
	}
	if pool != nil {
		return pool.Spec.Datacenter
	}
	return ""
}

// ResolveResourcePoolDatacenterFromRef returns the effective datacenter for a slot
// by loading the referenced VSphereResourcePool when needed.
func ResolveResourcePoolDatacenterFromRef(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, slot *infrav1.ResourceSlot) (string, error) {
	if slot != nil && slot.Datacenter != "" {
		return slot.Datacenter, nil
	}
	if poolRef == nil {
		return "", nil
	}

	pool := &infrav1.VSphereResourcePool{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		return "", err
	}
	return ResolveResourcePoolDatacenter(pool, slot), nil
}

// AllocateSlot finds an available or released slot in the pool for the given machine.
// It retries internally on conflict errors to avoid propagating transient conflicts
// caused by concurrent updates from the resource pool controller.
// consumerRef is checked against the pool's current consumer binding to prevent
// machines from different consumers allocating slots from the same pool.
func AllocateSlot(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, machine *infrav1.VSphereMachine, consumerRef *corev1.ObjectReference, desiredDatacenter string, allowedDatacenters []string) (*infrav1.ResourceSlot, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		slot, err := allocateSlotOnce(ctx, c, poolRef, machine, consumerRef, desiredDatacenter, allowedDatacenters)
		if err == nil {
			return slot, nil
		}
		if !apierrors.IsConflict(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, errors.Wrapf(lastErr, "transient conflict while allocating slot from pool %s/%s, will retry", poolRef.Namespace, poolRef.Name)
}

func allocateSlotOnce(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, machine *infrav1.VSphereMachine, consumerRef *corev1.ObjectReference, desiredDatacenter string, allowedDatacenters []string) (*infrav1.ResourceSlot, error) {
	pool := &infrav1.VSphereResourcePool{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		return nil, err
	}
	if err := ValidatePoolConsumer(pool, consumerRef); err != nil {
		return nil, err
	}
	desiredDatacenter = normalizeDesiredDatacenter(desiredDatacenter)

	allowedDatacenterSet := make(map[string]struct{}, len(allowedDatacenters))
	for _, datacenter := range allowedDatacenters {
		if datacenter == "" {
			continue
		}
		allowedDatacenterSet[datacenter] = struct{}{}
	}

	statusMap := make(map[string]infrav1.ResourceSlotStatus)
	for _, s := range pool.Status.ResourceStatuses {
		statusMap[s.Hostname] = s
	}

	// 1. Try to find a slot already assigned to this specific machine instance (by UID or Name)
	for i, slot := range pool.Spec.Resources {
		if !slotMatchesDatacenterConstraints(pool, &slot, desiredDatacenter, allowedDatacenterSet) {
			continue
		}
		if s, ok := statusMap[slot.Hostname]; ok {
			if s.MachineRef != nil {
				// Exact match by UID is safest
				if s.MachineRef.UID == machine.UID {
					return &pool.Spec.Resources[i], nil
				}
				// Match by Name/Namespace handles idempotency within the same reconcile loop
				// before UID is settled or if machine is recreated.
				if s.MachineRef.Name == machine.Name && s.MachineRef.Namespace == machine.Namespace {
					return &pool.Spec.Resources[i], nil
				}
			}
		}
	}

	// 2. Priority Reuse: Find a Released slot
	var selectedIdx = -1
	for i, slot := range pool.Spec.Resources {
		if !slotMatchesDatacenterConstraints(pool, &slot, desiredDatacenter, allowedDatacenterSet) {
			continue
		}
		if s, ok := statusMap[slot.Hostname]; ok {
			if s.State == "Released" {
				selectedIdx = i
				break
			}
		}
	}

	// 3. Fallback: Find an Available slot OR an uninitialized slot
	if selectedIdx == -1 {
		for i, slot := range pool.Spec.Resources {
			if !slotMatchesDatacenterConstraints(pool, &slot, desiredDatacenter, allowedDatacenterSet) {
				continue
			}
			s, ok := statusMap[slot.Hostname]
			if !ok || s.State == "Available" {
				selectedIdx = i
				break
			}
		}
	}

	if selectedIdx == -1 {
		if desiredDatacenter != "" {
			if len(allowedDatacenterSet) > 0 {
				return nil, errors.Errorf("no available slots in pool %s/%s matching datacenter %q and failure domain datacenters", pool.Namespace, pool.Name, desiredDatacenter)
			}
			return nil, errors.Errorf("no available slots in pool %s/%s matching datacenter %q", pool.Namespace, pool.Name, desiredDatacenter)
		}
		if len(allowedDatacenterSet) > 0 {
			return nil, errors.Errorf("no available slots in pool %s/%s matching failure domain datacenters", pool.Namespace, pool.Name)
		}
		return nil, errors.Errorf("no available slots in pool %s/%s (total slots: %d, status entries: %d)", pool.Namespace, pool.Name, len(pool.Spec.Resources), len(pool.Status.ResourceStatuses))
	}

	targetHostname := pool.Spec.Resources[selectedIdx].Hostname

	// Update or Create status entry
	found := false
	for i := range pool.Status.ResourceStatuses {
		if pool.Status.ResourceStatuses[i].Hostname == targetHostname {
			pool.Status.ResourceStatuses[i].State = "InUse"
			pool.Status.ResourceStatuses[i].MachineRef = &corev1.ObjectReference{
				Kind:      machine.Kind,
				Namespace: machine.Namespace,
				Name:      machine.Name,
				UID:       machine.UID,
			}
			pool.Status.ResourceStatuses[i].LastReleasedTime = nil
			found = true
			break
		}
	}

	if !found {
		pool.Status.ResourceStatuses = append(pool.Status.ResourceStatuses, infrav1.ResourceSlotStatus{
			Hostname: targetHostname,
			State:    "InUse",
			MachineRef: &corev1.ObjectReference{
				Kind:      machine.Kind,
				Namespace: machine.Namespace,
				Name:      machine.Name,
				UID:       machine.UID,
			},
		})
	}

	// Set consumerRef atomically with slot assignment to prevent races
	if pool.Status.ConsumerRef == nil && consumerRef != nil {
		pool.Status.ConsumerRef = &corev1.ObjectReference{
			APIVersion: consumerRef.APIVersion,
			Kind:       consumerRef.Kind,
			Namespace:  consumerRef.Namespace,
			Name:       consumerRef.Name,
			UID:        consumerRef.UID,
		}
	}

	// Persist the assignment and consumer binding in a single update.
	if err := c.Status().Update(ctx, pool); err != nil {
		return nil, err
	}

	selectedSlot := findSlotByHostname(pool, targetHostname)
	if selectedSlot == nil {
		return nil, errors.Errorf("failed to find selected slot %q in pool %s/%s", targetHostname, pool.Namespace, pool.Name)
	}

	slotCopy := *selectedSlot
	if slotCopy.Datacenter == "" {
		slotCopy.Datacenter = ResolveResourcePoolDatacenter(pool, selectedSlot)
	}

	return &slotCopy, nil
}

// ReleaseSlot marks the slot used by the machine as Released.
func ReleaseSlot(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, machineRef *corev1.ObjectReference) error {
	if poolRef == nil || machineRef == nil {
		return nil
	}

	pool := &infrav1.VSphereResourcePool{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	found := false
	for i, s := range pool.Status.ResourceStatuses {
		if s.MachineRef != nil && s.MachineRef.Name == machineRef.Name && s.MachineRef.Namespace == machineRef.Namespace {
			if machineRef.UID != "" && s.MachineRef.UID != "" && s.MachineRef.UID != machineRef.UID {
				continue
			}
			if pool.Status.ResourceStatuses[i].State != "Released" {
				pool.Status.ResourceStatuses[i].State = "Released"
				now := metav1.Now()
				pool.Status.ResourceStatuses[i].LastReleasedTime = &now
				found = true
			}
			break
		}
	}

	if found {
		if err := c.Status().Update(ctx, pool); err != nil {
			if apierrors.IsConflict(err) {
				return errors.Wrapf(err, "transient conflict while releasing slot to pool %s/%s, will retry", pool.Namespace, pool.Name)
			}
			return err
		}
	}
	return nil
}

// GetSlotForMachine returns the slot currently assigned to the machine.
func GetSlotForMachine(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, machineRef *corev1.ObjectReference) (*infrav1.ResourceSlot, error) {
	if poolRef == nil || machineRef == nil {
		return nil, nil
	}

	pool := &infrav1.VSphereResourcePool{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		return nil, err
	}

	for _, status := range pool.Status.ResourceStatuses {
		if status.MachineRef == nil {
			continue
		}
		if status.MachineRef.Name != machineRef.Name || status.MachineRef.Namespace != machineRef.Namespace {
			continue
		}
		if machineRef.UID != "" && status.MachineRef.UID != "" && status.MachineRef.UID != machineRef.UID {
			continue
		}
		return findSlotByHostname(pool, status.Hostname), nil
	}

	return nil, nil
}

// FindResourcePoolForMachine returns the pool currently holding the machine's slot assignment.
func FindResourcePoolForMachine(ctx context.Context, c client.Client, namespace string, machineRef *corev1.ObjectReference) (*corev1.ObjectReference, error) {
	if machineRef == nil {
		return nil, nil
	}

	pools := &infrav1.VSphereResourcePoolList{}
	if err := c.List(ctx, pools, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	for i := range pools.Items {
		pool := &pools.Items[i]
		for _, status := range pool.Status.ResourceStatuses {
			if status.MachineRef == nil {
				continue
			}
			if status.MachineRef.Name != machineRef.Name || status.MachineRef.Namespace != machineRef.Namespace {
				continue
			}
			if machineRef.UID != "" && status.MachineRef.UID != "" && status.MachineRef.UID != machineRef.UID {
				continue
			}
			return &corev1.ObjectReference{
				Namespace: pool.Namespace,
				Name:      pool.Name,
			}, nil
		}
	}

	return nil, nil
}

// ApplyDiskBackfill updates persistent disk metadata (UnitNumber, VolumePath,
// DiskUUID) in pool.Spec for the slot matching updatedSlot.Hostname.  Returns
// true if any field was changed.
func ApplyDiskBackfill(pool *infrav1.VSphereResourcePool, updatedSlot *infrav1.ResourceSlot) bool {
	if pool == nil || updatedSlot == nil {
		return false
	}
	updated := false
	for i := range pool.Spec.Resources {
		if pool.Spec.Resources[i].Hostname != updatedSlot.Hostname {
			continue
		}
		for j := range pool.Spec.Resources[i].PersistentDisks {
			pdInSpec := &pool.Spec.Resources[i].PersistentDisks[j]
			for _, updatedDisk := range updatedSlot.PersistentDisks {
				if pdInSpec.Name != updatedDisk.Name {
					continue
				}
				// Skip if both have UnitNumber set but they disagree — avoids
				// cross-writing between disks that happen to share a name.
				if pdInSpec.UnitNumber != nil && updatedDisk.UnitNumber != nil && *pdInSpec.UnitNumber != *updatedDisk.UnitNumber {
					continue
				}
				if pdInSpec.VolumePath != updatedDisk.VolumePath {
					pdInSpec.VolumePath = updatedDisk.VolumePath
					updated = true
				}
				if pdInSpec.DiskUUID != updatedDisk.DiskUUID {
					pdInSpec.DiskUUID = updatedDisk.DiskUUID
					updated = true
				}
				if (pdInSpec.UnitNumber == nil) != (updatedDisk.UnitNumber == nil) || (pdInSpec.UnitNumber != nil && updatedDisk.UnitNumber != nil && *pdInSpec.UnitNumber != *updatedDisk.UnitNumber) {
					if updatedDisk.UnitNumber == nil {
						pdInSpec.UnitNumber = nil
					} else {
						unitNumber := *updatedDisk.UnitNumber
						pdInSpec.UnitNumber = &unitNumber
					}
					updated = true
				}
			}
		}
		break
	}
	return updated
}

// PersistSlotChanges updates the VSphereResourcePool Spec with the backfilled slot information.
func PersistSlotChanges(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, updatedSlot *infrav1.ResourceSlot) error {
	if poolRef == nil || updatedSlot == nil {
		return nil
	}

	pool := &infrav1.VSphereResourcePool{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		return err
	}

	if !ApplyDiskBackfill(pool, updatedSlot) {
		return nil
	}
	if err := c.Update(ctx, pool); err != nil {
		if apierrors.IsConflict(err) {
			return errors.Wrapf(err, "transient conflict while persisting slot changes to pool %s/%s, will retry", pool.Namespace, pool.Name)
		}
		return err
	}
	return nil
}

