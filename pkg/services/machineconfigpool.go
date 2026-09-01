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
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	infrautil "sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

const (
	// diskMinUnitNumber and diskMaxUnitNumber bound a SCSI unit number; unit 7 is
	// reserved for the SCSI controller and is excluded.
	diskMinUnitNumber      = 0
	diskMaxUnitNumber      = 15
	diskReservedUnitNumber = 7
)

// ValidateSlotFields checks slot-level structural validity that must be shared
// by the pool reconciler and validating webhook: the pool must define at least
// one slot, every slot must declare a primary network, and every data disk must
// have valid size/unit/name/mount fields.
//
// Persistent and ephemeral disks share one name and mount-path namespace within
// a slot, so a name or mount path used by a persistent disk cannot be reused by
// an ephemeral disk and vice versa. Ephemeral disks have no user-declared unit
// number (it is controller-assigned), so only persistent disks contribute to
// unit-number uniqueness; cross-kind unit conflicts are prevented at clone time
// by the unitNumberAssigner.
//
// Keep this function pure: it is used by the pool reconciler to set the P1-2
// MembersValid condition and by the P1-3 validating webhook as a hard admission
// gate, so sharing the same implementation prevents status/admission drift.
// Hostname format is validated separately.
func ValidateSlotFields(pool *infrav1.VSphereMachineConfigPool) field.ErrorList {
	var allErrs field.ErrorList
	if pool == nil {
		return allErrs
	}
	configsPath := field.NewPath("spec", "configs")
	if len(pool.Spec.Configs) == 0 {
		allErrs = append(allErrs, field.Required(configsPath, "must include at least one slot"))
	}
	for i := range pool.Spec.Configs {
		slot := &pool.Spec.Configs[i]
		slotPath := configsPath.Index(i)
		networkPath := slotPath.Child("network")
		pdPathBase := slotPath.Child("persistentDisks")
		edPathBase := slotPath.Child("ephemeralDisks")

		if slot.Network == nil {
			allErrs = append(allErrs, field.Required(networkPath, "must be set"))
		} else {
			if slot.Network.Primary.NetworkName == "" {
				allErrs = append(allErrs, field.Required(networkPath.Child("primary", "networkName"), "must be set"))
			}
			for j := range slot.Network.Additional {
				if slot.Network.Additional[j].NetworkName == "" {
					allErrs = append(allErrs, field.Required(networkPath.Child("additional").Index(j).Child("networkName"), "must be set"))
				}
			}
		}

		seenNames := map[string]struct{}{}
		seenUnits := map[int32]struct{}{}
		seenMounts := map[string]struct{}{}
		for j := range slot.PersistentDisks {
			pd := &slot.PersistentDisks[j]
			diskPath := pdPathBase.Index(j)

			if pd.Name == "" {
				allErrs = append(allErrs, field.Required(diskPath.Child("name"), "must be set"))
			} else if _, dup := seenNames[pd.Name]; dup {
				allErrs = append(allErrs, field.Duplicate(diskPath.Child("name"), pd.Name))
			} else {
				seenNames[pd.Name] = struct{}{}
			}

			if pd.SizeGiB < 1 {
				allErrs = append(allErrs, field.Invalid(diskPath.Child("sizeGiB"), pd.SizeGiB, "must be at least 1"))
			}

			if pd.UnitNumber != nil {
				u := *pd.UnitNumber
				switch {
				case u < diskMinUnitNumber || u > diskMaxUnitNumber:
					allErrs = append(allErrs, field.Invalid(diskPath.Child("unitNumber"), u, "must be between 0 and 15"))
				case u == diskReservedUnitNumber:
					allErrs = append(allErrs, field.Invalid(diskPath.Child("unitNumber"), u, "unit number 7 is reserved for the SCSI controller"))
				default:
					if _, dup := seenUnits[u]; dup {
						allErrs = append(allErrs, field.Duplicate(diskPath.Child("unitNumber"), u))
					} else {
						seenUnits[u] = struct{}{}
					}
				}
			}

			if pd.MountPath != "" {
				mountPath, err := infrautil.NormalizeGuestMountPath(pd.MountPath)
				if err != nil {
					allErrs = append(allErrs, field.Invalid(diskPath.Child("mountPath"), pd.MountPath, err.Error()))
				} else if _, dup := seenMounts[mountPath]; dup {
					allErrs = append(allErrs, field.Duplicate(diskPath.Child("mountPath"), mountPath))
				} else {
					seenMounts[mountPath] = struct{}{}
				}
			}
		}

		for j := range slot.EphemeralDisks {
			ed := &slot.EphemeralDisks[j]
			diskPath := edPathBase.Index(j)

			if ed.Name == "" {
				allErrs = append(allErrs, field.Required(diskPath.Child("name"), "must be set"))
			} else if _, dup := seenNames[ed.Name]; dup {
				allErrs = append(allErrs, field.Duplicate(diskPath.Child("name"), ed.Name))
			} else {
				seenNames[ed.Name] = struct{}{}
			}

			if ed.SizeGiB < 1 {
				allErrs = append(allErrs, field.Invalid(diskPath.Child("sizeGiB"), ed.SizeGiB, "must be at least 1"))
			}

			if ed.MountPath != "" {
				mountPath, err := infrautil.NormalizeGuestMountPath(ed.MountPath)
				if err != nil {
					allErrs = append(allErrs, field.Invalid(diskPath.Child("mountPath"), ed.MountPath, err.Error()))
				} else if _, dup := seenMounts[mountPath]; dup {
					allErrs = append(allErrs, field.Duplicate(diskPath.Child("mountPath"), mountPath))
				} else {
					seenMounts[mountPath] = struct{}{}
				}
			}
		}
	}
	return allErrs
}

