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

// Package services contains tools for handling VSphere services.
package services

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterutilv1 "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta2conditions "sigs.k8s.io/cluster-api/util/conditions/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	infrautilv1 "sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

// VimMachineService reconciles VSphere VMs.
type VimMachineService struct {
	Client client.Client
}

// GetMachinesInCluster returns a list of VSphereMachine objects belonging to the cluster.
func (v *VimMachineService) GetMachinesInCluster(
	ctx context.Context,
	namespace, clusterName string) ([]client.Object, error) {
	labels := map[string]string{clusterv1.ClusterNameLabel: clusterName}
	machineList := &infrav1.VSphereMachineList{}

	if err := v.Client.List(
		ctx, machineList,
		client.InNamespace(namespace),
		client.MatchingLabels(labels)); err != nil {
		return nil, err
	}

	objects := []client.Object{}
	for _, machine := range machineList.Items {
		m := machine
		objects = append(objects, &m)
	}
	return objects, nil
}

// FetchVSphereMachine returns a new MachineContext containing the vsphereMachine.
func (v *VimMachineService) FetchVSphereMachine(ctx context.Context, name types.NamespacedName) (capvcontext.MachineContext, error) {
	vsphereMachine := &infrav1.VSphereMachine{}
	err := v.Client.Get(ctx, name, vsphereMachine)

	return &capvcontext.VIMMachineContext{VSphereMachine: vsphereMachine}, err
}

