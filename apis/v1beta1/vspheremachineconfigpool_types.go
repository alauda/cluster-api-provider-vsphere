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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// v1beta2 condition types and reasons for the VSphereMachineConfigPool object.
// These mirror the v1beta1 pool conditions in condition_consts.go and are
// dual-written by the pool reconciler.
const (
	// VSphereMachineConfigPoolReadyV1Beta2Condition is the summary of the pool's
	// health conditions (MembersValid, MembersUnique, PersistentDisksReady,
	// ClusterRefReady, VCenterAvailable). It excludes SlotAvailable.
	VSphereMachineConfigPoolReadyV1Beta2Condition = clusterv1.ReadyV1Beta2Condition

	// VSphereMachineConfigPoolReadyV1Beta2Reason surfaces when the pool readiness criteria are met.
	VSphereMachineConfigPoolReadyV1Beta2Reason = clusterv1.ReadyV1Beta2Reason

	// VSphereMachineConfigPoolNotReadyV1Beta2Reason surfaces when the pool readiness criteria are not met.
	VSphereMachineConfigPoolNotReadyV1Beta2Reason = clusterv1.NotReadyV1Beta2Reason

	// VSphereMachineConfigPoolReadyUnknownV1Beta2Reason surfaces when at least one pool readiness criterion is unknown.
	VSphereMachineConfigPoolReadyUnknownV1Beta2Reason = clusterv1.ReadyUnknownV1Beta2Reason

	// VSphereMachineConfigPoolClusterRefReadyV1Beta2Condition mirrors ClusterRefReadyCondition.
	VSphereMachineConfigPoolClusterRefReadyV1Beta2Condition = "ClusterRefReady"

	// VSphereMachineConfigPoolVCenterAvailableV1Beta2Condition mirrors VCenterAvailableCondition.
	VSphereMachineConfigPoolVCenterAvailableV1Beta2Condition = "VCenterAvailable"

	// VSphereMachineConfigPoolMembersValidV1Beta2Condition mirrors MachineConfigPoolMembersValidCondition.
	VSphereMachineConfigPoolMembersValidV1Beta2Condition = "MembersValid"

	// VSphereMachineConfigPoolMembersUniqueV1Beta2Condition mirrors MachineConfigPoolMembersUniqueCondition.
	VSphereMachineConfigPoolMembersUniqueV1Beta2Condition = "MembersUnique"

	// VSphereMachineConfigPoolSlotAvailableV1Beta2Condition mirrors MachineConfigPoolSlotAvailableCondition.
	VSphereMachineConfigPoolSlotAvailableV1Beta2Condition = "SlotAvailable"

	// VSphereMachineConfigPoolPersistentDisksReadyV1Beta2Condition mirrors MachineConfigPoolPersistentDisksReadyCondition.
	VSphereMachineConfigPoolPersistentDisksReadyV1Beta2Condition = "PersistentDisksReady"

	// VSphereMachineConfigPoolConditionSatisfiedV1Beta2Reason is the generic reason used
	// for the True state of the pool's health sub-conditions (v1beta2 requires a
	// reason on every condition, including True ones).
	VSphereMachineConfigPoolConditionSatisfiedV1Beta2Reason = "Satisfied"

	// VSphereMachineConfigPoolClusterRefNotReadyV1Beta2Reason surfaces when the referenced
	// Cluster/VSphereCluster is not available.
	VSphereMachineConfigPoolClusterRefNotReadyV1Beta2Reason = "ClusterRefNotReady"

	// VSphereMachineConfigPoolVCenterUnavailableV1Beta2Reason surfaces when vCenter credentials
	// cannot be resolved.
	VSphereMachineConfigPoolVCenterUnavailableV1Beta2Reason = "VCenterUnavailable"
)

