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
	"k8s.io/apimachinery/pkg/runtime"
)

// VSphereResourcePoolSpec defines the desired state of VSphereResourcePool.
type VSphereResourcePoolSpec struct {
	// Datacenter is the default vSphere datacenter for slots in this pool.
	// It is used when a slot does not define its own Datacenter.
	// +optional
	Datacenter string `json:"datacenter,omitempty"`

	// Server is the vCenter server address.
	// +optional
	Server string `json:"server,omitempty"`

	// Thumbprint is the vCenter certificate thumbprint.
	// +optional
	Thumbprint string `json:"thumbprint,omitempty"`

	// Resources is the list of pre-defined resource slots.
	Resources []ResourceSlot `json:"resources"`

	// ReleaseDelayHours is the time to wait before marking a released slot as "Available" for any machine.
	// During this period, the slot can only be reused if specifically requested or via priority reuse.
	// Default is 24.
	// +optional
	ReleaseDelayHours *int `json:"releaseDelayHours,omitempty"`

	// ConsumerRef is the single workload controller currently allowed to consume this pool.
	// Supported kinds are KubeadmControlPlane and MachineDeployment.
	// This is a logical binding only. It must not be translated into ownerReferences
	// and must not imply cascading delete semantics.
	// +optional
	ConsumerRef *corev1.ObjectReference `json:"consumerRef,omitempty"`
}

// ResourceSlot defines a single resource slot in the pool.
type ResourceSlot struct {
	// Hostname is the unique identifier for this slot and will be assigned to the VM.
	Hostname string `json:"hostname"`

	// Datacenter is the vSphere datacenter for this slot.
	// If set, it takes precedence over VSphereResourcePool.spec.datacenter.
	// If unset, the pool-level Datacenter acts as the default.
	// +optional
	Datacenter string `json:"datacenter,omitempty"`

	// Network configurations.
	// +optional
	Network []NetworkConfig `json:"network,omitempty"`

	// PersistentDisks that survive VM deletion.
	// +optional
	PersistentDisks []PersistentDisk `json:"persistentDisks,omitempty"`
}

// NetworkConfig defines the network configuration for a slot.
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

	// VolumePath is backfilled by the controller after the disk is created.
	// For vSphere, this is usually the datastore path to the .vmdk file.
	// +optional
	VolumePath string `json:"volumePath,omitempty"`

	// DiskUUID is backfilled by the controller.
	// +optional
	DiskUUID string `json:"diskUUID,omitempty"`
}

// VSphereResourcePoolStatus defines the observed state of VSphereResourcePool.
type VSphereResourcePoolStatus struct {
	// ResourceStatuses tracks the state of each slot.
	// +optional
	ResourceStatuses []ResourceSlotStatus `json:"resourceStatuses,omitempty"`
}

// ResourceSlotStatus tracks the state of a single slot.
type ResourceSlotStatus struct {
	// Hostname matches the Hostname in the Spec.
	Hostname string `json:"hostname"`

	// State can be: Available, InUse, Released
	State string `json:"state"`

	// MachineRef is the reference to the Machine currently using this slot.
	// +optional
	MachineRef *corev1.ObjectReference `json:"machineRef,omitempty"`

	// LastReleasedTime is the timestamp when the slot transitioned to Released.
	// +optional
	LastReleasedTime *metav1.Time `json:"lastReleasedTime,omitempty"`

	// ReclaimStatus tracks asynchronous reclaim progress for this slot.
	// +optional
	ReclaimStatus *ResourceSlotReclaimStatus `json:"reclaimStatus,omitempty"`
}

