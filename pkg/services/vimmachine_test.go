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

package services

import (
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	v1beta2conditions "sigs.k8s.io/cluster-api/util/conditions/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/context/fake"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

var ctx = ctrl.SetupSignalHandler()

func Test_VimMachineService_GenerateOverrideFunc(t *testing.T) {
	deplZone := func(suffix string) *infrav1.VSphereDeploymentZone {
		return &infrav1.VSphereDeploymentZone{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("zone-%s", suffix)},
			Spec: infrav1.VSphereDeploymentZoneSpec{
				Server:        fmt.Sprintf("server-%s", suffix),
				FailureDomain: fmt.Sprintf("fd-%s", suffix),
				ControlPlane:  ptr.To(true),
				PlacementConstraint: infrav1.PlacementConstraint{
					ResourcePool: fmt.Sprintf("rp-%s", suffix),
					Folder:       fmt.Sprintf("folder-%s", suffix),
				},
			},
		}
	}

	failureDomain := func(suffix string) *infrav1.VSphereFailureDomain {
		return &infrav1.VSphereFailureDomain{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("fd-%s", suffix)},
			Spec: infrav1.VSphereFailureDomainSpec{
				Topology: infrav1.Topology{
					Datacenter: fmt.Sprintf("dc-%s", suffix),
					Datastore:  fmt.Sprintf("ds-%s", suffix),
					Networks:   []string{fmt.Sprintf("nw-%s", suffix), "another-nw"},
				},
			},
		}
	}

	failureDomainWithNetworkConfigs := func(suffix string) *infrav1.VSphereFailureDomain {
		return &infrav1.VSphereFailureDomain{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("fd-%s", suffix)},
			Spec: infrav1.VSphereFailureDomainSpec{
				Topology: infrav1.Topology{
					Datacenter: fmt.Sprintf("dc-%s", suffix),
					Datastore:  fmt.Sprintf("ds-%s", suffix),
					NetworkConfigurations: []infrav1.NetworkConfiguration{
						{
							NetworkName: fmt.Sprintf("nw-%s", suffix),
							DHCP4:       ptr.To(true),
						},
						{
							NetworkName: "another-nw",
							DHCP6:       ptr.To(true),
						},
					},
				},
			},
		}
	}

	t.Run("does not generate an override function when Failure Domain is not present", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), deplZone("two"), failureDomain("one"), failureDomain("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		_, ok := vimMachineService.generateOverrideFunc(ctx, machineCtx)
		g.Expect(ok).To(BeFalse())
	})

	t.Run("generates an override function when Failure Domain is present", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), deplZone("two"), failureDomain("one"), failureDomain("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.Spec.FailureDomain = ptr.To("zone-one")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		_, ok := vimMachineService.generateOverrideFunc(ctx, machineCtx)
		g.Expect(ok).To(BeTrue())
	})

	t.Run("uses the deployment zone placement constraint & failure domains topology for VM values", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), deplZone("two"), failureDomain("one"), failureDomain("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.Spec.FailureDomain = ptr.To("zone-one")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		overrideFunc, ok := vimMachineService.generateOverrideFunc(ctx, machineCtx)
		g.Expect(ok).To(BeTrue())

		vm := &infrav1.VSphereVM{Spec: infrav1.VSphereVMSpec{}}
		overrideFunc(vm)

		g.Expect(vm.Spec.Server).To(Equal("server-one"))
		g.Expect(vm.Spec.Folder).To(Equal("folder-one"))
		g.Expect(vm.Spec.Datastore).To(Equal("ds-one"))
		g.Expect(vm.Spec.ResourcePool).To(Equal("rp-one"))
		g.Expect(vm.Spec.Datacenter).To(Equal("dc-one"))
	})

	t.Run("fails to generate an override function for non-existent failure domain value", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), deplZone("two"), failureDomain("one"), failureDomain("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.Spec.FailureDomain = ptr.To("non-existent-zone")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		overrideFunc, ok := vimMachineService.generateOverrideFunc(ctx, machineCtx)
		g.Expect(ok).To(BeFalse())
		g.Expect(overrideFunc).To(BeNil())
	})

	t.Run("overrides the n/w names from the networks list of the topology for equal number of networks", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), deplZone("two"), failureDomain("one"), failureDomain("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.Spec.FailureDomain = ptr.To("zone-one")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		vm := &infrav1.VSphereVM{
			Spec: infrav1.VSphereVMSpec{
				VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
					Network: infrav1.NetworkSpec{Devices: []infrav1.NetworkDeviceSpec{{NetworkName: "foo", DHCP4: false}, {NetworkName: "bar", DHCP6: false}}},
				},
			},
		}

		overrideFunc, ok := vimMachineService.generateOverrideFunc(ctx, machineCtx)
		g.Expect(ok).To(BeTrue())

		overrideFunc(vm)

		devices := vm.Spec.Network.Devices
		g.Expect(devices).To(HaveLen(2))
		g.Expect(devices[0].NetworkName).To(Equal("nw-one"))

		g.Expect(devices[1].NetworkName).To(Equal("another-nw"))
	})

	t.Run("overrides the n/w config from the network config list of the topology for equal number of networks", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), deplZone("two"), failureDomainWithNetworkConfigs("one"), failureDomainWithNetworkConfigs("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.Spec.FailureDomain = ptr.To("zone-one")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		vm := &infrav1.VSphereVM{
			Spec: infrav1.VSphereVMSpec{
				VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
					Network: infrav1.NetworkSpec{Devices: []infrav1.NetworkDeviceSpec{{NetworkName: "foo", DHCP4: false}, {NetworkName: "bar", DHCP6: false}}},
				},
			},
		}

		overrideFunc, ok := vimMachineService.generateOverrideFunc(ctx, machineCtx)
		g.Expect(ok).To(BeTrue())

		overrideFunc(vm)

		devices := vm.Spec.Network.Devices
		g.Expect(devices).To(HaveLen(2))
		g.Expect(devices[0].NetworkName).To(Equal("nw-one"))
		g.Expect(devices[0].DHCP4).To(BeTrue())

		g.Expect(devices[1].NetworkName).To(Equal("another-nw"))
		g.Expect(devices[1].DHCP6).To(BeTrue())
	})

	t.Run("appends the n/w names present in the networks list of the topology with number of devices in VMSpec < number of networks in the placement constraint", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), deplZone("two"), failureDomain("one"), failureDomain("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.Spec.FailureDomain = ptr.To("zone-one")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		vm := &infrav1.VSphereVM{
			Spec: infrav1.VSphereVMSpec{
				VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
					Network: infrav1.NetworkSpec{Devices: []infrav1.NetworkDeviceSpec{{NetworkName: "foo", DHCP4: false}}},
				},
			},
		}

		overrideFunc, ok := vimMachineService.generateOverrideFunc(ctx, machineCtx)
		g.Expect(ok).To(BeTrue())

		overrideFunc(vm)

		devices := vm.Spec.Network.Devices
		g.Expect(devices).To(HaveLen(2))
		g.Expect(devices[0].NetworkName).To(Equal("nw-one"))

		g.Expect(devices[1].NetworkName).To(Equal("another-nw"))
	})

	t.Run("appends the n/w configs present in the network config list of the topology with number of devices in VMSpec < number of networks in the placement constraint", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), deplZone("two"), failureDomainWithNetworkConfigs("one"), failureDomainWithNetworkConfigs("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.Spec.FailureDomain = ptr.To("zone-one")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		vm := &infrav1.VSphereVM{
			Spec: infrav1.VSphereVMSpec{
				VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
					Network: infrav1.NetworkSpec{Devices: []infrav1.NetworkDeviceSpec{{NetworkName: "foo", DHCP4: false}}},
				},
			},
		}

		overrideFunc, ok := vimMachineService.generateOverrideFunc(ctx, machineCtx)
		g.Expect(ok).To(BeTrue())

		overrideFunc(vm)

		devices := vm.Spec.Network.Devices
		g.Expect(devices).To(HaveLen(2))
		g.Expect(devices[0].NetworkName).To(Equal("nw-one"))
		g.Expect(devices[0].DHCP4).To(BeTrue())

		g.Expect(devices[1].NetworkName).To(Equal("another-nw"))
		g.Expect(devices[1].DHCP6).To(BeTrue())
	})

	t.Run("only overrides the n/w names present in the networks list of the topology with number of devices in VMSpec > number of networks in the placement constraint", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), deplZone("two"), failureDomain("one"), failureDomain("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.Spec.FailureDomain = ptr.To("zone-one")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		vm := &infrav1.VSphereVM{
			Spec: infrav1.VSphereVMSpec{
				VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
					Network: infrav1.NetworkSpec{Devices: []infrav1.NetworkDeviceSpec{{NetworkName: "foo", DHCP4: false}, {NetworkName: "bar", DHCP6: false}, {NetworkName: "baz", DHCP6: false}}},
				},
			},
		}

		overrideFunc, ok := vimMachineService.generateOverrideFunc(ctx, machineCtx)
		g.Expect(ok).To(BeTrue())

		overrideFunc(vm)

		devices := vm.Spec.Network.Devices
		g.Expect(devices).To(HaveLen(3))
		g.Expect(devices[0].NetworkName).To(Equal("nw-one"))

		g.Expect(devices[1].NetworkName).To(Equal("another-nw"))

		g.Expect(devices[2].NetworkName).To(Equal("baz"))
	})

	t.Run("only overrides the n/w configs present in the network config list of the topology with number of devices in VMSpec > number of networks in the placement constraint", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), deplZone("two"), failureDomainWithNetworkConfigs("one"), failureDomainWithNetworkConfigs("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.Spec.FailureDomain = ptr.To("zone-one")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		vm := &infrav1.VSphereVM{
			Spec: infrav1.VSphereVMSpec{
				VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
					Network: infrav1.NetworkSpec{Devices: []infrav1.NetworkDeviceSpec{{NetworkName: "foo", DHCP4: false}, {NetworkName: "bar", DHCP6: false}, {NetworkName: "baz", DHCP6: false}}},
				},
			},
		}

		overrideFunc, ok := vimMachineService.generateOverrideFunc(ctx, machineCtx)
		g.Expect(ok).To(BeTrue())

		overrideFunc(vm)

		devices := vm.Spec.Network.Devices
		g.Expect(devices).To(HaveLen(3))
		g.Expect(devices[0].NetworkName).To(Equal("nw-one"))
		g.Expect(devices[0].DHCP4).To(BeTrue())

		g.Expect(devices[1].NetworkName).To(Equal("another-nw"))
		g.Expect(devices[1].DHCP6).To(BeTrue())

		g.Expect(devices[2].NetworkName).To(Equal("baz"))
		g.Expect(devices[2].DHCP6).To(BeFalse())
	})
}

