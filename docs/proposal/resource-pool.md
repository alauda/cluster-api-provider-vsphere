# Proposal: Resource Pool for CAPV

## Goals
1. Provide a mechanism to assign fixed IP addresses, hostnames, and persistent disks to VMs in CAPV.
2. Support rolling updates with `maxSurge=0` where resources (IP, Disk) are reused by the new VM after the old one is deleted.
3. Support delayed deletion of released resources (24-48 hours) to allow for manual recovery or late reuse.
4. Prioritize reuse of "Released" resources over "Available" ones during scaling or upgrades.

## CRD Definitions

### VSphereResourcePool
The `VSphereResourcePool` defines a pool of "slots", where each slot contains a fixed IP, hostname, and a set of persistent disks.

```go
type VSphereResourcePool struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   VSphereResourcePoolSpec   `json:"spec,omitempty"`
    Status VSphereResourcePoolStatus `json:"status,omitempty"`
}

type VSphereResourcePoolSpec struct {
    // Datacenter is the name of the vSphere datacenter.
    Datacenter string `json:"datacenter,omitempty"`

    // Server is the vCenter server address.
    Server string `json:"server,omitempty"`

    // Thumbprint is the vCenter certificate thumbprint.
    Thumbprint string `json:"thumbprint,omitempty"`

    // Resources is the list of pre-defined resource slots.
    Resources []ResourceSlot `json:"resources"`
    
    // ReleaseDelayHours is the time to wait before marking a released slot as "Available" for any machine.
    // During this period, the slot can only be reused if specifically requested or via priority reuse.
    // Default is 24.
    ReleaseDelayHours *int `json:"releaseDelayHours,omitempty"`
}

type ResourceSlot struct {
    // Hostname is the unique identifier for this slot and will be assigned to the VM.
    Hostname string `json:"hostname"`
    
    // Network configurations.
    Network []NetworkConfig `json:"network,omitempty"`
    
    // PersistentDisks that survive VM deletion.
    PersistentDisks []PersistentDisk `json:"persistentDisks,omitempty"`
}

type NetworkConfig struct {
    // NetworkName is the name of the vSphere network (PortGroup or DVPortGroup).
    NetworkName string `json:"networkName"`

    // IPv4 configuration
    IP      string `json:"ip,omitempty"`
    Gateway string `json:"gateway,omitempty"`

    // IPv6 configuration
    IPv6        string `json:"ipv6,omitempty"`
    IPv6Gateway string `json:"ipv6Gateway,omitempty"`

    DNS []string `json:"dns,omitempty"`
}

type PersistentDisk struct {
    // Name is the disk name.
    Name string `json:"name"`
    // SizeGiB is the disk size.
    SizeGiB int32 `json:"sizeGiB"`
    // Datastore is the vSphere datastore name.
    Datastore string `json:"datastore,omitempty"`
    // StoragePolicy is the vSphere storage policy name.
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
    VolumePath string `json:"volumePath,omitempty"`
    
    // DiskUUID is backfilled by the controller.
    DiskUUID string `json:"diskUUID,omitempty"`
}

type VSphereResourcePoolStatus struct {
    // ResourceStatuses tracks the state of each slot.
    ResourceStatuses []ResourceSlotStatus `json:"resourceStatuses,omitempty"`
}

type ResourceSlotStatus struct {
    // Hostname matches the Hostname in the Spec.
    Hostname string `json:"hostname"`
    
    // State can be: Available, InUse, Released
    State string `json:"state"`
    
    // MachineRef is the reference to the Machine currently using this slot.
    MachineRef *corev1.ObjectReference `json:"machineRef,omitempty"`
    
    // LastReleasedTime is the timestamp when the MachineRef was cleared.
    LastReleasedTime *metav1.Time `json:"lastReleasedTime,omitempty"`

    // ReclaimTaskRef tracks the in-flight vCenter reclaim task for this slot.
    ReclaimTaskRef string `json:"reclaimTaskRef,omitempty"`

    // ReclaimTaskState tracks the state of the in-flight reclaim task.
    ReclaimTaskState string `json:"reclaimTaskState,omitempty"`

    // ReclaimVolumePath identifies the current disk being reclaimed.
    ReclaimVolumePath string `json:"reclaimVolumePath,omitempty"`

    // RetryAfter prevents tight retry loops after reclaim task failures.
    RetryAfter *metav1.Time `json:"retryAfter,omitempty"`

    // LastReclaimError stores the most recent reclaim failure.
    LastReclaimError string `json:"lastReclaimError,omitempty"`
}
```