// FetchVSphereCluster adds the VSphereCluster associated with the passed cluster to the machineContext.
func (v *VimMachineService) FetchVSphereCluster(ctx context.Context, cluster *clusterv1.Cluster, machineContext capvcontext.MachineContext) (capvcontext.MachineContext, error) {
	vimMachineCtx, ok := machineContext.(*capvcontext.VIMMachineContext)
	if !ok {
		return nil, errors.New("received unexpected VIMMachineContext type")
	}
	vsphereCluster := &infrav1.VSphereCluster{}
	vsphereClusterName := client.ObjectKey{
		Namespace: machineContext.GetObjectMeta().Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	err := v.Client.Get(ctx, vsphereClusterName, vsphereCluster)

	vimMachineCtx.VSphereCluster = vsphereCluster
	return vimMachineCtx, err
}

// ReconcileDelete reconciles delete events for the VSphere VM.
func (v *VimMachineService) ReconcileDelete(ctx context.Context, machineCtx capvcontext.MachineContext) error {
	vimMachineCtx, ok := machineCtx.(*capvcontext.VIMMachineContext)
	if !ok {
		return errors.New("received unexpected VIMMachineContext type")
	}

	vm, err := v.findVSphereVM(ctx, vimMachineCtx)
	// Attempt to find the associated VSphereVM resource.
	if err != nil {
		return err
	}

	if vm != nil && vm.GetDeletionTimestamp().IsZero() {
		// If the VSphereVM was found and it's not already enqueued for
		// deletion, go ahead and attempt to delete it.
		if err := v.Client.Delete(ctx, vm); err != nil {
			return err
		}
	}

	// VSphereMachine wraps a VMSphereVM, so we are mirroring status from the underlying VMSphereVM
	// in order to provide evidences about machine deletion.
	conditions.SetMirror(vimMachineCtx.VSphereMachine, infrav1.VMProvisionedCondition, vm)
	v1beta2conditions.SetMirrorCondition(vm, vimMachineCtx.VSphereMachine, infrav1.VSphereVMVirtualMachineProvisionedV1Beta2Condition,
		v1beta2conditions.TargetConditionType(infrav1.VSphereMachineVirtualMachineProvisionedV1Beta2Condition))
	return nil
}

// SyncFailureReason returns true if the VSphere Machine has failed.
func (v *VimMachineService) SyncFailureReason(ctx context.Context, machineCtx capvcontext.MachineContext) (bool, error) {
	vimMachineCtx, ok := machineCtx.(*capvcontext.VIMMachineContext)
	if !ok {
		return false, errors.New("received unexpected VIMMachineContext type")
	}

	vsphereVM, err := v.findVSphereVM(ctx, vimMachineCtx)
	if err != nil {
		return false, err
	}
	if vsphereVM != nil {
		// Reconcile VSphereMachine's failures
		vimMachineCtx.VSphereMachine.Status.FailureReason = vsphereVM.Status.FailureReason
		vimMachineCtx.VSphereMachine.Status.FailureMessage = vsphereVM.Status.FailureMessage
	}

	return vimMachineCtx.VSphereMachine.Status.FailureReason != nil || vimMachineCtx.VSphereMachine.Status.FailureMessage != nil, err
}

// ReconcileNormal reconciles create and update events for the VSphere VM.
func (v *VimMachineService) ReconcileNormal(ctx context.Context, machineCtx capvcontext.MachineContext) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	vimMachineCtx, ok := machineCtx.(*capvcontext.VIMMachineContext)
	if !ok {
		return false, errors.New("received unexpected VIMMachineContext type")
	}

	if err := v.reconcileMachineConfigPool(ctx, vimMachineCtx); err != nil {
		return false, err
	}

	vsphereVM, err := v.findVSphereVM(ctx, vimMachineCtx)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}

	log = log.WithValues("VSphereVM", klog.KObj(vsphereVM))
	ctx = ctrl.LoggerInto(ctx, log)
	vm, err := v.createOrPatchVSphereVM(ctx, vimMachineCtx, vsphereVM)
	if err != nil {
		return false, err
	}

	// Persist any changes back to the ResourcePool (e.g. backfilled VolumePath)
	if err := v.persistMachineConfigPoolChanges(ctx, vimMachineCtx); err != nil {
		return false, err
	}

	// Mirror the VM's real-time power state onto the VSphereMachine so that a VM powered off out of band
	// after its initial power-on surfaces the machine as not ready in v1beta2 as well.
	reconcilePoweredOnCondition(vimMachineCtx.VSphereMachine, vm)

	// Mirror the VM's BootstrapReady condition onto the VSphereMachine so bootstrap-delivery failures are
	// visible there with a precise reason (v1beta1 is already covered by the VMProvisioned mirror below).
	reconcileBootstrapReadyCondition(vimMachineCtx.VSphereMachine, vm)

	// Waits the VM's ready state.
	if !vm.Status.Ready {
		log.Info("Waiting for VSphereVM to become ready")
		// VSphereMachine wraps a VMSphereVM, so we are mirroring status from the underlying VMSphereVM
		// in order to provide evidences about machine provisioning while provisioning is actually happening.
		conditions.SetMirror(vimMachineCtx.VSphereMachine, infrav1.VMProvisionedCondition, vm)
		v1beta2conditions.SetMirrorCondition(vm, vimMachineCtx.VSphereMachine, infrav1.VSphereVMVirtualMachineProvisionedV1Beta2Condition,
			v1beta2conditions.TargetConditionType(infrav1.VSphereMachineVirtualMachineProvisionedV1Beta2Condition))
		return true, nil
	}

	// Reconcile the VSphereMachine's provider ID using the VM's BIOS UUID.
	if ok, err := v.reconcileProviderID(ctx, vimMachineCtx, vm); !ok {
		if err != nil {
			return false, errors.Wrapf(err, "unexpected error while reconciling provider ID for %s", vimMachineCtx)
		}
		return true, nil
	}

	// Reconcile the VSphereMachine's node addresses from the VM's IP addresses.
	if ok, err := v.reconcileNetwork(ctx, vimMachineCtx, vm); !ok {
		if err != nil {
			return false, errors.Wrapf(err, "unexpected error while reconciling network for %s", vimMachineCtx)
		}
		conditions.MarkFalse(vimMachineCtx.VSphereMachine, infrav1.VMProvisionedCondition, infrav1.WaitingForNetworkAddressesReason, clusterv1.ConditionSeverityInfo, "")
		v1beta2conditions.Set(vimMachineCtx.VSphereMachine, metav1.Condition{
			Type:   infrav1.VSphereMachineVirtualMachineProvisionedV1Beta2Condition,
			Status: metav1.ConditionFalse,
			Reason: infrav1.VSphereMachineVirtualMachineWaitingForNetworkAddressV1Beta2Reason,
		})
		return true, nil
	}

	vimMachineCtx.VSphereMachine.Status.Ready = true
	return false, nil
}

// reconcilePoweredOnCondition mirrors the VSphereVM's real-time PoweredOn condition onto the VSphereMachine
// so the machine surfaces as not ready when the VM is powered off out of band after its initial power-on.
//
// v1beta1 is already covered by the VMProvisioned mirror, which copies the VSphereVM's Ready condition (and
// Ready aggregates PoweredOn). Only the v1beta2 VirtualMachineProvisioned mirror tracks provisioning alone and
// stays True while powered off, so PoweredOn is mirrored separately for v1beta2. It is mirrored only once the
// VM reports it, so before the initial power-on the (missing) condition is ignored by the Ready summary.
func reconcilePoweredOnCondition(machine *infrav1.VSphereMachine, vm *infrav1.VSphereVM) {
	if v1beta2conditions.Get(vm, infrav1.VSphereVMPoweredOnV1Beta2Condition) == nil {
		return
	}
	v1beta2conditions.SetMirrorCondition(vm, machine, infrav1.VSphereVMPoweredOnV1Beta2Condition,
		v1beta2conditions.TargetConditionType(infrav1.VSphereMachinePoweredOnV1Beta2Condition))
}