func Test_mergeNetworkConfigurationToNetworkDeviceSpec(t *testing.T) {
	t.Run("all fields from NetworkConfiguration are overridden", func(t *testing.T) {
		g := NewWithT(t)

		device := infrav1.NetworkDeviceSpec{}

		mergeNetworkConfigurationInNetworkDeviceSpec(&device, infrav1.NetworkConfiguration{
			NetworkName:   "nw-name",
			DHCP4:         ptr.To(true),
			DHCP6:         ptr.To(false),
			Nameservers:   []string{"1.1.1.1"},
			SearchDomains: []string{"vmware.ci"},
			DHCP4Overrides: &infrav1.DHCPOverrides{
				Hostname:    ptr.To("hal"),
				RouteMetric: ptr.To(12345),
			},
			DHCP6Overrides: &infrav1.DHCPOverrides{
				Hostname:    ptr.To("hal"),
				RouteMetric: ptr.To(23456),
			},
			AddressesFromPools: []corev1.TypedLocalObjectReference{
				{
					APIGroup: ptr.To("api-group"),
					Name:     "my-pool-1",
					Kind:     "my-pool-kind",
				},
			},
		})

		g.Expect(device).To(Equal(infrav1.NetworkDeviceSpec{
			NetworkName:   "nw-name",
			DHCP4:         true,
			DHCP6:         false,
			Nameservers:   []string{"1.1.1.1"},
			SearchDomains: []string{"vmware.ci"},
			DHCP4Overrides: &infrav1.DHCPOverrides{
				Hostname:    ptr.To("hal"),
				RouteMetric: ptr.To(12345),
			},
			DHCP6Overrides: &infrav1.DHCPOverrides{
				Hostname:    ptr.To("hal"),
				RouteMetric: ptr.To(23456),
			},
			AddressesFromPools: []corev1.TypedLocalObjectReference{
				{
					APIGroup: ptr.To("api-group"),
					Name:     "my-pool-1",
					Kind:     "my-pool-kind",
				},
			},
		}))
	})
}