// ValidateHostnameUniqueness reports duplicate hostnames within a single pool.
// Shared by the P1-2 MembersUnique condition and the P1-3 webhook.
func ValidateHostnameUniqueness(pool *infrav1.VSphereMachineConfigPool) field.ErrorList {
	var allErrs field.ErrorList
	if pool == nil {
		return allErrs
	}
	seen := map[string]struct{}{}
	for i := range pool.Spec.Configs {
		hostname := pool.Spec.Configs[i].Hostname
		if hostname == "" {
			continue
		}
		path := field.NewPath("spec", "configs").Index(i).Child("hostname")
		if _, dup := seen[hostname]; dup {
			allErrs = append(allErrs, field.Duplicate(path, hostname))
		} else {
			seen[hostname] = struct{}{}
		}
	}
	return allErrs
}

// ValidateIPUniqueness reports duplicate primary IPv4/IPv6 addresses within a
// single pool. Shared by the P1-2 MembersUnique condition and the P1-3 webhook.
func ValidateIPUniqueness(pool *infrav1.VSphereMachineConfigPool) field.ErrorList {
	var allErrs field.ErrorList
	if pool == nil {
		return allErrs
	}
	seen := map[string]struct{}{}
	for i := range pool.Spec.Configs {
		slot := &pool.Spec.Configs[i]
		if slot.Network == nil {
			continue
		}
		primaryPath := field.NewPath("spec", "configs").Index(i).Child("network", "primary")
		for _, entry := range []struct {
			key string
			val string
		}{{"ip", slot.Network.Primary.IP}, {"ipv6", slot.Network.Primary.IPv6}} {
			if entry.val == "" {
				continue
			}
			if _, dup := seen[entry.val]; dup {
				allErrs = append(allErrs, field.Duplicate(primaryPath.Child(entry.key), entry.val))
			} else {
				seen[entry.val] = struct{}{}
			}
		}
	}
	return allErrs
}

// CrossPoolConflict is a single hostname or primary IP/IPv6 collision between a
// target pool and another pool bound to the same Cluster in the same namespace.
type CrossPoolConflict struct {
	// ConfigIndex is the index into the target pool's spec.configs of the
	// colliding slot.
	ConfigIndex int
	// Field is one of "hostname", "ip", "ipv6".
	Field string
	// Value is the colliding hostname or address.
	Value string
	// OtherPool is the name of the pool that also declares Value.
	OtherPool string
}

// CrossPoolUniquenessConflicts returns hostname and primary IP/IPv6 collisions
// between pool and every other pool in others that is bound to the same Cluster
// (same spec.clusterRef.name) in the same namespace. It is shared by the P1-3
// validating webhook (hard admission gate) and the P1-2 MembersUnique condition
// (best-effort backstop for admission races). Pools are matched by clusterRef so
// unrelated pools sharing a namespace do not conflict; the target pool is skipped
// by name. Results are ordered by the target pool's config index for stable
// output.
func CrossPoolUniquenessConflicts(pool *infrav1.VSphereMachineConfigPool, others []infrav1.VSphereMachineConfigPool) []CrossPoolConflict {
	if pool == nil || pool.Spec.ClusterRef.Name == "" {
		return nil
	}

	otherHostnames := map[string]string{}
	otherIPs := map[string]string{}
	for i := range others {
		o := &others[i]
		if o.Name == pool.Name || o.Namespace != pool.Namespace || o.Spec.ClusterRef.Name != pool.Spec.ClusterRef.Name {
			continue
		}
		for j := range o.Spec.Configs {
			slot := &o.Spec.Configs[j]
			if slot.Hostname != "" {
				if _, ok := otherHostnames[slot.Hostname]; !ok {
					otherHostnames[slot.Hostname] = o.Name
				}
			}
			if slot.Network == nil {
				continue
			}
			for _, ip := range []string{slot.Network.Primary.IP, slot.Network.Primary.IPv6} {
				if ip == "" {
					continue
				}
				if _, ok := otherIPs[ip]; !ok {
					otherIPs[ip] = o.Name
				}
			}
		}
	}

	var conflicts []CrossPoolConflict
	for i := range pool.Spec.Configs {
		slot := &pool.Spec.Configs[i]
		if slot.Hostname != "" {
			if other, ok := otherHostnames[slot.Hostname]; ok {
				conflicts = append(conflicts, CrossPoolConflict{ConfigIndex: i, Field: "hostname", Value: slot.Hostname, OtherPool: other})
			}
		}
		if slot.Network == nil {
			continue
		}
		for _, entry := range []struct{ field, val string }{
			{"ip", slot.Network.Primary.IP},
			{"ipv6", slot.Network.Primary.IPv6},
		} {
			if entry.val == "" {
				continue
			}
			if other, ok := otherIPs[entry.val]; ok {
				conflicts = append(conflicts, CrossPoolConflict{ConfigIndex: i, Field: entry.field, Value: entry.val, OtherPool: other})
			}
		}
	}
	return conflicts
}

