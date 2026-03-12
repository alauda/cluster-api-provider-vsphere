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
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

func findSlotByHostname(pool *infrav1.VSphereResourcePool, hostname string) *infrav1.ResourceSlot {
	for i := range pool.Spec.Resources {
		if pool.Spec.Resources[i].Hostname == hostname {
			return &pool.Spec.Resources[i]
		}
	}
	return nil
}

// AllocateSlot finds an available or released slot in the pool for the given machine.
func AllocateSlot(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, machine *infrav1.VSphereMachine) (*infrav1.ResourceSlot, error) {
	pool := &infrav1.VSphereResourcePool{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		return nil, err
	}

	statusMap := make(map[string]infrav1.ResourceSlotStatus)
	for _, s := range pool.Status.ResourceStatuses {
		statusMap[s.Hostname] = s
	}

	// 1. Try to find a slot already assigned to this specific machine instance (by UID or Name)
	for i, slot := range pool.Spec.Resources {
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
			s, ok := statusMap[slot.Hostname]
			if !ok || s.State == "Available" {
				selectedIdx = i
				break
			}
		}
	}

	if selectedIdx == -1 {
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

	// Persist the assignment.
	if err := c.Status().Update(ctx, pool); err != nil {
		if apierrors.IsConflict(err) {
			return nil, errors.Wrapf(err, "transient conflict while allocating slot from pool %s/%s, will retry", pool.Namespace, pool.Name)
		}
		return nil, err
	}

	return findSlotByHostname(pool, targetHostname), nil
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

// PersistSlotChanges updates the VSphereResourcePool Spec with the backfilled slot information.
func PersistSlotChanges(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, updatedSlot *infrav1.ResourceSlot) error {
	if poolRef == nil || updatedSlot == nil {
		return nil
	}

	pool := &infrav1.VSphereResourcePool{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		return err
	}

	updated := false
	for i := range pool.Spec.Resources {
		if pool.Spec.Resources[i].Hostname == updatedSlot.Hostname {
			// Backfill VolumePath and DiskUUID with UnitNumber validation
			for j := range pool.Spec.Resources[i].PersistentDisks {
				pdInSpec := &pool.Spec.Resources[i].PersistentDisks[j]
				for _, updatedDisk := range updatedSlot.PersistentDisks {
					// Match by Name AND UnitNumber to ensure we are updating the correct disk
					if pdInSpec.Name == updatedDisk.Name {
						// Only update if UnitNumber matches (or if one is nil and we trust the name)
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
			}
			break
		}
	}

	if updated {
		if err := c.Update(ctx, pool); err != nil {
			if apierrors.IsConflict(err) {
				return errors.Wrapf(err, "transient conflict while persisting slot changes to pool %s/%s, will retry", pool.Namespace, pool.Name)
			}
			return err
		}
	}
	return nil
}
