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

package util_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	vmwarev1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/vmware/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

func Test_GetMachinePreferredIPAddress(t *testing.T) {
	testCases := []struct {
		name        string
		machine     *infrav1.VSphereMachine
		ipAddr      string
		expectedErr error
	}{
		{
			name: "single IPv4 address, no preferred CIDR",
			machine: &infrav1.VSphereMachine{
				Status: infrav1.VSphereMachineStatus{
					Addresses: []clusterv1.MachineAddress{
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "192.168.0.1",
						},
					},
				},
			},
			ipAddr:      "192.168.0.1",
			expectedErr: nil,
		},
		{
			name: "single IPv6 address, no preferred CIDR",
			machine: &infrav1.VSphereMachine{
				Status: infrav1.VSphereMachineStatus{
					Addresses: []clusterv1.MachineAddress{
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "fdf3:35b5:9dad:6e09::0001",
						},
					},
				},
			},
			ipAddr:      "fdf3:35b5:9dad:6e09::0001",
			expectedErr: nil,
		},
		{
			name: "multiple IPv4 addresses, only 1 internal, no preferred CIDR",
			machine: &infrav1.VSphereMachine{
				Status: infrav1.VSphereMachineStatus{
					Addresses: []clusterv1.MachineAddress{
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "192.168.0.1",
						},
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "1.1.1.1",
						},
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "2.2.2.2",
						},
					},
				},
			},
			ipAddr:      "192.168.0.1",
			expectedErr: nil,
		},
		{
			name: "multiple IPv4 addresses, preferred CIDR set to v4",
			machine: &infrav1.VSphereMachine{
				Spec: infrav1.VSphereMachineSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							PreferredAPIServerCIDR: "192.168.0.0/16",
						},
					},
				},
				Status: infrav1.VSphereMachineStatus{
					Addresses: []clusterv1.MachineAddress{
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "192.168.0.1",
						},
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "172.17.0.1",
						},
					},
				},
			},
			ipAddr:      "192.168.0.1",
			expectedErr: nil,
		},
		{
			name: "multiple IPv4 and IPv6 addresses, preferred CIDR set to v4",
			machine: &infrav1.VSphereMachine{
				Spec: infrav1.VSphereMachineSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							PreferredAPIServerCIDR: "192.168.0.0/16",
						},
					},
				},
				Status: infrav1.VSphereMachineStatus{
					Addresses: []clusterv1.MachineAddress{
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "192.168.0.1",
						},
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "fdf3:35b5:9dad:6e09::0001",
						},
					},
				},
			},
			ipAddr:      "192.168.0.1",
			expectedErr: nil,
		},
		{
			name: "multiple IPv4 and IPv6 addresses, preferred CIDR set to v6",
			machine: &infrav1.VSphereMachine{
				Spec: infrav1.VSphereMachineSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							PreferredAPIServerCIDR: "fdf3:35b5:9dad:6e09::/64",
						},
					},
				},
				Status: infrav1.VSphereMachineStatus{

					Addresses: []clusterv1.MachineAddress{
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "192.168.0.1",
						},
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "fdf3:35b5:9dad:6e09::0001",
						},
					},
				},
			},
			ipAddr:      "fdf3:35b5:9dad:6e09::0001",
			expectedErr: nil,
		},
		{
			name: "no addresses found",
			machine: &infrav1.VSphereMachine{
				Spec: infrav1.VSphereMachineSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							PreferredAPIServerCIDR: "fdf3:35b5:9dad:6e09::/64",
						},
					},
				},
				Status: infrav1.VSphereMachineStatus{
					Addresses: []clusterv1.MachineAddress{},
				},
			},
			ipAddr:      "",
			expectedErr: util.ErrNoMachineIPAddr,
		},
		{
			name: "no addresses found with preferred CIDR",
			machine: &infrav1.VSphereMachine{
				Spec: infrav1.VSphereMachineSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							PreferredAPIServerCIDR: "192.168.0.0/16",
						},
					},
				},
				Status: infrav1.VSphereMachineStatus{

					Addresses: []clusterv1.MachineAddress{
						{
							Type:    clusterv1.MachineExternalIP,
							Address: "10.0.0.1",
						},
					},
				},
			},
			ipAddr:      "",
			expectedErr: util.ErrNoMachineIPAddr,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ipAddr, err := util.GetMachinePreferredIPAddress(tc.machine)
			if err != tc.expectedErr {
				t.Logf("expected err: %q", tc.expectedErr)
				t.Logf("actual err: %q", err)
				t.Errorf("unexpected error")
			}

			if ipAddr != tc.ipAddr {
				t.Logf("expected IP addr: %q", tc.ipAddr)
				t.Logf("actual IP addr: %q", ipAddr)
				t.Error("unexpected IP addr from machine context")
			}
		})
	}
}