// ValidateAllocatedSlotsImmutable forbids destructive edits to slots that are
// already allocated (InUse or Released) in oldPool's status. Called by the P1-3
// webhook on update. Allocated slots are matched by hostname (the slot identity).
// For each allocated slot it forbids: removing the slot entry, changing its
// primary IP/IPv6, removing an existing persistent disk, and changing an existing
// disk's sizeGiB, mountPath, or a already-set unitNumber (disks matched by name).
//
// Adding disks to an allocated slot is allowed (decision: does not take effect
// until VM recreation, harmless). unitNumber is only rejected on a
// concrete→different-concrete change: a nil→value transition is how the
// controller records the assigned SCSI slot (see ApplyDiskBackfill), and a
// value→nil clear self-heals because the reconciler re-derives it — blocking
// either would deadlock the controller's own spec writes.
func ValidateAllocatedSlotsImmutable(oldPool, newPool *infrav1.VSphereMachineConfigPool) field.ErrorList {
	var allErrs field.ErrorList
	if oldPool == nil || newPool == nil {
		return allErrs
	}

	allocated := map[string]struct{}{}
	for i := range oldPool.Status.ConfigStatuses {
		s := &oldPool.Status.ConfigStatuses[i]
		if s.State == infrav1.MachineConfigSlotStateInUse || s.State == infrav1.MachineConfigSlotStateReleased {
			allocated[s.Hostname] = struct{}{}
		}
	}
	if len(allocated) == 0 {
		return allErrs
	}

	newSlots := map[string]int{}
	for i := range newPool.Spec.Configs {
		newSlots[newPool.Spec.Configs[i].Hostname] = i
	}

	base := field.NewPath("spec", "configs")
	// Iterate old spec in declaration order for deterministic error ordering.
	for i := range oldPool.Spec.Configs {
		oldSlot := &oldPool.Spec.Configs[i]
		if _, ok := allocated[oldSlot.Hostname]; !ok {
			continue
		}
		newIdx, ok := newSlots[oldSlot.Hostname]
		if !ok {
			allErrs = append(allErrs, field.Forbidden(base, fmt.Sprintf("cannot remove allocated slot %q (InUse/Released)", oldSlot.Hostname)))
			continue
		}
		allErrs = append(allErrs, validateSlotImmutable(base.Index(newIdx), oldSlot, &newPool.Spec.Configs[newIdx])...)
	}
	return allErrs
}

func slotPrimaryIPs(slot *infrav1.MachineConfigSlot) (ip, ipv6 string) {
	if slot == nil || slot.Network == nil {
		return "", ""
	}
	return slot.Network.Primary.IP, slot.Network.Primary.IPv6
}

func validateSlotImmutable(slotPath *field.Path, oldSlot, newSlot *infrav1.MachineConfigSlot) field.ErrorList {
	var allErrs field.ErrorList
	hostname := oldSlot.Hostname

	oldIP, oldIPv6 := slotPrimaryIPs(oldSlot)
	newIP, newIPv6 := slotPrimaryIPs(newSlot)
	if oldIP != newIP {
		allErrs = append(allErrs, field.Forbidden(slotPath.Child("network", "primary", "ip"),
			fmt.Sprintf("primary IP is immutable for allocated slot %q: %q → %q", hostname, oldIP, newIP)))
	}
	if oldIPv6 != newIPv6 {
		allErrs = append(allErrs, field.Forbidden(slotPath.Child("network", "primary", "ipv6"),
			fmt.Sprintf("primary IPv6 is immutable for allocated slot %q: %q → %q", hostname, oldIPv6, newIPv6)))
	}

	newDisks := map[string]*infrav1.PersistentDisk{}
	for j := range newSlot.PersistentDisks {
		newDisks[newSlot.PersistentDisks[j].Name] = &newSlot.PersistentDisks[j]
	}
	diskBase := slotPath.Child("persistentDisks")
	for j := range oldSlot.PersistentDisks {
		oldDisk := &oldSlot.PersistentDisks[j]
		newDisk, ok := newDisks[oldDisk.Name]
		if !ok {
			allErrs = append(allErrs, field.Forbidden(diskBase,
				fmt.Sprintf("cannot remove persistent disk %q from allocated slot %q", oldDisk.Name, hostname)))
			continue
		}
		if oldDisk.SizeGiB != newDisk.SizeGiB {
			allErrs = append(allErrs, field.Forbidden(diskBase.Child("sizeGiB"),
				fmt.Sprintf("sizeGiB is immutable for disk %q on allocated slot %q: %d → %d", oldDisk.Name, hostname, oldDisk.SizeGiB, newDisk.SizeGiB)))
		}
		oldMountPath, oldMountErr := infrautil.NormalizeGuestMountPath(oldDisk.MountPath)
		newMountPath, newMountErr := infrautil.NormalizeGuestMountPath(newDisk.MountPath)
		if oldMountErr == nil && newMountErr == nil && oldMountPath != newMountPath {
			allErrs = append(allErrs, field.Forbidden(diskBase.Child("mountPath"),
				fmt.Sprintf("mountPath is immutable for disk %q on allocated slot %q: %q → %q", oldDisk.Name, hostname, oldDisk.MountPath, newDisk.MountPath)))
		}
		if oldDisk.UnitNumber != nil && newDisk.UnitNumber != nil && *oldDisk.UnitNumber != *newDisk.UnitNumber {
			allErrs = append(allErrs, field.Forbidden(diskBase.Child("unitNumber"),
				fmt.Sprintf("unitNumber is immutable once set for disk %q on allocated slot %q: %d → %d", oldDisk.Name, hostname, *oldDisk.UnitNumber, *newDisk.UnitNumber)))
		}
	}
	return allErrs
}

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