// VSphereMachineConfigPoolSpec defines the desired state of VSphereMachineConfigPool.
type VSphereMachineConfigPoolSpec struct {
	// ClusterRef references the CAPI Cluster (in the same namespace) whose
	// VSphereCluster provides vCenter server, thumbprint, and credential
	// chain (IdentityRef) for this pool's vCenter operations (e.g. disk
	// reclaim). Required. Can only be changed when consumerRef is nil.
	// The pool will not reconcile until the referenced Cluster and its
	// VSphereCluster infrastructure are available.
	ClusterRef corev1.ObjectReference `json:"clusterRef"`

	// Datacenter is the default vSphere datacenter for slots in this pool.
	// It is used when a slot does not define its own Datacenter.
	// +optional
	Datacenter string `json:"datacenter,omitempty"`

	// Configs is the list of pre-defined machine configuration slots.
	Configs []MachineConfigSlot `json:"configs"`

	// ReleaseDelayHours is the time to wait before marking a released slot as "Available" for any machine.
	// During this period, the slot can only be reused if specifically requested or via priority reuse.
	// Default is 24.
	// +optional
	ReleaseDelayHours *int `json:"releaseDelayHours,omitempty"`
}

// MachineConfigSlot defines a single machine configuration slot in the pool.
type MachineConfigSlot struct {
	// Hostname is the unique identifier for this slot and will be assigned to the VM.
	// It must also be a valid Kubernetes node name because CAPV uses it for
	// kubeadm nodeRegistration.name and the kubelet serving certificate DNS SAN.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Hostname string `json:"hostname"`

	// Datacenter is the vSphere datacenter for this slot.
	// If set, it takes precedence over VSphereMachineConfigPool.spec.datacenter.
	// If unset, the pool-level Datacenter acts as the default.
	// +optional
	Datacenter string `json:"datacenter,omitempty"`

	// Network describes the primary and additional network configurations for this slot.
	// +optional
	Network *MachineConfigSlotNetwork `json:"network,omitempty"`

	// PersistentDisks that survive VM deletion.
	// +optional
	PersistentDisks []PersistentDisk `json:"persistentDisks,omitempty"`

	// EphemeralDisks are non-persistent data disks. Unlike PersistentDisks they
	// are created fresh (as empty disks) whenever the slot's VM is (re)created
	// and deleted together with the VM: they do not participate in detach
	// preservation, reclaim, or slot-release gating. Their SCSI unit number is
	// assigned by the controller at clone time and observed in
	// status.ephemeralDiskStatuses; it is not user-declared.
	// +optional
	EphemeralDisks []EphemeralDisk `json:"ephemeralDisks,omitempty"`
}

// MachineConfigSlotNetwork defines the primary and additional network configurations for a slot.
type MachineConfigSlotNetwork struct {
	// Primary is the network configuration used for kubelet node IP registration.
	Primary NetworkConfig `json:"primary"`

	// Additional are the remaining network configurations attached after the primary device.
	// +optional
	Additional []NetworkConfig `json:"additional,omitempty"`
}

// NetworkConfig defines the network configuration for a slot device.
type NetworkConfig struct {
	// NetworkName is the name of the vSphere network (PortGroup or DVPortGroup).
	NetworkName string `json:"networkName"`

	// DeviceName explicitly assigns a guest OS interface name to the network device.
	// +optional
	DeviceName string `json:"deviceName,omitempty"`

	// IPv4 configuration
	// +optional
	IP string `json:"ip,omitempty"`
	// +optional
	Gateway string `json:"gateway,omitempty"`

	// IPv6 configuration
	// +optional
	IPv6 string `json:"ipv6,omitempty"`
	// +optional
	IPv6Gateway string `json:"ipv6Gateway,omitempty"`

	// DNS nameservers
	// +optional
	DNS []string `json:"dns,omitempty"`
}