// ResourceSlotReclaimStatus tracks async reclaim state for a slot.
type ResourceSlotReclaimStatus struct {
	// TaskRef tracks the in-flight vCenter reclaim task for this slot.
	// +optional
	TaskRef string `json:"taskRef,omitempty"`

	// State tracks the lifecycle of the reclaim task.
	// +optional
	State string `json:"state,omitempty"`

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

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=vsphereresourcepools,scope=Namespaced,categories=cluster-api

// VSphereResourcePool is the Schema for the vsphereresourcepools API.
type VSphereResourcePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VSphereResourcePoolSpec   `json:"spec,omitempty"`
	Status VSphereResourcePoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VSphereResourcePoolList contains a list of VSphereResourcePool.
type VSphereResourcePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VSphereResourcePool `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &VSphereResourcePool{}, &VSphereResourcePoolList{})
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *VSphereResourcePool) DeepCopyInto(out *VSphereResourcePool) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new VSphereResourcePool.
func (in *VSphereResourcePool) DeepCopy() *VSphereResourcePool {
	if in == nil {
		return nil
	}
	out := new(VSphereResourcePool)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *VSphereResourcePool) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *VSphereResourcePoolList) DeepCopyInto(out *VSphereResourcePoolList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]VSphereResourcePool, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new VSphereResourcePoolList.
func (in *VSphereResourcePoolList) DeepCopy() *VSphereResourcePoolList {
	if in == nil {
		return nil
	}
	out := new(VSphereResourcePoolList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *VSphereResourcePoolList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *VSphereResourcePoolSpec) DeepCopyInto(out *VSphereResourcePoolSpec) {
	*out = *in
	if in.Resources != nil {
		in, out := &in.Resources, &out.Resources
		*out = make([]ResourceSlot, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.ReleaseDelayHours != nil {
		in, out := &in.ReleaseDelayHours, &out.ReleaseDelayHours
		*out = new(int)
		**out = **in
	}
	if in.ConsumerRef != nil {
		in, out := &in.ConsumerRef, &out.ConsumerRef
		*out = new(corev1.ObjectReference)
		**out = **in
	}
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *ResourceSlot) DeepCopyInto(out *ResourceSlot) {
	*out = *in
	if in.Network != nil {
		in, out := &in.Network, &out.Network
		*out = make([]NetworkConfig, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	if in.PersistentDisks != nil {
		in, out := &in.PersistentDisks, &out.PersistentDisks
		*out = make([]PersistentDisk, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *NetworkConfig) DeepCopyInto(out *NetworkConfig) {
	*out = *in
	if in.DNS != nil {
		in, out := &in.DNS, &out.DNS
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *PersistentDisk) DeepCopyInto(out *PersistentDisk) {
	*out = *in
	if in.UnitNumber != nil {
		in, out := &in.UnitNumber, &out.UnitNumber
		*out = new(int32)
		**out = **in
	}
	if in.MountOptions != nil {
		in, out := &in.MountOptions, &out.MountOptions
		*out = make([]string, len(*in))
		copy(*out, *in)
	}
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *VSphereResourcePoolStatus) DeepCopyInto(out *VSphereResourcePoolStatus) {
	*out = *in
	if in.ResourceStatuses != nil {
		in, out := &in.ResourceStatuses, &out.ResourceStatuses
		*out = make([]ResourceSlotStatus, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *ResourceSlotStatus) DeepCopyInto(out *ResourceSlotStatus) {
	*out = *in
	if in.MachineRef != nil {
		in, out := &in.MachineRef, &out.MachineRef
		*out = new(corev1.ObjectReference)
		**out = **in
	}
	if in.LastReleasedTime != nil {
		in, out := &in.LastReleasedTime, &out.LastReleasedTime
		*out = (*in).DeepCopy()
	}
	if in.ReclaimStatus != nil {
		in, out := &in.ReclaimStatus, &out.ReclaimStatus
		*out = new(ResourceSlotReclaimStatus)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *ResourceSlotReclaimStatus) DeepCopyInto(out *ResourceSlotReclaimStatus) {
	*out = *in
	if in.RetryAfter != nil {
		in, out := &in.RetryAfter, &out.RetryAfter
		*out = (*in).DeepCopy()
	}
}