// reconcileBootstrapReadyCondition mirrors the VSphereVM's v1beta2 BootstrapReady condition onto the
// VSphereMachine so bootstrap-delivery failures surface on the machine with a precise reason.
//
// v1beta1 is already covered by the VMProvisioned mirror, which copies the VSphereVM's Ready condition
// (and Ready aggregates BootstrapReady). Only the v1beta2 side needs a dedicated mirror. It is mirrored
// only once the VM reports it, so before the VSphereVM is created the (missing) condition is ignored by
// the Ready summary.
func reconcileBootstrapReadyCondition(machine *infrav1.VSphereMachine, vm *infrav1.VSphereVM) {
	if v1beta2conditions.Get(vm, infrav1.VSphereVMBootstrapReadyV1Beta2Condition) == nil {
		return
	}
	v1beta2conditions.SetMirrorCondition(vm, machine, infrav1.VSphereVMBootstrapReadyV1Beta2Condition,
		v1beta2conditions.TargetConditionType(infrav1.VSphereMachineBootstrapReadyV1Beta2Condition))
}

// GetHostInfo returns the hostname or IP address of the infrastructure host for the VSphere VM.
func (v *VimMachineService) GetHostInfo(ctx context.Context, machineCtx capvcontext.MachineContext) (string, error) {
	log := ctrl.LoggerFrom(ctx)
	vimMachineCtx, ok := machineCtx.(*capvcontext.VIMMachineContext)
	if !ok {
		return "", errors.New("received unexpected VIMMachineContext type")
	}

	name, err := generateVMObjectName(vimMachineCtx, vimMachineCtx.Machine.Name)
	if err != nil {
		return "", err
	}

	vsphereVM := &infrav1.VSphereVM{}
	if err := v.Client.Get(ctx, client.ObjectKey{
		Namespace: vimMachineCtx.VSphereMachine.Namespace,
		Name:      name,
	}, vsphereVM); err != nil {
		return "", err
	}

	if conditions.IsTrue(vsphereVM, infrav1.VMProvisionedCondition) {
		return vsphereVM.Status.Host, nil
	}
	log.V(4).Info("Returning empty host info as VMProvisioned condition is not set to true")
	return "", nil
}

func (v *VimMachineService) findVSphereVM(ctx context.Context, vimMachineCtx *capvcontext.VIMMachineContext) (*infrav1.VSphereVM, error) {
	// Get ready to find the associated VSphereVM resource.
	vm := &infrav1.VSphereVM{}
	name, err := generateVMObjectName(vimMachineCtx, vimMachineCtx.Machine.Name)
	if err != nil {
		return nil, err
	}

	vmKey := types.NamespacedName{
		Namespace: vimMachineCtx.VSphereMachine.Namespace,
		Name:      name,
	}
	// Attempt to find the associated VSphereVM resource.
	if err := v.Client.Get(ctx, vmKey, vm); err != nil {
		return nil, err
	}
	return vm, nil
}

func (v *VimMachineService) reconcileProviderID(ctx context.Context, vimMachineCtx *capvcontext.VIMMachineContext, vm *infrav1.VSphereVM) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	biosUUID := vm.Spec.BiosUUID
	if biosUUID == "" {
		log.Info("providerID cannot be reconciled: VSphereVM.spec.biosUUID is empty")
		return false, nil
	}

	providerID := infrautilv1.ConvertUUIDToProviderID(biosUUID)
	if providerID == "" {
		return false, errors.Errorf("failed to reconcile providerID: invalid BIOS UUID %s for %s", biosUUID, vimMachineCtx)
	}
	if vimMachineCtx.VSphereMachine.Spec.ProviderID == nil || *vimMachineCtx.VSphereMachine.Spec.ProviderID != providerID {
		vimMachineCtx.VSphereMachine.Spec.ProviderID = &providerID
		log.Info("Updating providerID on VSphereMachine", "providerID", providerID)
	}

	return true, nil
}