func Test_GetMachineMetadata(t *testing.T) {
	testCases := []struct {
		name            string
		machine         *infrav1.VSphereVM
		networkStatuses []infrav1.NetworkStatus
		ipamState       map[string]infrav1.NetworkDeviceSpec
		persistentDisks []infrav1.PersistentDisk
		expected        string
	}{
		{
			name: "dhcp4",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName: "network1",
									MACAddr:     "00:00:00:00:00",
									DHCP4:       true,
								},
							},
						},
					},
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: false
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:00"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: true
      dhcp6: false
      accept-ra: false
`,
		},
		{
			name: "dhcp4+deviceName",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName: "network1",
									MACAddr:     "00:00:00:00:00",
									DHCP4:       true,
									DeviceName:  "ens192",
								},
							},
						},
					},
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: false
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:00"
      set-name: "ens192"
      wakeonlan: true
      dhcp4: true
      dhcp6: false
      accept-ra: false
`,
		},
		{
			name: "dhcp6",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName: "network1",
									MACAddr:     "00:00:00:00:00",
									DHCP6:       true,
								},
							},
						},
					},
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: false
  ipv6: true
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:00"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: false
      dhcp6: true
      accept-ra: true
`,
		},
		{
			name: "dhcp4+dhcp6",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName: "network1",
									MACAddr:     "00:00:00:00:00",
									DHCP4:       true,
									DHCP6:       true,
								},
							},
						},
					},
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: true
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:00"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: true
      dhcp6: true
      accept-ra: true
`,
		},
		{
			name: "dhcp4+dhcp6+dhcpOverrides",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName: "network1",
									MACAddr:     "00:00:00:00:00",
									DHCP4:       true,
									DHCP4Overrides: &infrav1.DHCPOverrides{
										Hostname:     toStringPtr("hal"),
										RouteMetric:  toIntPtr(12345),
										SendHostname: toBoolPtr(true),
										UseDNS:       toBoolPtr(true),
										UseDomains:   toStringPtr("true"),
										UseHostname:  toBoolPtr(true),
										UseMTU:       toBoolPtr(true),
										UseNTP:       toBoolPtr(true),
										UseRoutes:    toStringPtr("route"),
									},
									DHCP6: true,
									DHCP6Overrides: &infrav1.DHCPOverrides{
										Hostname:     toStringPtr("hal"),
										RouteMetric:  toIntPtr(12345),
										SendHostname: toBoolPtr(true),
										UseDNS:       toBoolPtr(true),
										UseDomains:   toStringPtr("true"),
										UseHostname:  toBoolPtr(true),
										UseMTU:       toBoolPtr(true),
										UseNTP:       toBoolPtr(true),
										UseRoutes:    toStringPtr("route"),
									},
								},
							},
						},
					},
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: true
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:00"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: true
      dhcp6: true
      accept-ra: true
      dhcp4-overrides:
        hostname: "hal"
        route-metric: 12345
        send-hostname: true
        use-dns: true
        use-domains: true
        use-hostname: true
        use-mtu: true
        use-ntp: true
        use-routes: "route"
      dhcp6-overrides:
        hostname: "hal"
        route-metric: 12345
        send-hostname: true
        use-dns: true
        use-domains: true
        use-hostname: true
        use-mtu: true
        use-ntp: true
        use-routes: "route"
`,
		},
		{
			name: "dhcp4+dhcp6+noDhcpOverrides",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName:    "network1",
									MACAddr:        "00:00:00:00:00",
									DHCP4:          true,
									DHCP4Overrides: nil,
									DHCP6:          true,
									DHCP6Overrides: nil,
								},
							},
						},
					},
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: true
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:00"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: true
      dhcp6: true
      accept-ra: true
`,
		},
		{
			name: "static4+dhcp6",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName: "network1",
									MACAddr:     "00:00:00:00:00",
									DHCP6:       true,
									IPAddrs:     []string{"192.168.4.21"},
									Gateway4:    "192.168.4.1",
								},
							},
						},
					},
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: true
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:00"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: false
      dhcp6: true
      accept-ra: true
      addresses:
      - "192.168.4.21"
      gateway4: "192.168.4.1"