// PersistentDisk defines a disk that survives VM deletion.
type PersistentDisk struct {
	// Name is the disk name.
	Name string `json:"name"`
	// SizeGiB is the disk size.
	// +kubebuilder:validation:Minimum=1
	SizeGiB int32 `json:"sizeGiB"`
	// Datastore is the vSphere datastore name.
	// +optional
	Datastore string `json:"datastore,omitempty"`
	// StoragePolicy is the vSphere storage policy name.
	// +optional
	StoragePolicy string `json:"storagePolicy,omitempty"`

	// UnitNumber is the SCSI unit number for the disk (0-15, excluding 7).
	// This ensures consistent disk ordering across VM recreations.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=15
	// +kubebuilder:validation:XValidation:rule="self != 7",message="unit number 7 is reserved for the SCSI controller"
	UnitNumber *int32 `json:"unitNumber,omitempty"`

	// MountPath is the mount path inside the VM guest OS (e.g., "/var/lib/etcd").
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// MountOptions for the filesystem mount.
	// +optional
	MountOptions []string `json:"mountOptions,omitempty"`

	// FSFormat is the filesystem format (default "ext4").
	// +optional
	FSFormat string `json:"fsFormat,omitempty"`

	// WipeFilesystem controls whether to wipe the filesystem content when the
	// slot is reused by a new VM. When true, the disk content is cleared on
	// the first boot of a new VM (reboots and manual service restarts are not
	// affected). Defaults to false (data is preserved across VM recreations).
	// +optional
	WipeFilesystem *bool `json:"wipeFilesystem,omitempty"`

	// VolumePath is backfilled by the controller after the disk is created.
	// For vSphere, this is usually the datastore path to the .vmdk file.
	// +optional
	VolumePath string `json:"volumePath,omitempty"`

	// DiskUUID is backfilled by the controller.
	// +optional
	DiskUUID string `json:"diskUUID,omitempty"`
}

// EphemeralDisk defines a non-persistent data disk that is created empty with
// the slot's VM and deleted together with it. It mirrors the guest-facing
// fields of PersistentDisk (size, placement, mount, format) but omits all
// persistence semantics: there is no UnitNumber spec field (the SCSI unit is
// controller-assigned at clone time and observed in
// status.ephemeralDiskStatuses), no VolumePath/DiskUUID backfill (the backing
// vmdk is always newly created), and no WipeFilesystem (a fresh disk is always
// empty and formatted from scratch).
type EphemeralDisk struct {
	// Name is the disk name. It shares a namespace with the slot's persistent
	// disks: a name must be unique across both lists within the slot.
	Name string `json:"name"`
	// SizeGiB is the disk size.
	// +kubebuilder:validation:Minimum=1
	SizeGiB int32 `json:"sizeGiB"`
	// Datastore is the vSphere datastore name.
	// +optional
	Datastore string `json:"datastore,omitempty"`
	// StoragePolicy is the vSphere storage policy name.
	// +optional
	StoragePolicy string `json:"storagePolicy,omitempty"`

	// MountPath is the mount path inside the VM guest OS (e.g., "/var/lib/containerd").
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// MountOptions for the filesystem mount.
	// +optional
	MountOptions []string `json:"mountOptions,omitempty"`

	// FSFormat is the filesystem format (default "ext4").
	// +optional
	FSFormat string `json:"fsFormat,omitempty"`

	// UnitNumber carries the controller-observed SCSI unit number in memory only
	// (json:"-", never serialized to the CRD). It is hydrated each reconcile from
	// status.ephemeralDiskStatuses so clone and cloud-config consumers can address
	// the disk, mirroring how PersistentDisk.UnitNumber is used downstream.
	UnitNumber *int32 `json:"-"`
}