func (v *VimMachineService) reconcileNetwork(ctx context.Context, vimMachineCtx *capvcontext.VIMMachineContext, vm *infrav1.VSphereVM) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	var errs []error
	networkStatusListOfIfaces := vm.Status.Network
	var networkStatusList []infrav1.NetworkStatus
	for i, networkStatusListMemberIface := range networkStatusListOfIfaces {
		if buf, err := json.Marshal(networkStatusListMemberIface); err != nil {
			log.Error(err,
				"unsupported data for member of status.network list",
				"index", i)
			errs = append(errs, err)
		} else {
			var networkStatus infrav1.NetworkStatus
			err := json.Unmarshal(buf, &networkStatus)
			if err == nil && networkStatus.MACAddr == "" {
				err = errors.New("macAddr is required")
				errs = append(errs, err)
			}
			if err != nil {
				log.Error(err,
					"unsupported data for member of status.network list",
					"index", i, "data", string(buf))
				errs = append(errs, err)
			} else {
				networkStatusList = append(networkStatusList, networkStatus)
			}
		}
	}
	vimMachineCtx.VSphereMachine.Status.Network = networkStatusList

	addresses := vm.Status.Addresses
	machineAddresses := make([]clusterv1.MachineAddress, 0, len(addresses))
	for _, addr := range addresses {
		machineAddresses = append(machineAddresses, clusterv1.MachineAddress{
			Type:    clusterv1.MachineExternalIP,
			Address: addr,
		})
	}
	machineAddresses = append(machineAddresses, clusterv1.MachineAddress{
		Type:    clusterv1.MachineInternalDNS,
		Address: vm.GetName(),
	})
	vimMachineCtx.VSphereMachine.Status.Addresses = machineAddresses

	if !hasMachineIPAddress(vimMachineCtx.VSphereMachine.Status.Addresses) {
		log.Info("Network cannot be reconciled: waiting for IP addresses")
		return false, kerrors.NewAggregate(errs)
	}

	return true, nil
}

func hasMachineIPAddress(addresses []clusterv1.MachineAddress) bool {
	for _, addr := range addresses {
		if addr.Type != clusterv1.MachineInternalIP && addr.Type != clusterv1.MachineExternalIP {
			continue
		}
		if strings.TrimSpace(addr.Address) == "" {
			continue
		}
		return true
	}
	return false
}

func (v *VimMachineService) createOrPatchVSphereVM(ctx context.Context, vimMachineCtx *capvcontext.VIMMachineContext, vsphereVM *infrav1.VSphereVM) (*infrav1.VSphereVM, error) {
	log := ctrl.LoggerFrom(ctx)
	// Create or update the VSphereVM resource.
	name, err := generateVMObjectName(vimMachineCtx, vimMachineCtx.Machine.Name)
	if err != nil {
		return nil, err
	}

	vm := &infrav1.VSphereVM{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: vimMachineCtx.VSphereMachine.Namespace,
			Name:      name,
		},
	}
	mutateFn := func() (err error) {
		// Ensure the VSphereMachine is marked as an owner of the VSphereVM.
		vm.SetOwnerReferences(clusterutilv1.EnsureOwnerRef(
			vm.OwnerReferences,
			metav1.OwnerReference{
				APIVersion: infrav1.GroupVersion.String(),
				Kind:       "VSphereMachine",
				Name:       vimMachineCtx.VSphereMachine.Name,
				UID:        vimMachineCtx.VSphereMachine.UID,
			}))

		// Instruct the VSphereVM to use the CAPI bootstrap data resource.
		// TODO: BootstrapRef field should be replaced with BootstrapSecret of type string
		vm.Spec.BootstrapRef = &corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Secret",
			Name:       *vimMachineCtx.Machine.Spec.Bootstrap.DataSecretName,
			Namespace:  vimMachineCtx.Machine.ObjectMeta.Namespace,
		}

		// Initialize the VSphereVM's labels map if it is nil.
		if vm.Labels == nil {
			vm.Labels = map[string]string{}
		}
		if vm.Annotations == nil {
			vm.Annotations = map[string]string{}
		}

		// Ensure the VSphereVM has a label that can be used when searching for
		// resources associated with the target cluster.
		vm.Labels[clusterv1.ClusterNameLabel] = vimMachineCtx.Machine.Labels[clusterv1.ClusterNameLabel]

		// For convenience, add a label that makes it easy to figure out if the
		// VSphereVM resource is part of some control plane.
		if val, ok := vimMachineCtx.Machine.Labels[clusterv1.MachineControlPlaneLabel]; ok {
			vm.Labels[clusterv1.MachineControlPlaneLabel] = val
		}

		// Copy the VSphereMachine's VM clone spec into the VSphereVM's
		// clone spec.
		vimMachineCtx.VSphereMachine.Spec.VirtualMachineCloneSpec.DeepCopyInto(&vm.Spec.VirtualMachineCloneSpec)

		if vimMachineCtx.MachineConfigSlot != nil {
			vm.Annotations["infrastructure.cluster.x-k8s.io/machine-config-slot-hostname"] = vimMachineCtx.MachineConfigSlot.Hostname
			v.mergeSlotNetwork(vm, vimMachineCtx.MachineConfigSlot.Network)
			v.mergeSlotPersistentDisks(vm, vimMachineCtx.MachineConfigSlot.PersistentDisks)
			v.mergeSlotEphemeralDisks(vm, vimMachineCtx.MachineConfigSlot.EphemeralDisks)
		}

		// If Failure Domain is present on CAPI machine, use that to override the vm clone spec.
		if overrideFunc, ok := v.generateOverrideFunc(ctx, vimMachineCtx); ok {
			overrideFunc(vm)
		}

		if vimMachineCtx.MachineConfigSlot != nil {
			datacenter, err := ResolveMachineConfigPoolDatacenterFromRef(ctx, v.Client, vimMachineCtx.VSphereMachine.Spec.MachineConfigPoolRef, vimMachineCtx.MachineConfigSlot)
			if err != nil {
				return err
			}
			if datacenter != "" {
				vm.Spec.Datacenter = datacenter
			}
		}

		// Several of the VSphereVM's clone spec properties can be derived
		// from multiple places. The order is:
		//
		//   1. From the Machine.Spec.FailureDomain
		//   2. From the VSphereMachine.Spec (the DeepCopyInto above)
		//   3. From the VSphereCluster.Spec
		if vm.Spec.Server == "" {
			vm.Spec.Server = vimMachineCtx.VSphereCluster.Spec.Server
		}
		if vm.Spec.Thumbprint == "" {
			vm.Spec.Thumbprint = vimMachineCtx.VSphereCluster.Spec.Thumbprint
		}
		if vsphereVM != nil {
			vm.Spec.BiosUUID = vsphereVM.Spec.BiosUUID
		}
		vm.Spec.PowerOffMode = vimMachineCtx.VSphereMachine.Spec.PowerOffMode
		vm.Spec.GuestSoftPowerOffTimeout = vimMachineCtx.VSphereMachine.Spec.GuestSoftPowerOffTimeout
		return nil
	}

	result, err := ctrlutil.CreateOrPatch(ctx, v.Client, vm, mutateFn)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to CreateOrPatch VSphereVM")
	}
	switch result {
	case ctrlutil.OperationResultNone:
		log.V(3).Info("No update required for VSphereVM")
	case ctrlutil.OperationResultCreated:
		log.Info("Created VSphereVM")
	case ctrlutil.OperationResultUpdated:
		log.Info("Updated VSphereVM")
	case ctrlutil.OperationResultUpdatedStatus:
		log.Info("Updated VSphereVM and VSphereVM status")
	case ctrlutil.OperationResultUpdatedStatusOnly:
		log.Info("Updated VSphereVM status")
	}

	return vm, nil
}