func Test_mergeSlotNetwork(t *testing.T) {
	t.Run("slot static ip overrides CAPV IPAM inputs", func(t *testing.T) {
		g := NewWithT(t)
		service := &VimMachineService{}
		apiGroup := "ipam.cluster.x-k8s.io"

		vm := &infrav1.VSphereVM{
			Spec: infrav1.VSphereVMSpec{
				VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
					Network: infrav1.NetworkSpec{
						Devices: []infrav1.NetworkDeviceSpec{
							{
								NetworkName: "original-network",
								DHCP4:       true,
								AddressesFromPools: []corev1.TypedLocalObjectReference{
									{
										APIGroup: &apiGroup,
										Kind:     "IPPool",
										Name:     "pool-a",
									},
								},
							},
						},
					},
				},
			},
		}

		service.mergeSlotNetwork(vm, &infrav1.MachineConfigSlotNetwork{
			Primary: infrav1.NetworkConfig{
				NetworkName: "slot-network",
				DeviceName:  "ens192",
				IP:          "192.168.1.10/24",
				Gateway:     "192.168.1.1",
				DNS:         []string{"8.8.8.8"},
			},
		})

		g.Expect(vm.Spec.Network.Devices).To(HaveLen(1))
		g.Expect(vm.Spec.Network.Devices[0].NetworkName).To(Equal("slot-network"))
		g.Expect(vm.Spec.Network.Devices[0].DeviceName).To(Equal("ens192"))
		g.Expect(vm.Spec.Network.Devices[0].IPAddrs).To(Equal([]string{"192.168.1.10/24"}))
		g.Expect(vm.Spec.Network.Devices[0].Gateway4).To(Equal("192.168.1.1"))
		g.Expect(vm.Spec.Network.Devices[0].DHCP4).To(BeFalse())
		g.Expect(vm.Spec.Network.Devices[0].Nameservers).To(Equal([]string{"8.8.8.8"}))
		g.Expect(vm.Spec.Network.Devices[0].AddressesFromPools).To(BeNil())
	})

	t.Run("slot without ip keeps CAPV network allocation inputs", func(t *testing.T) {
		g := NewWithT(t)
		service := &VimMachineService{}
		apiGroup := "ipam.cluster.x-k8s.io"

		vm := &infrav1.VSphereVM{
			Spec: infrav1.VSphereVMSpec{
				VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
					Network: infrav1.NetworkSpec{
						Devices: []infrav1.NetworkDeviceSpec{
							{
								NetworkName: "original-network",
								DHCP4:       true,
								AddressesFromPools: []corev1.TypedLocalObjectReference{
									{
										APIGroup: &apiGroup,
										Kind:     "IPPool",
										Name:     "pool-a",
									},
								},
							},
						},
					},
				},
			},
		}

		service.mergeSlotNetwork(vm, &infrav1.MachineConfigSlotNetwork{
			Primary: infrav1.NetworkConfig{
				NetworkName: "slot-network",
				DeviceName:  "ens224",
			},
		})

		g.Expect(vm.Spec.Network.Devices).To(HaveLen(1))
		g.Expect(vm.Spec.Network.Devices[0].NetworkName).To(Equal("slot-network"))
		g.Expect(vm.Spec.Network.Devices[0].DeviceName).To(Equal("ens224"))
		g.Expect(vm.Spec.Network.Devices[0].DHCP4).To(BeTrue())
		g.Expect(vm.Spec.Network.Devices[0].IPAddrs).To(BeEmpty())
		g.Expect(vm.Spec.Network.Devices[0].AddressesFromPools).To(HaveLen(1))
		g.Expect(vm.Spec.Network.Devices[0].AddressesFromPools[0].Name).To(Equal("pool-a"))
	})
}

func Test_VimMachineService_GetHostInfo(t *testing.T) {
	var (
		hostAddr = "1.2.3.4"
	)

	getVSphereVM := func(hostAddr string, conditionStatus corev1.ConditionStatus) *infrav1.VSphereVM {
		return &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: fake.Namespace,
				Name:      fake.Clusterv1a2Name,
			},
			Status: infrav1.VSphereVMStatus{
				Host: hostAddr,
				Conditions: []clusterv1.Condition{
					{
						Type:   infrav1.VMProvisionedCondition,
						Status: conditionStatus,
					},
				},
			},
		}
	}

	t.Run("fetches host address from the VSphereVM object when VMProvisioned condition is set", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(getVSphereVM(hostAddr, corev1.ConditionTrue))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		vimMachineService := &VimMachineService{controllerManagerContext.Client}
		host, err := vimMachineService.GetHostInfo(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(host).To(Equal(hostAddr))
	})

	t.Run("returns empty string when VMProvisioned condition is unset", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(getVSphereVM(hostAddr, corev1.ConditionFalse))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		vimMachineService := &VimMachineService{controllerManagerContext.Client}
		host, err := vimMachineService.GetHostInfo(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(host).To(BeEmpty())
	})
}