func ValidatePoolConsumer(pool *infrav1.VSphereMachineConfigPool, consumerRef *corev1.ObjectReference) error {
	if pool == nil || pool.Status.ConsumerRef == nil || consumerRef == nil {
		return nil
	}
	if ConsumerRefsEqual(pool.Status.ConsumerRef, consumerRef) {
		return nil
	}
	return errors.Errorf("machine config pool %s/%s is bound to %s %s/%s", pool.Namespace, pool.Name, pool.Status.ConsumerRef.Kind, pool.Status.ConsumerRef.Namespace, pool.Status.ConsumerRef.Name)
}

func IsPoolFullyReusable(pool *infrav1.VSphereMachineConfigPool) bool {
	if pool == nil {
		return true
	}
	statusMap := make(map[string]infrav1.MachineConfigSlotStatus, len(pool.Status.ConfigStatuses))
	for _, status := range pool.Status.ConfigStatuses {
		statusMap[status.Hostname] = status
	}

	for i := range pool.Spec.Configs {
		slot := &pool.Spec.Configs[i]
		status, ok := statusMap[slot.Hostname]
		if ok {
			if status.State == infrav1.MachineConfigSlotStateInUse || status.State == infrav1.MachineConfigSlotStateReleased {
				return false
			}
		}
		if HasReclaimablePersistentDiskBacking(pool, slot) {
			return false
		}
	}
	return true
}