// generateVMObjectName returns a new VM object name in specific cases, otherwise return the same
// passed in the parameter.
func generateVMObjectName(vimMachineCtx *capvcontext.VIMMachineContext, machineName string) (string, error) {
	name, err := GenerateVSphereVMName(machineName, vimMachineCtx.VSphereMachine.Spec.NamingStrategy)
	if err != nil {
		return "", err
	}
	// Windows VM names must have 15 characters length at max.
	if vimMachineCtx.VSphereMachine.Spec.OS == infrav1.Windows && len(machineName) > 15 {
		return strings.TrimSuffix(name[0:9], "-") + "-" + name[len(name)-5:], nil
	}
	return name, nil
}

// GenerateVSphereVMName generates the name of a VSphereVM based on the naming strategy.
func GenerateVSphereVMName(machineName string, namingStrategy *infrav1.VSphereVMNamingStrategy) (string, error) {
	// Per default the name of the VSphereVM should be equal to the Machine name (this is the same as "{{ .machine.name }}")
	if namingStrategy == nil || namingStrategy.Template == nil {
		// Note: No need to trim to max length in this case as valid Machine names will also be valid VSphereVM names.
		return machineName, nil
	}

	name, err := infrautilv1.GenerateMachineNameFromTemplate(machineName, namingStrategy.Template)
	if err != nil {
		return name, errors.Wrap(err, "failed to generate name for VSphereVM")
	}

	return name, nil
}