func Test_VimMachineService_createOrPatchVSphereVM(t *testing.T) {
	var (
		hostAddr            = "1.2.3.4"
		fakeLongClusterName = "fake-long-clustername"
	)

	getVSphereVM := func(hostAddr string, conditionStatus corev1.ConditionStatus) *infrav1.VSphereVM {
		return &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: fake.Namespace,
				Name:      fakeLongClusterName,
			},
			Status: infrav1.VSphereVMStatus{
				Host: hostAddr,
				Conditions: []clusterv1.Condition{
					{
						Type:   infrav1.VMProvisionedCondition,
						Status: conditionStatus,
					},
				},
			},
		}
	}

	deplZone := func(suffix string) *infrav1.VSphereDeploymentZone {
		return &infrav1.VSphereDeploymentZone{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("zone-%s", suffix)},
			Spec: infrav1.VSphereDeploymentZoneSpec{
				Server:        fmt.Sprintf("server-%s", suffix),
				FailureDomain: fmt.Sprintf("fd-%s", suffix),
				ControlPlane:  ptr.To(true),
				PlacementConstraint: infrav1.PlacementConstraint{
					ResourcePool: fmt.Sprintf("rp-%s", suffix),
					Folder:       fmt.Sprintf("folder-%s", suffix),
				},
			},
		}
	}

	failureDomain := func(suffix string) *infrav1.VSphereFailureDomain {
		return &infrav1.VSphereFailureDomain{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("fd-%s", suffix)},
			Spec: infrav1.VSphereFailureDomainSpec{
				Topology: infrav1.Topology{
					Datacenter: fmt.Sprintf("dc-%s", suffix),
					Datastore:  fmt.Sprintf("ds-%s", suffix),
					Networks:   []string{fmt.Sprintf("nw-%s", suffix), "another-nw"},
				},
			},
		}
	}

	t.Run("returns a renamed VSphereVM object when VSphereMachine OS is Windows", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(getVSphereVM(hostAddr, corev1.ConditionTrue), deplZone("one"), deplZone("two"), failureDomain("one"), failureDomain("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.VSphereMachine.Spec.OS = infrav1.Windows
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		failureDomain := "zone-one"
		machineCtx.Machine.Spec.FailureDomain = &failureDomain
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		vm, err := vimMachineService.createOrPatchVSphereVM(ctx, machineCtx, getVSphereVM(hostAddr, corev1.ConditionTrue))
		vmName := vm.Name
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(vmName).To(Equal("fake-long-rname"))
	})

	t.Run("returns the same VSphereVM name when VSphereMachine OS is Linux", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(getVSphereVM(hostAddr, corev1.ConditionTrue), deplZone("one"), deplZone("two"), failureDomain("one"), failureDomain("two"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.VSphereMachine.Spec.OS = infrav1.Linux
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		vm, err := vimMachineService.createOrPatchVSphereVM(ctx, machineCtx, getVSphereVM(hostAddr, corev1.ConditionTrue))
		vmName := vm.Name
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(vmName).To(Equal(fakeLongClusterName))
	})

	t.Run("uses machine config slot datacenter over failure domain datacenter", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(getVSphereVM(hostAddr, corev1.ConditionTrue), deplZone("one"), failureDomain("one"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		failureDomain := "zone-one"
		machineCtx.Machine.Spec.FailureDomain = &failureDomain
		machineCtx.MachineConfigSlot = &infrav1.MachineConfigSlot{
			Hostname:   "worker-01",
			Datacenter: "dc-slot",
		}
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		vm, err := vimMachineService.createOrPatchVSphereVM(ctx, machineCtx, getVSphereVM(hostAddr, corev1.ConditionTrue))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(vm.Spec.Datacenter).To(Equal("dc-slot"))
	})

	t.Run("falls back to pool datacenter when slot datacenter is empty", func(t *testing.T) {
		g := NewWithT(t)
		pool := &infrav1.VSphereMachineConfigPool{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pool-1",
				Namespace: fake.Namespace,
			},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Datacenter: "dc-pool",
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "worker-01",
				}},
			},
		}
		controllerManagerContext := fake.NewControllerManagerContext(getVSphereVM(hostAddr, corev1.ConditionTrue), deplZone("one"), failureDomain("one"), pool)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		failureDomain := "zone-one"
		machineCtx.Machine.Spec.FailureDomain = &failureDomain
		machineCtx.VSphereMachine.Spec.MachineConfigPoolRef = &corev1.ObjectReference{
			Name:      pool.Name,
			Namespace: pool.Namespace,
		}
		machineCtx.MachineConfigSlot = &infrav1.MachineConfigSlot{
			Hostname: "worker-01",
		}
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		vm, err := vimMachineService.createOrPatchVSphereVM(ctx, machineCtx, getVSphereVM(hostAddr, corev1.ConditionTrue))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(vm.Spec.Datacenter).To(Equal("dc-pool"))
	})
}

func Test_VimMachineService_reconcileProviderID(t *testing.T) {
	var (
		hostAddr            = "1.2.3.4"
		fakeLongClusterName = "fake-long-clustername"
	)

	getVSphereVM := func(hostAddr string, conditionStatus corev1.ConditionStatus) *infrav1.VSphereVM {
		return &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: fake.Namespace,
				Name:      fakeLongClusterName,
			},
			Status: infrav1.VSphereVMStatus{
				Host: hostAddr,
				Conditions: []clusterv1.Condition{
					{
						Type:   infrav1.VMProvisionedCondition,
						Status: conditionStatus,
					},
				},
			},
		}
	}

	vsphereVM := getVSphereVM(hostAddr, corev1.ConditionTrue)
	biosUUID := "42055285-ff20-2c28-965c-05558ea1b4c7"

	t.Run("returns false when VSphereVM biosUUID is not set", func(t *testing.T) {
		g := NewWithT(t)
		vsphereVM.Spec.BiosUUID = ""
		controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		ok, err := vimMachineService.reconcileProviderID(ctx, machineCtx, vsphereVM)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ok).To(BeFalse())
	})

	t.Run("returns true when VSphereVM biosUUID is valid", func(t *testing.T) {
		g := NewWithT(t)
		vsphereVM.Spec.BiosUUID = biosUUID
		controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		ok, err := vimMachineService.reconcileProviderID(ctx, machineCtx, vsphereVM)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ok).To(BeTrue())
		g.Expect(*machineCtx.VSphereMachine.Spec.ProviderID).To(Equal(util.ProviderIDPrefix + biosUUID))
	})

	t.Run("returns error when VSphereVM biosUUID is not valid", func(t *testing.T) {
		g := NewWithT(t)
		vsphereVM.Spec.BiosUUID = "abcde"
		controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		_, err := vimMachineService.reconcileProviderID(ctx, machineCtx, vsphereVM)
		g.Expect(err).To(HaveOccurred())
	})
}

