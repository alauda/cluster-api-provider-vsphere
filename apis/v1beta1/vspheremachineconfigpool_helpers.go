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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FindDiskStatus locates an existing persistent-disk status entry by
// (hostname, name). The returned pointer references
// pool.Status.PersistentDiskStatuses[i] for in-place mutation. Returns
// (nil, -1) when not found.
func FindDiskStatus(pool *VSphereMachineConfigPool, hostname, name string) (*PersistentDiskStatus, int) {
	if pool == nil {
		return nil, -1
	}
	for i := range pool.Status.PersistentDiskStatuses {
		e := &pool.Status.PersistentDiskStatuses[i]
		if e.Hostname == hostname && e.Name == name {
			return e, i
		}
	}
	return nil, -1
}

// UpsertDiskStatus inserts or updates the (hostname, name) entry in place.
// LastTransitionTime is refreshed only when Phase actually changes; on
// no-phase-change updates the existing timestamp is preserved so churn does not
// bump it.
func UpsertDiskStatus(pool *VSphereMachineConfigPool, rec PersistentDiskStatus) {
	if pool == nil {
		return
	}
	existing, idx := FindDiskStatus(pool, rec.Hostname, rec.Name)
	if existing == nil {
		if rec.Phase != "" && rec.LastTransitionTime.IsZero() {
			rec.LastTransitionTime = metav1.Now()
		}
		pool.Status.PersistentDiskStatuses = append(pool.Status.PersistentDiskStatuses, rec)
		return
	}
	if rec.Phase != existing.Phase {
		rec.LastTransitionTime = metav1.Now()
	} else if rec.LastTransitionTime.IsZero() {
		rec.LastTransitionTime = existing.LastTransitionTime
	}
	pool.Status.PersistentDiskStatuses[idx] = rec
}

// TombstoneDiskStatus marks the (hostname, name) disk as Reclaimed: its backing
// vmdk has been deleted. Unlike RemoveDiskStatus it keeps the record (as a
// tombstone) with all observed backing (VolumePath/DiskUUID/UnitNumber) and
// in-flight reclaim fields (TaskRef/RetryAfter/LastError) cleared. Keeping the
// record stops SeedPersistentDiskStatuses from re-creating it from spec's frozen
// VolumePath on the next reconcile (which would restart reclaim endlessly), while
// the cleared fields make HasReclaimablePersistentDiskBacking treat the disk as
// done. Backfill overwrites the tombstone when the slot is reused.
func TombstoneDiskStatus(pool *VSphereMachineConfigPool, hostname, name string) {
	UpsertDiskStatus(pool, PersistentDiskStatus{
		Hostname: hostname,
		Name:     name,
		Phase:    PersistentDiskPhaseReclaimed,
	})
}

// RemoveDiskStatus drops the (hostname, name) entry if present.
func RemoveDiskStatus(pool *VSphereMachineConfigPool, hostname, name string) {
	if pool == nil {
		return
	}
	out := pool.Status.PersistentDiskStatuses[:0]
	for _, e := range pool.Status.PersistentDiskStatuses {
		if e.Hostname == hostname && e.Name == name {
			continue
		}
		out = append(out, e)
	}
	pool.Status.PersistentDiskStatuses = out
}

// FindEphemeralDiskStatus locates an existing ephemeral-disk status entry by
// (hostname, name). The returned pointer references
// pool.Status.EphemeralDiskStatuses[i] for in-place mutation. Returns
// (nil, -1) when not found.
func FindEphemeralDiskStatus(pool *VSphereMachineConfigPool, hostname, name string) (*EphemeralDiskStatus, int) {
	if pool == nil {
		return nil, -1
	}
	for i := range pool.Status.EphemeralDiskStatuses {
		e := &pool.Status.EphemeralDiskStatuses[i]
		if e.Hostname == hostname && e.Name == name {
			return e, i
		}
	}
	return nil, -1
}

// UpsertEphemeralDiskStatus inserts or updates the (hostname, name) entry in
// place. Unlike UpsertDiskStatus there is no Phase/LastTransitionTime bookkeeping:
// the record carries only the observed SCSI unit number.
func UpsertEphemeralDiskStatus(pool *VSphereMachineConfigPool, rec EphemeralDiskStatus) {
	if pool == nil {
		return
	}
	existing, idx := FindEphemeralDiskStatus(pool, rec.Hostname, rec.Name)
	if existing == nil {
		pool.Status.EphemeralDiskStatuses = append(pool.Status.EphemeralDiskStatuses, rec)
		return
	}
	pool.Status.EphemeralDiskStatuses[idx] = rec
}