`,
		},
		{
			name: "static4+dhcp6+static-routes",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName: "network1",
									MACAddr:     "00:00:00:00:00",
									DHCP6:       true,
									IPAddrs:     []string{"192.168.4.21"},
									Gateway4:    "192.168.4.1",
								},
							},
							Routes: []infrav1.NetworkRouteSpec{
								{
									To:     "192.168.5.1/24",
									Via:    "192.168.4.254",
									Metric: 3,
								},
							},
						},
					},
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: true
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:00"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: false
      dhcp6: true
      accept-ra: true
      addresses:
      - "192.168.4.21"
      gateway4: "192.168.4.1"
  routes:
  - to: "192.168.5.1/24"
    via: "192.168.4.254"
    metric: 3
`,
		},
		{
			name: "2nets",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName: "network1",
									MACAddr:     "00:00:00:00:00",
									DHCP4:       true,
									Routes: []infrav1.NetworkRouteSpec{
										{
											To:     "192.168.5.1/24",
											Via:    "192.168.4.254",
											Metric: 3,
										},
									},
								},
								{
									NetworkName: "network12",
									MACAddr:     "00:00:00:00:01",
									DHCP6:       true,
									MTU:         mtu(100),
								},
							},
						},
					},
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: true
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:00"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: true
      dhcp6: false
      accept-ra: false
      routes:
      - to: "192.168.5.1/24"
        via: "192.168.4.254"
        metric: 3
    id1:
      match:
        macaddress: "00:00:00:00:01"
      set-name: "eth1"
      wakeonlan: true
      dhcp4: false
      dhcp6: true
      accept-ra: true
      mtu: 100