func TestVimMachineServiceResolveMachineConfigPoolDatacenterConstraints(t *testing.T) {
	deplZone := func(suffix string) *infrav1.VSphereDeploymentZone {
		return &infrav1.VSphereDeploymentZone{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("zone-%s", suffix)},
			Spec: infrav1.VSphereDeploymentZoneSpec{
				FailureDomain: fmt.Sprintf("fd-%s", suffix),
			},
		}
	}

	failureDomain := func(suffix string) *infrav1.VSphereFailureDomain {
		return &infrav1.VSphereFailureDomain{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("fd-%s", suffix)},
			Spec: infrav1.VSphereFailureDomainSpec{
				Topology: infrav1.Topology{
					Datacenter: fmt.Sprintf("dc-%s", suffix),
				},
			},
		}
	}

	t.Run("uses template datacenter as hard requirement and still validates failure domain references", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), failureDomain("one"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.VSphereMachine.Spec.Datacenter = "dc-template"
		machineCtx.Machine.Spec.FailureDomain = ptr.To("zone-one")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		desiredDatacenter, err := vimMachineService.resolveDesiredDatacenter(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(desiredDatacenter).To(Equal("dc-template"))

		allowedDatacenters, err := vimMachineService.resolveFailureDomainDatacenters(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(allowedDatacenters).To(Equal([]string{"dc-one"}))
	})

	t.Run("uses failure domain datacenter filter when template datacenter is empty", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext(deplZone("one"), failureDomain("one"))
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.VSphereMachine.Spec.Datacenter = ""
		machineCtx.Machine.Spec.FailureDomain = ptr.To("zone-one")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		desiredDatacenter, err := vimMachineService.resolveDesiredDatacenter(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(desiredDatacenter).To(BeEmpty())

		allowedDatacenters, err := vimMachineService.resolveFailureDomainDatacenters(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(allowedDatacenters).To(Equal([]string{"dc-one"}))
	})

	t.Run("returns an error when a configured failure domain cannot be resolved", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext()
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.VSphereMachine.Spec.Datacenter = "dc-template"
		machineCtx.Machine.Spec.FailureDomain = ptr.To("zone-missing")
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		_, err := vimMachineService.resolveDesiredDatacenter(ctx, machineCtx)
		g.Expect(err).To(HaveOccurred())
	})
}

func TestVimMachineServiceReconcileMachineConfigPoolBackfillsDatacenter(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-1",
			Namespace: fake.Namespace,
		},
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Datacenter: "dc-pool",
			Configs: []infrav1.MachineConfigSlot{
				{Hostname: "worker-01"},
			},
		},
	}
	md := &clusterv1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "md-a",
			Namespace: fake.Namespace,
			UID:       "md-uid",
		},
	}
	ms := &clusterv1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ms-a",
			Namespace: fake.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "MachineDeployment",
				Name:       "md-a",
				UID:        "md-uid",
			}},
		},
	}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, md, ms).WithStatusSubresource(pool).Build()
	machineCtx := &capvcontext.VIMMachineContext{
		BaseMachineContext: &capvcontext.BaseMachineContext{
			Machine: &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "machine-1",
					Namespace: fake.Namespace,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: clusterv1.GroupVersion.String(),
						Kind:       "MachineSet",
						Name:       "ms-a",
					}},
				},
			},
		},
		VSphereMachine: &infrav1.VSphereMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "machine-1",
				Namespace: fake.Namespace,
				UID:       "machine-uid",
			},
		},
	}
	machineCtx.VSphereMachine.Spec.MachineConfigPoolRef = &corev1.ObjectReference{
		Name:      pool.Name,
		Namespace: pool.Namespace,
	}
	machineCtx.VSphereMachine.Spec.Datacenter = ""

	vimMachineService := &VimMachineService{Client: client}
	err := vimMachineService.reconcileMachineConfigPool(ctx, machineCtx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(machineCtx.MachineConfigSlot).NotTo(BeNil())
	g.Expect(machineCtx.MachineConfigSlot.Datacenter).To(Equal("dc-pool"))
}

func TestVimMachineServiceReconcileMachineConfigPoolBackfillsWildcardDatacenter(t *testing.T) {
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	_ = infrav1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	pool := &infrav1.VSphereMachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-1",
			Namespace: fake.Namespace,
		},
		Spec: infrav1.VSphereMachineConfigPoolSpec{
			ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
			Datacenter: "dc-pool",
			Configs: []infrav1.MachineConfigSlot{
				{Hostname: "worker-01"},
			},
		},
	}
	md := &clusterv1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "md-a",
			Namespace: fake.Namespace,
			UID:       "md-uid",
		},
	}
	ms := &clusterv1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ms-a",
			Namespace: fake.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "MachineDeployment",
				Name:       "md-a",
				UID:        "md-uid",
			}},
		},
	}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, md, ms).WithStatusSubresource(pool).Build()
	machineCtx := &capvcontext.VIMMachineContext{
		BaseMachineContext: &capvcontext.BaseMachineContext{
			Machine: &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "machine-1",
					Namespace: fake.Namespace,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: clusterv1.GroupVersion.String(),
						Kind:       "MachineSet",
						Name:       "ms-a",
					}},
				},
			},
		},
		VSphereMachine: &infrav1.VSphereMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "machine-1",
				Namespace: fake.Namespace,
				UID:       "machine-uid",
			},
		},
	}
	machineCtx.VSphereMachine.Spec.MachineConfigPoolRef = &corev1.ObjectReference{
		Name:      pool.Name,
		Namespace: pool.Namespace,
	}
	machineCtx.VSphereMachine.Spec.Datacenter = "*"

	vimMachineService := &VimMachineService{Client: client}
	err := vimMachineService.reconcileMachineConfigPool(ctx, machineCtx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(machineCtx.MachineConfigSlot).NotTo(BeNil())
	g.Expect(machineCtx.MachineConfigSlot.Datacenter).To(Equal("dc-pool"))
}

