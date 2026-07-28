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

// ValidateSlotFields checks the structural validity of every slot's persistent
// disk fields (unit number range, size, and intra-slot uniqueness of disk name,
// unit number, and mount path). It is a pure function shared by the pool
// reconciler (P1-2 MembersValid condition) and the validating webhook (P1-3) so
// the two never drift. Hostname format is validated separately.
func ValidateSlotFields(pool *infrav1.VSphereMachineConfigPool) field.ErrorList {
	var allErrs field.ErrorList
	if pool == nil {
		return allErrs
	}
	for i := range pool.Spec.Configs {
		slot := &pool.Spec.Configs[i]
		diskPathBase := field.NewPath("spec", "configs").Index(i).Child("persistentDisks")

		seenNames := map[string]struct{}{}
		seenUnits := map[int32]struct{}{}
		seenMounts := map[string]struct{}{}
		for j := range slot.PersistentDisks {
			pd := &slot.PersistentDisks[j]
			diskPath := diskPathBase.Index(j)

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
				if _, dup := seenMounts[pd.MountPath]; dup {
					allErrs = append(allErrs, field.Duplicate(diskPath.Child("mountPath"), pd.MountPath))
				} else {
					seenMounts[pd.MountPath] = struct{}{}
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
		if oldDisk.MountPath != newDisk.MountPath {
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

func hasReclaimablePersistentDiskBacking(slot *infrav1.MachineConfigSlot) bool {
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
	if slotCopy.Datacenter == "" {
		slotCopy.Datacenter = ResolveMachineConfigPoolDatacenter(pool, selectedSlot)
	}

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
		return findSlotByHostname(pool, status.Hostname), nil
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

// ApplyDiskBackfill updates persistent disk metadata (UnitNumber, VolumePath,
// DiskUUID) in pool.Spec for the slot matching updatedSlot.Hostname.  Returns
// true if any field was changed.
func ApplyDiskBackfill(pool *infrav1.VSphereMachineConfigPool, updatedSlot *infrav1.MachineConfigSlot) bool {
	if pool == nil || updatedSlot == nil {
		return false
	}
	updated := false
	for i := range pool.Spec.Configs {
		if pool.Spec.Configs[i].Hostname != updatedSlot.Hostname {
			continue
		}
		for j := range pool.Spec.Configs[i].PersistentDisks {
			pdInSpec := &pool.Spec.Configs[i].PersistentDisks[j]
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

// PersistSlotChanges updates the VSphereMachineConfigPool Spec with the backfilled slot information.
func PersistSlotChanges(ctx context.Context, c client.Client, poolRef *corev1.ObjectReference, updatedSlot *infrav1.MachineConfigSlot) error {
	if poolRef == nil || updatedSlot == nil {
		return nil
	}

	pool := &infrav1.VSphereMachineConfigPool{}
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