`,
		},
		{
			name: "2nets-static+dhcp",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName:   "network1",
									MACAddr:       "00:00:00:00:00",
									IPAddrs:       []string{"192.168.4.21"},
									Gateway4:      "192.168.4.1",
									MTU:           mtu(0),
									Nameservers:   []string{"1.1.1.1"},
									SearchDomains: []string{"vmware.ci"},
								},
								{
									NetworkName:   "network12",
									MACAddr:       "00:00:00:00:01",
									DHCP6:         true,
									SearchDomains: []string{"vmware6.ci"},
								},
							},
						},
					},
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: true
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:00"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: false
      dhcp6: false
      accept-ra: false
      addresses:
      - "192.168.4.21"
      gateway4: "192.168.4.1"
      nameservers:
        addresses:
        - "1.1.1.1"
        search:
        - "vmware.ci"
    id1:
      match:
        macaddress: "00:00:00:00:01"
      set-name: "eth1"
      wakeonlan: true
      dhcp4: false
      dhcp6: true
      accept-ra: true
      nameservers:
        search:
        - "vmware6.ci"
`,
		},
		{
			name: "2nets+network-statuses",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName: "network1",
									MACAddr:     "00:00:00:00:00",
									DHCP4:       true,
								},
								{
									NetworkName: "network12",
									MACAddr:     "00:00:00:00:01",
									DHCP6:       true,
								},
							},
						},
					},
				},
			},
			networkStatuses: []infrav1.NetworkStatus{
				{MACAddr: "00:00:00:00:ab"},
				{MACAddr: "00:00:00:00:cd"},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: true
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:ab"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: true
      dhcp6: false
      accept-ra: false
    id1:
      match:
        macaddress: "00:00:00:00:cd"
      set-name: "eth1"
      wakeonlan: true
      dhcp4: false
      dhcp6: true
      accept-ra: true
`,
		},
		{
			name: "ipam state is used to render metadata",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName: "network1",
									MACAddr:     "00:00:00:00:00",
								},
								{
									NetworkName: "network2",
									MACAddr:     "00:00:00:00:01",
									DHCP4:       true,
								},
								{
									NetworkName: "network3",
									MACAddr:     "00:00:00:00:02",
								},
							},
						},
					},
				},
			},
			ipamState: map[string]infrav1.NetworkDeviceSpec{
				"00:00:00:00:00": {
					IPAddrs: []string{
						"10.10.50.50/24",
					},
					Gateway4: "10.10.50.1",
				},
				"00:00:00:00:02": {
					IPAddrs: []string{
						"fe80::3/64",
					},
					Gateway6: "fe80::1",
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: false
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:00"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: false
      dhcp6: false
      accept-ra: false
      addresses:
      - "10.10.50.50/24"
      gateway4: "10.10.50.1"
    id1:
      match:
        macaddress: "00:00:00:00:01"
      set-name: "eth1"
      wakeonlan: true
      dhcp4: true
      dhcp6: false
      accept-ra: false
    id2:
      match:
        macaddress: "00:00:00:00:02"
      set-name: "eth2"
      wakeonlan: true
      dhcp4: false
      dhcp6: false
      accept-ra: false
      addresses:
      - "fe80::3/64"
      gateway6: "fe80::1"
`,
		},
		{
			name: "more-network-statuses-than-spec-devices",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									NetworkName: "network1",
									MACAddr:     "00:00:00:00:00",
									DHCP4:       true,
								},
								{
									NetworkName: "network12",
									MACAddr:     "00:00:00:00:01",
									DHCP6:       true,
								},
							},
						},
					},
				},
			},
			networkStatuses: []infrav1.NetworkStatus{
				{MACAddr: "00:00:00:00:ab"},
				{MACAddr: "00:00:00:00:cd"},
				{MACAddr: "00:00:00:00:ef"},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: true
  ipv6: true
network:
  version: 2
  ethernets:
    id0:
      match:
        macaddress: "00:00:00:00:ab"
      set-name: "eth0"
      wakeonlan: true
      dhcp4: true
      dhcp6: false
      accept-ra: false
    id1:
      match:
        macaddress: "00:00:00:00:cd"
      set-name: "eth1"
      wakeonlan: true
      dhcp4: false
      dhcp6: true
      accept-ra: true
    id2:
      match:
        macaddress: "00:00:00:00:ef"
      set-name: "eth2"
      wakeonlan: true
      dhcp4: false
      dhcp6: false
      accept-ra: false
`,
		},
		{
			name: "persistent disks are defaulted and filtered",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{},
					},
				},
			},
			persistentDisks: []infrav1.PersistentDisk{
				{
					Name:       "data-1",
					UnitNumber: toInt32Ptr(2),
					MountPath:  "/var/lib/data",
				},
				{
					Name: "data-2",
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: false
  ipv6: false
network:
  version: 2
  ethernets:
`,
		},
		{
			name: "static network without mac still renders metadata",
			machine: &infrav1.VSphereVM{
				Spec: infrav1.VSphereVMSpec{
					VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
						Network: infrav1.NetworkSpec{
							Devices: []infrav1.NetworkDeviceSpec{
								{
									IPAddrs:     []string{"192.168.10.10/24"},
									Gateway4:    "192.168.10.1",
									Nameservers: []string{"8.8.8.8"},
								},
							},
						},
					},
				},
			},
			persistentDisks: []infrav1.PersistentDisk{
				{
					Name:       "data-1",
					UnitNumber: toInt32Ptr(0),
					MountPath:  "/var/cpaas",
				},
			},
			expected: `
instance-id: "test-vm"
local-hostname: "test-vm"
wait-on-network:
  ipv4: false
  ipv6: false
network:
  version: 2
  ethernets:
    id0:
      set-name: "eth0"
      wakeonlan: true
      dhcp4: false
      dhcp6: false
      accept-ra: false
      addresses:
      - "192.168.10.10/24"
      gateway4: "192.168.10.1"
      nameservers:
        addresses:
        - "8.8.8.8"
`,
		},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tc.machine.Name = tc.name
			actVal, err := util.GetMachineMetadata("test-vm", *tc.machine, tc.ipamState, tc.persistentDisks, tc.networkStatuses...)
			if err != nil {
				t.Fatal(err)
			}

			if string(actVal) != tc.expected {
				t.Logf("actual metadata value: %s", actVal)
				t.Logf("expected metadata value: %s", tc.expected)
				t.Error("unexpected metadata value")
			}
		})
	}
}

