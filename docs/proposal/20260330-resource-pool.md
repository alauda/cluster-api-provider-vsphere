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
    // ClusterRef references the CAPI Cluster (in the same namespace) whose
    // VSphereCluster provides vCenter server, thumbprint, and credential
    // chain (IdentityRef) for this pool's vCenter operations (e.g. disk
    // reclaim). Required. Can only be changed when consumerRef is nil.
    // The pool will not reconcile until the referenced Cluster and its
    // VSphereCluster infrastructure are available.
    ClusterRef corev1.ObjectReference `json:"clusterRef"`

    // Datacenter is the default vSphere datacenter for slots in this pool.
    // It is used when a slot does not define its own Datacenter.
    Datacenter string `json:"datacenter,omitempty"`

    // Resources is the list of pre-defined resource slots.
    Resources []ResourceSlot `json:"resources"`
    
    // ReleaseDelayHours is the time to wait before marking a released slot as "Available" for any machine.
    // During this period, the slot can only be reused if specifically requested or via priority reuse.
    // Default is 24.
    ReleaseDelayHours *int `json:"releaseDelayHours,omitempty"`

    // ConsumerRef is the single workload controller currently allowed to consume this pool.
    // Supported kinds are KubeadmControlPlane and MachineDeployment.
    // This is a logical binding only. It must not be translated into ownerReferences
    // and must not imply cascading delete semantics.
    ConsumerRef *corev1.ObjectReference `json:"consumerRef,omitempty"`
}

type ResourceSlot struct {
    // Hostname is the unique identifier for this slot and will be assigned to the VM.
    Hostname string `json:"hostname"`

    // Datacenter is the vSphere datacenter for this slot.
    // If set, it takes precedence over VSphereResourcePool.spec.datacenter.
    // If unset, the pool-level Datacenter acts as the default.
    Datacenter string `json:"datacenter,omitempty"`
    
    // Network configurations.
    Network []NetworkConfig `json:"network,omitempty"`
    
    // PersistentDisks that survive VM deletion.
    PersistentDisks []PersistentDisk `json:"persistentDisks,omitempty"`
}

type NetworkConfig struct {
    // NetworkName is the name of the vSphere network (PortGroup or DVPortGroup).
    NetworkName string `json:"networkName"`

    // DeviceName explicitly assigns a guest OS interface name to the network device.
    // +optional
    DeviceName string `json:"deviceName,omitempty"`

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

    // Conditions defines current state of the resource pool.
    // Supported condition types:
    //   - ClusterRefReady: the referenced Cluster and VSphereCluster are found and available.
    //   - VCenterAvailable: vCenter credentials can be resolved and a session can be established.
    Conditions clusterv1.Conditions `json:"conditions,omitempty"`
}

type ResourceSlotStatus struct {
    // Hostname matches the Hostname in the Spec.
    Hostname string `json:"hostname"`
    
    // State can be: Available, InUse, Released
    State string `json:"state"`
    
    // MachineRef is the reference to the VSphereMachine currently using this slot.
    MachineRef *corev1.ObjectReference `json:"machineRef,omitempty"`
    
    // LastReleasedTime is the timestamp when the slot transitioned to Released.
    LastReleasedTime *metav1.Time `json:"lastReleasedTime,omitempty"`

    // ReclaimStatus tracks asynchronous reclaim progress for this slot.
    ReclaimStatus *ResourceSlotReclaimStatus `json:"reclaimStatus,omitempty"`
}