// generateOverrideFunc returns a function which can override the values in the VSphereVM Spec
// with the values from the FailureDomain (if any) set on the owner CAPI machine.
func (v *VimMachineService) generateOverrideFunc(ctx context.Context, vimMachineCtx *capvcontext.VIMMachineContext) (func(vm *infrav1.VSphereVM), bool) {
	log := ctrl.LoggerFrom(ctx)
	failureDomainName := vimMachineCtx.Machine.Spec.FailureDomain
	if failureDomainName == nil {
		return nil, false
	}

	// Use the failureDomain name to fetch the vSphereDeploymentZone object
	var vsphereDeploymentZone infrav1.VSphereDeploymentZone
	if err := v.Client.Get(ctx, client.ObjectKey{Name: *failureDomainName}, &vsphereDeploymentZone); err != nil {
		log.Error(err, "Failed to get VSphereDeploymentZone", "VSphereDeploymentZone", klog.KRef("", *failureDomainName))
		return nil, false
	}

	var vsphereFailureDomain infrav1.VSphereFailureDomain
	if err := v.Client.Get(ctx, client.ObjectKey{Name: vsphereDeploymentZone.Spec.FailureDomain}, &vsphereFailureDomain); err != nil {
		log.Error(err, "Failed to get VSphereFailureDomain", "VSphereFailureDomain", klog.KRef("", vsphereDeploymentZone.Spec.FailureDomain))
		return nil, false
	}

	overrideWithFailureDomainFunc := func(vm *infrav1.VSphereVM) {
		vm.Spec.Server = vsphereDeploymentZone.Spec.Server
		vm.Spec.Datacenter = vsphereFailureDomain.Spec.Topology.Datacenter
		if vsphereDeploymentZone.Spec.PlacementConstraint.Folder != "" {
			vm.Spec.Folder = vsphereDeploymentZone.Spec.PlacementConstraint.Folder
		}
		if vsphereDeploymentZone.Spec.PlacementConstraint.ResourcePool != "" {
			vm.Spec.ResourcePool = vsphereDeploymentZone.Spec.PlacementConstraint.ResourcePool
		}
		if vsphereFailureDomain.Spec.Topology.Datastore != "" {
			vm.Spec.Datastore = vsphereFailureDomain.Spec.Topology.Datastore
		}
		if len(vsphereFailureDomain.Spec.Topology.Networks) > 0 {
			vm.Spec.Network.Devices = overrideNetworkDeviceSpecs(vm.Spec.Network.Devices, vsphereFailureDomain.Spec.Topology.Networks, mergeFailureDomainNetworkName)
		}
		if len(vsphereFailureDomain.Spec.Topology.NetworkConfigurations) > 0 {
			vm.Spec.Network.Devices = overrideNetworkDeviceSpecs(vm.Spec.Network.Devices, vsphereFailureDomain.Spec.Topology.NetworkConfigurations, mergeNetworkConfigurationInNetworkDeviceSpec)
		}
	}
	return overrideWithFailureDomainFunc, true
}

// overrideNetworkDeviceSpecs updates the network devices with the network definitions from the PlacementConstraint.
// The substitution is done based on the order in which the network devices have been defined.
//
// In case there are more network definitions than the number of network devices specified, the definitions are appended to the list.
func overrideNetworkDeviceSpecs[T any](deviceSpecs []infrav1.NetworkDeviceSpec, networks []T, mergeFunc func(device *infrav1.NetworkDeviceSpec, network T)) []infrav1.NetworkDeviceSpec {
	index, length := 0, len(networks)

	devices := make([]infrav1.NetworkDeviceSpec, 0, max(length, len(deviceSpecs)))
	// override the networks on the VM spec with placement constraint network definitions
	for i := range deviceSpecs {
		vmNetworkDeviceSpec := deviceSpecs[i]
		if i < length {
			index++
			mergeFunc(&vmNetworkDeviceSpec, networks[i])
		}
		devices = append(devices, vmNetworkDeviceSpec)
	}
	// append the remaining network definitions to the VM spec
	for ; index < length; index++ {
		device := infrav1.NetworkDeviceSpec{}
		mergeFunc(&device, networks[index])
		devices = append(devices, device)
	}

	return devices
}

func mergeFailureDomainNetworkName(device *infrav1.NetworkDeviceSpec, network string) {
	device.NetworkName = network
}

func mergeNetworkConfigurationInNetworkDeviceSpec(device *infrav1.NetworkDeviceSpec, nc infrav1.NetworkConfiguration) {
	if nc.NetworkName != "" {
		device.NetworkName = nc.NetworkName
	}
	if nc.DHCP4 != nil {
		device.DHCP4 = *nc.DHCP4
	}
	if nc.DHCP6 != nil {
		device.DHCP6 = *nc.DHCP6
	}
	if len(nc.Nameservers) > 0 {
		device.Nameservers = make([]string, len(nc.Nameservers))
		copy(device.Nameservers, nc.Nameservers)
	}

	if len(nc.SearchDomains) > 0 {
		device.SearchDomains = make([]string, len(nc.SearchDomains))
		copy(device.SearchDomains, nc.SearchDomains)
	}

	if nc.DHCP4Overrides != nil {
		device.DHCP4Overrides = nc.DHCP4Overrides.DeepCopy()
	}

	if nc.DHCP6Overrides != nil {
		device.DHCP6Overrides = nc.DHCP6Overrides.DeepCopy()
	}

	if len(nc.AddressesFromPools) > 0 {
		device.AddressesFromPools = make([]corev1.TypedLocalObjectReference, len(nc.AddressesFromPools))
		copy(device.AddressesFromPools, nc.AddressesFromPools)
	}
}