func Test_GetPersistentDiskCloudConfig(t *testing.T) {
	actual, err := util.GetPersistentDiskCloudConfig([]infrav1.PersistentDisk{{
		Name:       "data-1",
		UnitNumber: toInt32Ptr(2),
		MountPath:  "/var/lib/data",
		DiskUUID:   "6000C29d-45cb-2787-e901-a2a0131b2e82",
	}})
	if err != nil {
		t.Fatal(err)
	}

	var actualObj interface{}
	if err := yaml.Unmarshal(actual, &actualObj); err != nil {
		t.Fatalf("failed to parse actual cloud-config: %v", err)
	}
	actualMap := actualObj.(map[interface{}]interface{})
	writeFiles, ok := actualMap["write_files"].([]interface{})
	if !ok || len(writeFiles) != 3 {
		t.Fatalf("expected 3 write_files entries, got: %#v", actualMap["write_files"])
	}
	configEntry := writeFiles[0].(map[interface{}]interface{})
	encodedConfig := configEntry["content"].(string)
	decodedConfig, err := base64.StdEncoding.DecodeString(encodedConfig)
	if err != nil {
		t.Fatalf("failed to decode persistent disk config: %v", err)
	}
	if !strings.Contains(string(decodedConfig), "6000C29d-45cb-2787-e901-a2a0131b2e82") {
		t.Fatalf("expected persistent disk config to contain disk UUID, got: %s", string(decodedConfig))
	}
	scriptEntry := writeFiles[1].(map[interface{}]interface{})
	encodedScript := scriptEntry["content"].(string)
	decodedScript, err := base64.StdEncoding.DecodeString(encodedScript)
	if err != nil {
		t.Fatalf("failed to decode persistent disk reconcile script: %v", err)
	}
	scriptText := string(decodedScript)
	for _, expected := range []string{
		"find_device_by_uuid()",
		"Prefer a device-mapper node when multipath is configured",
		"grep -qi '^mpath-'",
		"set -- ${unique_devices}",
	} {
		if !strings.Contains(scriptText, expected) {
			t.Fatalf("expected reconcile script to contain %q, got: %s", expected, scriptText)
		}
	}
	if strings.Contains(scriptText, "match_count") {
		t.Fatalf("expected reconcile script to stop relying on single-match counting, got: %s", scriptText)
	}
	runcmd, ok := actualMap["runcmd"].([]interface{})
	if !ok || len(runcmd) != 3 {
		t.Fatalf("expected 3 runcmd entries, got: %#v", actualMap["runcmd"])
	}
	actualStr := string(actual)
	for _, expected := range []string{
		"/etc/capv/persistent-disks.tsv",
		"/usr/local/bin/capv-persistent-disk-reconcile.sh",
		"/etc/systemd/system/capv-persistent-disk-reconcile.service",
		"systemctl",
	} {
		if !strings.Contains(actualStr, expected) {
			t.Fatalf("expected generated cloud-config to contain %q, got: %s", expected, actualStr)
		}
	}
}

func Test_GetKubeletServingCertCloudConfig(t *testing.T) {
	vm := infrav1.VSphereVM{
		Spec: infrav1.VSphereVMSpec{
			VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
				Network: infrav1.NetworkSpec{
					Devices: []infrav1.NetworkDeviceSpec{
						{IPAddrs: []string{"192.168.130.219/20"}},
						{IPAddrs: []string{"192.168.164.244/24"}},
					},
				},
			},
		},
	}

	caCertPEM, caKeyPEM := newTestCA(t)
	actual, err := util.GetKubeletServingCertCloudConfig("master-01", vm, nil, caCertPEM, caKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) == 0 {
		t.Fatal("expected kubelet serving cert cloud-config to be generated")
	}

	var actualObj interface{}
	if err := yaml.Unmarshal(actual, &actualObj); err != nil {
		t.Fatalf("failed to parse actual cloud-config: %v", err)
	}
	actualMap := actualObj.(map[interface{}]interface{})
	writeFiles, ok := actualMap["write_files"].([]interface{})
	if !ok || len(writeFiles) != 3 {
		t.Fatalf("expected 3 write_files entries, got: %#v", actualMap["write_files"])
	}

	certEntry := writeFiles[0].(map[interface{}]interface{})
	encodedCert := certEntry["content"].(string)
	decodedCert, err := base64.StdEncoding.DecodeString(encodedCert)
	if err != nil {
		t.Fatalf("failed to decode kubelet cert: %v", err)
	}
	certBlock, _ := pem.Decode(decodedCert)
	if certBlock == nil {
		t.Fatal("expected kubelet cert PEM block")
	}
	kubeletCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("failed to parse kubelet cert: %v", err)
	}
	if kubeletCert.Subject.CommonName != "kubelet" {
		t.Fatalf("expected kubelet cert CN kubelet, got %q", kubeletCert.Subject.CommonName)
	}
	if len(kubeletCert.DNSNames) != 1 || kubeletCert.DNSNames[0] != "master-01" {
		t.Fatalf("expected kubelet cert DNS SAN master-01, got %#v", kubeletCert.DNSNames)
	}
	actualIPs := []string{}
	for _, ip := range kubeletCert.IPAddresses {
		actualIPs = append(actualIPs, ip.String())
	}
	for _, expected := range []string{"192.168.130.219", "192.168.164.244"} {
		if !strings.Contains(strings.Join(actualIPs, ","), expected) {
			t.Fatalf("expected kubelet cert IP SANs to contain %q, got %#v", expected, actualIPs)
		}
	}

	keyEntry := writeFiles[1].(map[interface{}]interface{})
	encodedKey := keyEntry["content"].(string)
	decodedKey, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		t.Fatalf("failed to decode kubelet key: %v", err)
	}
	keyBlock, _ := pem.Decode(decodedKey)
	if keyBlock == nil {
		t.Fatal("expected kubelet key PEM block")
	}
	if _, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err != nil {
		t.Fatalf("failed to parse kubelet key: %v", err)
	}

	patchEntry := writeFiles[2].(map[interface{}]interface{})
	patchText := patchEntry["content"].(string)
	for _, expected := range []string{
		"tlsCertFile",
		"/etc/kubernetes/pki/kubelet.crt",
		"tlsPrivateKeyFile",
		"/etc/kubernetes/pki/kubelet.key",
	} {
		if !strings.Contains(patchText, expected) {
			t.Fatalf("expected kubelet patch to contain %q, got: %s", expected, patchText)
		}
	}
	if _, ok := actualMap["runcmd"]; ok {
		t.Fatalf("expected kubelet serving cert cloud-config to stop relying on runcmd, got: %#v", actualMap["runcmd"])
	}
}