// VSphereMachineConfigPoolStatus defines the observed state of VSphereMachineConfigPool.
type VSphereMachineConfigPoolStatus struct {
	// ConfigStatuses tracks the state of each slot.
	// +optional
	ConfigStatuses []MachineConfigSlotStatus `json:"configStatuses,omitempty"`

	// Total is the total number of configuration slots defined in the pool.
	// +optional
	Total int32 `json:"total,omitempty"`

	// Available is the number of slots currently free for allocation
	// (slots in the Available state).
	// +optional
	Available int32 `json:"available,omitempty"`

	// Allocated is the number of slots currently bound to a VSphereMachine
	// (slots in the InUse state).
	// +optional
	Allocated int32 `json:"allocated,omitempty"`

	// ConsumerRef is the workload controller (KubeadmControlPlane or MachineDeployment)
	// currently bound to this pool. Set automatically by the controller when a machine
	// allocates a slot. Cleared when the pool becomes fully reusable.
	// +optional
	ConsumerRef *corev1.ObjectReference `json:"consumerRef,omitempty"`

	// Conditions defines current state of the machine config pool.
	// +optional
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`

	// PersistentDiskStatuses tracks the observed state of each persistent disk,
	// keyed by (hostname, disk name). The controller backfills the vmdk path,
	// disk UUID, and actually-assigned SCSI unit here instead of writing them
	// back onto spec, and drives reclaim through the per-disk Phase.
	// +optional
	// +listType=map
	// +listMapKey=hostname
	// +listMapKey=name
	PersistentDiskStatuses []PersistentDiskStatus `json:"persistentDiskStatuses,omitempty"`

	// EphemeralDiskStatuses records the controller-observed SCSI unit number of
	// each non-persistent disk, keyed by (hostname, disk name). The unit is
	// assigned at clone time and reused (hydrated back onto the in-memory slot)
	// on the next reconcile so the guest disk table can address the disk. Unlike
	// PersistentDiskStatuses this carries no VolumePath/DiskUUID/Phase: ephemeral
	// disks are recreated with the VM and never reclaimed.
	// +optional
	// +listType=map
	// +listMapKey=hostname
	// +listMapKey=name
	EphemeralDiskStatuses []EphemeralDiskStatus `json:"ephemeralDiskStatuses,omitempty"`

	// v1beta2 groups all the fields that will be added or modified in
	// VSphereMachineConfigPool's status with the V1Beta2 version.
	// +optional
	V1Beta2 *VSphereMachineConfigPoolV1Beta2Status `json:"v1beta2,omitempty"`
}

// VSphereMachineConfigPoolV1Beta2Status groups all the fields that will be added or
// modified in VSphereMachineConfigPoolStatus with the V1Beta2 version.
// See https://github.com/kubernetes-sigs/cluster-api/blob/main/docs/proposals/20240916-improve-status-in-CAPI-resources.md for more context.
type VSphereMachineConfigPoolV1Beta2Status struct {
	// conditions represents the observations of a VSphereMachineConfigPool's current state.
	// Known condition types are Ready, MembersValid, MembersUnique, SlotAvailable,
	// PersistentDisksReady, ClusterRefReady and VCenterAvailable.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MachineConfigSlotState describes the allocation state of a slot.
type MachineConfigSlotState string

const (
	// MachineConfigSlotStateAvailable means the slot is free for allocation.
	MachineConfigSlotStateAvailable MachineConfigSlotState = "Available"
	// MachineConfigSlotStateInUse means the slot is currently bound to a VSphereMachine.
	MachineConfigSlotStateInUse MachineConfigSlotState = "InUse"
	// MachineConfigSlotStateReleased means the slot's machine is gone and the
	// slot is waiting out the release delay before its persistent resources
	// are reclaimed and it transitions back to Available.
	MachineConfigSlotStateReleased MachineConfigSlotState = "Released"
)

// MachineConfigSlotReclaimState describes the lifecycle of an in-flight reclaim task.
type MachineConfigSlotReclaimState string

const (
	// MachineConfigSlotReclaimStateRunning means a vCenter reclaim task is currently in flight.
	MachineConfigSlotReclaimStateRunning MachineConfigSlotReclaimState = "Running"
	// MachineConfigSlotReclaimStateFailed means the last reclaim task failed and
	// the controller is waiting out RetryAfter before the next attempt.
	MachineConfigSlotReclaimStateFailed MachineConfigSlotReclaimState = "Failed"
	// MachineConfigSlotReclaimStateCompleted means the slot's persistent resources
	// have been reclaimed successfully.
	MachineConfigSlotReclaimStateCompleted MachineConfigSlotReclaimState = "Completed"
)

// MachineConfigSlotStatus tracks the state of a single slot.
type MachineConfigSlotStatus struct {
	// Hostname matches the Hostname in the Spec.
	Hostname string `json:"hostname"`

	// State is the allocation state of the slot.
	// +kubebuilder:validation:Enum=Available;InUse;Released
	State MachineConfigSlotState `json:"state"`

	// MachineRef is the reference to the Machine currently using this slot.
	// +optional
	MachineRef *corev1.ObjectReference `json:"machineRef,omitempty"`

	// LastReleasedTime is the timestamp when the slot transitioned to Released.
	// +optional
	LastReleasedTime *metav1.Time `json:"lastReleasedTime,omitempty"`

	// ReclaimStatus tracks asynchronous reclaim progress for this slot.
	// +optional
	ReclaimStatus *MachineConfigSlotReclaimStatus `json:"reclaimStatus,omitempty"`
}

// MachineConfigSlotReclaimStatus tracks async reclaim state for a slot.
type MachineConfigSlotReclaimStatus struct {
	// TaskRef tracks the in-flight vCenter reclaim task for this slot.
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// State tracks the lifecycle of the reclaim task.
	// +kubebuilder:validation:Enum=Running;Failed;Completed
	// +optional
	State MachineConfigSlotReclaimState `json:"state,omitempty"`

	// VolumePath tracks the persistent disk currently being reclaimed.
	// +optional
	VolumePath string `json:"volumePath,omitempty"`

	// RetryAfter prevents tight retry loops after reclaim task failures.
	// +optional
	RetryAfter *metav1.Time `json:"retryAfter,omitempty"`

	// LastError stores the latest reclaim task failure.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// PersistentDiskPhase describes the lifecycle of a slot's persistent disk as
// observed by the controller.
type PersistentDiskPhase string

const (
	// PersistentDiskPhaseCreating means the disk has been requested but its
	// vmdk path / UUID have not been backfilled yet.
	PersistentDiskPhaseCreating PersistentDiskPhase = "Creating"
	// PersistentDiskPhaseAttached means the disk is created and attached to the
	// slot's VM.
	PersistentDiskPhaseAttached PersistentDiskPhase = "Attached"
	// PersistentDiskPhaseAvailable means the disk exists and is detached (its
	// VM was deleted) but has not been reclaimed; it can be re-attached when the
	// slot is reused.
	PersistentDiskPhaseAvailable PersistentDiskPhase = "Available"
	// PersistentDiskPhaseReclaiming means an asynchronous vCenter task to delete
	// the disk is in flight.
	PersistentDiskPhaseReclaiming PersistentDiskPhase = "Reclaiming"
	// PersistentDiskPhaseError means the last reclaim task failed; the controller
	// waits out RetryAfter before the next attempt.
	PersistentDiskPhaseError PersistentDiskPhase = "Error"
	// PersistentDiskPhaseReclaimed is the terminal tombstone: the disk's backing
	// vmdk has been deleted. The record is deliberately kept (not removed) with its
	// observed backing cleared, so the release-1 migration does not re-seed the disk
	// from spec's frozen VolumePath on the next reconcile and drive an endless
	// reclaim loop. It is overwritten by backfill when the slot is reused.
	PersistentDiskPhaseReclaimed PersistentDiskPhase = "Reclaimed"
)

// PersistentDiskStatus tracks the observed state of a single persistent disk.
// It replaces the controller-backfilled VolumePath/DiskUUID on spec and the
// per-slot ReclaimStatus, keyed by (Hostname, Name).
type PersistentDiskStatus struct {
	// Hostname is the slot this disk belongs to (matches MachineConfigSlot.Hostname).
	Hostname string `json:"hostname"`

	// Name is the disk name (matches PersistentDisk.Name within the slot).
	Name string `json:"name"`

	// VolumePath is the datastore path to the backing .vmdk, backfilled after
	// the disk is created.
	// +optional
	VolumePath string `json:"volumePath,omitempty"`

	// DiskUUID is the disk UUID, backfilled after the disk is created. The guest
	// uses it to identify and mount the disk.
	// +optional
	DiskUUID string `json:"diskUUID,omitempty"`

	// UnitNumber is the SCSI unit number the disk was actually attached at. When
	// spec does not pin a UnitNumber, this observed value is reused on the next
	// VM recreation to keep disk ordering stable.
	// +optional
	UnitNumber *int32 `json:"unitNumber,omitempty"`

	// Phase is the observed lifecycle of the disk: Creating -> Attached while in
	// use, Attached -> Available when the VM is released, and
	// Available -> Reclaiming -> Reclaimed as the backing is deleted (Error on a
	// failed reclaim, retried after RetryAfter). Reclaimed is a terminal tombstone:
	// the backing is gone but the record is kept so the release-1 migration does not
	// re-seed the disk from spec's frozen VolumePath.
	// +kubebuilder:validation:Enum=Creating;Attached;Available;Reclaiming;Reclaimed;Error
	// +optional
	Phase PersistentDiskPhase `json:"phase,omitempty"`

	// OwnerMachineUID is the UID of the Machine currently owning the disk. It is
	// preserved across VM delete/recreate so a rolling upgrade re-attaches the
	// disk to the rebuilt machine.
	// +optional
	OwnerMachineUID string `json:"ownerMachineUID,omitempty"`

	// OwnerMachineName is the name of the Machine currently owning the disk.
	// +optional
	OwnerMachineName string `json:"ownerMachineName,omitempty"`

	// TaskRef tracks the in-flight vCenter reclaim task for this disk. CAPV
	// reclaims disks asynchronously, so the task moniker is polled across
	// reconciles (DCS/HCS call their SDKs synchronously and have no equivalent).
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// RetryAfter prevents tight retry loops after a failed reclaim task.
	// +optional
	RetryAfter *metav1.Time `json:"retryAfter,omitempty"`

	// LastError stores the latest reclaim failure, surfaced when Phase is Error.
	// +optional
	LastError string `json:"lastError,omitempty"`

	// LastTransitionTime is the time Phase last changed.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// EphemeralDiskStatus records the controller-observed SCSI unit number of a
// single non-persistent disk, keyed by (Hostname, Name). It deliberately omits
// VolumePath/DiskUUID/Phase: an ephemeral disk's backing vmdk is always newly
// created and deleted with the VM, so there is nothing to reclaim and no
// lifecycle to track. The recorded UnitNumber is hydrated back onto the
// in-memory slot each reconcile so the guest disk table can address the disk.
type EphemeralDiskStatus struct {
	// Hostname is the slot this disk belongs to (matches MachineConfigSlot.Hostname).
	Hostname string `json:"hostname"`

	// Name is the disk name (matches EphemeralDisk.Name within the slot).
	Name string `json:"name"`

	// UnitNumber is the SCSI unit number the disk was attached at, assigned by
	// the controller at clone time and reused on the next VM recreation to keep
	// guest disk addressing stable.
	// +optional
	UnitNumber *int32 `json:"unitNumber,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=vspheremachineconfigpools,scope=Namespaced,categories=cluster-api
// +kubebuilder:printcolumn:name="Total",type="integer",JSONPath=".status.total",description="Total configuration slots in the pool"
// +kubebuilder:printcolumn:name="Available",type="integer",JSONPath=".status.available",description="Slots free for allocation"
// +kubebuilder:printcolumn:name="Allocated",type="integer",JSONPath=".status.allocated",description="Slots bound to a machine"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time duration since creation"

// VSphereMachineConfigPool is the Schema for the vspheremachineconfigpools API.
type VSphereMachineConfigPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VSphereMachineConfigPoolSpec   `json:"spec,omitempty"`
	Status VSphereMachineConfigPoolStatus `json:"status,omitempty"`
}