### VSphereMachine
The `VSphereMachineSpec` will be extended to support referencing a resource pool.

```go
type VSphereMachineSpec struct {
    // ... existing fields ...

    // ResourcePoolRef is a reference to a VSphereResourcePool to use for this machine.
    // +optional
    ResourcePoolRef *corev1.ObjectReference `json:"resourcePoolRef,omitempty"`
}
```

## Workflow

### 1. Machine Provisioning
When a `VSphereMachine` is created and it references a `VSphereResourcePool`:
- The CAPV controller (or a dedicated pool controller) looks for a `ResourceSlot` in the pool.
- Priority:
    1. A slot previously used by a machine with the same "identity" (if identifiable).
    2. A "Released" slot (to reuse existing disks/IPs).
    3. An "Available" slot.
- The controller marks the slot as `InUse` and updates `VSphereResourcePool.Status.ResourceStatuses` with the `MachineRef` pointing to the `VSphereMachine`.
- The `VSphereVM` is created using the standard generated name (e.g., with random suffixes).
- Slot data is merged into the `VSphereVM` before backend VM creation:
    - `Hostname` from the slot is used as the guest hostname.
    - `PersistentDisks` from the slot are merged into `VSphereVM.spec.dataDisks`.
    - `Network` from the slot is merged into `VSphereVM.spec.network.devices`.
- **IP precedence**:
    - If the slot provides `IP` or `IPv6`, CAPV writes the slot-provided IP, gateway, and DNS values into the VM spec before create, and clears `AddressesFromPools` for that NIC. This means slot-defined addressing takes precedence over CAPV IPAM.
    - If the slot does not provide an IP, CAPV preserves the original network configuration and continues to use the existing CAPV DHCP / static IP / IPAM logic.
- **Creation gate for CAPV IPAM**:
    - If `AddressesFromPools` remains configured after slot merge, CAPV creates and waits for `IPAddressClaim` objects to be fulfilled before creating the backend VM.
    - This guarantees that CAPV-managed IP allocation completes before VM creation, while still allowing slot-defined IPs to bypass IPAM entirely.
- **Metadata Injection**: 
    - The controller injects disk metadata (UnitNumber, MountPath, FSFormat) into the VM's `ExtraConfig` under `guestinfo.metadata`.
    - **Hostname Overriding**: Instead of using the `VSphereVM` name, the controller explicitly passes the **slot's Hostname** to the metadata generator. This ensures `local-hostname` and `instance-id` in `guestinfo.metadata` match the slot definition, which `cloud-init` then uses to set the OS hostname.
- If it's a `PersistentDisk`, the controller ensures the disk exists. If `VolumePath` is empty, it creates a new disk and fills `VolumePath`. If not empty, it attaches the existing disk.
- If a persistent disk is reused and its SCSI `UnitNumber` was not yet recorded in the slot spec, the controller backfills it and persists it to the resource pool spec.

### 2. Rolling Update (maxSurge=0)
- `MachineDeployment` deletes `Machine-v1`.
- `VSphereMachine-v1` is deleted, and its `VSphereVM` is destroyed.
- **Crucially**: The `PersistentDisk` is **detached** but **NOT deleted** from the datastore.
- The `ResourceSlot` status is updated to `State: Released`, `LastReleasedTime: now`.
- `MachineDeployment` creates `Machine-v2`.
- `Machine-v2` requests a slot. The controller prefers `Released` slots before `Available` slots, so the released slot is reused first.
- `Machine-v2` reuses the IP, Hostname, and attaches the existing `PersistentDisk` via `VolumePath`.
- If the owner `VSphereMachine` object is already gone by the time the `VSphereVM` delete path runs, CAPV can still find the pool from the slot status binding and safely release the slot.