func newTestCA(t *testing.T) ([]byte, []byte) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate CA key: %v", err)
	}
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("failed to generate CA serial number: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "test-ca",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("failed to create CA cert: %v", err)
	}
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)})
	return caCertPEM, caKeyPEM
}

func Test_MergeCloudConfigUserData(t *testing.T) {
	userData := []byte(`## template: jinja
#cloud-config

write_files:
- path: /tmp/example
  content: hello
runcmd:
- echo ok
`)
	diskConfig, err := util.GetPersistentDiskCloudConfig([]infrav1.PersistentDisk{{
		Name:       "data-1",
		UnitNumber: toInt32Ptr(2),
		MountPath:  "/var/lib/data",
	}})
	if err != nil {
		t.Fatal(err)
	}

	actual, err := util.MergeCloudConfigUserData(userData, diskConfig)
	if err != nil {
		t.Fatal(err)
	}

	actualStr := string(actual)
	for _, expected := range []string{
		"## template: jinja",
		"#cloud-config",
		"write_files:",
		"runcmd:",
		"/etc/capv/persistent-disks.tsv",
	} {
		if !strings.Contains(actualStr, expected) {
			t.Fatalf("expected merged cloud-config to contain %q, got: %s", expected, actualStr)
		}
	}
}

func Test_MergeCloudConfigUserData_Idempotent(t *testing.T) {
	userData := []byte(`## template: jinja
#cloud-config

runcmd:
- echo ok
`)
	diskConfig, err := util.GetPersistentDiskCloudConfig([]infrav1.PersistentDisk{{
		Name:       "data-1",
		UnitNumber: toInt32Ptr(2),
		MountPath:  "/var/lib/data",
	}})
	if err != nil {
		t.Fatal(err)
	}

	first, err := util.MergeCloudConfigUserData(userData, diskConfig)
	if err != nil {
		t.Fatal(err)
	}
	second, err := util.MergeCloudConfigUserData(first, diskConfig)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Count(string(second), "/etc/capv/persistent-disks.tsv") != 1 {
		t.Fatalf("expected persistent disk config to be merged once, got: %s", string(second))
	}
	if strings.Count(string(second), "/etc/systemd/system/capv-persistent-disk-reconcile.service") != 1 {
		t.Fatalf("expected persistent disk service to be merged once, got: %s", string(second))
	}
}

func toInt32Ptr(v int32) *int32 {
	return &v
}