func Test_VimMachineService_reconcileNetwork(t *testing.T) {
	var (
		hostAddr            = "1.2.3.4"
		fakeLongClusterName = "fake-long-clustername"
	)

	getVSphereVM := func(hostAddr string, conditionStatus corev1.ConditionStatus, addresses []string, networkStatus []infrav1.NetworkStatus) *infrav1.VSphereVM {
		return &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: fake.Namespace,
				Name:      fakeLongClusterName,
			},
			Status: infrav1.VSphereVMStatus{
				Host:      hostAddr,
				Ready:     conditionStatus == corev1.ConditionTrue,
				Addresses: addresses,
				Network:   networkStatus,
				Conditions: []clusterv1.Condition{
					{
						Type:   infrav1.VMProvisionedCondition,
						Status: conditionStatus,
					},
				},
			},
		}
	}

	networkStatus := []infrav1.NetworkStatus{
		{Connected: true, IPAddrs: []string{hostAddr}, MACAddr: "aa:bb:cc:dd:ee:ff", NetworkName: "fake"},
	}
	networkStatusWithoutMACAddr := []infrav1.NetworkStatus{
		{Connected: true, IPAddrs: []string{hostAddr}, MACAddr: "", NetworkName: "fake"},
	}
	addresses := []string{"1.2.3.4"}

	t.Run("returns false when VSphereVM addresses and networkStatus are both valid", func(t *testing.T) {
		g := NewWithT(t)
		vsphereVM := getVSphereVM(hostAddr, corev1.ConditionTrue, addresses, networkStatus)
		controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		ok, err := vimMachineService.reconcileNetwork(ctx, machineCtx, vsphereVM)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ok).To(BeTrue())
		g.Expect(machineCtx.VSphereMachine.Status.Addresses).To(ContainElement(clusterv1.MachineAddress{
			Type:    clusterv1.MachineInternalDNS,
			Address: vsphereVM.Name,
		}))
	})
	t.Run("returns true when VSphereVM address is set and network status has no MAC address", func(t *testing.T) {
		g := NewWithT(t)
		vsphereVM := getVSphereVM(hostAddr, corev1.ConditionTrue, addresses, networkStatusWithoutMACAddr)
		controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		ok, err := vimMachineService.reconcileNetwork(ctx, machineCtx, vsphereVM)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ok).To(BeTrue())
	})
	t.Run("returns false when VSphereVM has no IP addresses", func(t *testing.T) {
		g := NewWithT(t)
		vsphereVM := getVSphereVM(hostAddr, corev1.ConditionTrue, nil, networkStatus)
		controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		ok, err := vimMachineService.reconcileNetwork(ctx, machineCtx, vsphereVM)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ok).To(BeFalse())
		g.Expect(machineCtx.VSphereMachine.Status.Addresses).To(ContainElement(clusterv1.MachineAddress{
			Type:    clusterv1.MachineInternalDNS,
			Address: vsphereVM.Name,
		}))
		g.Expect(hasMachineIPAddress(machineCtx.VSphereMachine.Status.Addresses)).To(BeFalse())
	})
}