func (v *VimMachineService) reconcileMachineConfigPool(ctx context.Context, vimMachineCtx *capvcontext.VIMMachineContext) error {
	if vimMachineCtx.VSphereMachine.Spec.MachineConfigPoolRef == nil {
		return nil
	}
	log := ctrl.LoggerFrom(ctx)
	log.Info("Reconciling MachineConfigPool", "pool", klog.KRef(vimMachineCtx.VSphereMachine.Spec.MachineConfigPoolRef.Namespace, vimMachineCtx.VSphereMachine.Spec.MachineConfigPoolRef.Name))

	desiredDatacenter, err := v.resolveDesiredDatacenter(ctx, vimMachineCtx)
	if err != nil {
		return errors.Wrap(err, "failed to resolve desired datacenter for machine config pool allocation")
	}

	allowedFailureDomainDatacenters, err := v.resolveFailureDomainDatacenters(ctx, vimMachineCtx)
	if err != nil {
		return errors.Wrap(err, "failed to resolve failure domain datacenters for machine config pool allocation")
	}

	consumerRef, err := ResolveMachineConsumerRef(ctx, v.Client, vimMachineCtx.Machine)
	if err != nil {
		return errors.Wrap(err, "failed to resolve machine consumer for machine config pool allocation")
	}
	slot, err := AllocateSlot(ctx, v.Client, vimMachineCtx.VSphereMachine.Spec.MachineConfigPoolRef, vimMachineCtx.VSphereMachine, consumerRef, desiredDatacenter, allowedFailureDomainDatacenters)
	if err != nil {
		reason := infrav1.MachineConfigPoolNoAvailableSlotsReason
		if strings.Contains(err.Error(), "is bound to") {
			reason = infrav1.MachineConfigPoolBoundToOtherConsumerReason
		}
		conditions.MarkFalse(vimMachineCtx.VSphereMachine, infrav1.MachineConfigPoolReadyCondition,
			reason, clusterv1.ConditionSeverityWarning, "%s", err.Error())
		return errors.Wrap(err, "failed to allocate slot from pool")
	}
	conditions.MarkTrue(vimMachineCtx.VSphereMachine, infrav1.MachineConfigPoolReadyCondition)
	vimMachineCtx.MachineConfigSlot = slot
	return nil
}

func (v *VimMachineService) resolveDesiredDatacenter(ctx context.Context, vimMachineCtx *capvcontext.VIMMachineContext) (string, error) {
	if vimMachineCtx == nil || vimMachineCtx.VSphereMachine == nil {
		return "", nil
	}

	desiredDatacenter := normalizeDesiredDatacenter(vimMachineCtx.VSphereMachine.Spec.Datacenter)
	if desiredDatacenter != "" {
		if _, err := v.resolveFailureDomainDatacenters(ctx, vimMachineCtx); err != nil {
			return "", err
		}
		return desiredDatacenter, nil
	}

	return "", nil
}

func (v *VimMachineService) resolveFailureDomainDatacenters(ctx context.Context, vimMachineCtx *capvcontext.VIMMachineContext) ([]string, error) {
	if vimMachineCtx == nil || vimMachineCtx.Machine == nil || vimMachineCtx.Machine.Spec.FailureDomain == nil {
		return nil, nil
	}

	var vsphereDeploymentZone infrav1.VSphereDeploymentZone
	if err := v.Client.Get(ctx, client.ObjectKey{Name: *vimMachineCtx.Machine.Spec.FailureDomain}, &vsphereDeploymentZone); err != nil {
		return nil, err
	}

	var vsphereFailureDomain infrav1.VSphereFailureDomain
	if err := v.Client.Get(ctx, client.ObjectKey{Name: vsphereDeploymentZone.Spec.FailureDomain}, &vsphereFailureDomain); err != nil {
		return nil, err
	}

	if vsphereFailureDomain.Spec.Topology.Datacenter != "" {
		return []string{vsphereFailureDomain.Spec.Topology.Datacenter}, nil
	}
	return nil, nil
}

