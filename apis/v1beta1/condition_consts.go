/*
Copyright 2021 The Kubernetes Authors.

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

import clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

// Conditions and condition Reasons for the VSphereCluster object.

const (
	// FailureDomainsAvailableCondition documents the status of the failure domains
	// associated to the VSphereCluster.
	FailureDomainsAvailableCondition clusterv1.ConditionType = "FailureDomainsAvailable"

	// FailureDomainsSkippedReason (Severity=Info) documents that some of the failure domain statuses
	// associated to the VSphereCluster are reported as not ready.
	FailureDomainsSkippedReason = "FailureDomainsSkipped"

	// WaitingForFailureDomainStatusReason (Severity=Info) documents that some of the failure domains
	// associated to the VSphereCluster are not reporting the Ready status.
	// Instead of reporting a false ready status, these failure domains are still under the process of reconciling
	// and hence not yet reporting their status.
	WaitingForFailureDomainStatusReason = "WaitingForFailureDomainStatus"

	// FailureDomainsExhaustedByMachineConfigPoolReason (Severity=Warning) documents that all failure domains
	// were excluded because their datacenters have no available machine config pool slots.
	FailureDomainsExhaustedByMachineConfigPoolReason = "FailureDomainsExhaustedByMachineConfigPool"

	// KubeOvnAppReleaseReadyCondition documents kube-ovn AppRelease readiness.
	// It does not gate VSphereCluster.Status.Ready.
	KubeOvnAppReleaseReadyCondition clusterv1.ConditionType = "KubeOvnAppReleaseReady"

	// KubeOvnAppReleaseReadyReason documents that the kube-ovn AppRelease is synced and healthy.
	KubeOvnAppReleaseReadyReason = "AppReleaseReady"

	// KubeOvnAppReleaseReconcilingReason documents that the kube-ovn AppRelease is being created, updated, or waiting for status.
	KubeOvnAppReleaseReconcilingReason = "AppReleaseReconciling"

	// KubeOvnAppReleaseNotReadyReason documents that the kube-ovn AppRelease is observed but not ready.
	KubeOvnAppReleaseNotReadyReason = "AppReleaseNotReady"

	// KubeOvnAppReleaseInvalidConfigurationReason documents invalid kube-ovn AppRelease input configuration.
	KubeOvnAppReleaseInvalidConfigurationReason = "InvalidKubeOvnConfiguration"
)

// Conditions and condition Reasons for the VSphereMachine and the VSphereVM object.
//
// NOTE: VSphereMachine wraps a VMSphereVM, some we are using a unique set of conditions and reasons in order
// to ensure a consistent UX; differences between the two objects will be highlighted in the comments.

const (
	// VMProvisionedCondition documents the status of the provisioning of a VSphereMachine and its underlying VSphereVM.
	VMProvisionedCondition clusterv1.ConditionType = "VMProvisioned"

	// WaitingForClusterInfrastructureReason (Severity=Info) documents a VSphereMachine waiting for the cluster
	// infrastructure to be ready before starting the provisioning process.
	//
	// NOTE: This reason does not apply to VSphereVM (this state happens before the VSphereVM is actually created).
	WaitingForClusterInfrastructureReason = "WaitingForClusterInfrastructure"

	// WaitingForBootstrapDataReason (Severity=Info) documents a VSphereMachine waiting for the bootstrap
	// script to be ready before starting the provisioning process.
	//
	// NOTE: This reason does not apply to VSphereVM (this state happens before the VSphereVM is actually created).
	WaitingForBootstrapDataReason = "WaitingForBootstrapData"

	// WaitingForStaticIPAllocationReason (Severity=Info) documents a VSphereVM waiting for the allocation of
	// a static IP address.
	WaitingForStaticIPAllocationReason = "WaitingForStaticIPAllocation"

	// WaitingForIPAllocationReason (Severity=Info) documents a VSphereVM waiting for the allocation of
	// an IP address.
	// This is used when the dhcp4 or dhcp6 for a VSphereVM is set and the VSphereVM is waiting for the
	// relevant IP address  to show up on the VM.
	WaitingForIPAllocationReason = "WaitingForIPAllocation"

	// CloningReason documents (Severity=Info) a VSphereMachine/VSphereVM currently executing the clone operation.
	CloningReason = "Cloning"

	// CloningFailedReason (Severity=Warning) documents a VSphereMachine/VSphereVM controller detecting
	// an error while provisioning; those kind of errors are usually transient and failed provisioning
	// are automatically re-tried by the controller.
	CloningFailedReason = "CloningFailed"

	// PoweringOnReason documents (Severity=Info) a VSphereMachine/VSphereVM currently executing the power on sequence.
	PoweringOnReason = "PoweringOn"

	// PoweringOnFailedReason (Severity=Warning) documents a VSphereMachine/VSphereVM controller detecting
	// an error while powering on; those kind of errors are usually transient and failed provisioning
	// are automatically re-tried by the controller.
	PoweringOnFailedReason = "PoweringOnFailed"

	// NotFoundByBIOSUUIDReason (Severity=Warning) documents a VSphereVM which can't be found by BIOS UUID.
	// Those kind of errors could be transient sometimes and failed VSphereVM are automatically
	// reconciled by the controller.
	NotFoundByBIOSUUIDReason = "NotFoundByBIOSUUID"

	// TaskFailure (Severity=Warning) documents a VSphereMachine/VSphere task failure; the reconcile look will automatically
	// retry the operation, but a user intervention might be required to fix the problem.
	TaskFailure = "TaskFailure"

	// WaitingForNetworkAddressesReason (Severity=Info) documents a VSphereMachine waiting for the machine network
	// settings to be reported after machine being powered on.
	//
	// NOTE: This reason does not apply to VSphereVM (this state happens after the VSphereVM is in ready state).
	WaitingForNetworkAddressesReason = "WaitingForNetworkAddresses"

	// TagsAttachmentFailedReason (Severity=Error) documents a VSphereMachine/VSphereVM tags attachment failure.
	TagsAttachmentFailedReason = "TagsAttachmentFailed"

	// PCIDevicesDetachedCondition documents the status of the attached PCI devices on the VSphereVM.
	// It is a negative condition to notify the user that the device(s) is no longer attached to
	// the underlying VM and would require manual intervention to fix the situation.
	//
	// NOTE: This condition does not apply to VSphereMachine.
	PCIDevicesDetachedCondition clusterv1.ConditionType = "PCIDevicesDetached"

	// NotFoundReason (Severity=Warning) documents the VSphereVM not having the PCI device attached during VM startup.
	// This would indicate that the PCI devices were removed out of band by an external entity.
	NotFoundReason = "NotFound"

	// InitialPowerOnCompletedCondition is a one-way latch documenting that the VSphereVM has completed its
	// initial power-on. Once the VM is first observed powered on it is set to True and never changes again.
	// The controller uses it to decide whether to power on a VM found powered off: while unset the VM is
	// powered on as part of provisioning; once set, a powered-off VM is treated as an out-of-band operator
	// action (e.g. maintenance) and is not powered back on.
	//
	// NOTE: This condition is internal to VSphereVM and is not aggregated into the Ready condition.
	InitialPowerOnCompletedCondition clusterv1.ConditionType = "InitialPowerOnCompleted"

	// PoweredOnCondition reflects the real-time power state of the underlying VM. It is aggregated into the
	// VSphereVM Ready condition; the owning VSphereMachine in turn mirrors the VSphereVM Ready condition, so a
	// VM that was powered off out of band after its initial power-on surfaces as not ready on both objects.
	PoweredOnCondition clusterv1.ConditionType = "PoweredOn"

	// PoweredOffReason (Severity=Info) documents that the VM is powered off after its initial power-on
	// completed, i.e. it was stopped out of band and the controller intentionally does not power it back on.
	PoweredOffReason = "PoweredOff"
)

// Conditions and Reasons related to utilizing a VSphereIdentity to make connections to a VCenter.
// Can currently be used by VSphereCluster and VSphereVM.
const (
	// VCenterAvailableCondition documents the connectivity with vcenter
	// for a given resource.
	VCenterAvailableCondition clusterv1.ConditionType = "VCenterAvailable"

	// VCenterUnreachableReason (Severity=Error) documents a controller detecting
	// issues with VCenter reachability.
	VCenterUnreachableReason = "VCenterUnreachable"
)

const (
	// ClusterModulesAvailableCondition documents the availability of cluster modules for the VSphereCluster object.
	ClusterModulesAvailableCondition clusterv1.ConditionType = "ClusterModulesAvailable"

	// MissingVCenterVersionReason (Severity=Warning) documents a controller detecting
	//  the scenario in which the vCenter version is not set in the status of the VSphereCluster object.
	MissingVCenterVersionReason = "MissingVCenterVersion"

	// VCenterVersionIncompatibleReason (Severity=Info) documents the case where the vCenter version of the
	// VSphereCluster object does not support cluster modules.
	VCenterVersionIncompatibleReason = "VCenterVersionIncompatible"

	// ClusterModuleSetupFailedReason (Severity=Warning) documents a controller detecting
	// issues when setting up anti-affinity constraints via cluster modules for objects
	// belonging to the cluster.
	ClusterModuleSetupFailedReason = "ClusterModuleSetupFailed"
)

const (
	// CredentialsAvailableCondidtion is used by VSphereClusterIdentity when a credential
	// secret is available and unused by other VSphereClusterIdentities.
	CredentialsAvailableCondidtion clusterv1.ConditionType = "CredentialsAvailable"

	// SecretNotAvailableReason is used when the secret referenced by the VSphereClusterIdentity cannot be found.
	SecretNotAvailableReason = "SecretNotAvailable"

	// SecretOwnerReferenceFailedReason is used for errors while updating the owner reference of the secret.
	SecretOwnerReferenceFailedReason = "SecretOwnerReferenceFailed"

	// SecretAlreadyInUseReason is used when another VSphereClusterIdentity is using the secret.
	SecretAlreadyInUseReason = "SecretInUse"
)

const (
	// PlacementConstraintMetCondition documents whether the placement constraint is configured correctly or not.
	PlacementConstraintMetCondition clusterv1.ConditionType = "PlacementConstraintMet"

	// ResourcePoolNotFoundReason (Severity=Error) documents that the resource pool in the placement constraint
	// associated to the VSphereDeploymentZone is misconfigured.
	ResourcePoolNotFoundReason = "ResourcePoolNotFound"

	// FolderNotFoundReason (Severity=Error) documents that the folder in the placement constraint
	// associated to the VSphereDeploymentZone is misconfigured.
	FolderNotFoundReason = "FolderNotFound"
)

const (
	// VSphereFailureDomainValidatedCondition documents whether the failure domain for the deployment zone is configured correctly or not.
	VSphereFailureDomainValidatedCondition clusterv1.ConditionType = "VSphereFailureDomainValidated"

	// RegionMisconfiguredReason (Severity=Error) documents that the region for the Failure Domain associated to
	// the VSphereDeploymentZone is misconfigured.
	RegionMisconfiguredReason = "FailureDomainRegionMisconfigured"

	// ZoneMisconfiguredReason (Severity=Error) documents that the zone for the Failure Domain associated to
	// the VSphereDeploymentZone is misconfigured.
	ZoneMisconfiguredReason = "FailureDomainZoneMisconfigured"

	// ComputeClusterNotFoundReason (Severity=Error) documents that the Compute Cluster for the Failure Domain
	// associated to the VSphereDeploymentZone cannot be found.
	ComputeClusterNotFoundReason = "ComputeClusterNotFound"

	// HostsMisconfiguredReason (Severity=Error) documents that the VM & Host Group details for the Failure Domain
	// associated to the VSphereDeploymentZone are misconfigured.
	HostsMisconfiguredReason = "HostsMisconfigured"

	// HostsAffinityMisconfiguredReason (Severity=Warning) documents that the VM & Host Group affinity rule for the FailureDomain is disabled.
	HostsAffinityMisconfiguredReason = "HostsAffinityMisconfigured"

	// NetworkNotFoundReason (Severity=Error) documents that the networks in the topology for the Failure Domain
	// associated to the VSphereDeploymentZone are misconfigured.
	NetworkNotFoundReason = "NetworkNotFound"

	// DatastoreNotFoundReason (Severity=Error) documents that the datastore in the topology for the Failure Domain
	// associated to the VSphereDeploymentZone is misconfigured.
	DatastoreNotFoundReason = "DatastoreNotFound"
)

const (
	// IPAddressClaimedCondition documents the status of claiming an IP address
	// from an IPAM provider.
	IPAddressClaimedCondition clusterv1.ConditionType = "IPAddressClaimed"

	// IPAddressClaimsBeingCreatedReason (Severity=Info) documents that claims for the
	// IP addresses required by the VSphereVM are being created.
	IPAddressClaimsBeingCreatedReason = "IPAddressClaimsBeingCreated"

	// WaitingForIPAddressReason (Severity=Info) documents that the VSphereVM is
	// currently waiting for an IP address to be provisioned.
	WaitingForIPAddressReason = "WaitingForIPAddress"

	// IPAddressInvalidReason (Severity=Error) documents that the IP address
	// provided by the IPAM provider is not valid.
	IPAddressInvalidReason = "IPAddressInvalid"

	// IPAddressClaimNotFoundReason (Severity=Error) documents that the IPAddressClaim
	// cannot be found.
	IPAddressClaimNotFoundReason = "IPAddressClaimNotFound"
)

// Conditions and condition Reasons for the VSphereMachineConfigPool object.

const (
	// ClusterRefReadyCondition reports whether the referenced Cluster and
	// its VSphereCluster infrastructure are found and available.
	ClusterRefReadyCondition clusterv1.ConditionType = "ClusterRefReady"

	// ClusterRefReadyReason documents that the referenced Cluster and VSphereCluster are available.
	ClusterRefReadyReason = "ClusterRefReady"

	// ClusterNotFoundReason (Severity=Warning) documents that the Cluster referenced
	// by ClusterRef cannot be found.
	ClusterNotFoundReason = "ClusterNotFound"

	// VSphereClusterNotFoundReason (Severity=Warning) documents that the VSphereCluster
	// referenced by the Cluster's InfrastructureRef cannot be found.
	VSphereClusterNotFoundReason = "VSphereClusterNotFound"

	// IdentityCredentialsUnavailableReason (Severity=Warning) documents that the
	// vCenter credentials could not be resolved from the VSphereCluster's IdentityRef.
	IdentityCredentialsUnavailableReason = "IdentityCredentialsUnavailable"
)

// Pool-level health conditions for the VSphereMachineConfigPool object. The
// pool's Ready condition summarizes the health conditions below (MembersValid,
// MembersUnique, PersistentDisksReady) together with ClusterRefReady and
// VCenterAvailable. SlotAvailable is a capacity signal and deliberately does
// NOT contribute to Ready — a fully-allocated fixed-IP pool is a healthy,
// expected state.
const (
	// MachineConfigPoolMembersValidCondition reports whether every slot's fields
	// are structurally valid (persistent disk unit numbers, sizes, and intra-slot
	// disk uniqueness).
	MachineConfigPoolMembersValidCondition clusterv1.ConditionType = "MembersValid"

	// MachineConfigPoolInvalidMemberConfigReason (Severity=Warning) documents that
	// at least one slot has invalid field values.
	MachineConfigPoolInvalidMemberConfigReason = "InvalidMemberConfig"

	// MachineConfigPoolMembersUniqueCondition reports whether hostname and primary
	// IP/IPv6 are unique within the pool and across pools bound to the same cluster.
	MachineConfigPoolMembersUniqueCondition clusterv1.ConditionType = "MembersUnique"

	// MachineConfigPoolDuplicateHostnameReason (Severity=Warning) documents that a
	// hostname is used by more than one slot.
	MachineConfigPoolDuplicateHostnameReason = "DuplicateHostname"

	// MachineConfigPoolDuplicateIPAddressReason (Severity=Warning) documents that a
	// primary IP/IPv6 is used by more than one slot.
	MachineConfigPoolDuplicateIPAddressReason = "DuplicateIPAddress"

	// MachineConfigPoolSlotAvailableCondition reports whether the pool has at least
	// one Available slot. Capacity signal only; does not contribute to Ready.
	MachineConfigPoolSlotAvailableCondition clusterv1.ConditionType = "SlotAvailable"

	// MachineConfigPoolAllSlotsInUseReason (Severity=Info) documents that every slot
	// is currently allocated.
	MachineConfigPoolAllSlotsInUseReason = "AllSlotsInUse"

	// MachineConfigPoolWaitingForReclaimReason (Severity=Info) documents that free
	// capacity is pending release/reclaim of one or more slots.
	MachineConfigPoolWaitingForReclaimReason = "WaitingForReclaim"

	// MachineConfigPoolPersistentDisksReadyCondition reports whether every
	// persistent disk is in a settled healthy state: idle on an available slot,
	// or fully provisioned on an in-use slot. It is False while an in-use slot's
	// disks are still being provisioned, or when a disk is stuck in a failed
	// reclaim or an attachment-blocked state. Slots being reclaimed normally
	// (not yet failed) do not pull this condition down.
	MachineConfigPoolPersistentDisksReadyCondition clusterv1.ConditionType = "PersistentDisksReady"

	// MachineConfigPoolDisksProvisioningReason (Severity=Info) documents that an
	// in-use slot's persistent disks have not yet been created in vCenter
	// (VolumePath not backfilled), i.e. the slot is still preparing.
	MachineConfigPoolDisksProvisioningReason = "DisksProvisioning"

	// MachineConfigPoolReclaimFailedReason (Severity=Warning) documents that a
	// persistent disk reclaim task failed.
	MachineConfigPoolReclaimFailedReason = "ReclaimFailed"

	// MachineConfigPoolDiskStillAttachedReason (Severity=Warning) documents that a
	// persistent disk cannot be reclaimed because it is still attached to a VM.
	MachineConfigPoolDiskStillAttachedReason = "DiskStillAttached"
)

// Conditions and condition Reasons for VSphereMachine machine config pool allocation.

const (
	// MachineConfigPoolReadyCondition reports whether the static machine config
	// pool slot has been successfully allocated for this machine. Only set when
	// spec.machineConfigPoolRef is configured.
	MachineConfigPoolReadyCondition clusterv1.ConditionType = "MachineConfigPoolReady"

	// MachineConfigPoolSlotAllocatedReason documents that a slot has been allocated.
	MachineConfigPoolSlotAllocatedReason = "SlotAllocated"

	// MachineConfigPoolBoundToOtherConsumerReason (Severity=Warning) documents that
	// the pool is bound to a different consumer (KCP or MachineDeployment).
	MachineConfigPoolBoundToOtherConsumerReason = "PoolBoundToOtherConsumer"

	// MachineConfigPoolNoAvailableSlotsReason (Severity=Warning) documents that no
	// slots matching the required datacenter/failure domain are available.
	MachineConfigPoolNoAvailableSlotsReason = "NoAvailableSlots"
)

const (
	// GuestSoftPowerOffSucceededCondition documents the status of performing guest initiated
	// graceful shutdown.
	GuestSoftPowerOffSucceededCondition clusterv1.ConditionType = "GuestSoftPowerOffSucceeded"

	// GuestSoftPowerOffInProgressReason (Severity=Info) documents that the guest receives
	// a graceful shutdown request.
	GuestSoftPowerOffInProgressReason = "GuestSoftPowerOffInProgress"

	// GuestSoftPowerOffFailedReason (Severity=Warning) documents that the graceful
	// shutdown request fails.
	GuestSoftPowerOffFailedReason = "GuestSoftPowerOffFailed"
)