### 3. Resource Cleanup (Automatic Reclaim)
- A dedicated `VSphereResourcePool` controller checks slots in the `Released` state.
- **Retention Period**: By default, a released slot is "reserved" for its previous owner (or for manual recovery) for a period defined by `ReleaseDelayHours` (e.g., 24-48 hours).
- **Reclamation**: If `now - LastReleasedTime > ReleaseDelayHours`:
    - **Disk Cleanup**: The controller automatically deletes the associated `PersistentDisk` from the vSphere datastore to reclaim space.
    - **Slot Reset**: The `VolumePath` and `DiskUUID` in the `PersistentDisk` spec are cleared.
    - **State Transition**: The slot state is set back to `Available`, making it "clean" and ready to be picked up by any new machine (including those that don't need the previous data).
- **Async reclaim task model**:
    - Reclaim is asynchronous. The controller starts at most one vCenter task per slot and records it in `ReclaimTaskRef`.
    - `ReclaimVolumePath` tracks the current disk being reclaimed for that slot.
    - If a slot has multiple persistent disks, they are reclaimed one by one across reconciles, never with multiple concurrent reclaim tasks for the same slot.
    - If a reclaim task fails, the controller records `LastReclaimError` and uses `RetryAfter` to avoid tight retry loops.

### 4. Resource Pool Deletion
- Deleting a `VSphereResourcePool` is blocked by a finalizer until all slots are safely released.
- "Safe" means:
    - Any slot still bound to an existing `VSphereMachine` blocks deletion and surfaces an error.
    - If the referenced `VSphereMachine` no longer exists, the slot may transition to `Released` and continue reclaim.
    - Any released slot with reclaimable persistent disks must finish reclaim before the pool finalizer is removed.
- The finalizer is removed only after no live machine is using a slot and all reclaim work is complete.

## Evaluation of docs/proposal/proposal.go

The original fields in `proposal.go` are largely sufficient but require vSphere-specific adaptations:
- `DatastoreUrn/Name` -> Simplified to `Datastore` (string) as commonly used in CAPV.
- `VolumeUrn` -> Replaced with `VolumePath` and `DiskUUID` to align with vSphere's file-based storage and identification.
- `SequenceNum` -> Renamed to `UnitNumber` to align with vSphere SCSI controller terminology, which is critical for consistent disk ordering.
- `DVSwitchName/PortGroupName` -> Unified into `NetworkName`, which is the standard CAPV field for both standard and distributed portgroups.
- `AdditionNic` -> Integrated into `ResourceSlot.Network` as a list of `NetworkConfig`.

## MachineDeployment Configuration
To achieve the desired behavior, the `MachineDeployment` must be configured with:
```yaml
strategy:
  type: RollingUpdate
  rollingUpdate:
    maxSurge: 0
    maxUnavailable: 1
```

## Implementation Notes
- **SCSI Controller Constraint**: For the cloud-init metadata injection to work correctly, all persistent disks in the resource pool must be attached to the **first SCSI controller** (address `0000:00:10.0`). The current template assumes this PCI address when generating `/dev/disk/by-path/` paths.
- **Metadata-Driven Mounting**: Instead of manual commands, the system relies on `cloud-init` (or `ignition`) to read the injected metadata and perform formatting/mounting.
    - `cloud-init`'s `disk_setup` and `fs_setup` can be dynamically generated based on the `PersistentDisk` list.
    - **UnitNumber Mapping**: Since `UnitNumber` is fixed, the mapping to OS device nodes is predictable on most Linux distributions:

| UnitNumber | Linux Device (Typical) | Note |
| :--- | :--- | :--- |
| 0 | `/dev/sda` | Usually the OS/Root disk |
| 1 | `/dev/sdb` | First data disk |
| 2 | `/dev/sdc` | Second data disk |
| 7 | N/A | Reserved by vSphere SCSI controller |

- **Resource Selection Logic**:
    - The controller manages the binding between `VSphereMachine` and `ResourceSlot` within the `VSphereResourcePool` status.
    - The controller prefers `Released` slots (to maximize reuse) over `Available` ones.
    - On initial provisioning, if allocation is serial, slot selection follows the order of `spec.resources`.
- **Controller Responsibilities**:
    - The `VSphereVM` controller needs to be aware of `PersistentDisks` that should not be deleted upon VM destruction.
    - The `VSphereVM` controller is responsible for gating backend VM creation on CAPV IPAM fulfillment when slot IPs are not provided.
    - A dedicated controller for `VSphereResourcePool` manages status tracking, delayed reclaim, async reclaim task polling, and safe deletion semantics.
