/*
Copyright 2019 The Kubernetes Authors.

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

package govmomi

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/pbm"
	pbmTypes "github.com/vmware/govmomi/pbm/types"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta2conditions "sigs.k8s.io/cluster-api/util/conditions/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/govmomi/cluster"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/govmomi/clustermodules"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/govmomi/extra"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/govmomi/ipam"
	govmominet "sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/govmomi/net"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/govmomi/pci"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

const (
	bootstrapSecretKubeletServingCertKey = "kubelet-serving.crt"
	bootstrapSecretKubeletServingKeyKey  = "kubelet-serving.key"
)

// VMService provdes API to interact with the VMs using govmomi.
type VMService struct{}

type nodeIdentity struct {
	Hostname string
	NodeIP   string
}

// ReconcileVM makes sure that the VM is in the desired state by:
//  1. Creating the VM if it does not exist, then...
//  2. Updating the VM with the bootstrap data, such as the cloud-init meta and user data, before...
//  3. Powering on the VM, and finally...
//  4. Returning the real-time state of the VM to the caller
func (vms *VMService) ReconcileVM(ctx context.Context, vmCtx *capvcontext.VMContext) (vm infrav1.VirtualMachine, _ error) {
	// Initialize the result.
	vm = infrav1.VirtualMachine{
		Name:  vmCtx.VSphereVM.Name,
		State: infrav1.VirtualMachineStatePending,
	}

	// If there is an in-flight task associated with this VM then do not
	// reconcile the VM until the task is completed.
	if inFlight, err := reconcileInFlightTask(ctx, vmCtx); err != nil || inFlight {
		return vm, err
	}

	// This deferred function will trigger a reconcile event for the
	// VSphereVM resource once its associated task completes. If
	// there is no task for the VSphereVM resource then no reconcile
	// event is triggered.
	defer reconcileVSphereVMOnTaskCompletion(ctx, vmCtx)

	// Before going further, we need the VM's managed object reference.
	vmRef, err := findVM(ctx, vmCtx)
	if err != nil {
		if !isNotFound(err) {
			return vm, err
		}

		// If the machine was not found by BIOS UUID, it could mean that the machine got deleted from vcenter directly,
		// but sometimes this error is transient, for instance, if the storage was temporarily disconnected but
		// later recovered, the machine will recover from this error.
		if wasNotFoundByBIOSUUID(err) {
			conditions.MarkFalse(vmCtx.VSphereVM, infrav1.VMProvisionedCondition, infrav1.NotFoundByBIOSUUIDReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
			v1beta2conditions.Set(vmCtx.VSphereVM, metav1.Condition{
				Type:    infrav1.VSphereVMVirtualMachineProvisionedV1Beta2Condition,
				Status:  metav1.ConditionFalse,
				Reason:  infrav1.VSphereVMVirtualMachineNotFoundByBIOSUUIDV1Beta2Reason,
				Message: err.Error(),
			})
			vm.State = infrav1.VirtualMachineStateNotFound
			return vm, err
		}

		// Otherwise, this is a new machine and the VM should be created.
		// NOTE: We are setting this condition only in case it does not exist, so we avoid to get flickering LastConditionTime
		// in case of cloning errors or powering on errors.
		if !conditions.Has(vmCtx.VSphereVM, infrav1.VMProvisionedCondition) {
			conditions.MarkFalse(vmCtx.VSphereVM, infrav1.VMProvisionedCondition, infrav1.CloningReason, clusterv1.ConditionSeverityInfo, "")
			v1beta2conditions.Set(vmCtx.VSphereVM, metav1.Condition{
				Type:   infrav1.VSphereVMVirtualMachineProvisionedV1Beta2Condition,
				Status: metav1.ConditionFalse,
				Reason: infrav1.VSphereVMVirtualMachineWaitingForCloneV1Beta2Reason,
			})
		}

		log := ctrl.LoggerFrom(ctx)

		// Get the bootstrap data.
		log.Info("Preparing bootstrap data for new VM create")
		bootstrapData, format, err := vms.getBootstrapData(ctx, vmCtx)
		if err != nil {
			// Attribute a precise BootstrapReady reason instead of misreporting these bootstrap-delivery
			// failures as a clone failure. VMProvisioned stays at CloningReason (provisioning is in progress
			// but blocked); BootstrapReady carries the real cause and pulls the Ready condition down.
			reason := infrav1.BootstrapSecretGetFailedReason
			v1beta2Reason := infrav1.VSphereVMBootstrapSecretGetFailedV1Beta2Reason
			if _, ok := err.(*bootstrapSecretContentError); ok {
				reason = infrav1.BootstrapSecretContentInvalidReason
				v1beta2Reason = infrav1.VSphereVMBootstrapSecretContentInvalidV1Beta2Reason
			}
			conditions.MarkFalse(vmCtx.VSphereVM, infrav1.BootstrapReadyCondition, reason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
			v1beta2conditions.Set(vmCtx.VSphereVM, metav1.Condition{
				Type:    infrav1.VSphereVMBootstrapReadyV1Beta2Condition,
				Status:  metav1.ConditionFalse,
				Reason:  v1beta2Reason,
				Message: err.Error(),
			})
			return vm, err
		}
		log.Info("Prepared bootstrap data for new VM create", "format", string(format), "bytes", len(bootstrapData))

		// Create the VM.
		log.Info("Calling createVM", "format", string(format), "bootstrapBytes", len(bootstrapData))
		err = createVM(ctx, vmCtx, bootstrapData, format)
		if err != nil {
			conditions.MarkFalse(vmCtx.VSphereVM, infrav1.VMProvisionedCondition, infrav1.CloningFailedReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
			v1beta2conditions.Set(vmCtx.VSphereVM, metav1.Condition{
				Type:    infrav1.VSphereVMVirtualMachineProvisionedV1Beta2Condition,
				Status:  metav1.ConditionFalse,
				Reason:  infrav1.VSphereVMVirtualMachineNotProvisionedV1Beta2Reason,
				Message: err.Error(),
			})
			return vm, err
		}
		// The bootstrap data was delivered to the VM as part of the clone request.
		conditions.MarkTrue(vmCtx.VSphereVM, infrav1.BootstrapReadyCondition)
		v1beta2conditions.Set(vmCtx.VSphereVM, metav1.Condition{
			Type:   infrav1.VSphereVMBootstrapReadyV1Beta2Condition,
			Status: metav1.ConditionTrue,
			Reason: infrav1.VSphereVMBootstrapReadyV1Beta2Reason,
		})
		if err := persistMachineConfigSlotBackfill(ctx, vmCtx); err != nil {
			return vm, err
		}
		log.Info("createVM call completed")
		return vm, nil
	}

	//
	// At this point we know the VM exists, so it needs to be updated.
	//

	// Create a new virtualMachineContext to reconcile the VM.
	virtualMachineCtx := &virtualMachineContext{
		VMContext: *vmCtx,
		Obj:       object.NewVirtualMachine(vmCtx.Session.Client.Client, vmRef),
		Ref:       vmRef,
		State:     &vm,
	}
	vm.VMRef = vmRef.String()

	vms.reconcileUUID(ctx, virtualMachineCtx)

	if ok, err := vms.reconcileHardwareVersion(ctx, virtualMachineCtx); err != nil || !ok {
		return vm, err
	}

	if err := vms.reconcilePCIDevices(ctx, virtualMachineCtx); err != nil {
		return vm, err
	}

	if err := vms.reconcileNetworkStatus(ctx, virtualMachineCtx); err != nil {
		return vm, err
	}

	if ok, err := vms.reconcileIPAddresses(ctx, virtualMachineCtx); err != nil || !ok {
		return vm, err
	}

	// Discover persistent-disk identifiers before the first power-on so we can
	// update guestinfo user-data with a durable reconcile service.
	if err := vms.reconcilePersistentDiskStatuses(ctx, virtualMachineCtx); err != nil {
		return vm, err
	}

	if ok, err := vms.reconcileBootstrapUserData(ctx, virtualMachineCtx); err != nil || !ok {
		return vm, err
	}

	if ok, err := vms.reconcileMetadata(ctx, virtualMachineCtx); err != nil || !ok {
		return vm, err
	}

	if err := vms.reconcileStoragePolicy(ctx, virtualMachineCtx); err != nil {
		return vm, err
	}

	if ok, err := vms.reconcileVMGroupInfo(ctx, virtualMachineCtx); err != nil || !ok {
		return vm, err
	}

	if err := vms.reconcileClusterModuleMembership(ctx, virtualMachineCtx); err != nil {
		return vm, err
	}

	if ok, err := vms.reconcilePowerState(ctx, virtualMachineCtx); err != nil || !ok {
		return vm, err
	}

	if err := vms.reconcileHostInfo(ctx, virtualMachineCtx); err != nil {
		return vm, err
	}

	if err := vms.reconcileTags(ctx, virtualMachineCtx); err != nil {
		conditions.MarkFalse(vmCtx.VSphereVM, infrav1.VMProvisionedCondition, infrav1.TagsAttachmentFailedReason, clusterv1.ConditionSeverityError, "%s", err.Error())
		v1beta2conditions.Set(vmCtx.VSphereVM, metav1.Condition{
			Type:    infrav1.VSphereVMVirtualMachineProvisionedV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.VSphereVMVirtualMachineNotProvisionedV1Beta2Reason,
			Message: err.Error(),
		})
		return vm, err
	}

	vm.State = infrav1.VirtualMachineStateReady
	return vm, nil
}

// DestroyVM powers off and destroys a virtual machine.
func (vms *VMService) DestroyVM(ctx context.Context, vmCtx *capvcontext.VMContext) (reconcile.Result, infrav1.VirtualMachine, error) {
	log := ctrl.LoggerFrom(ctx)

	vm := infrav1.VirtualMachine{
		Name:  vmCtx.VSphereVM.Name,
		State: infrav1.VirtualMachineStatePending,
	}

	// If there is an in-flight task associated with this VM then do not
	// reconcile the VM until the task is completed.
	if inFlight, err := reconcileInFlightTask(ctx, vmCtx); err != nil || inFlight {
		return reconcile.Result{}, vm, err
	}

	// This deferred function will trigger a reconcile event for the
	// VSphereVM resource once its associated task completes. If
	// there is no task for the VSphereVM resource then no reconcile
	// event is triggered.
	defer reconcileVSphereVMOnTaskCompletion(ctx, vmCtx)

	// Before going further, we need the VM's managed object reference.
	vmRef, err := findVM(ctx, vmCtx)
	if err != nil {
		// If the VM's MoRef could not be found then the VM no longer exists. This
		// is the desired state.
		if isNotFound(err) || isFolderNotFound(err) {
			vm.State = infrav1.VirtualMachineStateNotFound
			return reconcile.Result{}, vm, nil
		}
		return reconcile.Result{}, vm, err
	}

	// At this point we know the VM exists, so it needs to be destroyed.
	//

	// Create a new virtualMachineContext to reconcile the VM.
	virtualMachineCtx := &virtualMachineContext{
		VMContext: *vmCtx,
		Obj:       object.NewVirtualMachine(vmCtx.Session.Client.Client, vmRef),
		Ref:       vmRef,
		State:     &vm,
	}

	// Detach persistent disks before destruction to prevent them from being deleted.
	if virtualMachineCtx.MachineConfigSlot != nil && len(virtualMachineCtx.MachineConfigSlot.PersistentDisks) > 0 {
		if err := vms.detachPersistentDisks(ctx, virtualMachineCtx); err != nil {
			return reconcile.Result{}, vm, errors.Wrapf(err, "failed to detach persistent disks for vm %s", virtualMachineCtx)
		}
	}

	// Shut down the VM
	powerState, err := vms.getPowerState(ctx, virtualMachineCtx)
	if err != nil {
		return reconcile.Result{}, vm, err
	}

	if powerState == infrav1.VirtualMachinePowerStatePoweredOn {
		// Trigger the soft power off and set the condition.
		softPowerOffPending, err := vms.triggerSoftPowerOff(ctx, virtualMachineCtx)
		if err != nil {
			return reconcile.Result{}, vm, err
		}

		if softPowerOffPending {
			// Return to reconcile later when we have a pending soft power off operation.
			return reconcile.Result{RequeueAfter: time.Second * 20}, vm, nil
		}

		// Hard shut off VM.
		task, err := virtualMachineCtx.Obj.PowerOff(ctx)
		if err != nil {
			return reconcile.Result{}, vm, err
		}

		virtualMachineCtx.VSphereVM.Status.TaskRef = task.Reference().Value
		if err = virtualMachineCtx.Patch(ctx); err != nil {
			return reconcile.Result{}, vm, err
		}

		log.Info("Wait for VM to be powered off")
		return reconcile.Result{}, vm, nil
	}

	// Only set the GuestPowerOffCondition to true when the guest shutdown has been initiated.
	if conditions.Has(virtualMachineCtx.VSphereVM, infrav1.GuestSoftPowerOffSucceededCondition) {
		conditions.MarkTrue(virtualMachineCtx.VSphereVM, infrav1.GuestSoftPowerOffSucceededCondition)
		v1beta2conditions.Set(virtualMachineCtx.VSphereVM, metav1.Condition{
			Type:   infrav1.VSphereVMGuestSoftPowerOffSucceededV1Beta2Condition,
			Status: metav1.ConditionTrue,
			Reason: infrav1.VSphereVMGuestSoftPowerOffSucceededV1Beta2Reason,
		})
	}

	log.Info("VM is powered off")
	if vmCtx.ClusterModuleInfo != nil {
		log := log.WithValues("moduleUUID", *vmCtx.ClusterModuleInfo)
		ctx := ctrl.LoggerInto(ctx, log)

		provider := clustermodules.NewProvider(vmCtx.Session.TagManager.Client)
		err := provider.RemoveMoRefFromModule(ctx, *vmCtx.ClusterModuleInfo, virtualMachineCtx.Ref)
		if err != nil && !rest.IsStatusError(err, http.StatusNotFound) {
			return reconcile.Result{}, vm, err
		}
		vmCtx.VSphereVM.Status.ModuleUUID = nil
	}

	// At this point the VM is not powered on and can be destroyed. Store the
	// destroy task's reference and return a requeue error.
	log.Info("Destroying vm")
	task, err := virtualMachineCtx.Obj.Destroy(ctx)
	if err != nil {
		return reconcile.Result{}, vm, err
	}
	vmCtx.VSphereVM.Status.TaskRef = task.Reference().Value
	log.Info("Wait for VM to be destroyed")
	return reconcile.Result{}, vm, nil
}

func (vms *VMService) reconcileNetworkStatus(ctx context.Context, virtualMachineCtx *virtualMachineContext) error {
	netStatus, err := vms.getNetworkStatus(ctx, virtualMachineCtx)
	if err != nil {
		return err
	}
	virtualMachineCtx.State.Network = netStatus
	return nil
}

// reconcileIPAddresses works to check that all the IPAddressClaim objects for the
// VSphereVM object have been bound.
// This function is a no-op if the VSphereVM has no associated IPAddressClaims.
// A discovered IPAddress is expected to contain a valid IP, Prefix and Gateway.
func (vms *VMService) reconcileIPAddresses(ctx context.Context, virtualMachineCtx *virtualMachineContext) (bool, error) {
	ipamState, err := ipam.BuildState(ctx, virtualMachineCtx.VMContext, virtualMachineCtx.State.Network)
	if err != nil && !errors.Is(err, ipam.ErrWaitingForIPAddr) {
		return false, err
	}
	if errors.Is(err, ipam.ErrWaitingForIPAddr) {
		conditions.MarkFalse(virtualMachineCtx.VSphereVM, infrav1.VMProvisionedCondition, infrav1.WaitingForIPAddressReason, clusterv1.ConditionSeverityInfo, "%s", err.Error())
		v1beta2conditions.Set(virtualMachineCtx.VSphereVM, metav1.Condition{
			Type:    infrav1.VSphereVMVirtualMachineProvisionedV1Beta2Condition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1.VSphereVMVirtualMachineWaitingForIPAddressV1Beta2Reason,
			Message: err.Error(),
		})
		return false, nil
	}
	virtualMachineCtx.IPAMState = ipamState
	return true, nil
}

func (vms *VMService) reconcileMetadata(ctx context.Context, virtualMachineCtx *virtualMachineContext) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	existingMetadata, err := vms.getMetadata(ctx, virtualMachineCtx)
	if err != nil {
		return false, err
	}

	var persistentDisks []infrav1.PersistentDisk
	if virtualMachineCtx.MachineConfigSlot != nil {
		persistentDisks = virtualMachineCtx.MachineConfigSlot.PersistentDisks
	}

	identity, err := resolveNodeIdentity(virtualMachineCtx)
	if err != nil {
		return false, err
	}

	newMetadata, err := util.GetMachineMetadata(identity.Hostname, *virtualMachineCtx.VSphereVM, virtualMachineCtx.IPAMState, persistentDisks, virtualMachineCtx.State.Network...)
	if err != nil {
		return false, err
	}

	// If the metadata is the same then return early.
	if string(newMetadata) == existingMetadata {
		return true, nil
	}

	log.Info("Updating VM metadata")
	taskRef, err := vms.setMetadata(ctx, virtualMachineCtx, newMetadata)
	if err != nil {
		return false, errors.Wrapf(err, "unable to set metadata on vm %s", virtualMachineCtx)
	}

	virtualMachineCtx.VSphereVM.Status.TaskRef = taskRef
	log.Info("Wait for VM metadata to be updated")
	return false, nil
}

func (vms *VMService) reconcilePowerState(ctx context.Context, virtualMachineCtx *virtualMachineContext) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	powerState, err := vms.getPowerState(ctx, virtualMachineCtx)
	if err != nil {
		return false, err
	}
	switch powerState {
	case infrav1.VirtualMachinePowerStatePoweredOff:
		// Once the VM has completed its initial power-on, a subsequent powered-off state is treated as an
		// out-of-band operator action (e.g. stopping the node for maintenance). The controller must not
		// power it back on; it only reflects the powered-off state and leaves the VM to the operator.
		if conditions.IsTrue(virtualMachineCtx.VSphereVM, infrav1.InitialPowerOnCompletedCondition) {
			log.Info("VM is powered off after initial power-on; leaving it powered off for out-of-band operation")
			conditions.MarkFalse(virtualMachineCtx.VSphereVM, infrav1.PoweredOnCondition, infrav1.PoweredOffReason, clusterv1.ConditionSeverityInfo, "VM was powered off out of band after initial power-on")
			v1beta2conditions.Set(virtualMachineCtx.VSphereVM, metav1.Condition{
				Type:   infrav1.VSphereVMPoweredOnV1Beta2Condition,
				Status: metav1.ConditionFalse,
				Reason: infrav1.VSphereVMPoweredOffV1Beta2Reason,
			})
			virtualMachineCtx.VSphereVM.Status.Ready = false
			return false, nil
		}

		log.Info("Powering on VM")
		task, err := virtualMachineCtx.Obj.PowerOn(ctx)
		if err != nil {
			conditions.MarkFalse(virtualMachineCtx.VSphereVM, infrav1.VMProvisionedCondition, infrav1.PoweringOnFailedReason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
			v1beta2conditions.Set(virtualMachineCtx.VSphereVM, metav1.Condition{
				Type:    infrav1.VSphereVMVirtualMachineProvisionedV1Beta2Condition,
				Status:  metav1.ConditionFalse,
				Reason:  infrav1.VSphereVMVirtualMachineNotProvisionedV1Beta2Reason,
				Message: err.Error(),
			})
			return false, errors.Wrapf(err, "failed to trigger power on op for vm %s", virtualMachineCtx)
		}
		conditions.MarkFalse(virtualMachineCtx.VSphereVM, infrav1.VMProvisionedCondition, infrav1.PoweringOnReason, clusterv1.ConditionSeverityInfo, "")
		v1beta2conditions.Set(virtualMachineCtx.VSphereVM, metav1.Condition{
			Type:   infrav1.VSphereVMVirtualMachineProvisionedV1Beta2Condition,
			Status: metav1.ConditionFalse,
			Reason: infrav1.VSphereVMVirtualMachinePoweringOnV1Beta2Reason,
		})

		// Update the VSphereVM.Status.TaskRef to track the power-on task.
		virtualMachineCtx.VSphereVM.Status.TaskRef = task.Reference().Value
		if err = virtualMachineCtx.Patch(ctx); err != nil {
			return false, err
		}

		// Once the VM is successfully powered on, a reconcile request should be
		// triggered once the VM reports IP addresses are available.
		reconcileVSphereVMWhenNetworkIsReady(ctx, virtualMachineCtx, task)

		log.Info("Wait for VM to be powered on")
		return false, nil
	case infrav1.VirtualMachinePowerStatePoweredOn:
		log.Info("VM is powered on")
		// Latch the initial power-on: once set to True this condition never changes and gates whether a
		// later powered-off VM is powered back on (it is not).
		conditions.MarkTrue(virtualMachineCtx.VSphereVM, infrav1.InitialPowerOnCompletedCondition)
		// Reflect the real-time power state; this is aggregated into the Ready condition.
		conditions.MarkTrue(virtualMachineCtx.VSphereVM, infrav1.PoweredOnCondition)
		v1beta2conditions.Set(virtualMachineCtx.VSphereVM, metav1.Condition{
			Type:   infrav1.VSphereVMPoweredOnV1Beta2Condition,
			Status: metav1.ConditionTrue,
			Reason: infrav1.VSphereVMPoweredOnV1Beta2Reason,
		})
		return true, nil
	default:
		return false, errors.Errorf("unexpected power state %q for vm %s", powerState, virtualMachineCtx)
	}
}

func (vms *VMService) reconcileStoragePolicy(ctx context.Context, virtualMachineCtx *virtualMachineContext) error {
	log := ctrl.LoggerFrom(ctx)

	if virtualMachineCtx.VSphereVM.Spec.StoragePolicyName == "" {
		log.V(5).Info("Storage policy not defined. skipping reconcile storage policy")
		return nil
	}

	// return early if the VM is already powered on
	powerState, err := vms.getPowerState(ctx, virtualMachineCtx)
	if err != nil {
		return err
	}
	if powerState == infrav1.VirtualMachinePowerStatePoweredOn {
		log.Info("VM powered on. Skipping reconcile storage policy")
		return nil
	}

	pbmClient, err := pbm.NewClient(ctx, virtualMachineCtx.Session.Client.Client)
	if err != nil {
		return errors.Wrap(err, "unable to create pbm client")
	}
	storageProfileID, err := pbmClient.ProfileIDByName(ctx, virtualMachineCtx.VSphereVM.Spec.StoragePolicyName)
	if err != nil {
		return errors.Wrap(err, "unable to retrieve storage profile ID")
	}

	var changes []types.BaseVirtualDeviceConfigSpec
	devices, err := virtualMachineCtx.Obj.Device(ctx)
	if err != nil {
		return err
	}

	disksRefs := make([]pbmTypes.PbmServerObjectRef, 0)
	// diskMap is just an auxiliar map so we don't need to iterate over and over disks to get their configs
	// if we realize they are not on the right storage policy
	diskMap := make(map[string]*types.VirtualDisk)

	disks := devices.SelectByType((*types.VirtualDisk)(nil))

	// We iterate over disks and create an array of disks refs, so we just need to make a single call
	// against vCenter, instead of one call per disk
	// the diskMap is an auxiliar way of, besides the disksRefs, we have a "searchable" disk configuration
	// in case we need to reconfigure a disk, to get its config
	for _, d := range disks {
		disk := d.(*types.VirtualDisk)
		// entities associated with storage policy has key in the form <vm-ID>:<disk>
		diskID := fmt.Sprintf("%s:%d", virtualMachineCtx.Obj.Reference().Value, disk.Key)
		diskMap[diskID] = disk

		disksRefs = append(disksRefs, pbmTypes.PbmServerObjectRef{
			ObjectType: string(pbmTypes.PbmObjectTypeVirtualDiskId),
			Key:        diskID,
		})
	}

	diskObjects, err := pbmClient.QueryAssociatedProfiles(ctx, disksRefs)
	if err != nil {
		return errors.Wrap(err, "unable to query disks associated profiles")
	}

	// Ensure storage policy is set correctly for all disks of the VM
	for k := range diskObjects {
		if !isStoragePolicyIDPresent(storageProfileID, diskObjects[k]) {
			log.V(5).Info("Storage policy not found on disk, adding for reconciliation", "disk", diskObjects[k].Object.Key)
			config := &types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationEdit,
				Device:    diskMap[diskObjects[k].Object.Key],
				Profile: []types.BaseVirtualMachineProfileSpec{
					&types.VirtualMachineDefinedProfileSpec{ProfileId: storageProfileID},
				},
			}
			changes = append(changes, config)
		}
	}

	// If there are pending changes for Storage Policies, do it before moving next
	if len(changes) > 0 {
		task, err := virtualMachineCtx.Obj.Reconfigure(ctx, types.VirtualMachineConfigSpec{
			VmProfile: []types.BaseVirtualMachineProfileSpec{
				&types.VirtualMachineDefinedProfileSpec{ProfileId: storageProfileID},
			},
			DeviceChange: changes,
		})
		if err != nil {
			return errors.Wrapf(err, "unable to set storagePolicy on vm %s", virtualMachineCtx)
		}
		virtualMachineCtx.VSphereVM.Status.TaskRef = task.Reference().Value
	}
	return nil
}

func (vms *VMService) reconcileUUID(ctx context.Context, virtualMachineCtx *virtualMachineContext) {
	virtualMachineCtx.State.BiosUUID = virtualMachineCtx.Obj.UUID(ctx)
}

func (vms *VMService) reconcileHardwareVersion(ctx context.Context, virtualMachineCtx *virtualMachineContext) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	if virtualMachineCtx.VSphereVM.Spec.HardwareVersion != "" {
		var virtualMachine mo.VirtualMachine
		if err := virtualMachineCtx.Obj.Properties(ctx, virtualMachineCtx.Obj.Reference(), []string{"config.version"}, &virtualMachine); err != nil {
			return false, errors.Wrapf(err, "error getting guestInfo version information from VM %s", virtualMachineCtx.VSphereVM.Name)
		}
		toUpgrade, err := util.LessThan(virtualMachine.Config.Version, virtualMachineCtx.VSphereVM.Spec.HardwareVersion)
		if err != nil {
			return false, errors.Wrapf(err, "failed to parse hardware version")
		}
		if toUpgrade {
			log.Info("Upgrading hardware version", "fromVersion", virtualMachine.Config.Version, "toVersion", virtualMachineCtx.VSphereVM.Spec.HardwareVersion)
			task, err := virtualMachineCtx.Obj.UpgradeVM(ctx, virtualMachineCtx.VSphereVM.Spec.HardwareVersion)
			if err != nil {
				return false, errors.Wrapf(err, "error trigging upgrade op for machine %s", virtualMachineCtx)
			}
			virtualMachineCtx.VSphereVM.Status.TaskRef = task.Reference().Value
			return false, nil
		}
	}
	return true, nil
}

func (vms *VMService) reconcilePCIDevices(ctx context.Context, virtualMachineCtx *virtualMachineContext) error {
	log := ctrl.LoggerFrom(ctx)

	if expectedPciDevices := virtualMachineCtx.VSphereVM.Spec.VirtualMachineCloneSpec.PciDevices; len(expectedPciDevices) != 0 {
		specsToBeAdded, err := pci.CalculateDevicesToBeAdded(ctx, virtualMachineCtx.Obj, expectedPciDevices)
		if err != nil {
			return err
		}

		if len(specsToBeAdded) == 0 {
			if conditions.Has(virtualMachineCtx.VSphereVM, infrav1.PCIDevicesDetachedCondition) {
				conditions.Delete(virtualMachineCtx.VSphereVM, infrav1.PCIDevicesDetachedCondition)

				v1beta2conditions.Delete(virtualMachineCtx.VSphereVM, infrav1.VSphereVMPCIDevicesDetachedV1Beta2Condition)
			}
			log.V(5).Info("No new PCI devices to be added")
			return nil
		}

		powerState, err := virtualMachineCtx.Obj.PowerState(ctx)
		if err != nil {
			return err
		}
		if powerState == types.VirtualMachinePowerStatePoweredOn {
			// This would arise only when the PCI device is manually removed from
			// the VM post creation.
			log.Info("PCI device cannot be attached in powered on state")
			conditions.MarkFalse(virtualMachineCtx.VSphereVM,
				infrav1.PCIDevicesDetachedCondition,
				infrav1.NotFoundReason,
				clusterv1.ConditionSeverityWarning,
				"PCI devices removed after VM was powered on")

			v1beta2conditions.Set(virtualMachineCtx.VSphereVM, metav1.Condition{
				Type:    infrav1.VSphereVMPCIDevicesDetachedV1Beta2Condition,
				Status:  metav1.ConditionFalse,
				Reason:  infrav1.VSphereVMPCIDevicesDetachedNotFoundV1Beta2Reason,
				Message: "PCI devices removed after VM was powered on",
			})
			return errors.Errorf("missing PCI devices")
		}
		log.Info("PCI devices to be added", "number", len(specsToBeAdded))
		if err := virtualMachineCtx.Obj.AddDevice(ctx, pci.ConstructDeviceSpecs(specsToBeAdded)...); err != nil {
			return errors.Wrapf(err, "error adding pci devices for %q", virtualMachineCtx)
		}
	}
	return nil
}

func (vms *VMService) getMetadata(ctx context.Context, virtualMachineCtx *virtualMachineContext) (string, error) {
	return vms.getGuestInfoValue(ctx, virtualMachineCtx, guestInfoKeyMetadata)
}

func (vms *VMService) getUserData(ctx context.Context, virtualMachineCtx *virtualMachineContext) (string, error) {
	return vms.getGuestInfoValue(ctx, virtualMachineCtx, guestInfoKeyUserData)
}

func (vms *VMService) getGuestInfoValue(ctx context.Context, virtualMachineCtx *virtualMachineContext, key string) (string, error) {
	var (
		obj mo.VirtualMachine

		pc    = property.DefaultCollector(virtualMachineCtx.Session.Client.Client)
		props = []string{"config.extraConfig"}
	)

	if err := pc.RetrieveOne(ctx, virtualMachineCtx.Ref, props, &obj); err != nil {
		return "", errors.Wrapf(err, "unable to fetch props %v for vm %s", props, virtualMachineCtx)
	}
	if obj.Config == nil {
		return "", nil
	}

	var base64Value string
	for _, ec := range obj.Config.ExtraConfig {
		if optVal := ec.GetOptionValue(); optVal != nil && optVal.Key == key {
			if v, ok := optVal.Value.(string); ok {
				base64Value = v
			}
		}
	}

	if base64Value == "" {
		return "", nil
	}

	metadataBuf, err := base64.StdEncoding.DecodeString(base64Value)
	if err != nil {
		return "", errors.Wrapf(err, "unable to decode guestinfo %s for %s", key, virtualMachineCtx)
	}

	return string(metadataBuf), nil
}

func (vms *VMService) reconcileHostInfo(ctx context.Context, virtualMachineCtx *virtualMachineContext) error {
	host, err := virtualMachineCtx.Obj.HostSystem(ctx)
	if err != nil {
		return err
	}
	name, err := host.ObjectName(ctx)
	if err != nil {
		return err
	}
	virtualMachineCtx.VSphereVM.Status.Host = name
	return nil
}

func (vms *VMService) setMetadata(ctx context.Context, virtualMachineCtx *virtualMachineContext, metadata []byte) (string, error) {
	var extraConfig extra.Config

	extraConfig.SetCloudInitMetadata(metadata)

	task, err := virtualMachineCtx.Obj.Reconfigure(ctx, types.VirtualMachineConfigSpec{
		ExtraConfig: extraConfig,
	})
	if err != nil {
		return "", errors.Wrapf(err, "unable to set metadata on vm %s", virtualMachineCtx)
	}

	return task.Reference().Value, nil
}

func (vms *VMService) setUserData(ctx context.Context, virtualMachineCtx *virtualMachineContext, userData []byte, format bootstrapv1.Format) (string, error) {
	var extraConfig extra.Config

	switch format {
	case bootstrapv1.CloudConfig:
		extraConfig.SetCloudInitUserData(userData)
	case bootstrapv1.Ignition:
		extraConfig.SetIgnitionUserData(userData)
	default:
		return "", errors.Errorf("unsupported bootstrap format %q for vm %s", format, virtualMachineCtx)
	}

	task, err := virtualMachineCtx.Obj.Reconfigure(ctx, types.VirtualMachineConfigSpec{
		ExtraConfig: extraConfig,
	})
	if err != nil {
		return "", errors.Wrapf(err, "unable to set user data on vm %s", virtualMachineCtx)
	}

	return task.Reference().Value, nil
}

func (vms *VMService) reconcileBootstrapUserData(ctx context.Context, virtualMachineCtx *virtualMachineContext) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	powerState, err := vms.getPowerState(ctx, virtualMachineCtx)
	if err != nil {
		return false, err
	}
	if powerState != infrav1.VirtualMachinePowerStatePoweredOff {
		return true, nil
	}

	bootstrapData, format, err := vms.getBootstrapData(ctx, &virtualMachineCtx.VMContext)
	if err != nil {
		return false, err
	}
	if format != bootstrapv1.CloudConfig {
		return true, nil
	}

	existingUserData, err := vms.getUserData(ctx, virtualMachineCtx)
	if err != nil {
		return false, err
	}

	mergedUserData := bootstrapData
	if hasKubeletServingCertUserData(existingUserData) {
		// Preserve the existing kubelet serving cert payload so user-data stays stable
		// across reconciles before the VM powers on.
		mergedUserData = []byte(existingUserData)
	}

	identity, err := resolveNodeIdentity(virtualMachineCtx)
	if err != nil {
		return false, err
	}
	mergedUserData, err = util.UpdateKubeadmNodeRegistration(mergedUserData, identity.Hostname, identity.NodeIP)
	if err != nil {
		return false, err
	}

	if virtualMachineCtx.MachineConfigSlot != nil && len(virtualMachineCtx.MachineConfigSlot.PersistentDisks) > 0 {
		diskConfig, err := util.GetPersistentDiskCloudConfig(virtualMachineCtx.MachineConfigSlot.PersistentDisks)
		if err != nil {
			return false, err
		}
		if len(diskConfig) > 0 {
			mergedUserData, err = util.MergeCloudConfigUserData(mergedUserData, diskConfig)
			if err != nil {
				return false, err
			}
		}
	}

	if !hasKubeletServingCertUserData(existingUserData) {
		kubeletServingCertConfig, err := vms.buildKubeletServingCertCloudConfig(ctx, virtualMachineCtx)
		if err != nil {
			return false, err
		}
		if len(kubeletServingCertConfig) > 0 {
			mergedUserData, err = util.MergeCloudConfigUserData(mergedUserData, kubeletServingCertConfig)
			if err != nil {
				return false, err
			}
		}
	} else {
		log.Info("Reusing existing kubelet serving cert bootstrap data")
	}

	if string(mergedUserData) == existingUserData {
		return true, nil
	}

	log.Info("Updating VM user-data with bootstrap extensions")
	taskRef, err := vms.setUserData(ctx, virtualMachineCtx, mergedUserData, format)
	if err != nil {
		return false, err
	}
	virtualMachineCtx.VSphereVM.Status.TaskRef = taskRef
	log.Info("Wait for VM user-data to be updated")
	return false, nil
}

func hasKubeletServingCertUserData(userData string) bool {
	if userData == "" {
		return false
	}
	// Check for the actual cert file write_files entries (base64-encoded PEM
	// content), not just path references that may appear in kubelet patches.
	// The real entries use "path: /etc/kubernetes/pki/kubelet.crt" as a YAML
	// key inside a write_files list item alongside "encoding: b64".
	return strings.Contains(userData, "path: /etc/kubernetes/pki/kubelet.crt") &&
		strings.Contains(userData, "path: /etc/kubernetes/pki/kubelet.key")
}

func (vms *VMService) getNetworkStatus(ctx context.Context, virtualMachineCtx *virtualMachineContext) ([]infrav1.NetworkStatus, error) {
	log := ctrl.LoggerFrom(ctx)

	allNetStatus, err := govmominet.GetNetworkStatus(ctx, virtualMachineCtx.Session.Client.Client, virtualMachineCtx.Ref)
	if err != nil {
		return nil, err
	}
	log.V(4).Info("Got allNetStatus", "status", allNetStatus)
	apiNetStatus := []infrav1.NetworkStatus{}
	for _, s := range allNetStatus {
		apiNetStatus = append(apiNetStatus, infrav1.NetworkStatus{
			Connected:   s.Connected,
			IPAddrs:     sanitizeIPAddrs(ctx, s.IPAddrs),
			MACAddr:     s.MACAddr,
			NetworkName: s.NetworkName,
		})
	}
	return apiNetStatus, nil
}

// bootstrapSecretGetError wraps a failure to read the bootstrap data Secret so the caller can surface a
// BootstrapSecretGetFailed reason distinct from a genuine clone failure.
type bootstrapSecretGetError struct{ err error }

func (e *bootstrapSecretGetError) Error() string { return e.err.Error() }
func (e *bootstrapSecretGetError) Unwrap() error { return e.err }

// bootstrapSecretContentError indicates the bootstrap data Secret was read but is missing required content.
type bootstrapSecretContentError struct{ msg string }

func (e *bootstrapSecretContentError) Error() string { return e.msg }

// getBootstrapData obtains a machine's bootstrap data from the relevant k8s secret and returns the
// data and its format. Failures are returned as typed errors (bootstrapSecretGetError,
// bootstrapSecretContentError) so callers can attribute a precise BootstrapReady reason.
func (vms *VMService) getBootstrapData(ctx context.Context, vmCtx *capvcontext.VMContext) ([]byte, bootstrapv1.Format, error) {
	log := ctrl.LoggerFrom(ctx)

	secret, err := vms.getBootstrapSecret(ctx, vmCtx)
	if err != nil {
		return nil, "", &bootstrapSecretGetError{err: err}
	}
	if secret == nil {
		log.Info("VM has no bootstrap data")
		return nil, "", nil
	}

	format, ok := secret.Data["format"]
	if !ok || len(format) == 0 {
		// Bootstrap data format is missing or empty - assume cloud-config.
		format = []byte(bootstrapv1.CloudConfig)
	}

	value, ok := secret.Data["value"]
	if !ok {
		return nil, "", &bootstrapSecretContentError{msg: "error retrieving bootstrap data: secret value key is missing"}
	}

	log.Info("Loaded bootstrap data", "format", string(format), "bytes", len(value), "hasMachineConfigSlot", vmCtx.MachineConfigSlot != nil)

	return value, bootstrapv1.Format(format), nil
}

func (vms *VMService) getBootstrapSecret(ctx context.Context, vmCtx *capvcontext.VMContext) (*corev1.Secret, error) {
	if vmCtx.VSphereVM.Spec.BootstrapRef == nil {
		return nil, nil
	}

	secret := &corev1.Secret{}
	secretKey := apitypes.NamespacedName{
		Namespace: vmCtx.VSphereVM.Spec.BootstrapRef.Namespace,
		Name:      vmCtx.VSphereVM.Spec.BootstrapRef.Name,
	}
	if err := vmCtx.Client.Get(ctx, secretKey, secret); err != nil {
		return nil, errors.Wrapf(err, "failed to get bootstrap data secret for %s", vmCtx)
	}
	return secret, nil
}

func (vms *VMService) buildKubeletServingCertCloudConfig(ctx context.Context, virtualMachineCtx *virtualMachineContext) ([]byte, error) {
	clusterName := virtualMachineCtx.VSphereVM.Labels[clusterv1.ClusterNameLabel]
	if clusterName == "" {
		return nil, nil
	}

	caSecret := &corev1.Secret{}
	caSecretKey := apitypes.NamespacedName{
		Namespace: virtualMachineCtx.VSphereVM.Namespace,
		Name:      fmt.Sprintf("%s-ca", clusterName),
	}
	if err := virtualMachineCtx.Client.Get(ctx, caSecretKey, caSecret); err != nil {
		return nil, errors.Wrapf(err, "failed to get cluster CA secret for %s", virtualMachineCtx)
	}

	caCert, ok := caSecret.Data["tls.crt"]
	if !ok || len(caCert) == 0 {
		return nil, errors.Errorf("cluster CA secret %s/%s is missing tls.crt", caSecretKey.Namespace, caSecretKey.Name)
	}
	caKey, ok := caSecret.Data["tls.key"]
	if !ok || len(caKey) == 0 {
		return nil, errors.Errorf("cluster CA secret %s/%s is missing tls.key", caSecretKey.Namespace, caSecretKey.Name)
	}

	identity, err := resolveNodeIdentity(virtualMachineCtx)
	if err != nil {
		return nil, err
	}

	bootstrapSecret, err := vms.getBootstrapSecret(ctx, &virtualMachineCtx.VMContext)
	if err != nil {
		return nil, err
	}
	if bootstrapSecret == nil {
		return nil, nil
	}
	if bootstrapSecret.Data == nil {
		bootstrapSecret.Data = map[string][]byte{}
	}

	kubeletCertPEM := bootstrapSecret.Data[bootstrapSecretKubeletServingCertKey]
	kubeletKeyPEM := bootstrapSecret.Data[bootstrapSecretKubeletServingKeyKey]
	expectedIPs := util.GetKubeletServingCertIPs(*virtualMachineCtx.VSphereVM, virtualMachineCtx.IPAMState, virtualMachineCtx.State.Network...)
	if len(kubeletCertPEM) == 0 || len(kubeletKeyPEM) == 0 || !certMatchesNodeIdentity(kubeletCertPEM, identity.Hostname, expectedIPs) {
		kubeletCertPEM, kubeletKeyPEM, err = util.NewKubeletServingCertData(
			identity.Hostname,
			*virtualMachineCtx.VSphereVM,
			virtualMachineCtx.IPAMState,
			caCert,
			caKey,
			virtualMachineCtx.State.Network...,
		)
		if err != nil {
			return nil, err
		}
		if len(kubeletCertPEM) == 0 || len(kubeletKeyPEM) == 0 {
			return nil, nil
		}
		bootstrapSecret.Data[bootstrapSecretKubeletServingCertKey] = kubeletCertPEM
		bootstrapSecret.Data[bootstrapSecretKubeletServingKeyKey] = kubeletKeyPEM
		if err := virtualMachineCtx.Client.Update(ctx, bootstrapSecret); err != nil {
			return nil, errors.Wrapf(err, "failed to persist kubelet serving cert data to bootstrap secret for %s", virtualMachineCtx)
		}
	}

	return util.GetKubeletServingCertCloudConfigFromPEM(kubeletCertPEM, kubeletKeyPEM)
}

func resolveNodeIdentity(virtualMachineCtx *virtualMachineContext) (*nodeIdentity, error) {
	if virtualMachineCtx == nil || virtualMachineCtx.VSphereVM == nil {
		return &nodeIdentity{}, nil
	}

	identity := &nodeIdentity{
		Hostname: virtualMachineCtx.VSphereVM.Name,
	}
	if virtualMachineCtx.MachineConfigSlot != nil {
		if virtualMachineCtx.MachineConfigSlot.Hostname != "" {
			identity.Hostname = virtualMachineCtx.MachineConfigSlot.Hostname
		}
		var networkStatuses []infrav1.NetworkStatus
		if virtualMachineCtx.State != nil {
			networkStatuses = virtualMachineCtx.State.Network
		}
		nodeIP, err := util.GetPrimaryNodeIPAddress(
			*virtualMachineCtx.VSphereVM,
			virtualMachineCtx.MachineConfigSlot,
			virtualMachineCtx.IPAMState,
			networkStatuses...,
		)
		if err != nil {
			switch errors.Cause(err) {
			case util.ErrNoMachineIPAddr, util.ErrPrimaryMachineIPAddr:
				return identity, nil
			default:
				return nil, err
			}
		}
		identity.NodeIP = nodeIP
	}

	return identity, nil
}

func certMatchesNodeIdentity(certPEM []byte, dnsName string, expectedIPs []string) bool {
	if len(certPEM) == 0 || dnsName == "" {
		return false
	}
	cert, err := util.ParseCertificatePEM(certPEM)
	if err != nil {
		return false
	}
	hasDNSName := false
	for _, existingDNSName := range cert.DNSNames {
		if existingDNSName == dnsName {
			hasDNSName = true
			break
		}
	}
	if !hasDNSName {
		return false
	}
	existingIPs := map[string]struct{}{}
	for _, ip := range cert.IPAddresses {
		existingIPs[ip.String()] = struct{}{}
	}
	if len(expectedIPs) != len(existingIPs) {
		return false
	}
	for _, expectedIP := range expectedIPs {
		if _, ok := existingIPs[expectedIP]; !ok {
			return false
		}
	}
	return true
}

func (vms *VMService) reconcileVMGroupInfo(ctx context.Context, virtualMachineCtx *virtualMachineContext) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	if virtualMachineCtx.VSphereFailureDomain == nil || virtualMachineCtx.VSphereFailureDomain.Spec.Topology.Hosts == nil {
		log.V(5).Info("Hosts topology in failure domain not defined. skipping reconcile VM group")
		return true, nil
	}

	topology := virtualMachineCtx.VSphereFailureDomain.Spec.Topology
	vmGroup, err := cluster.FindVMGroup(ctx, virtualMachineCtx, *topology.ComputeCluster, topology.Hosts.VMGroupName)
	if err != nil {
		return false, errors.Wrapf(err, "unable to find VM Group %s", topology.Hosts.VMGroupName)
	}

	hasVM, err := vmGroup.HasVM(virtualMachineCtx.Ref)
	if err != nil {
		return false, errors.Wrapf(err, "unable to find VM Group %s membership", topology.Hosts.VMGroupName)
	}

	if !hasVM {
		task, err := vmGroup.Add(ctx, virtualMachineCtx.Ref)
		if err != nil {
			return false, errors.Wrapf(err, "failed to add VM %s to VM group", virtualMachineCtx.VSphereVM.Name)
		}
		virtualMachineCtx.VSphereVM.Status.TaskRef = task.Reference().Value
		log.Info("Wait for VM to be added to group")
		return false, nil
	}
	return true, nil
}

func (vms *VMService) reconcileTags(ctx context.Context, virtualMachineCtx *virtualMachineContext) error {
	log := ctrl.LoggerFrom(ctx)

	if len(virtualMachineCtx.VSphereVM.Spec.TagIDs) == 0 {
		log.V(5).Info("No tags defined. skipping tags reconciliation")
		return nil
	}

	err := virtualMachineCtx.Session.TagManager.AttachMultipleTagsToObject(ctx, virtualMachineCtx.VSphereVM.Spec.TagIDs, virtualMachineCtx.Ref)
	if err != nil {
		return errors.Wrapf(err, "failed to attach tags %v to VM %s", virtualMachineCtx.VSphereVM.Spec.TagIDs, virtualMachineCtx.VSphereVM.Name)
	}

	return nil
}

func (vms *VMService) reconcileClusterModuleMembership(ctx context.Context, virtualMachineCtx *virtualMachineContext) error {
	log := ctrl.LoggerFrom(ctx)

	if virtualMachineCtx.ClusterModuleInfo != nil {
		log := log.WithValues("moduleUUID", *virtualMachineCtx.ClusterModuleInfo)
		ctx := ctrl.LoggerInto(ctx, log)

		provider := clustermodules.NewProvider(virtualMachineCtx.Session.TagManager.Client)

		if err := provider.AddMoRefToModule(ctx, *virtualMachineCtx.ClusterModuleInfo, virtualMachineCtx.Ref); err != nil {
			return err
		}
		virtualMachineCtx.VSphereVM.Status.ModuleUUID = virtualMachineCtx.ClusterModuleInfo
	}
	return nil
}

func (vms *VMService) detachPersistentDisks(ctx context.Context, virtualMachineCtx *virtualMachineContext) error {
	log := ctrl.LoggerFrom(ctx)
	devices, err := virtualMachineCtx.Obj.Device(ctx)
	if err != nil {
		return err
	}

	disks := devices.SelectByType((*types.VirtualDisk)(nil))
	var deviceChanges []types.BaseVirtualDeviceConfigSpec

	if virtualMachineCtx.MachineConfigSlot == nil {
		return nil
	}

	scsiKeys := findSCSIControllerKeys(devices)

	for _, d := range disks {
		disk := d.(*types.VirtualDisk)
		// Only consider disks on SCSI controllers; persistent data disks are
		// never placed on IDE/SATA controllers.
		if len(scsiKeys) > 0 {
			if _, onSCSI := scsiKeys[disk.GetVirtualDevice().ControllerKey]; !onSCSI {
				continue
			}
		}
		if backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
			for _, pd := range virtualMachineCtx.MachineConfigSlot.PersistentDisks {
				match := false
				if pd.VolumePath != "" {
					// VolumePath is the most precise identifier — use it as the primary match.
					if backing.FileName == pd.VolumePath {
						match = true
						if pd.UnitNumber != nil && disk.UnitNumber != nil && *pd.UnitNumber != *disk.UnitNumber {
							log.Info("WARNING: persistent disk VolumePath matches but UnitNumber differs",
								"disk", pd.Name, "path", backing.FileName,
								"expectedUnit", *pd.UnitNumber, "actualUnit", *disk.UnitNumber)
						}
					}
				} else if pd.UnitNumber != nil && disk.UnitNumber != nil && *pd.UnitNumber == *disk.UnitNumber {
					// Fall back to UnitNumber only when VolumePath is not available.
					match = true
				}

				if match {
					log.Info("Detaching persistent disk", "disk", pd.Name, "path", backing.FileName, "unit", disk.UnitNumber)
					deviceChanges = append(deviceChanges, &types.VirtualDeviceConfigSpec{
						Operation: types.VirtualDeviceConfigSpecOperationRemove,
						Device:    disk,
					})
					break
				}
			}
		}
	}

	if len(deviceChanges) > 0 {
		task, err := virtualMachineCtx.Obj.Reconfigure(ctx, types.VirtualMachineConfigSpec{
			DeviceChange: deviceChanges,
		})
		if err != nil {
			return err
		}
		return task.Wait(ctx)
	}

	return nil
}

func (vms *VMService) reconcilePersistentDiskStatuses(ctx context.Context, virtualMachineCtx *virtualMachineContext) error {
	if virtualMachineCtx.MachineConfigSlot == nil || len(virtualMachineCtx.MachineConfigSlot.PersistentDisks) == 0 {
		return nil
	}

	devices, err := virtualMachineCtx.Obj.Device(ctx)
	if err != nil {
		return err
	}

	disks := devices.SelectByType((*types.VirtualDisk)(nil))
	scsiKeys := findSCSIControllerKeys(devices)
	updated := false
	usedDiskKeys := map[int32]struct{}{}
	referencedDiskKeys := findReferencedPersistentDiskKeys(virtualMachineCtx.MachineConfigSlot.PersistentDisks, disks, scsiKeys)

	for i := range virtualMachineCtx.MachineConfigSlot.PersistentDisks {
		pd := &virtualMachineCtx.MachineConfigSlot.PersistentDisks[i]
		disk := findPersistentDiskDevice(pd, disks, usedDiskKeys, referencedDiskKeys, scsiKeys)
		if disk == nil {
			continue
		}

		usedDiskKeys[disk.Key] = struct{}{}
		if pd.UnitNumber == nil && disk.UnitNumber != nil {
			unitNumber := *disk.UnitNumber
			pd.UnitNumber = &unitNumber
			updated = true
		}

		if backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
			if pd.VolumePath != backing.FileName {
				pd.VolumePath = backing.FileName
				updated = true
			}
			if pd.DiskUUID != backing.Uuid {
				pd.DiskUUID = backing.Uuid
				updated = true
			}
		}
	}

	if updated {
		if err := persistMachineConfigSlotBackfill(ctx, &virtualMachineCtx.VMContext); err != nil {
			return err
		}
	}
	if err := util.ValidatePersistentDiskBackfill(virtualMachineCtx.MachineConfigSlot.PersistentDisks); err != nil {
		return errors.Wrapf(err, "persistent disk metadata incomplete for machine config slot %q; refusing to generate persistent disk user-data or power on VM", virtualMachineCtx.MachineConfigSlot.Hostname)
	}
	return nil
}

// findSCSIControllerKeys returns the keys of all SCSI controllers found in devices.
func findSCSIControllerKeys(devices object.VirtualDeviceList) map[int32]struct{} {
	keys := map[int32]struct{}{}
	for _, device := range devices {
		if sc, ok := device.(types.BaseVirtualSCSIController); ok {
			keys[sc.GetVirtualSCSIController().Key] = struct{}{}
		}
	}
	return keys
}

// findPersistentDiskDevice locates the VM disk that corresponds to a persistent
// disk spec. It uses a three-tier match strategy:
//  1. VolumePath (exact VMDK file path — globally unique, most reliable)
//  2. UnitNumber on a SCSI controller (stable across VM recreations when the
//     same unit is reused)
//  3. Capacity on a SCSI controller (last resort; chooses the first deterministic
//     unused and unreferenced candidate)
//
// Tiers 2 and 3 are restricted to SCSI controllers because CAPV always places
// persistent data disks on SCSI. This prevents false matches against the OS
// disk when it lives on an IDE or SATA controller.
func findPersistentDiskDevice(pd *infrav1.PersistentDisk, disks object.VirtualDeviceList, usedDiskKeys, referencedDiskKeys map[int32]struct{}, scsiKeys map[int32]struct{}) *types.VirtualDisk {
	if pd == nil {
		return nil
	}

	// Tier 1: match by VolumePath (any controller — the path is unique).
	if pd.VolumePath != "" {
		for _, d := range disks {
			disk := d.(*types.VirtualDisk)
			if _, used := usedDiskKeys[disk.Key]; used {
				continue
			}
			if backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
				if backing.FileName == pd.VolumePath {
					return disk
				}
			}
		}
	}

	// Tier 2: match by UnitNumber on a SCSI controller.
	if pd.UnitNumber != nil {
		for _, d := range disks {
			disk := d.(*types.VirtualDisk)
			if _, onSCSI := scsiKeys[disk.GetVirtualDevice().ControllerKey]; !onSCSI {
				continue
			}
			if disk.UnitNumber == nil || *disk.UnitNumber != *pd.UnitNumber {
				continue
			}
			if _, used := usedDiskKeys[disk.Key]; used {
				continue
			}
			return disk
		}
	}

	expectedCapacityKB := int64(pd.SizeGiB) * 1024 * 1024
	candidates := []*types.VirtualDisk{}
	for _, d := range disks {
		disk := d.(*types.VirtualDisk)
		if _, onSCSI := scsiKeys[disk.GetVirtualDevice().ControllerKey]; !onSCSI {
			continue
		}
		if _, used := usedDiskKeys[disk.Key]; used {
			continue
		}
		if _, referenced := referencedDiskKeys[disk.Key]; referenced {
			continue
		}
		if disk.CapacityInKB != expectedCapacityKB {
			continue
		}
		candidates = append(candidates, disk)
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return compareVirtualDisks(candidates[i], candidates[j]) < 0
	})
	return candidates[0]
}

func findReferencedPersistentDiskKeys(persistentDisks []infrav1.PersistentDisk, disks object.VirtualDeviceList, scsiKeys map[int32]struct{}) map[int32]struct{} {
	referenced := map[int32]struct{}{}
	for i := range persistentDisks {
		pd := &persistentDisks[i]
		if pd.VolumePath != "" {
			for _, d := range disks {
				disk := d.(*types.VirtualDisk)
				if backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok && backing.FileName == pd.VolumePath {
					referenced[disk.Key] = struct{}{}
				}
			}
		}
		if pd.UnitNumber != nil {
			for _, d := range disks {
				disk := d.(*types.VirtualDisk)
				if _, onSCSI := scsiKeys[disk.GetVirtualDevice().ControllerKey]; !onSCSI {
					continue
				}
				if disk.UnitNumber != nil && *disk.UnitNumber == *pd.UnitNumber {
					referenced[disk.Key] = struct{}{}
				}
			}
		}
	}
	return referenced
}

func compareVirtualDisks(a, b *types.VirtualDisk) int {
	if a.GetVirtualDevice().ControllerKey != b.GetVirtualDevice().ControllerKey {
		if a.GetVirtualDevice().ControllerKey < b.GetVirtualDevice().ControllerKey {
			return -1
		}
		return 1
	}
	if compareUnitNumber(a.UnitNumber, b.UnitNumber) != 0 {
		return compareUnitNumber(a.UnitNumber, b.UnitNumber)
	}
	if a.Key != b.Key {
		if a.Key < b.Key {
			return -1
		}
		return 1
	}
	aPath := diskBackingFileName(a)
	bPath := diskBackingFileName(b)
	if aPath < bPath {
		return -1
	}
	if aPath > bPath {
		return 1
	}
	return 0
}

func compareUnitNumber(a, b *int32) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return 1
	}
	if b == nil {
		return -1
	}
	if *a < *b {
		return -1
	}
	if *a > *b {
		return 1
	}
	return 0
}

func diskBackingFileName(disk *types.VirtualDisk) string {
	if backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
		return backing.FileName
	}
	return ""
}

func persistMachineConfigSlotBackfill(ctx context.Context, vmCtx *capvcontext.VMContext) error {
	if vmCtx == nil || vmCtx.Client == nil || vmCtx.MachineConfigSlot == nil {
		return nil
	}

	machine, err := util.GetOwnerVSphereMachine(ctx, vmCtx.Client, vmCtx.VSphereVM.ObjectMeta)
	if err != nil || machine == nil || machine.Spec.MachineConfigPoolRef == nil {
		return err
	}

	poolKey := client.ObjectKey{
		Namespace: machine.Spec.MachineConfigPoolRef.Namespace,
		Name:      machine.Spec.MachineConfigPoolRef.Name,
	}

	for range 3 {
		pool := &infrav1.VSphereMachineConfigPool{}
		if err := vmCtx.Client.Get(ctx, poolKey, pool); err != nil {
			return errors.Wrapf(err, "failed to get machine config pool for vm %s", vmCtx.VSphereVM.Name)
		}

		if !services.ApplyDiskBackfill(pool, vmCtx.MachineConfigSlot) {
			return nil
		}

		if err := vmCtx.Client.Update(ctx, pool); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return errors.Wrapf(err, "failed to persist machine config pool disk backfill for vm %s", vmCtx.VSphereVM.Name)
		}
		return nil
	}
	return errors.Errorf("transient conflict while persisting machine config pool disk backfill for vm %s, exhausted retries", vmCtx.VSphereVM.Name)
}