// GetConditions returns the conditions for a VSphereMachineConfigPool.
func (r *VSphereMachineConfigPool) GetConditions() clusterv1.Conditions {
	return r.Status.Conditions
}

// SetConditions sets the conditions on a VSphereMachineConfigPool.
func (r *VSphereMachineConfigPool) SetConditions(conditions clusterv1.Conditions) {
	r.Status.Conditions = conditions
}

// GetV1Beta2Conditions returns the set of v1beta2 conditions for this object.
func (r *VSphereMachineConfigPool) GetV1Beta2Conditions() []metav1.Condition {
	if r.Status.V1Beta2 == nil {
		return nil
	}
	return r.Status.V1Beta2.Conditions
}

// SetV1Beta2Conditions sets the v1beta2 conditions for this object.
func (r *VSphereMachineConfigPool) SetV1Beta2Conditions(conditions []metav1.Condition) {
	if r.Status.V1Beta2 == nil {
		r.Status.V1Beta2 = &VSphereMachineConfigPoolV1Beta2Status{}
	}
	r.Status.V1Beta2.Conditions = conditions
}

// +kubebuilder:object:root=true

// VSphereMachineConfigPoolList contains a list of VSphereMachineConfigPool.
type VSphereMachineConfigPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VSphereMachineConfigPool `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &VSphereMachineConfigPool{}, &VSphereMachineConfigPoolList{})
}