func Test_MergeCloudConfigUserData_WriteFilesReplaceByPath(t *testing.T) {
	// Simulates the scenario where DiskUUID changes between reconciles:
	// the updated write_files entry should replace the old one, not append.
	oldDiskConfig, err := util.GetPersistentDiskCloudConfig([]infrav1.PersistentDisk{{
		Name:       "data-1",
		UnitNumber: toInt32Ptr(1),
		MountPath:  "/var/lib/data",
		DiskUUID:   "old-uuid",
	}})
	if err != nil {
		t.Fatal(err)
	}
	newDiskConfig, err := util.GetPersistentDiskCloudConfig([]infrav1.PersistentDisk{{
		Name:       "data-1",
		UnitNumber: toInt32Ptr(1),
		MountPath:  "/var/lib/data",
		DiskUUID:   "new-uuid",
	}})
	if err != nil {
		t.Fatal(err)
	}

	base := []byte(`#cloud-config

runcmd:
- echo ok
`)
	// First merge: old disk config into base.
	first, err := util.MergeCloudConfigUserData(base, oldDiskConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "old-uuid") {
		t.Fatalf("expected old-uuid in first merge, got: %s", string(first))
	}

	// Second merge: new disk config into result that already has old config.
	second, err := util.MergeCloudConfigUserData(first, newDiskConfig)
	if err != nil {
		t.Fatal(err)
	}

	result := string(second)
	if strings.Contains(result, "old-uuid") {
		t.Fatalf("old-uuid should have been replaced, got: %s", result)
	}
	if !strings.Contains(result, "new-uuid") {
		t.Fatalf("expected new-uuid in second merge, got: %s", result)
	}
	// Each file path should appear exactly once.
	if strings.Count(result, "/etc/capv/persistent-disks.tsv") != 1 {
		t.Fatalf("expected TSV file entry exactly once, got: %s", result)
	}
	if strings.Count(result, "/usr/local/bin/capv-persistent-disk-reconcile.sh") != 1 {
		t.Fatalf("expected reconcile script entry exactly once, got: %s", result)
	}
}

func Test_MergeCloudConfigUserData_PreservesNonOverlappingFiles(t *testing.T) {
	base := []byte(`#cloud-config

write_files:
- path: /tmp/custom-file
  content: keep-me
runcmd:
- echo ok
`)
	diskConfig, err := util.GetPersistentDiskCloudConfig([]infrav1.PersistentDisk{{
		Name:       "data-1",
		UnitNumber: toInt32Ptr(1),
		MountPath:  "/var/lib/data",
	}})
	if err != nil {
		t.Fatal(err)
	}

	merged, err := util.MergeCloudConfigUserData(base, diskConfig)
	if err != nil {
		t.Fatal(err)
	}
	result := string(merged)

	if !strings.Contains(result, "/tmp/custom-file") {
		t.Fatalf("expected non-overlapping write_files entry to be preserved, got: %s", result)
	}
	if !strings.Contains(result, "keep-me") {
		t.Fatalf("expected non-overlapping file content to be preserved, got: %s", result)
	}
	if !strings.Contains(result, "/etc/capv/persistent-disks.tsv") {
		t.Fatalf("expected disk config file to be present, got: %s", result)
	}
}

func TestConvertProviderIDToUUID(t *testing.T) {
	g := gomega.NewGomegaWithT(t)

	testCases := []struct {
		name         string
		providerID   *string
		expectedUUID string
	}{
		{
			name:         "nil providerID",
			providerID:   nil,
			expectedUUID: "",
		},
		{
			name:         "empty providerID",
			providerID:   toStringPtr(""),
			expectedUUID: "",
		},
		{
			name:         "invalid providerID",
			providerID:   toStringPtr("1234"),
			expectedUUID: "",
		},
		{
			name:         "missing prefix",
			providerID:   toStringPtr("12345678-1234-1234-1234-123456789abc"),
			expectedUUID: "",
		},
		{
			name:         "valid providerID",
			providerID:   toStringPtr("vsphere://12345678-1234-1234-1234-123456789abc"),
			expectedUUID: "12345678-1234-1234-1234-123456789abc",
		},
		{
			name:         "mixed case",
			providerID:   toStringPtr("vsphere://12345678-1234-1234-1234-123456789AbC"),
			expectedUUID: "12345678-1234-1234-1234-123456789AbC",
		},
		{
			name:         "invalid hex chars",
			providerID:   toStringPtr("vsphere://12345678-1234-1234-1234-123456789abg"),
			expectedUUID: "",
		},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(*testing.T) {
			actualUUID := util.ConvertProviderIDToUUID(tc.providerID)
			g.Expect(actualUUID).To(gomega.Equal(tc.expectedUUID))
		})
	}
}