func Test_VimMachineService_ReconcileNormal(t *testing.T) {
	var (
		hostAddr            = "1.2.3.4"
		fakeLongClusterName = "fake-long-clustername"
	)

	getVSphereVM := func(hostAddr string, conditionStatus corev1.ConditionStatus, addresses []string, networkStatus []infrav1.NetworkStatus) *infrav1.VSphereVM {
		return &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: fake.Namespace,
				Name:      fakeLongClusterName,
			},
			Status: infrav1.VSphereVMStatus{
				Host:      hostAddr,
				Ready:     conditionStatus == corev1.ConditionTrue,
				Addresses: addresses,
				Network:   networkStatus,
				Conditions: []clusterv1.Condition{
					{
						Type:   infrav1.VMProvisionedCondition,
						Status: conditionStatus,
					},
					{
						Type:   clusterv1.ReadyCondition,
						Status: conditionStatus,
					},
				},
			},
		}
	}

	networkStatus := []infrav1.NetworkStatus{
		{Connected: true, IPAddrs: []string{hostAddr}, MACAddr: "aa:bb:cc:dd:ee:ff", NetworkName: "fake"},
	}
	addresses := []string{"1.2.3.4"}
	biosUUID := "42055285-ff20-2c28-965c-05558ea1b4c7"
	t.Run("completes the reconciliation with an existing resource", func(t *testing.T) {
		g := NewWithT(t)
		vsphereVM := getVSphereVM(hostAddr, corev1.ConditionTrue, addresses, networkStatus)
		vsphereVM.Spec.BiosUUID = biosUUID
		controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		requeue, err := vimMachineService.ReconcileNormal(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(requeue).To(BeFalse())
		g.Expect(machineCtx.VSphereMachine.Status.Ready).To(BeTrue())
	})
	t.Run("requeues when VSphereVM is ready but has no IP addresses", func(t *testing.T) {
		g := NewWithT(t)
		vsphereVM := getVSphereVM(hostAddr, corev1.ConditionTrue, nil, networkStatus)
		vsphereVM.Spec.BiosUUID = biosUUID
		controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		requeue, err := vimMachineService.ReconcileNormal(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(requeue).To(BeTrue())
		g.Expect(machineCtx.VSphereMachine.Status.Ready).To(BeFalse())
		g.Expect(conditions.Get(machineCtx.VSphereMachine, infrav1.VMProvisionedCondition).Reason).To(Equal(infrav1.WaitingForNetworkAddressesReason))
		g.Expect(hasMachineIPAddress(machineCtx.VSphereMachine.Status.Addresses)).To(BeFalse())
	})
	t.Run("creates the VSphereVM when no resource found", func(t *testing.T) {
		g := NewWithT(t)
		controllerManagerContext := fake.NewControllerManagerContext()
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		requeue, err := vimMachineService.ReconcileNormal(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(requeue).To(BeTrue())
		g.Expect(machineCtx.VSphereMachine.Status.Ready).To(BeFalse())
	})
	t.Run("returns error when the BIOS UUID is invalid", func(t *testing.T) {
		g := NewWithT(t)
		vsphereVM := getVSphereVM(hostAddr, corev1.ConditionTrue, addresses, networkStatus)
		vsphereVM.Spec.BiosUUID = "abcde"
		controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		_, err := vimMachineService.ReconcileNormal(ctx, machineCtx)
		g.Expect(err).To(HaveOccurred())
	})
	t.Run("requeues when the BIOS UUID is not set", func(t *testing.T) {
		g := NewWithT(t)
		vsphereVM := getVSphereVM(hostAddr, corev1.ConditionTrue, addresses, networkStatus)
		vsphereVM.Spec.BiosUUID = ""
		controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		requeue, err := vimMachineService.ReconcileNormal(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(requeue).To(BeTrue())
	})
	t.Run("requeues when VSphereVM is not ready", func(t *testing.T) {
		g := NewWithT(t)
		vsphereVM := getVSphereVM(hostAddr, corev1.ConditionFalse, nil, nil)
		controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
		machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
		machineCtx.Machine.SetName(fakeLongClusterName)
		machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
		vimMachineService := &VimMachineService{controllerManagerContext.Client}

		requeue, err := vimMachineService.ReconcileNormal(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(requeue).To(BeTrue())
	})
}

func Test_VimMachineService_ReconcileDelete(t *testing.T) {
	var (
		hostAddr            = "1.2.3.4"
		fakeLongClusterName = "fake-long-clustername"
	)

	getVSphereVM := func(hostAddr string, conditionStatus corev1.ConditionStatus) *infrav1.VSphereVM {
		return &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fakeLongClusterName,
				Namespace: fake.Namespace,
			},
			Status: infrav1.VSphereVMStatus{
				Host: hostAddr,
				Conditions: []clusterv1.Condition{
					{
						Type:   infrav1.VMProvisionedCondition,
						Status: conditionStatus,
					},
					{
						Type:   clusterv1.ReadyCondition,
						Status: conditionStatus,
					},
				},
			},
		}
	}

	vsphereVM := getVSphereVM(hostAddr, corev1.ConditionTrue)
	controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
	machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
	machineCtx.Machine.SetName(fakeLongClusterName)
	machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
	vimMachineService := &VimMachineService{controllerManagerContext.Client}

	t.Run("deletes VSphereVM", func(t *testing.T) {
		g := NewWithT(t)
		err := vimMachineService.ReconcileDelete(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(conditions.Get(machineCtx.VSphereMachine, infrav1.VMProvisionedCondition).Status).To(Equal(conditions.Get(vsphereVM, clusterv1.ReadyCondition).Status))
	})
}

func Test_VimMachineService_FetchVSphereMachine(t *testing.T) {
	var (
		fakeLongClusterName = "fake-long-clustername"
	)

	vsphereMachine := &infrav1.VSphereMachine{
		TypeMeta: metav1.TypeMeta{
			Kind:       "VSphereMachine",
			APIVersion: infrav1.GroupVersion.Identifier(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fake.Namespace,
			Name:      fakeLongClusterName,
		},
		Spec: infrav1.VSphereMachineSpec{
			VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
				Datacenter: "dc0",
				Network: infrav1.NetworkSpec{
					Devices: []infrav1.NetworkDeviceSpec{
						{
							NetworkName: "VM Network",
							DHCP4:       true,
							DHCP6:       true,
						},
					},
				},
				NumCPUs:   2,
				MemoryMiB: 2048,
				DiskGiB:   20,
			},
		},
	}

	controllerManagerContext := fake.NewControllerManagerContext(vsphereMachine)
	machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
	machineCtx.Machine.SetName(fakeLongClusterName)
	machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
	vimMachineService := &VimMachineService{controllerManagerContext.Client}

	t.Run("fetches VSphereMachine successfully", func(t *testing.T) {
		g := NewWithT(t)
		_, err := vimMachineService.FetchVSphereMachine(ctx, ctrlclient.ObjectKeyFromObject(vsphereMachine))
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func Test_VimMachineService_FetchVSphereCluster(t *testing.T) {
	var (
		fakeLongClusterName = "fake-long-clustername"
	)

	vsphereCluster := &infrav1.VSphereCluster{
		TypeMeta: metav1.TypeMeta{
			Kind:       "VSphereCluster",
			APIVersion: infrav1.GroupVersion.Identifier(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fake.Namespace,
			Name:      fake.InfrastructureRefName,
		},
		Spec: infrav1.VSphereClusterSpec{
			Server:     "test-server",
			Thumbprint: "test-thumbprint",
			ControlPlaneEndpoint: infrav1.APIEndpoint{
				Host: "1.2.3.4",
				Port: 443,
			},
		},
	}

	controllerManagerContext := fake.NewControllerManagerContext(vsphereCluster)
	machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
	machineCtx.Machine.SetName(fakeLongClusterName)
	machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
	vimMachineService := &VimMachineService{controllerManagerContext.Client}

	t.Run("fetches VSphereCluster successfully", func(t *testing.T) {
		g := NewWithT(t)
		_, err := vimMachineService.FetchVSphereCluster(ctx, machineCtx.Cluster, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func Test_VimMachineService_SyncFailureReason(t *testing.T) {
	var (
		hostAddr            = "1.2.3.4"
		fakeLongClusterName = "fake-long-clustername"
	)

	getVSphereVM := func(hostAddr string, conditionStatus corev1.ConditionStatus) *infrav1.VSphereVM {
		return &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fakeLongClusterName,
				Namespace: fake.Namespace,
			},
			Status: infrav1.VSphereVMStatus{
				Host: hostAddr,
				Conditions: []clusterv1.Condition{
					{
						Type:   infrav1.VMProvisionedCondition,
						Status: conditionStatus,
					},
				},
				Ready: conditionStatus == corev1.ConditionTrue,
			},
		}
	}

	vsphereVM := getVSphereVM(hostAddr, corev1.ConditionTrue)
	controllerManagerContext := fake.NewControllerManagerContext(vsphereVM)
	machineCtx := fake.NewMachineContext(ctx, fake.NewClusterContext(ctx, controllerManagerContext), controllerManagerContext)
	machineCtx.Machine.SetName(fakeLongClusterName)
	machineCtx.Machine.SetLabels(map[string]string{clusterv1.MachineControlPlaneLabel: "fake-control-plane"})
	vimMachineService := &VimMachineService{controllerManagerContext.Client}

	t.Run("syncs failure reason successfully", func(t *testing.T) {
		g := NewWithT(t)
		_, err := vimMachineService.SyncFailureReason(ctx, machineCtx)
		g.Expect(err).NotTo(HaveOccurred())
	})
}

func Test_GenerateVSphereVMName(t *testing.T) {
	maxNameLength := 63

	tests := []struct {
		name        string
		machineName string
		template    *string
		want        []gomegatypes.GomegaMatcher
		wantErr     bool
	}{
		{
			name:        "default template",
			machineName: "quick-start-d34gt4-md-0-wqc85-8nxwc-gfd5v",
			template:    nil,
			want: []gomegatypes.GomegaMatcher{
				Equal("quick-start-d34gt4-md-0-wqc85-8nxwc-gfd5v"),
			},
		},
		{
			name:        "template which doesn't respect max length: trim to max length",
			machineName: "quick-start-d34gt4-md-0-wqc85-8nxwc-gfd5v", // 41 characters
			template:    ptr.To("{{ .machine.name }}-{{ .machine.name }}"),
			want: []gomegatypes.GomegaMatcher{
				Equal("quick-start-d34gt4-md-0-wqc85-8nxwc-gfd5v-quick-start-d34gt4-md"), // 63 characters
			},
		},
		{
			name:        "template for 20 characters: keep machine name if name has 20 characters",
			machineName: "quick-md-8nxwc-gfd5v", // 20 characters
			template:    ptr.To("{{ if le (len .machine.name) 20 }}{{ .machine.name }}{{else}}{{ trimSuffix \"-\" (trunc 14 .machine.name) }}-{{ trunc -5 .machine.name }}{{end}}"),
			want: []gomegatypes.GomegaMatcher{
				Equal("quick-md-8nxwc-gfd5v"), // 20 characters
			},
		},
		{
			name:        "template for 20 characters: trim to 20 characters if name has more than 20 characters",
			machineName: "quick-start-d34gt4-md-0-wqc85-8nxwc-gfd5v", // 41 characters
			template:    ptr.To("{{ if le (len .machine.name) 20 }}{{ .machine.name }}{{else}}{{ trimSuffix \"-\" (trunc 14 .machine.name) }}-{{ trunc -5 .machine.name }}{{end}}"),
			want: []gomegatypes.GomegaMatcher{
				Equal("quick-start-d3-gfd5v"), // 20 characters
			},
		},
		{
			name:        "template for 20 characters: trim to 19 characters if name has more than 20 characters and last character of prefix is -",
			machineName: "quick-start-d-34gt4-md-0-wqc85-8nxwc-gfd5v", // 42 characters
			template:    ptr.To("{{ if le (len .machine.name) 20 }}{{ .machine.name }}{{else}}{{ trimSuffix \"-\" (trunc 14 .machine.name) }}-{{ trunc -5 .machine.name }}{{end}}"),
			want: []gomegatypes.GomegaMatcher{
				Equal("quick-start-d-gfd5v"), // 19 characters
			},
		},
		{
			name:        "template with a prefix and only 5 random character from the machine name",
			machineName: "quick-start-d-34gt4-md-0-wqc85-8nxwc-gfd5v", // 42 characters
			template:    ptr.To("vm-{{ trunc -5 .machine.name }}"),
			want: []gomegatypes.GomegaMatcher{
				Equal("vm-gfd5v"), // 8 characters
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := GenerateVSphereVMName(tt.machineName, &infrav1.VSphereVMNamingStrategy{
				Template: tt.template,
			})

			if (err != nil) != tt.wantErr {
				t.Errorf("GenerateVSphereVMName error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(got) > maxNameLength {
				t.Errorf("generated name should never be longer than %d, got %d", maxNameLength, len(got))
			}

			for _, matcher := range tt.want {
				g.Expect(got).To(matcher)
			}
		})
	}
}

func Test_reconcilePoweredOnCondition(t *testing.T) {
	newVM := func(set func(*infrav1.VSphereVM)) *infrav1.VSphereVM {
		vm := &infrav1.VSphereVM{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"}}
		if set != nil {
			set(vm)
		}
		return vm
	}

	t.Run("VM has not reported PoweredOn yet: machine condition stays unset", func(t *testing.T) {
		g := NewWithT(t)
		machine := &infrav1.VSphereMachine{}
		reconcilePoweredOnCondition(machine, newVM(nil))
		g.Expect(v1beta2conditions.Get(machine, infrav1.VSphereMachinePoweredOnV1Beta2Condition)).To(BeNil())
	})

	t.Run("VM powered off out of band: machine mirrors PoweredOn=False", func(t *testing.T) {
		g := NewWithT(t)
		machine := &infrav1.VSphereMachine{}
		vm := newVM(func(vm *infrav1.VSphereVM) {
			v1beta2conditions.Set(vm, metav1.Condition{
				Type:   infrav1.VSphereVMPoweredOnV1Beta2Condition,
				Status: metav1.ConditionFalse,
				Reason: infrav1.VSphereVMPoweredOffV1Beta2Reason,
			})
		})
		reconcilePoweredOnCondition(machine, vm)
		c := v1beta2conditions.Get(machine, infrav1.VSphereMachinePoweredOnV1Beta2Condition)
		g.Expect(c).ToNot(BeNil())
		g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(c.Reason).To(Equal(infrav1.VSphereVMPoweredOffV1Beta2Reason))
	})

	t.Run("VM powered on: machine mirrors PoweredOn=True", func(t *testing.T) {
		g := NewWithT(t)
		machine := &infrav1.VSphereMachine{}
		vm := newVM(func(vm *infrav1.VSphereVM) {
			v1beta2conditions.Set(vm, metav1.Condition{
				Type:   infrav1.VSphereVMPoweredOnV1Beta2Condition,
				Status: metav1.ConditionTrue,
				Reason: infrav1.VSphereVMPoweredOnV1Beta2Reason,
			})
		})
		reconcilePoweredOnCondition(machine, vm)
		c := v1beta2conditions.Get(machine, infrav1.VSphereMachinePoweredOnV1Beta2Condition)
		g.Expect(c).ToNot(BeNil())
		g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
	})
}