// HasReclaimablePersistentDiskBacking reports whether any of the slot's disks
// still has backing to reclaim, or a reclaim in flight. The observed volume path
// and reclaim task now live in pool.Status.PersistentDiskStatuses; spec's frozen
// VolumePath is honored as a fallback for objects that predate the status
// migration and have not been seeded yet.
func HasReclaimablePersistentDiskBacking(pool *infrav1.VSphereMachineConfigPool, slot *infrav1.MachineConfigSlot) bool {
	for i := range slot.PersistentDisks {
		pd := &slot.PersistentDisks[i]
		if rec, _ := infrav1.FindDiskStatus(pool, slot.Hostname, pd.Name); rec != nil {
			if rec.VolumePath != "" || rec.TaskRef != "" || rec.RetryAfter != nil ||
				rec.Phase == infrav1.PersistentDiskPhaseReclaiming || rec.Phase == infrav1.PersistentDiskPhaseError {
				return true
			}
			continue
		}
		if pd.VolumePath != "" {
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

func slotMatchesDatacenterConstraints(pool *infrav1.VSphereMachineConfigPool, slot *infrav1.MachineConfigSlot, desiredDatacenter string, allowedDatacenters map[string]struct{}) bool {
	desiredDatacenter = normalizeDesiredDatacenter(desiredDatacenter)
	resolvedDatacenter := ResolveMachineConfigPoolDatacenter(pool, slot)
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

func findSlotByHostname(pool *infrav1.VSphereMachineConfigPool, hostname string) *infrav1.MachineConfigSlot {
	for i := range pool.Spec.Configs {
		if pool.Spec.Configs[i].Hostname == hostname {
			return &pool.Spec.Configs[i]
		}
	}
	return nil
}

// HydrateSlotFromStatus overlays the controller-observed disk state (VolumePath,
// DiskUUID, and the actually-assigned UnitNumber) from
// pool.Status.PersistentDiskStatuses onto the in-memory slot's persistent disks.
// Downstream consumers (VM clone reuse in clone.go, guest cloud-config in
// util.GetPersistentDiskCloudConfig) keep reading these values off the slot, so
// they see the observed state regardless of whether it still lives on spec
// (legacy, frozen) or only in status (post-migration).
//
// A UnitNumber pinned explicitly on spec wins; otherwise the last observed unit
// is reused to keep SCSI ordering stable across VM recreation.
func HydrateSlotFromStatus(pool *infrav1.VSphereMachineConfigPool, slot *infrav1.MachineConfigSlot) {
	if pool == nil || slot == nil {
		return
	}
	for i := range slot.PersistentDisks {
		pd := &slot.PersistentDisks[i]
		rec, _ := infrav1.FindDiskStatus(pool, slot.Hostname, pd.Name)
		if rec == nil {
			continue
		}
		if rec.Phase == infrav1.PersistentDiskPhaseReclaimed {
			// Backing was reclaimed; clear any frozen spec observation so a reused
			// slot provisions a fresh disk instead of trying to reattach the
			// deleted vmdk. Backfill overwrites this tombstone with the new disk.
			pd.VolumePath = ""
			pd.DiskUUID = ""
			continue
		}
		if rec.VolumePath != "" {
			pd.VolumePath = rec.VolumePath
		}
		if rec.DiskUUID != "" {
			pd.DiskUUID = rec.DiskUUID
		}
		if pd.UnitNumber == nil && rec.UnitNumber != nil {
			u := *rec.UnitNumber
			pd.UnitNumber = &u
		}
	}
	for i := range slot.EphemeralDisks {
		ed := &slot.EphemeralDisks[i]
		rec, _ := infrav1.FindEphemeralDiskStatus(pool, slot.Hostname, ed.Name)
		if rec == nil {
			continue
		}
		// Ephemeral disks have no spec unit; reuse the last observed unit so the
		// guest disk table addresses the disk at a stable SCSI position across VM
		// recreation.
		if ed.UnitNumber == nil && rec.UnitNumber != nil {
			u := *rec.UnitNumber
			ed.UnitNumber = &u
		}
	}
}

// ObservedVolumePath returns the disk's controller-observed VolumePath. The
// value now lives in pool.Status.PersistentDiskStatuses; spec's frozen VolumePath
// is used as a fallback for objects that predate the status migration and have
// not been seeded yet.
func ObservedVolumePath(pool *infrav1.VSphereMachineConfigPool, hostname, name, specVolumePath string) string {
	if rec, _ := infrav1.FindDiskStatus(pool, hostname, name); rec != nil {
		return rec.VolumePath
	}
	return specVolumePath
}

// SeedPersistentDiskStatuses performs the release-1 migration: for every disk
// whose observed state currently lives only on spec (frozen VolumePath/DiskUUID/
// UnitNumber) or in a legacy per-slot ReclaimStatus, it creates the equivalent
// pool.Status.PersistentDiskStatuses record so later releases can drop the spec
// fields and the ReclaimStatus type. It never overwrites an existing record and
// returns true if it changed status (so the caller persists it).
func SeedPersistentDiskStatuses(pool *infrav1.VSphereMachineConfigPool) bool {
	if pool == nil {
		return false
	}
	slotStatus := make(map[string]*infrav1.MachineConfigSlotStatus, len(pool.Status.ConfigStatuses))
	for i := range pool.Status.ConfigStatuses {
		slotStatus[pool.Status.ConfigStatuses[i].Hostname] = &pool.Status.ConfigStatuses[i]
	}

	changed := false
	for i := range pool.Spec.Configs {
		slot := &pool.Spec.Configs[i]
		st := slotStatus[slot.Hostname]
		for j := range slot.PersistentDisks {
			pd := &slot.PersistentDisks[j]
			if pd.VolumePath == "" && pd.DiskUUID == "" {
				continue
			}
			if rec, _ := infrav1.FindDiskStatus(pool, slot.Hostname, pd.Name); rec != nil {
				continue
			}
			rec := infrav1.PersistentDiskStatus{
				Hostname:   slot.Hostname,
				Name:       pd.Name,
				VolumePath: pd.VolumePath,
				DiskUUID:   pd.DiskUUID,
				UnitNumber: cloneInt32(pd.UnitNumber),
				Phase:      seededDiskPhase(st),
			}
			if st != nil && st.MachineRef != nil {
				rec.OwnerMachineName = st.MachineRef.Name
				rec.OwnerMachineUID = string(st.MachineRef.UID)
			}
			foldLegacyReclaim(&rec, st, pd.VolumePath)
			infrav1.UpsertDiskStatus(pool, rec)
			changed = true
		}
		// Once every disk with legacy backing has a status record, the per-slot
		// ReclaimStatus has been folded in; clear it so status has a single
		// source of truth (the type is removed in the next release).
		if st != nil && st.ReclaimStatus != nil {
			st.ReclaimStatus = nil
			changed = true
		}
	}
	return changed
}

// markSlotDisksAvailable moves the slot's Attached disks to Available, i.e. the
// VM was released so the disks are detached but still backed and reusable.
func markSlotDisksAvailable(pool *infrav1.VSphereMachineConfigPool, hostname string) {
	for i := range pool.Status.PersistentDiskStatuses {
		rec := &pool.Status.PersistentDiskStatuses[i]
		if rec.Hostname == hostname && rec.Phase == infrav1.PersistentDiskPhaseAttached {
			rec.Phase = infrav1.PersistentDiskPhaseAvailable
			rec.LastTransitionTime = metav1.Now()
		}
	}
}

// seededDiskPhase maps a slot's allocation state to the initial phase of a
// seeded disk record.
func seededDiskPhase(st *infrav1.MachineConfigSlotStatus) infrav1.PersistentDiskPhase {
	if st == nil {
		return infrav1.PersistentDiskPhaseAvailable
	}
	switch st.State {
	case infrav1.MachineConfigSlotStateInUse:
		return infrav1.PersistentDiskPhaseAttached
	default:
		return infrav1.PersistentDiskPhaseAvailable
	}
}

// foldLegacyReclaim carries an in-flight legacy per-slot ReclaimStatus onto the
// disk record it refers to, so reclamation continues seamlessly across the
// migration.
func foldLegacyReclaim(rec *infrav1.PersistentDiskStatus, st *infrav1.MachineConfigSlotStatus, volumePath string) {
	if st == nil || st.ReclaimStatus == nil {
		return
	}
	rs := st.ReclaimStatus
	if rs.VolumePath != "" && rs.VolumePath != volumePath {
		return
	}
	switch rs.State {
	case infrav1.MachineConfigSlotReclaimStateRunning:
		rec.Phase = infrav1.PersistentDiskPhaseReclaiming
		rec.TaskRef = rs.TaskRef
	case infrav1.MachineConfigSlotReclaimStateFailed:
		rec.Phase = infrav1.PersistentDiskPhaseError
		rec.RetryAfter = rs.RetryAfter
		rec.LastError = rs.LastError
	}
}

// ResolveMachineConfigPoolDatacenter returns the effective datacenter for a slot.
// Slot-level Datacenter takes precedence over the pool-level default.
func ResolveMachineConfigPoolDatacenter(pool *infrav1.VSphereMachineConfigPool, slot *infrav1.MachineConfigSlot) string {
	if slot != nil && slot.Datacenter != "" {
		return slot.Datacenter
	}
	if pool != nil {
		return pool.Spec.Datacenter
	}
	return ""
}

// ResolveMachineConfigPoolDatacenterFromRef returns the effective datacenter for a slot
// by loading the referenced VSphereMachineConfigPool when needed.
func ResolveMachineConfigPoolDatacenterFromRef(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, slot *infrav1.MachineConfigSlot) (string, error) {
	if slot != nil && slot.Datacenter != "" {
		return slot.Datacenter, nil
	}
	if poolRef == nil {
		return "", nil
	}

	pool := &infrav1.VSphereMachineConfigPool{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		return "", err
	}
	return ResolveMachineConfigPoolDatacenter(pool, slot), nil
}

// DatacentersWithAvailableSlots returns the set of datacenter names that have
// at least one allocatable slot (Available, Released, or uninitialized) across
// all provided pools.
func DatacentersWithAvailableSlots(pools []infrav1.VSphereMachineConfigPool) map[string]struct{} {
	result := make(map[string]struct{})
	for i := range pools {
		pool := &pools[i]
		statusMap := make(map[string]infrav1.MachineConfigSlotStatus)
		for _, s := range pool.Status.ConfigStatuses {
			statusMap[s.Hostname] = s
		}
		for j := range pool.Spec.Configs {
			slot := &pool.Spec.Configs[j]
			dc := ResolveMachineConfigPoolDatacenter(pool, slot)
			if dc == "" {
				continue
			}
			s := statusMap[slot.Hostname]
			if s.State == "" || s.State == infrav1.MachineConfigSlotStateAvailable || s.State == infrav1.MachineConfigSlotStateReleased {
				result[dc] = struct{}{}
			}
		}
	}
	return result
}

// AllocateSlot finds an available or released slot in the pool for the given machine.
// It retries internally on conflict errors to avoid propagating transient conflicts
// caused by concurrent updates from the machine config pool controller.
// consumerRef is checked against the pool's current consumer binding to prevent
// machines from different consumers allocating slots from the same pool.
func AllocateSlot(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, machine *infrav1.VSphereMachine, consumerRef *corev1.ObjectReference, desiredDatacenter string, allowedDatacenters []string) (*infrav1.MachineConfigSlot, error) {
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

func allocateSlotOnce(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, machine *infrav1.VSphereMachine, consumerRef *corev1.ObjectReference, desiredDatacenter string, allowedDatacenters []string) (*infrav1.MachineConfigSlot, error) {
	pool := &infrav1.VSphereMachineConfigPool{}
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

	statusMap := make(map[string]infrav1.MachineConfigSlotStatus)
	for _, s := range pool.Status.ConfigStatuses {
		statusMap[s.Hostname] = s
	}

	// 1. Try to find a slot already assigned to this specific machine instance (by UID or Name)
	for i, slot := range pool.Spec.Configs {
		if !slotMatchesDatacenterConstraints(pool, &slot, desiredDatacenter, allowedDatacenterSet) {
			continue
		}
		if s, ok := statusMap[slot.Hostname]; ok {
			if s.MachineRef != nil {
				// Exact match by UID is safest
				if s.MachineRef.UID == machine.UID {
					return &pool.Spec.Configs[i], nil
				}
				// Match by Name/Namespace handles idempotency within the same reconcile loop
				// before UID is settled or if machine is recreated.
				if s.MachineRef.Name == machine.Name && s.MachineRef.Namespace == machine.Namespace {
					return &pool.Spec.Configs[i], nil
				}
			}
		}
	}

	// 2. Priority Reuse: Find a Released slot
	var selectedIdx = -1
	for i, slot := range pool.Spec.Configs {
		if !slotMatchesDatacenterConstraints(pool, &slot, desiredDatacenter, allowedDatacenterSet) {
			continue
		}
		if s, ok := statusMap[slot.Hostname]; ok {
			if s.State == infrav1.MachineConfigSlotStateReleased {
				selectedIdx = i
				break
			}
		}
	}

	// 3. Fallback: Find an Available slot OR an uninitialized slot
	if selectedIdx == -1 {
		for i, slot := range pool.Spec.Configs {
			if !slotMatchesDatacenterConstraints(pool, &slot, desiredDatacenter, allowedDatacenterSet) {
				continue
			}
			s, ok := statusMap[slot.Hostname]
			if !ok || s.State == infrav1.MachineConfigSlotStateAvailable {
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
		return nil, errors.Errorf("no available slots in pool %s/%s (total slots: %d, status entries: %d)", pool.Namespace, pool.Name, len(pool.Spec.Configs), len(pool.Status.ConfigStatuses))
	}

	targetHostname := pool.Spec.Configs[selectedIdx].Hostname

	// Update or Create status entry
	found := false
	for i := range pool.Status.ConfigStatuses {
		if pool.Status.ConfigStatuses[i].Hostname == targetHostname {
			pool.Status.ConfigStatuses[i].State = infrav1.MachineConfigSlotStateInUse
			pool.Status.ConfigStatuses[i].MachineRef = &corev1.ObjectReference{
				Kind:      machine.Kind,
				Namespace: machine.Namespace,
				Name:      machine.Name,
				UID:       machine.UID,
			}
			pool.Status.ConfigStatuses[i].LastReleasedTime = nil
			found = true
			break
		}
	}

	if !found {
		pool.Status.ConfigStatuses = append(pool.Status.ConfigStatuses, infrav1.MachineConfigSlotStatus{
			Hostname: targetHostname,
			State:    infrav1.MachineConfigSlotStateInUse,
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
	slotCopy.PersistentDisks = append([]infrav1.PersistentDisk(nil), selectedSlot.PersistentDisks...)
	slotCopy.EphemeralDisks = append([]infrav1.EphemeralDisk(nil), selectedSlot.EphemeralDisks...)
	if slotCopy.Datacenter == "" {
		slotCopy.Datacenter = ResolveMachineConfigPoolDatacenter(pool, selectedSlot)
	}
	HydrateSlotFromStatus(pool, &slotCopy)

	return &slotCopy, nil
}

// ReleaseSlot marks the slot used by the machine as Released.
func ReleaseSlot(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, machineRef *corev1.ObjectReference) error {
	if poolRef == nil || machineRef == nil {
		return nil
	}

	pool := &infrav1.VSphereMachineConfigPool{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	found := false
	for i, s := range pool.Status.ConfigStatuses {
		if s.MachineRef != nil && s.MachineRef.Name == machineRef.Name && s.MachineRef.Namespace == machineRef.Namespace {
			if machineRef.UID != "" && s.MachineRef.UID != "" && s.MachineRef.UID != machineRef.UID {
				continue
			}
			if pool.Status.ConfigStatuses[i].State != infrav1.MachineConfigSlotStateReleased {
				pool.Status.ConfigStatuses[i].State = infrav1.MachineConfigSlotStateReleased
				now := metav1.Now()
				pool.Status.ConfigStatuses[i].LastReleasedTime = &now
				markSlotDisksAvailable(pool, s.Hostname)
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
func GetSlotForMachine(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, machineRef *corev1.ObjectReference) (*infrav1.MachineConfigSlot, error) {
	if poolRef == nil || machineRef == nil {
		return nil, nil
	}

	pool := &infrav1.VSphereMachineConfigPool{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		return nil, err
	}

	for _, status := range pool.Status.ConfigStatuses {
		if status.MachineRef == nil {
			continue
		}
		if status.MachineRef.Name != machineRef.Name || status.MachineRef.Namespace != machineRef.Namespace {
			continue
		}
		if machineRef.UID != "" && status.MachineRef.UID != "" && status.MachineRef.UID != machineRef.UID {
			continue
		}
		slot := findSlotByHostname(pool, status.Hostname)
		if slot == nil {
			return nil, nil
		}
		slotCopy := *slot
		slotCopy.PersistentDisks = append([]infrav1.PersistentDisk(nil), slot.PersistentDisks...)
		slotCopy.EphemeralDisks = append([]infrav1.EphemeralDisk(nil), slot.EphemeralDisks...)
		HydrateSlotFromStatus(pool, &slotCopy)
		return &slotCopy, nil
	}

	return nil, nil
}

// FindMachineConfigPoolForMachine returns the pool currently holding the machine's slot assignment.
func FindMachineConfigPoolForMachine(ctx context.Context, c client.Client, namespace string, machineRef *corev1.ObjectReference) (*corev1.ObjectReference, error) {
	if machineRef == nil {
		return nil, nil
	}

	pools := &infrav1.VSphereMachineConfigPoolList{}
	if err := c.List(ctx, pools, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	for i := range pools.Items {
		pool := &pools.Items[i]
		for _, status := range pool.Status.ConfigStatuses {
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

// ApplyDiskBackfill records the controller-observed disk metadata (VolumePath,
// DiskUUID, actually-assigned UnitNumber) from updatedSlot into
// pool.Status.PersistentDiskStatuses, keyed by (hostname, disk name). Disks that
// have both a verified VolumePath and DiskUUID are marked Attached; a disk with
// only an assigned unit is recorded as Creating until vCenter observation proves
// the backing exists. It returns true if any observed field changed, so the caller
// only issues a status update when needed. Spec is never written — observed state
// lives in status (see HydrateSlotFromStatus for the read side).
func ApplyDiskBackfill(pool *infrav1.VSphereMachineConfigPool, updatedSlot *infrav1.MachineConfigSlot, ownerName, ownerUID string) bool {
	if pool == nil || updatedSlot == nil {
		return false
	}
	changed := false
	for j := range updatedSlot.PersistentDisks {
		pd := &updatedSlot.PersistentDisks[j]
		existing, _ := infrav1.FindDiskStatus(pool, updatedSlot.Hostname, pd.Name)

		// Pick the phase from provisioning progress:
		//   - VolumePath and DiskUUID known -> Attached (created/observed/reused vmdk).
		//   - only UnitNumber  -> Creating (clone assigned a unit but the vmdk is not
		//     observed yet; unit is recorded for device configuration, not identity matching).
		//   - neither          -> nothing to record; the pool reconciler seeds/observes
		//     it. Leave any existing entry untouched.
		var phase infrav1.PersistentDiskPhase
		volumePath, diskUUID := "", ""
		switch {
		case pd.VolumePath != "" && pd.DiskUUID != "":
			phase = infrav1.PersistentDiskPhaseAttached
			volumePath, diskUUID = pd.VolumePath, pd.DiskUUID
		case pd.UnitNumber != nil:
			phase = infrav1.PersistentDiskPhaseCreating
		default:
			continue
		}

		// Never downgrade an already-active disk to Creating. Only seed Creating
		// when there is no record yet, the record is still Creating, or it is a
		// reused Reclaimed tombstone. In the normal flow an active disk's VolumePath
		// is hydrated back onto the slot before this runs, so it takes the Attached
		// branch and never reaches here; this is defense against a lost hydrate.
		if phase == infrav1.PersistentDiskPhaseCreating && existing != nil {
			switch existing.Phase {
			case infrav1.PersistentDiskPhaseCreating, infrav1.PersistentDiskPhaseReclaimed:
				// ok to (re)seed Creating
			default:
				continue
			}
		}

		rec := infrav1.PersistentDiskStatus{
			Hostname:         updatedSlot.Hostname,
			Name:             pd.Name,
			VolumePath:       volumePath,
			DiskUUID:         diskUUID,
			UnitNumber:       cloneInt32(pd.UnitNumber),
			Phase:            phase,
			OwnerMachineName: ownerName,
			OwnerMachineUID:  ownerUID,
		}
		if existing != nil {
			// Preserve any in-flight reclaim bookkeeping; a re-attach after
			// reuse legitimately moves the disk back to Attached and drops it.
			if diskObservedEqual(existing, &rec) {
				continue
			}
		}
		infrav1.UpsertDiskStatus(pool, rec)
		changed = true
	}
	for j := range updatedSlot.EphemeralDisks {
		ed := &updatedSlot.EphemeralDisks[j]
		// The only observed state for an ephemeral disk is its assigned SCSI unit,
		// which becomes available after clone. Record it (unlike persistent disks,
		// there is no VolumePath gate) so the next reconcile can hydrate it back
		// onto the slot; skip until the unit is known.
		if ed.UnitNumber == nil {
			continue
		}
		existing, _ := infrav1.FindEphemeralDiskStatus(pool, updatedSlot.Hostname, ed.Name)
		if existing != nil && ptr.Equal(existing.UnitNumber, ed.UnitNumber) {
			continue
		}
		infrav1.UpsertEphemeralDiskStatus(pool, infrav1.EphemeralDiskStatus{
			Hostname:   updatedSlot.Hostname,
			Name:       ed.Name,
			UnitNumber: cloneInt32(ed.UnitNumber),
		})
		changed = true
	}
	return changed
}

func cloneInt32(v *int32) *int32 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

// diskObservedEqual reports whether the observed and ownership fields of two
// disk status records match. Reclaim task fields and timestamps are ignored so
// a plain re-backfill does not churn the status.
func diskObservedEqual(a, b *infrav1.PersistentDiskStatus) bool {
	return a.VolumePath == b.VolumePath &&
		a.DiskUUID == b.DiskUUID &&
		ptr.Equal(a.UnitNumber, b.UnitNumber) &&
		a.Phase == b.Phase &&
		a.OwnerMachineName == b.OwnerMachineName &&
		a.OwnerMachineUID == b.OwnerMachineUID
}

// PersistSlotChanges records the backfilled slot disk metadata into the pool
// status via a status update. ownerName/ownerUID identify the machine that owns
// the slot.
func PersistSlotChanges(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, updatedSlot *infrav1.MachineConfigSlot, ownerName, ownerUID string) error {
	if poolRef == nil || updatedSlot == nil {
		return nil
	}

	pool := &infrav1.VSphereMachineConfigPool{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: poolRef.Namespace, Name: poolRef.Name}, pool); err != nil {
		return err
	}

	if !ApplyDiskBackfill(pool, updatedSlot, ownerName, ownerUID) {
		return nil
	}
	if err := c.Status().Update(ctx, pool); err != nil {
		if apierrors.IsConflict(err) {
			return errors.Wrapf(err, "transient conflict while persisting slot changes to pool %s/%s, will retry", pool.Namespace, pool.Name)
		}
		return err
	}
	return nil
}