func TestConvertUUIDtoProviderID(t *testing.T) {
	g := gomega.NewGomegaWithT(t)

	testCases := []struct {
		name               string
		uuid               string
		expectedProviderID string
	}{
		{
			name:               "empty uuid",
			uuid:               "",
			expectedProviderID: "",
		},
		{
			name:               "invalid uuid",
			uuid:               "1234",
			expectedProviderID: "",
		},
		{
			name:               "valid uuid",
			uuid:               "12345678-1234-1234-1234-123456789abc",
			expectedProviderID: "vsphere://12345678-1234-1234-1234-123456789abc",
		},
		{
			name:               "mixed case",
			uuid:               "12345678-1234-1234-1234-123456789AbC",
			expectedProviderID: "vsphere://12345678-1234-1234-1234-123456789AbC",
		},
		{
			name:               "invalid hex chars",
			uuid:               "12345678-1234-1234-1234-123456789abg",
			expectedProviderID: "",
		},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(*testing.T) {
			actualProviderID := util.ConvertUUIDToProviderID(tc.uuid)
			g.Expect(actualProviderID).To(gomega.Equal(tc.expectedProviderID))
		})
	}
}

func Test_MachinesAsString(t *testing.T) {
	tests := []struct {
		machines     []*clusterv1.Machine
		errorMessage string
	}{
		{
			machines: []*clusterv1.Machine{
				{ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "m1-ns"}},
			},
			errorMessage: "m1-ns/m1",
		},
		{
			machines: []*clusterv1.Machine{
				{ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "m1-ns"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "m2", Namespace: "m2-ns"}},
			},
			errorMessage: "m1-ns/m1 and m2-ns/m2",
		},
		{
			machines: []*clusterv1.Machine{
				{ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "m1-ns"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "m2", Namespace: "m2-ns"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "m3", Namespace: "m3-ns"}},
			},
			errorMessage: "m1-ns/m1, m2-ns/m2 and m3-ns/m3",
		},
	}

	for _, tt := range tests {
		g := gomega.NewWithT(t)
		msg := util.MachinesAsString(tt.machines)
		g.Expect(msg).To(gomega.Equal(tt.errorMessage))
	}
}

func Test_GetVSphereClusterFromVSphereMachine(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = vmwarev1.AddToScheme(scheme)

	ns := "util-test"

	incorrectMachine := &vmwarev1.VSphereMachine{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"foo": "bar"}},
	}
	machine := &vmwarev1.VSphereMachine{
		ObjectMeta: metav1.ObjectMeta{
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "foo"},
			Name:      "foo-machine-1",
			Namespace: ns,
		},
	}
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: ns,
		},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: &corev1.ObjectReference{
				Name: "foo-abcdef", // auto generated name
			},
		},
	}
	vsphereCluster := &vmwarev1.VSphereCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo-abcdef",
			Namespace: ns,
		},
	}

	testCases := []struct {
		name         string
		initObjects  []client.Object
		inputMachine *vmwarev1.VSphereMachine
		hasError     bool
	}{
		{
			name:         "for machine without CAPI cluster name label",
			hasError:     true,
			inputMachine: incorrectMachine,
		},
		{
			name:         "for non-existent CAPI cluster",
			hasError:     true,
			inputMachine: machine,
		},
		{
			name:         "for non-existent VSphereCluster",
			hasError:     true,
			inputMachine: machine,
			initObjects:  []client.Object{cluster},
		},
		{
			name:         "for non-existent VSphereCluster",
			inputMachine: machine,
			initObjects:  []client.Object{cluster, vsphereCluster},
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)

			client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.initObjects...).Build()
			_, err := util.GetVSphereClusterFromVMwareMachine(context.Background(), client, tt.inputMachine)
			if tt.hasError {
				g.Expect(err).To(gomega.HaveOccurred())
			} else {
				g.Expect(err).NotTo(gomega.HaveOccurred())
			}
		})
	}
}

func mtu(i int64) *int64 {
	if i == 0 {
		return nil
	}
	return &i
}

func toStringPtr(s string) *string {
	return &s
}

func toBoolPtr(b bool) *bool {
	return &b
}

func toIntPtr(i int) *int {
	return &i
}
