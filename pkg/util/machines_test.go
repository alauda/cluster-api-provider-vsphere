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
	"regexp"
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
		VolumePath: "[ds] vm/data-1.vmdk",
		DiskUUID:   "6000C29d-45cb-2787-e901-a2a0131b2e82",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var actualObj interface{}
	if err := yaml.Unmarshal(actual, &actualObj); err != nil {
		t.Fatalf("failed to parse actual cloud-config: %v", err)
	}
	actualMap := actualObj.(map[interface{}]interface{})
	writeFiles, ok := actualMap["write_files"].([]interface{})
	if !ok || len(writeFiles) != 5 {
		t.Fatalf("expected 5 write_files entries (tsv, script, service, containerd drop-in, kubelet drop-in), got %d: %#v", len(writeFiles), actualMap["write_files"])
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

func Test_GetPersistentDiskCloudConfigCreatesPodLogSymlinkForPersistentContainerdDisk(t *testing.T) {
	actual, err := util.GetPersistentDiskCloudConfig([]infrav1.PersistentDisk{{
		Name:       "containerd-data",
		UnitNumber: toInt32Ptr(2),
		MountPath:  "/var/lib/containerd/",
		VolumePath: "[ds] vm/containerd-data.vmdk",
		DiskUUID:   "6000C29d-45cb-2787-e901-a2a0131b2e82",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var actualObj interface{}
	if err := yaml.Unmarshal(actual, &actualObj); err != nil {
		t.Fatalf("failed to parse actual cloud-config: %v", err)
	}
	actualMap := actualObj.(map[interface{}]interface{})
	writeFiles := actualMap["write_files"].([]interface{})

	configEntry := writeFiles[0].(map[interface{}]interface{})
	decodedConfig, err := base64.StdEncoding.DecodeString(configEntry["content"].(string))
	if err != nil {
		t.Fatalf("failed to decode disk table: %v", err)
	}
	if !strings.Contains(string(decodedConfig), "containerd-data\t2\t/var/lib/containerd\text4") {
		t.Fatalf("expected disk table to contain containerd mount path, got: %s", string(decodedConfig))
	}

	scriptEntry := writeFiles[1].(map[interface{}]interface{})
	decodedScript, err := base64.StdEncoding.DecodeString(scriptEntry["content"].(string))
	if err != nil {
		t.Fatalf("failed to decode reconcile script: %v", err)
	}
	scriptText := string(decodedScript)
	for _, expected := range []string{
		"CONTAINERD_MOUNT_PATH=\"/var/lib/containerd\"",
		"POD_LOG_TARGET_PATH=\"${CONTAINERD_MOUNT_PATH}/logs\"",
		"POD_LOG_PATH=\"/var/log/pods\"",
		"ensure_pod_log_symlink()",
		"[ \"${1:-}\" = \"${CONTAINERD_MOUNT_PATH}\" ] || return 0",
		"mountpoint -q \"${CONTAINERD_MOUNT_PATH}\"",
		"mkdir -p /var/log \"${POD_LOG_TARGET_PATH}\"",
		"for item in \"${POD_LOG_PATH}\"/* \"${POD_LOG_PATH}\"/.[!.]* \"${POD_LOG_PATH}\"/..?*; do",
		"mv \"${item}\" \"${POD_LOG_TARGET_PATH}/\"",
		"rmdir \"${POD_LOG_PATH}\"",
		"ln -s \"${POD_LOG_TARGET_PATH}\" \"${POD_LOG_PATH}\"",
		"ensure_pod_log_symlink \"${mount_path}\"",
	} {
		if !strings.Contains(scriptText, expected) {
			t.Fatalf("expected reconcile script to contain %q, got: %s", expected, scriptText)
		}
	}
}

func Test_GetPersistentDiskCloudConfigWithEphemeralDisks(t *testing.T) {
	actual, err := util.GetPersistentDiskCloudConfig(
		[]infrav1.PersistentDisk{{
			Name:       "data-1",
			UnitNumber: toInt32Ptr(2),
			MountPath:  "/var/lib/data",
			VolumePath: "[ds] vm/data-1.vmdk",
			DiskUUID:   "6000C29d-45cb-2787-e901-a2a0131b2e82",
		}},
		[]infrav1.EphemeralDisk{{
			Name:       "cache-1",
			UnitNumber: toInt32Ptr(3),
			MountPath:  "/var/lib/containerd",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	var actualObj interface{}
	if err := yaml.Unmarshal(actual, &actualObj); err != nil {
		t.Fatalf("failed to parse actual cloud-config: %v", err)
	}
	actualMap := actualObj.(map[interface{}]interface{})
	writeFiles := actualMap["write_files"].([]interface{})
	configEntry := writeFiles[0].(map[interface{}]interface{})
	decodedConfig, err := base64.StdEncoding.DecodeString(configEntry["content"].(string))
	if err != nil {
		t.Fatalf("failed to decode disk table: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(decodedConfig), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 disk-table rows (one persistent, one ephemeral), got %d: %q", len(lines), string(decodedConfig))
	}

	// Persistent row keeps its backfilled UUID.
	persistentFields := strings.Split(lines[0], "\t")
	if persistentFields[0] != "data-1" || persistentFields[5] != "6000C29d-45cb-2787-e901-a2a0131b2e82" {
		t.Fatalf("unexpected persistent row: %q", lines[0])
	}

	// Ephemeral row: name/unit/mount/fsFormat set, disk-UUID column empty (so the
	// guest falls back to by-unit lookup) and wipe flag false.
	ephemeralFields := strings.Split(lines[1], "\t")
	if len(ephemeralFields) != 7 {
		t.Fatalf("expected 7 tab-separated fields in ephemeral row, got %d: %q", len(ephemeralFields), lines[1])
	}
	if got, want := ephemeralFields[0], "cache-1"; got != want {
		t.Fatalf("ephemeral name = %q, want %q", got, want)
	}
	if got, want := ephemeralFields[1], "3"; got != want {
		t.Fatalf("ephemeral unit = %q, want %q", got, want)
	}
	if got, want := ephemeralFields[2], "/var/lib/containerd"; got != want {
		t.Fatalf("ephemeral mountPath = %q, want %q", got, want)
	}
	if got, want := ephemeralFields[3], "ext4"; got != want {
		t.Fatalf("ephemeral fsFormat = %q, want %q (should default)", got, want)
	}
	if ephemeralFields[5] != "" {
		t.Fatalf("expected empty disk-UUID column for ephemeral disk, got %q", ephemeralFields[5])
	}
	if got, want := ephemeralFields[6], "false"; got != want {
		t.Fatalf("ephemeral wipe flag = %q, want %q", got, want)
	}
}

func Test_GetPersistentDiskCloudConfigEphemeralOnly(t *testing.T) {
	// A slot with only ephemeral disks still emits the reconcile machinery.
	actual, err := util.GetPersistentDiskCloudConfig(nil, []infrav1.EphemeralDisk{{
		Name:       "cache-1",
		UnitNumber: toInt32Ptr(4),
		MountPath:  "/var/lib/containerd",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(actual), "/etc/capv/persistent-disks.tsv") {
		t.Fatalf("expected ephemeral-only config to emit the disk table, got: %s", string(actual))
	}

	var actualObj interface{}
	if err := yaml.Unmarshal(actual, &actualObj); err != nil {
		t.Fatalf("failed to parse actual cloud-config: %v", err)
	}
	actualMap := actualObj.(map[interface{}]interface{})
	writeFiles := actualMap["write_files"].([]interface{})
	scriptEntry := writeFiles[1].(map[interface{}]interface{})
	decodedScript, err := base64.StdEncoding.DecodeString(scriptEntry["content"].(string))
	if err != nil {
		t.Fatalf("failed to decode reconcile script: %v", err)
	}
	if !strings.Contains(string(decodedScript), "ensure_pod_log_symlink \"${mount_path}\"") {
		t.Fatalf("expected ephemeral-only containerd disk to install pod log symlink logic, got: %s", string(decodedScript))
	}
}

func Test_GetPersistentDiskCloudConfigEphemeralRequiresUnit(t *testing.T) {
	_, err := util.GetPersistentDiskCloudConfig(nil, []infrav1.EphemeralDisk{{
		Name:      "cache-1",
		MountPath: "/var/lib/containerd",
	}})
	if err == nil {
		t.Fatal("expected ephemeral disk without an observed unit number to fail")
	}
	if !strings.Contains(err.Error(), "cache-1") || !strings.Contains(err.Error(), "unitNumber") {
		t.Fatalf("expected error to mention disk and unitNumber, got: %v", err)
	}
}

func Test_GetPersistentDiskCloudConfigRejectsInvalidMountPath(t *testing.T) {
	testCases := []struct {
		name      string
		mountPath string
		expected  string
	}{
		{name: "relative path", mountPath: "var/lib/data", expected: "absolute Linux path"},
		{name: "tab in path", mountPath: "/var/lib/\tdata", expected: "tab or newline"},
		{name: "newline in path", mountPath: "/var/lib/\ndata", expected: "tab or newline"},
		{name: "root path", mountPath: "/", expected: "root path"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := util.GetPersistentDiskCloudConfig([]infrav1.PersistentDisk{{
				Name:       "data-1",
				UnitNumber: toInt32Ptr(2),
				MountPath:  tc.mountPath,
				VolumePath: "[ds] vm/data-1.vmdk",
				DiskUUID:   "6000C29d-45cb-2787-e901-a2a0131b2e82",
			}}, nil)
			if err == nil {
				t.Fatal("expected invalid mountPath to fail")
			}
			if !strings.Contains(err.Error(), "data-1") || !strings.Contains(err.Error(), tc.expected) {
				t.Fatalf("expected error to mention disk and %q, got: %v", tc.expected, err)
			}
		})
	}
}

func Test_NormalizeGuestMountPath(t *testing.T) {
	testCases := []struct {
		name      string
		input     string
		expected  string
		wantError string
	}{
		{name: "empty", input: "", expected: ""},
		{name: "trim and clean", input: " /data//logs/ ", expected: "/data/logs"},
		{name: "relative path", input: "data", wantError: "absolute Linux path"},
		{name: "newline", input: "/data\nlogs", wantError: "tab or newline"},
		{name: "root", input: "/", wantError: "root path"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual, err := util.NormalizeGuestMountPath(tc.input)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("expected error containing %q, got path=%q err=%v", tc.wantError, actual, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if actual != tc.expected {
				t.Fatalf("normalized path = %q, want %q", actual, tc.expected)
			}
		})
	}
}

func Test_GetPersistentDiskCloudConfigRequiresCompleteBackfill(t *testing.T) {
	testCases := []struct {
		name           string
		persistentDisk infrav1.PersistentDisk
		expectedField  string
	}{
		{
			name: "missing unit number",
			persistentDisk: infrav1.PersistentDisk{
				Name:       "data-1",
				VolumePath: "[ds] vm/data-1.vmdk",
				DiskUUID:   "uuid-data-1",
			},
			expectedField: "unitNumber",
		},
		{
			name: "missing volume path",
			persistentDisk: infrav1.PersistentDisk{
				Name:       "data-1",
				UnitNumber: toInt32Ptr(1),
				DiskUUID:   "uuid-data-1",
			},
			expectedField: "volumePath",
		},
		{
			name: "missing disk uuid",
			persistentDisk: infrav1.PersistentDisk{
				Name:       "data-1",
				UnitNumber: toInt32Ptr(1),
				VolumePath: "[ds] vm/data-1.vmdk",
			},
			expectedField: "diskUUID",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := util.GetPersistentDiskCloudConfig([]infrav1.PersistentDisk{tc.persistentDisk}, nil)
			if err == nil {
				t.Fatal("expected incomplete persistent disk metadata to fail")
			}
			if !strings.Contains(err.Error(), "data-1") || !strings.Contains(err.Error(), tc.expectedField) {
				t.Fatalf("expected error to mention disk and %s, got: %v", tc.expectedField, err)
			}
		})
	}

	err := util.ValidatePersistentDiskBackfill([]infrav1.PersistentDisk{
		{Name: "data-1", VolumePath: "[ds] vm/data-1.vmdk"},
		{Name: "data-2", UnitNumber: toInt32Ptr(2), DiskUUID: "uuid-data-2"},
	})
	if err == nil {
		t.Fatal("expected aggregated validation error")
	}
	for _, expected := range []string{"data-1", "unitNumber", "diskUUID", "data-2", "volumePath"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("expected aggregated error to contain %q, got: %v", expected, err)
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
	if !ok || len(writeFiles) != 2 {
		t.Fatalf("expected 2 write_files entries, got: %#v", actualMap["write_files"])
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

	if _, ok := actualMap["runcmd"]; ok {
		t.Fatalf("expected kubelet serving cert cloud-config to stop relying on runcmd, got: %#v", actualMap["runcmd"])
	}
}

func Test_GetPrimaryNodeIPAddress_UsesGuestReportedIPs(t *testing.T) {
	vm := infrav1.VSphereVM{
		Spec: infrav1.VSphereVMSpec{
			VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
				Network: infrav1.NetworkSpec{
					Devices: []infrav1.NetworkDeviceSpec{
						{NetworkName: "mgmt", DHCP4: true},
						{NetworkName: "workload", DHCP4: true},
					},
				},
			},
		},
	}
	slot := &infrav1.MachineConfigSlot{
		Hostname: "slot-1",
		Network: &infrav1.MachineConfigSlotNetwork{
			Primary: infrav1.NetworkConfig{NetworkName: "mgmt"},
			Additional: []infrav1.NetworkConfig{
				{NetworkName: "workload"},
			},
		},
	}

	ip, err := util.GetPrimaryNodeIPAddress(vm, slot, nil,
		infrav1.NetworkStatus{MACAddr: "00:50:56:aa:bb:01", IPAddrs: []string{"192.168.10.20/24"}},
		infrav1.NetworkStatus{MACAddr: "00:50:56:aa:bb:02", IPAddrs: []string{"172.16.10.20/24"}},
	)
	if err != nil {
		t.Fatalf("expected guest-reported primary IP to resolve, got error: %v", err)
	}
	if ip != "192.168.10.20" {
		t.Fatalf("expected primary guest-reported IP 192.168.10.20, got %q", ip)
	}
}

func Test_GetMachineMetadata_DoesNotRenderGuestReportedIPsAsStaticAddresses(t *testing.T) {
	vm := infrav1.VSphereVM{
		Spec: infrav1.VSphereVMSpec{
			VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
				Network: infrav1.NetworkSpec{
					Devices: []infrav1.NetworkDeviceSpec{
						{NetworkName: "mgmt", DHCP4: true},
					},
				},
			},
		},
	}

	actual, err := util.GetMachineMetadata("master-01", vm, nil, nil,
		infrav1.NetworkStatus{MACAddr: "00:50:56:aa:bb:01", IPAddrs: []string{"192.168.10.20/24"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	actualStr := string(actual)
	if strings.Contains(actualStr, "192.168.10.20/24") {
		t.Fatalf("expected guest-reported DHCP IPs to stay out of metadata addresses, got: %s", actualStr)
	}
	if !strings.Contains(actualStr, "dhcp4: true") {
		t.Fatalf("expected DHCP metadata to be preserved, got: %s", actualStr)
	}
}

func Test_GetKubeletServingCertCloudConfig_UsesGuestReportedIPs(t *testing.T) {
	vm := infrav1.VSphereVM{
		Spec: infrav1.VSphereVMSpec{
			VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
				Network: infrav1.NetworkSpec{
					Devices: []infrav1.NetworkDeviceSpec{
						{NetworkName: "mgmt", DHCP4: true},
						{NetworkName: "workload", DHCP4: true},
					},
				},
			},
		},
	}

	caCertPEM, caKeyPEM := newTestCA(t)
	actual, err := util.GetKubeletServingCertCloudConfig("master-01", vm, nil, caCertPEM, caKeyPEM,
		infrav1.NetworkStatus{MACAddr: "00:50:56:aa:bb:01", IPAddrs: []string{"192.168.10.20/24"}},
		infrav1.NetworkStatus{MACAddr: "00:50:56:aa:bb:02", IPAddrs: []string{"172.16.10.20/24"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	var actualObj interface{}
	if err := yaml.Unmarshal(actual, &actualObj); err != nil {
		t.Fatalf("failed to parse actual cloud-config: %v", err)
	}
	actualMap := actualObj.(map[interface{}]interface{})
	writeFiles, ok := actualMap["write_files"].([]interface{})
	if !ok || len(writeFiles) != 2 {
		t.Fatalf("expected 2 write_files entries, got: %#v", actualMap["write_files"])
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

	actualIPs := []string{}
	for _, ip := range kubeletCert.IPAddresses {
		actualIPs = append(actualIPs, ip.String())
	}
	for _, expected := range []string{"192.168.10.20", "172.16.10.20"} {
		if !strings.Contains(strings.Join(actualIPs, ","), expected) {
			t.Fatalf("expected kubelet cert IP SANs to contain %q, got %#v", expected, actualIPs)
		}
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
		VolumePath: "[ds] vm/data-1.vmdk",
		DiskUUID:   "uuid-data-1",
	}}, nil)
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
		VolumePath: "[ds] vm/data-1.vmdk",
		DiskUUID:   "uuid-data-1",
	}}, nil)
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
		VolumePath: "[ds] vm/data-1.vmdk",
		DiskUUID:   "old-uuid",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	newDiskConfig, err := util.GetPersistentDiskCloudConfig([]infrav1.PersistentDisk{{
		Name:       "data-1",
		UnitNumber: toInt32Ptr(1),
		MountPath:  "/var/lib/data",
		VolumePath: "[ds] vm/data-1.vmdk",
		DiskUUID:   "new-uuid",
	}}, nil)
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
	// TSV content is base64-encoded in the YAML, so check for the encoded form.
	oldUUIDEncoded := base64.StdEncoding.EncodeToString([]byte("data-1\t1\t/var/lib/data\text4\tdefaults\told-uuid\tfalse\n"))
	if !strings.Contains(string(first), oldUUIDEncoded) {
		t.Fatalf("expected base64-encoded old-uuid TSV in first merge, got: %s", string(first))
	}

	// Second merge: new disk config into result that already has old config.
	second, err := util.MergeCloudConfigUserData(first, newDiskConfig)
	if err != nil {
		t.Fatal(err)
	}

	result := string(second)
	if strings.Contains(result, oldUUIDEncoded) {
		t.Fatalf("old-uuid should have been replaced, got: %s", result)
	}
	newUUIDEncoded := base64.StdEncoding.EncodeToString([]byte("data-1\t1\t/var/lib/data\text4\tdefaults\tnew-uuid\tfalse\n"))
	if !strings.Contains(result, newUUIDEncoded) {
		t.Fatalf("expected base64-encoded new-uuid TSV in second merge, got: %s", result)
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
		VolumePath: "[ds] vm/data-1.vmdk",
		DiskUUID:   "uuid-data-1",
	}}, nil)
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

func TestUpdateKubeadmNodeRegistration(t *testing.T) {
	userData := []byte(`## template: jinja
#cloud-config

write_files:
- path: /run/kubeadm/kubeadm.yaml
  content: |
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: InitConfiguration
    nodeRegistration:
      name: master-01-os
    ---
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: ClusterConfiguration
    kubernetesVersion: v1.34.5
- path: /run/kubeadm/kubeadm-join-config.yaml
  content: |
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: JoinConfiguration
    nodeRegistration:
      name: worker-01-os
    ---
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: ClusterConfiguration
    kubernetesVersion: v1.34.5
`)

	actual, err := util.UpdateKubeadmNodeRegistration(userData, "master-01", "192.168.130.219")
	if err != nil {
		t.Fatal(err)
	}

	actualStr := string(actual)
	if !strings.Contains(actualStr, "name: master-01") {
		t.Fatalf("expected kubeadm nodeRegistration name to be updated, got: %s", actualStr)
	}
	if !strings.Contains(actualStr, "name: node-ip") || !strings.Contains(actualStr, "value: 192.168.130.219") {
		t.Fatalf("expected kubeadm nodeRegistration kubeletExtraArgs.node-ip to be updated, got: %s", actualStr)
	}
	if strings.Contains(actualStr, "name: master-01-os") || strings.Contains(actualStr, "name: worker-01-os") {
		t.Fatalf("expected old node names to be replaced, got: %s", actualStr)
	}
	if !strings.Contains(actualStr, "kind: ClusterConfiguration") || !strings.Contains(actualStr, "kubernetesVersion: v1.34.5") {
		t.Fatalf("expected kubeadm cluster configuration to be preserved, got: %s", actualStr)
	}

	actual, err = util.UpdateKubeadmNodeRegistration(actual, "master-01", "192.168.130.219")
	if err != nil {
		t.Fatalf("expected repeated init update to succeed, got: %v", err)
	}
}

func TestUpdateKubeadmNodeRegistrationClusterConfigFirst(t *testing.T) {
	// Reproduces the real-world scenario where ClusterConfiguration appears
	// before InitConfiguration in the multi-document kubeadm YAML. The upstream
	// UniversalDeserializer only decodes a single document, so without splitting
	// the YAML first this would fail with "unknown kubeadm types".
	userData := []byte(`## template: jinja
#cloud-config

write_files:
- path: /run/kubeadm/kubeadm.yaml
  content: |
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: ClusterConfiguration
    kubernetesVersion: v1.34.5
    ---
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: InitConfiguration
    nodeRegistration:
      name: master-01-os
`)

	actual, err := util.UpdateKubeadmNodeRegistration(userData, "master-01", "10.0.0.1")
	if err != nil {
		t.Fatalf("expected multi-doc with ClusterConfiguration first to succeed, got: %v", err)
	}

	actualStr := string(actual)
	if !strings.Contains(actualStr, "name: master-01") {
		t.Fatalf("expected nodeRegistration name to be updated, got: %s", actualStr)
	}
	if !strings.Contains(actualStr, "kind: ClusterConfiguration") {
		t.Fatalf("expected ClusterConfiguration to be preserved, got: %s", actualStr)
	}
}

func TestUpdateKubeadmNodeRegistrationJoinWithoutKubernetesVersion(t *testing.T) {
	userData := []byte(`## template: jinja
#cloud-config

write_files:
- path: /run/kubeadm/kubeadm-join-config.yaml
  content: |
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: JoinConfiguration
    discovery:
      bootstrapToken:
        apiServerEndpoint: 192.168.0.10:6443
        token: abcdef.0123456789abcdef
        caCertHashes:
        - sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    nodeRegistration:
      name: worker-01-os
`)

	actual, err := util.UpdateKubeadmNodeRegistration(userData, "worker-01", "192.168.130.220")
	if err != nil {
		t.Fatal(err)
	}

	actualStr := string(actual)
	if !strings.Contains(actualStr, "name: worker-01") {
		t.Fatalf("expected kubeadm join nodeRegistration name to be updated, got: %s", actualStr)
	}
	if !strings.Contains(actualStr, "name: node-ip") || !strings.Contains(actualStr, "value: 192.168.130.220") {
		t.Fatalf("expected kubeadm join kubeletExtraArgs.node-ip to be updated, got: %s", actualStr)
	}
	if strings.Contains(actualStr, "worker-01-os") {
		t.Fatalf("expected old join node name to be replaced, got: %s", actualStr)
	}
	if !strings.Contains(actualStr, "apiVersion: kubeadm.k8s.io/v1beta4") {
		t.Fatalf("expected proper kubeadm apiVersion in output, got: %s", actualStr)
	}
	if !strings.Contains(actualStr, "kind: JoinConfiguration") {
		t.Fatalf("expected proper kubeadm kind in output, got: %s", actualStr)
	}
}

func TestUpdateKubeadmNodeRegistrationJoinWithV1beta3(t *testing.T) {
	userData := []byte(`## template: jinja
#cloud-config

write_files:
- path: /run/kubeadm/kubeadm-join-config.yaml
  content: |
    apiVersion: kubeadm.k8s.io/v1beta3
    kind: JoinConfiguration
    discovery:
      bootstrapToken:
        apiServerEndpoint: 192.168.0.10:6443
        token: abcdef.0123456789abcdef
        caCertHashes:
        - sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    nodeRegistration:
      name: worker-01-os
`)

	actual, err := util.UpdateKubeadmNodeRegistration(userData, "worker-01", "192.168.130.220")
	if err != nil {
		t.Fatal(err)
	}

	actualStr := string(actual)
	if !strings.Contains(actualStr, "name: worker-01") {
		t.Fatalf("expected kubeadm join nodeRegistration name to be updated, got: %s", actualStr)
	}
	if !strings.Contains(actualStr, "apiVersion: kubeadm.k8s.io/v1beta3") {
		t.Fatalf("expected proper kubeadm apiVersion v1beta3 in output, got: %s", actualStr)
	}
	if !strings.Contains(actualStr, "kind: JoinConfiguration") {
		t.Fatalf("expected proper kubeadm kind in output, got: %s", actualStr)
	}
}

func TestUpdateKubeadmNodeRegistrationJoinWithoutAPIVersion(t *testing.T) {
	// Without a valid apiVersion, UnmarshalJoinConfiguration fails to parse the
	// document, so UpdateKubeadmNodeRegistration should return an error.
	userData := []byte(`## template: jinja
#cloud-config

write_files:
- path: /run/kubeadm/kubeadm-join-config.yaml
  content: |
    kind: JoinConfiguration
    discovery:
      bootstrapToken:
        apiServerEndpoint: 192.168.0.10:6443
        token: abcdef.0123456789abcdef
        caCertHashes:
        - sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    nodeRegistration:
      name: worker-01-os
`)

	_, err := util.UpdateKubeadmNodeRegistration(userData, "worker-01", "192.168.130.220")
	if err == nil {
		t.Fatal("expected error when apiVersion is missing, got nil")
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

func Test_DeterministicDiskPath(t *testing.T) {
	// Stable slot identity produces an exact "[datastore] <hostname>-<ip>/<hostname>-<ip>-<disk>.vmdk".
	// Hostname, datastore, and disk name are required; an empty IP is valid for DHCP slots.
	tests := []struct {
		name      string
		hostname  string
		ip        string
		datastore string
		diskName  string
		want      string
	}{
		{name: "happy path", hostname: "master-1", ip: "192.168.1.10", datastore: "datastore1", diskName: "etcd", want: "[datastore1] master-1-192.168.1.10/master-1-192.168.1.10-etcd.vmdk"},
		{name: "datastore with spaces", hostname: "master-1", ip: "192.168.1.10", datastore: "my datastore", diskName: "data-0", want: "[my datastore] master-1-192.168.1.10/master-1-192.168.1.10-data-0.vmdk"},
		{name: "empty datastore returns empty", hostname: "master-1", ip: "192.168.1.10", datastore: "", diskName: "etcd", want: ""},
		{name: "empty hostname returns empty", hostname: "", ip: "192.168.1.10", datastore: "ds", diskName: "etcd", want: ""},
		{name: "empty ip remains deterministic for DHCP", hostname: "master-1", ip: "", datastore: "ds", diskName: "etcd", want: "[ds] master-1/master-1-etcd.vmdk"},
		{name: "empty disk name returns empty", hostname: "master-1", ip: "192.168.1.10", datastore: "ds", diskName: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(util.DeterministicDiskPath(tt.hostname, tt.ip, tt.datastore, tt.diskName)).To(gomega.Equal(tt.want))
		})
	}

	t.Run("different host/ip pairs do not collide", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(util.DeterministicDiskPath("master-1", "192.168.1.10", "ds", "data")).ToNot(gomega.Equal(util.DeterministicDiskPath("master-1", "192.168.1.11", "ds", "data")))
	})
}

func Test_DeterministicDiskName(t *testing.T) {
	hexSuffix := regexp.MustCompile(`^-[0-9a-f]{5}$`)

	t.Run("clean name is readable host-disk", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(util.DeterministicDiskName("master-1", "192.168.1.10", "etcd")).To(gomega.Equal("master-1-192.168.1.10-etcd"))
		g.Expect(util.DeterministicDiskName("master.1_a-b", "192.168.1.10", "d.0_x-y")).To(gomega.Equal("master.1_a-b-192.168.1.10-d.0_x-y"))
	})

	t.Run("empty IP does not add an extra separator", func(t *testing.T) {
		g := gomega.NewWithT(t)
		g.Expect(util.DeterministicDiskName("master-1", "", "etcd")).To(gomega.Equal("master-1-etcd"))
	})

	t.Run("unsafe chars are sanitized and disambiguated by a hash suffix", func(t *testing.T) {
		g := gomega.NewWithT(t)
		got := util.DeterministicDiskName("master-1", "192.168.1.10", "a/b")
		// Sanitized prefix preserved, unsafe byte replaced by '-', hash appended.
		g.Expect(got).To(gomega.HavePrefix("master-1-192.168.1.10-a-b"))
		g.Expect(hexSuffix.MatchString(got[len("master-1-192.168.1.10-a-b"):])).To(gomega.BeTrue())
	})

	t.Run("unsafe names that sanitize alike are kept apart by the raw-input hash", func(t *testing.T) {
		g := gomega.NewWithT(t)
		// "a/b" and "a]b" both sanitize to "a-b"; the hash over the raw inputs disambiguates.
		g.Expect(util.DeterministicDiskName("master-1", "192.168.1.10", "a/b")).ToNot(gomega.Equal(util.DeterministicDiskName("master-1", "192.168.1.10", "a]b")))
	})

	t.Run("over-long name is truncated and hashed within the 250-byte budget", func(t *testing.T) {
		g := gomega.NewWithT(t)
		got := util.DeterministicDiskName("master-1", "192.168.1.10", strings.Repeat("a", 300))
		g.Expect(len(got)).To(gomega.Equal(250)) // 255 minus the ".vmdk" the caller appends
		g.Expect(hexSuffix.MatchString(got[len(got)-6:])).To(gomega.BeTrue())
	})

	t.Run("is idempotent", func(t *testing.T) {
		g := gomega.NewWithT(t)
		for _, diskName := range []string{"etcd", "a/b", strings.Repeat("a", 300)} {
			g.Expect(util.DeterministicDiskName("master-1", "192.168.1.10", diskName)).To(gomega.Equal(util.DeterministicDiskName("master-1", "192.168.1.10", diskName)))
		}
	})
}

func toBoolPtr(b bool) *bool {
	return &b
}

func toIntPtr(i int) *int {
	return &i
}