type ResourceSlotReclaimStatus struct {
    // TaskRef tracks the in-flight vCenter reclaim task for this slot.
    TaskRef string `json:"taskRef,omitempty"`

    // State tracks the state of the in-flight reclaim task.
    State string `json:"state,omitempty"`

    // VolumePath identifies the current disk being reclaimed.
    VolumePath string `json:"volumePath,omitempty"`

    // RetryAfter prevents tight retry loops after reclaim task failures.
    RetryAfter *metav1.Time `json:"retryAfter,omitempty"`

    // LastError stores the most recent reclaim failure.
    LastError string `json:"lastError,omitempty"`
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

## Consumer Binding Model

`VSphereResourcePool` is intended to be consumed by exactly one workload controller at a time:

- one `KubeadmControlPlane`, or
- one `MachineDeployment`

This relationship is modeled by `VSphereResourcePool.spec.consumerRef`.

Important semantics:

- `consumerRef` is only a logical binding used for validation and allocation control.
- CAPV must not set Kubernetes `ownerReferences` from the pool to the consumer.
- Deleting the consumer must not trigger cascading deletion of the pool.
- A pool may later be rebound to a different `MachineDeployment` or `KubeadmControlPlane`, but only after the previous consumer has fully released the pool.
- A pool may be created with `consumerRef` unset. This is the normal initial state before any `KubeadmControlPlane` or `MachineDeployment` claims the pool.

### Allowed Reference Shape

`spec.consumerRef` reuses `corev1.ObjectReference` and is constrained as follows:

- `APIVersion` must be one of:
  - `controlplane.cluster.x-k8s.io/v1beta1`
  - `cluster.x-k8s.io/v1beta1`
- `Kind` must be one of:
  - `KubeadmControlPlane`
  - `MachineDeployment`
- `Namespace` may be omitted (defaults to the `VSphereResourcePool` namespace). If explicitly set, it must equal the pool namespace. Cross-namespace consumer references are not supported.
- `Name` must be non-empty.
- CAPV may read `UID` when present, but the primary identity for admission and lookup remains `APIVersion/Kind/Namespace/Name`.

### Binding and Unbinding Rules

- If `consumerRef` is unset, the pool is currently unbound and may be claimed by a single consumer.
- If `consumerRef` is set, only `VSphereMachine` objects belonging to that consumer may allocate slots from the pool.
- If the referenced consumer still exists, the binding remains active.
- If the referenced consumer has been deleted, CAPV must keep the binding until all slots in the pool are fully reusable.
- "Fully reusable" means CAPV can infer from `status.resourceStatuses` and `spec.resources` that:
  - every slot status is either absent or `Available`
  - no slot is `InUse`
  - no slot is `Released`
  - no slot has an in-flight reclaim task in `reclaimStatus.taskRef`
  - no slot has a failed reclaim retry window in `reclaimStatus.retryAfter`
  - no slot still has reclaimable persistent-disk backing in `spec.resources[].persistentDisks[].volumePath`
- Only after all slots are fully reusable may CAPV clear `spec.consumerRef`.
- This prevents a newly bound consumer from racing with release/reclaim work still belonging to the previous consumer.

### Rebinding

- A pool may be rebound to a different `MachineDeployment` or `KubeadmControlPlane`.
- Rebinding is allowed only when the previous binding has been fully cleared.
- CAPV must reject rebinding while the pool still has active `InUse` or `Released` slots, in-flight reclaim work, or reclaimable disk backing.

### Binding Establishment

- A `KubeadmControlPlane` or `MachineDeployment` declares its intended pool through the `VSphereMachineTemplate` it references.
- Admission validates whether that declaration is compatible with the current pool state.
- The actual binding is established by CAPV controller logic, not by webhook mutation and not by slot allocation.
- CAPV establishes the binding when the first `VSphereMachine` for that consumer reconciles and attempts to use the pool.
- Binding must use optimistic concurrency on `VSphereResourcePool.metadata.resourceVersion`.
  If two consumers race for the same unbound pool:
  - both may pass webhook validation when `consumerRef` is still empty
  - only one controller update may successfully write `spec.consumerRef`
  - the loser must re-read the pool, detect that it is now bound to another consumer, and fail
- Therefore, configuration reference is not equivalent to a successful binding. The source of truth for the active binding is `VSphereResourcePool.spec.consumerRef`.

### Mutability Rules

- `spec.consumerRef` may be added to an unbound pool.
- `spec.consumerRef` may be cleared (by CAPV controller or manually) only after the previous consumer no longer exists and the pool is fully reusable.
- Clearing `spec.consumerRef` is rejected while the referenced consumer still exists or while the pool is not fully reusable.
- User-initiated rebinding from one consumer to another should be rejected unless the pool is already unbound.
- `spec.resources[*].hostname` should remain immutable after creation.
- `spec.resources[*].network` and `spec.resources[*].persistentDisks` remain declarative inputs, except for controller-managed backfill fields such as `volumePath`, `diskUUID`, and `unitNumber`.

## Workflow

### 1. Machine Provisioning
When a `VSphereMachine` is created and it references a `VSphereResourcePool`:
- The CAPV controller (or a dedicated pool controller) looks for a `ResourceSlot` in the pool.
- Before slot selection, CAPV validates that the machine is allowed to consume the referenced pool:
    - If `VSphereResourcePool.spec.consumerRef` is set, the machine's effective consumer must match it.
    - Effective consumer is derived from the machine owner chain:
      - control-plane machines resolve to their `KubeadmControlPlane`
      - worker machines resolve to their `MachineDeployment`
    - If the effective consumer does not match the pool `consumerRef`, reconciliation fails.
- If `VSphereResourcePool.spec.consumerRef` is empty, CAPV attempts to bind the pool to the machine's effective consumer using optimistic concurrency on the pool `resourceVersion`.
  - If the update succeeds, the machine may continue allocation.
  - If the update conflicts, CAPV re-reads the pool.
  - If the pool is now bound to another consumer, reconciliation fails.
  - If the pool is still unbound, CAPV may retry.
- Before slot selection, CAPV resolves the machine's desired datacenter:
    - If `VSphereMachine.spec.datacenter` / template-derived clone spec datacenter is set, that becomes a required datacenter constraint.
    - If a `FailureDomain` is set, CAPV must still resolve it successfully. The failure domain contributes an additional allowed-datacenter constraint.
    - If both template / machine datacenter and failure-domain datacenter are present, the selected slot must satisfy both constraints.
    - If template / machine datacenter is empty and a `FailureDomain` is set, slot selection is filtered by the datacenter(s) allowed by the resolved failure domain.
    - If neither template / machine datacenter nor `FailureDomain` provides a datacenter, CAPV uses the selected slot's resolved datacenter.
- CAPV resolves a slot datacenter as follows:
    - `ResourceSlot.datacenter` is matched first.
    - If `slot.datacenter` is empty, CAPV falls back to `VSphereResourcePool.spec.datacenter`.
- A slot is eligible only if its resolved datacenter satisfies all active constraints:
    - The template / machine datacenter, when specified.
    - The resolved `FailureDomain` datacenter set, when specified.
    - If no slot in the pool satisfies the active constraints, reconciliation fails instead of silently choosing a slot from a different datacenter.
- Priority:
    1. A slot already assigned to the same `VSphereMachine` instance (matched by `UID`, or by `Name/Namespace` as an idempotency fallback before `UID` is settled), subject to the active datacenter constraints above.
    2. The first `Released` slot in `spec.resources` order that satisfies the active datacenter constraints, if any.
    3. The first `Available` (or uninitialized) slot in `spec.resources` order that satisfies the active datacenter constraints, if any.
- The controller marks the slot as `InUse` and updates `VSphereResourcePool.Status.ResourceStatuses` with the `MachineRef` pointing to the `VSphereMachine`.
- The `VSphereVM` is created using the standard generated name (e.g., with random suffixes).
- Slot data is merged into the `VSphereVM` before backend VM creation:
    - If template / machine datacenter is set, CAPV preserves it on the resulting `VSphereVM`, but the selected slot must also satisfy any configured `FailureDomain`.
    - If template / machine datacenter is not set, CAPV resolves `Datacenter` from the selected slot first. If `slot.datacenter` is empty, CAPV falls back to `VSphereResourcePool.spec.datacenter`.
    - When template / machine datacenter is not set, CAPV backfills the final resolved datacenter onto `VSphereMachine.spec.datacenter` after slot allocation so later reconcile steps observe the explicit resolved value.
    - `Hostname` from the slot is used as the guest hostname.
    - `PersistentDisks` from the slot are merged into `VSphereVM.spec.dataDisks`.
    - `Network` from the slot is merged into `VSphereVM.spec.network.devices`.
- **IP precedence**:
    - If the slot provides `IP` or `IPv6`, CAPV writes the slot-provided IP, gateway, and DNS values into the VM spec before create, disables `DHCP4`/`DHCP6` accordingly, and clears `AddressesFromPools` for that NIC. This means slot-defined addressing takes precedence over CAPV IPAM and DHCP.
    - If the slot does not provide an IP, CAPV preserves the original network configuration and continues to use the existing CAPV DHCP / static IP / IPAM logic.
- **Creation gate for CAPV IPAM**:
    - If `AddressesFromPools` remains configured after slot merge, CAPV creates and waits for `IPAddressClaim` objects to be fulfilled before creating the backend VM.
    - This guarantees that CAPV-managed IP allocation completes before VM creation, while still allowing slot-defined IPs to bypass IPAM entirely.
- **Metadata Injection**: 
    - **Hostname Overriding**: Instead of using the `VSphereVM` name, the controller explicitly passes the **slot's Hostname** to the metadata generator. This ensures `local-hostname` and `instance-id` in `guestinfo.metadata` match the slot definition, which `cloud-init` then uses to set the OS hostname.
- **Slot hostname annotation**:
    - The controller also writes `infrastructure.cluster.x-k8s.io/resource-slot-hostname=<slot.Hostname>` onto the `VSphereVM`.
    - This provides a durable hint that allows the `VSphereVM` reconcile path to recover the slot definition from the pool spec even if the slot-to-machine status binding is temporarily unavailable.
- **User-data augmentation**:
    - CAPV merges persistent-disk cloud-config fragments and kubelet serving certificate configuration into the VM's `guestinfo.userdata` (ExtraConfig) before first power-on.
    - Persistent-disk cloud-config includes `write_files` (e.g., `/etc/capv/persistent-disks.tsv`), helper scripts, systemd services, and related `disk_setup`/`fs_setup`/`mounts` directives.
    - Kubelet serving certificate cloud-config generates `kubelet.crt`/`kubelet.key` using the cluster CA, with SANs derived from the machine's network addresses.
    - This is how disk formatting, mount configuration, and certificate setup are delivered to the guest OS via cloud-init for the current implementation.
- **Persistent disk provisioning**: If `VolumePath` is empty, CAPV creates a new disk and fills `VolumePath`. If not empty, CAPV attaches the existing disk.
- **Persistent disk discovery and backfill**: After the VM is created, CAPV discovers attached disks using a three-tier matching strategy:
    1. **VolumePath**: Exact VMDK file path match (globally unique, most reliable, matches against any controller type).
    2. **UnitNumber**: SCSI unit number match (stable across VM recreations, restricted to SCSI controllers).
    3. **Capacity**: Disk size match on SCSI controllers (last resort, returns no match if ambiguous).
    - Tiers 2 and 3 are restricted to SCSI controllers to prevent false matches against OS disks on IDE or SATA controllers.
    - Discovered `UnitNumber`, `VolumePath`, and `DiskUUID` are backfilled into the resource pool spec for persistence across VM recreations.

### 2. Rolling Update (maxSurge=0)
- `MachineDeployment` deletes `Machine-v1`.
- `VSphereMachine-v1` is deleted, and its `VSphereVM` is destroyed.
- **Crucially**: The `PersistentDisk` is **detached** but **NOT deleted** from the datastore.
- The `ResourceSlot` status is updated to `State: Released`, `LastReleasedTime: now`. The `MachineRef` is intentionally preserved for orphan detection, release verification, and audit purposes; it is only overwritten when the slot is re-allocated to a new machine.
- `MachineDeployment` creates `Machine-v2`.
- `Machine-v2` requests a slot. The controller prefers `Released` slots before `Available` slots, so in a serial rolling update it will typically reuse the first released slot.
- `Machine-v2` therefore usually reuses the released slot's IP, Hostname, and existing `PersistentDisk` via `VolumePath`, but the current implementation does not guarantee an identity-based match back to a specific previous machine instance.
- If the owner `VSphereMachine` object is already gone by the time the `VSphereVM` delete path runs, CAPV locates the pool by scanning all `VSphereResourcePool` objects in the namespace and matching the `MachineRef` in slot status entries. This allows CAPV to safely release the slot even without a direct pool reference from the deleted machine.
- **Orphan detection**: The `VSphereResourcePool` controller periodically checks slots in `InUse` state. If a slot's `MachineRef` points to a `VSphereMachine` that no longer exists, the controller automatically transitions the slot to `Released`.
- During later `VSphereVM` reconciles, if the slot cannot be resolved from the current status binding, CAPV can fall back to the `resource-slot-hostname` annotation and reload the slot definition from `VSphereResourcePool.spec.resources`.

### 3. Resource Cleanup (Automatic Reclaim)
- A dedicated `VSphereResourcePool` controller checks slots in the `Released` state.
- **Retention Period**: By default, a released slot is "reserved" for its previous owner (or for manual recovery) for a period defined by `ReleaseDelayHours` (e.g., 24-48 hours).
- **Reclamation**: If `now - LastReleasedTime > ReleaseDelayHours`:
    - **Disk Cleanup**: The controller automatically deletes the associated `PersistentDisk` from the vSphere datastore to reclaim space.
      The reclaim path resolves vCenter connection parameters via the `ClusterRef` credential chain, and resolves the datacenter from the slot first; if the slot does not define one, it falls back to the pool-level default datacenter.
    - **Spec cleanup**: After the reclaim task succeeds, the controller clears `VolumePath` and `DiskUUID` from the `PersistentDisk` spec and requeues.
    - **State Transition**: On a later reconcile, once the slot no longer has reclaimable persistent disk backing, the slot state is set back to `Available`, making it "clean" and ready to be picked up by any new machine (including those that don't need the previous data).
- **Async reclaim task model**:
    - Reclaim is asynchronous. The controller starts at most one vCenter task per slot and records it in `reclaimStatus`.
    - `reclaimStatus.taskRef` tracks the current vCenter task for that slot.
    - `reclaimStatus.volumePath` tracks the current disk being reclaimed for that slot.
    - If a slot has multiple persistent disks, they are reclaimed one by one across reconciles, never with multiple concurrent reclaim tasks for the same slot.
    - If a reclaim task fails, the controller records `reclaimStatus.lastError` and uses `reclaimStatus.retryAfter` to avoid tight retry loops.
    - Reclaim completion is therefore not modeled as a single atomic transition; a slot may temporarily remain `Released` with reclaim metadata already cleaned from `spec` while it waits for the follow-up reconcile that marks it `Available`.

### 4. Resource Pool Deletion
- Deleting a `VSphereResourcePool` is blocked by a finalizer until all slots are safely released.
- "Safe" means:
    - Any slot still bound to an existing `VSphereMachine` blocks deletion and surfaces an error.
    - If the referenced `VSphereMachine` no longer exists, the slot may transition to `Released` and continue reclaim.
    - Any released slot with reclaimable persistent disks must finish reclaim before the pool finalizer is removed.
- The finalizer is removed only after no live machine is using a slot and all reclaim work is complete.

## Validation of `resourcePoolRef`

To ensure `VSphereResourcePool` and workload controllers are effectively used in a one-to-one manner, CAPV should validate pool references from both sides.

### Pool-side validation

When creating or updating a `VSphereResourcePool`:

- `spec.clusterRef.name` must be set.
- `spec.clusterRef.apiVersion`, if set, must be `cluster.x-k8s.io/v1beta1`.
- `spec.clusterRef.kind`, if set, must be `Cluster`.
- `spec.clusterRef.namespace`, if set, must match the pool namespace.
- `spec.clusterRef` can only be changed when `spec.consumerRef` is nil.
- If `spec.consumerRef` is set, `kind` must be either `KubeadmControlPlane` or `MachineDeployment`.
- `apiVersion` must match the supported group for that kind.
- The referenced consumer object must exist.
- If another `VSphereResourcePool` in the same namespace already points at the same consumer, validation fails.
  This prevents multiple pools from being bound to the same `KubeadmControlPlane` or `MachineDeployment`.
- If `spec.consumerRef` is changed while the pool is not yet fully reusable, validation fails.

Recommended implementation:

- use a validating webhook on `VSphereResourcePool`
- on create/update, list pools in the same namespace and reject duplicates by `(apiVersion, kind, namespace, name)`
- on update, compare old/new `consumerRef`
  - if unchanged, allow
  - if changed and old `consumerRef` is still set, require the pool to already be unbound
  - otherwise reject

### Consumer-side validation

When reconciling a `VSphereMachine`, CAPV should validate the referenced pool before allocation:

- `VSphereMachine.spec.resourcePoolRef` must point to an existing `VSphereResourcePool`.
- If the pool `consumerRef` is set, it must match the effective consumer of the machine.
- If the pool is already bound to a different consumer, reconciliation fails immediately instead of attempting slot allocation.

### Template / controller-level validation

CAPV should also validate references at the source of truth for machine generation:

- For control plane, validate the `VSphereMachineTemplate` referenced by `KubeadmControlPlane`.
- For workers, validate the `VSphereMachineTemplate` referenced by `MachineDeployment`.
- If multiple `KubeadmControlPlane` / `MachineDeployment` objects resolve to the same pool, admission should reject the conflicting configuration.

This ensures duplicate references are caught before machines are created, rather than only during machine reconcile.

Recommended admission split:

- `VSphereResourcePool` webhook validates the pool-side binding uniqueness.
- `KubeadmControlPlane` / `MachineDeployment` webhook validates that the referenced `VSphereMachineTemplate`
  ultimately points to an existing pool and that the pool `consumerRef` is either:
  - empty and compatible with the object being admitted, or
  - already bound to that same object.
- `VSphereMachine` reconcile establishes the actual binding when needed and keeps the final runtime safety check in case an older object bypassed webhook validation or the referenced objects changed between admission and reconcile.

### Duplicate Reference Detection

To prevent repeated use of the same pool, CAPV should treat the following as invalid:

- two `VSphereResourcePool` objects that both bind to the same `KubeadmControlPlane`
- two `VSphereResourcePool` objects that both bind to the same `MachineDeployment`
- one `KubeadmControlPlane` and one `MachineDeployment` that both resolve to the same pool through their machine templates
- one consumer whose template points to pool `A` while pool `A.consumerRef` points to a different consumer

This should be checked as early as possible in admission, but must also be re-checked at machine reconcile time for safety.

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
- **Credential Resolution via ClusterRef**:
    - `VSphereResourcePool.spec.clusterRef` is required. It references the CAPI `Cluster` object in the same namespace.
    - `clusterRef` can only be changed when `consumerRef` is nil (i.e. the pool is not bound to any consumer).
    - vCenter server address and thumbprint are derived from the `VSphereCluster` referenced by `Cluster.spec.infrastructureRef`.
    - vCenter credentials are resolved via the chain: `ClusterRef` → `Cluster` → `VSphereCluster` → `IdentityRef` → `Secret`.
    - If `VSphereCluster.spec.identityRef` is not set, the controller falls back to the global controller manager credentials. If neither is available, the pool reports `VCenterAvailable=False`.
    - The pool will not reconcile until the referenced Cluster and VSphereCluster are available.
    - Two conditions track this readiness: `ClusterRefReady` (Cluster and VSphereCluster exist) and `VCenterAvailable` (credentials resolved successfully).
    - The controller watches `Cluster` and `VSphereCluster` objects to react to changes without polling.
- **Datacenter Resolution**:
    - `VSphereResourcePool.spec.datacenter` is treated as a default value for the pool.
    - `ResourceSlot.datacenter`, when set, takes precedence over the pool-level datacenter.
    - At least one of `VSphereResourcePool.spec.datacenter` and `ResourceSlot.datacenter` must be set for every slot.
    - During provisioning, template / machine datacenter and failure-domain datacenter are combined as constraints; failure-domain resolution does not override a template datacenter.
    - A slot is eligible only if its resolved datacenter matches the template / machine datacenter when one is specified, and is also allowed by the resolved failure-domain datacenter set when one is specified.
    - If template and failure domain do not specify a datacenter, the selected slot's resolved datacenter becomes the authoritative datacenter for the machine and VM.
    - When slot selection supplies the datacenter, CAPV backfills that resolved value onto `VSphereMachine.spec.datacenter`.
- **Consumer Binding**:
    - `VSphereResourcePool.spec.consumerRef` is a logical binding, not a Kubernetes owner relationship.
    - CAPV must not add `ownerReferences` from the pool to the referenced `KubeadmControlPlane` or `MachineDeployment`.
    - Consumer deletion does not delete the pool.
    - Consumer deletion only makes the pool eligible for unbinding after all slot lifecycle and reclaim work has completed.
    - `KubeadmControlPlane` / `MachineDeployment` webhook validation is only a pre-check. It does not establish the binding.
    - The actual binding is created during `VSphereMachine` reconcile using optimistic concurrency on `VSphereResourcePool.resourceVersion`.
    - Slot allocation reuses the same reconcile path after binding succeeds; no separate binding controller is required.
- **Reclaim Tracking**:
    - Reclaim-related task metadata should be grouped under `status.resourceStatuses[].reclaimStatus`.
    - Suggested task state enum is `Running`, `Failed`, `Completed`.
    - This keeps asynchronous reclaim state localized and avoids scattering task-related fields across `ResourceSlotStatus`.
- **SCSI Controller Selection**:
    - Data disks are attached to the VM's primary disk controller when that controller is SCSI.
    - If the primary disk controller is not SCSI, CAPV falls back to the first SCSI controller found on the template VM.
    - CAPV does not currently enforce a specific PCI address such as `0000:00:10.0`; however, the guest-side persistent-disk bootstrap logic assumes the Linux `/dev/disk/by-path/*-scsi-0:0:<unit>:0` naming pattern.
- **Cloud-config-Driven Mounting**:
    - The current implementation merges disk metadata into the VM's `guestinfo.userdata` by generating additional cloud-config content.
    - CAPV merges `disk_setup`, `fs_setup`, `mounts`, `write_files`, and helper commands into the VM user-data based on the `PersistentDisk` list.
    - This means the behavior is currently tied to the cloud-config bootstrap format; the document should not imply a generic mount implementation independent of bootstrap format.
    - **UnitNumber Device Resolution**: `UnitNumber` is used as the durable identifier for resolving data disks inside the guest, but CAPV does not rely on kernel-assigned names like `/dev/sdb` or `/dev/sdc`.
    - The guest-side helper scripts resolve disks primarily through `/dev/disk/by-path` (and related symlink discovery) instead of assuming stable `/dev/sdX` names.

| UnitNumber | Guest-side lookup behavior | Note |
| :--- | :--- | :--- |
| 0 | Resolved via `/dev/disk/by-path/*-scsi-0:0:0:0` | Often the first persistent data disk in this design |
| 1 | Resolved via `/dev/disk/by-path/*-scsi-0:0:1:0` | Lookup is by SCSI unit, not by `/dev/sdX` |
| 2 | Resolved via `/dev/disk/by-path/*-scsi-0:0:2:0` | Additional disks follow the same pattern |
| 7 | Reserved | vSphere SCSI controller unit |

- **Resource Selection Logic**:
    - The controller manages the binding between `VSphereMachine` and `ResourceSlot` within the `VSphereResourcePool` status.
    - The controller first computes the machine's desired datacenter from template / machine settings and `FailureDomain`.
    - It then filters candidate slots to those whose resolved datacenter matches that desired datacenter, if one is present.
    - The controller first checks whether the current `VSphereMachine` already owns a matching slot (by `UID`, or by `Name/Namespace` as an idempotency fallback).
    - If no existing matching binding is found, it prefers the first `Released` matching slot over the first `Available` matching slot.
    - On initial provisioning, if allocation is serial, slot selection follows the order of `spec.resources` after datacenter filtering.
    - If no slot matches the desired datacenter, reconciliation fails instead of selecting a slot from another datacenter.
- **Controller Responsibilities**:
    - The `VSphereVM` controller needs to be aware of `PersistentDisks` that should not be deleted upon VM destruction.
    - The `VSphereVM` controller is responsible for gating backend VM creation on CAPV IPAM fulfillment when slot IPs are not provided.
    - A dedicated controller for `VSphereResourcePool` manages status tracking, delayed reclaim, async reclaim task polling, and safe deletion semantics.

## Testing Matrix

The implementation should include at least the following tests.

### API / webhook tests

- accept `consumerRef` for `KubeadmControlPlane`
- accept `consumerRef` for `MachineDeployment`
- reject unsupported `kind`
- reject mismatched `apiVersion`
- reject `consumerRef.namespace` that differs from the pool namespace
- reject two pools bound to the same consumer
- reject rebinding while any slot is not fully reusable

### Machine reconcile tests

- control-plane machine may allocate from a pool bound to its `KubeadmControlPlane`
- worker machine may allocate from a pool bound to its `MachineDeployment`
- control-plane machine is rejected when referencing a worker-bound pool
- worker machine is rejected when referencing a control-plane-bound pool
- machine is rejected when template points to a pool bound to a different consumer

### Pool lifecycle tests

- deleting a `MachineDeployment` does not delete the pool
- deleting a `KubeadmControlPlane` does not delete the pool
- binding is retained after consumer deletion while any slot is `Released`
- binding is retained after consumer deletion while any reclaim task is still active
- binding is cleared after consumer deletion only when all slots become fully reusable
- a new consumer may bind the pool only after unbinding is complete

### Reclaim-status tests

- task start populates `reclaimStatus.taskRef`, `reclaimStatus.state`, and `reclaimStatus.volumePath`
- task success clears reclaimable disk metadata and resets `reclaimStatus`
- task failure records `reclaimStatus.lastError` and `reclaimStatus.retryAfter`
- multi-disk reclaim proceeds one disk at a time using the same nested reclaim status model

## Open Questions

- Should CAPV reject a `VSphereMachineTemplate` with `resourcePoolRef` pointing to an unbound pool when another object already references that same pool in persisted configuration, even before runtime binding has happened?
- Should rebinding require all slots to be `Available`, or is a weaker condition acceptable if future requirements allow cross-consumer reuse of released slots without reclaim?