func (v *VimMachineService) persistMachineConfigPoolChanges(ctx context.Context, vimMachineCtx *capvcontext.VIMMachineContext) error {
	if vimMachineCtx.VSphereMachine.Spec.MachineConfigPoolRef == nil || vimMachineCtx.MachineConfigSlot == nil {
		return nil
	}

	poolKey := client.ObjectKey{Namespace: vimMachineCtx.VSphereMachine.Spec.MachineConfigPoolRef.Namespace, Name: vimMachineCtx.VSphereMachine.Spec.MachineConfigPoolRef.Name}
	for range 3 {
		pool := &infrav1.VSphereMachineConfigPool{}
		if err := v.Client.Get(ctx, poolKey, pool); err != nil {
			return err
		}

		// Record the observed disk state (VolumePath/DiskUUID/UnitNumber) into
		// pool status; spec is no longer written back by the controller.
		if !ApplyDiskBackfill(pool, vimMachineCtx.MachineConfigSlot, vimMachineCtx.VSphereMachine.Name, string(vimMachineCtx.VSphereMachine.UID)) {
			return nil
		}
		if err := v.Client.Status().Update(ctx, pool); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return err
		}
		return nil
	}
	return errors.New("transient conflict while persisting machine config pool changes, exhausted retries")
}

func (v *VimMachineService) mergeSlotNetwork(vm *infrav1.VSphereVM, slotNetwork *infrav1.MachineConfigSlotNetwork) {
	if slotNetwork == nil {
		return
	}
	slotNetworks := make([]infrav1.NetworkConfig, 0, 1+len(slotNetwork.Additional))
	slotNetworks = append(slotNetworks, slotNetwork.Primary)
	slotNetworks = append(slotNetworks, slotNetwork.Additional...)
	// Use slot networks to override or append to vm devices
	newDevices := make([]infrav1.NetworkDeviceSpec, 0, max(len(vm.Spec.Network.Devices), len(slotNetworks)))
	for i := 0; i < len(slotNetworks) || i < len(vm.Spec.Network.Devices); i++ {
		var dev infrav1.NetworkDeviceSpec
		if i < len(vm.Spec.Network.Devices) {
			dev = vm.Spec.Network.Devices[i]
		}
		if i < len(slotNetworks) {
			sn := slotNetworks[i]
			slotProvidesStaticIP := sn.IP != "" || sn.IPv6 != ""
			dev.NetworkName = sn.NetworkName
			if sn.DeviceName != "" {
				dev.DeviceName = sn.DeviceName
			}
			if sn.IP != "" {
				dev.IPAddrs = []string{sn.IP}
				dev.DHCP4 = false
			}
			if sn.Gateway != "" {
				dev.Gateway4 = sn.Gateway
			}
			if sn.IPv6 != "" {
				dev.IPAddrs = append(dev.IPAddrs, sn.IPv6)
				dev.DHCP6 = false
			}
			if sn.IPv6Gateway != "" {
				dev.Gateway6 = sn.IPv6Gateway
			}
			if len(sn.DNS) > 0 {
				dev.Nameservers = sn.DNS
			}
			if slotProvidesStaticIP {
				// Slot-provided addresses take precedence over CAPV IPAM/DHCP inputs.
				dev.AddressesFromPools = nil
			}
		}
		newDevices = append(newDevices, dev)
	}
	vm.Spec.Network.Devices = newDevices
}

func (v *VimMachineService) mergeSlotPersistentDisks(vm *infrav1.VSphereVM, persistentDisks []infrav1.PersistentDisk) {
	for _, pd := range persistentDisks {
		upsertDataDisk(vm, pd.Name, pd.SizeGiB)
	}
}

// mergeSlotEphemeralDisks folds the slot's non-persistent disks into the
// VSphereVM's DataDisks by name, mirroring mergeSlotPersistentDisks. Only Name
// and SizeGiB are carried here; datastore/storage-policy placement and the
// controller-assigned SCSI unit are applied later in clone.go by matching the
// data disk back to the slot's EphemeralDisks. Names are unique across the
// slot's persistent and ephemeral disks, so a given DataDisk matches at most
// one of the two lists.
func (v *VimMachineService) mergeSlotEphemeralDisks(vm *infrav1.VSphereVM, ephemeralDisks []infrav1.EphemeralDisk) {
	for _, ed := range ephemeralDisks {
		upsertDataDisk(vm, ed.Name, ed.SizeGiB)
	}
}

// upsertDataDisk adds a data disk with the given name and size to the VM's
// DataDisks, or updates the size of the existing entry with that name. Disk names
// are unique across a slot's persistent and ephemeral lists, so the name is a
// safe key.
func upsertDataDisk(vm *infrav1.VSphereVM, name string, sizeGiB int32) {
	for i := range vm.Spec.DataDisks {
		if vm.Spec.DataDisks[i].Name == name {
			vm.Spec.DataDisks[i].SizeGiB = sizeGiB
			return
		}
	}
	vm.Spec.DataDisks = append(vm.Spec.DataDisks, infrav1.VSphereDisk{
		Name:    name,
		SizeGiB: sizeGiB,
	})
}
