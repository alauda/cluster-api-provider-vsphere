/*
Copyright 2024 The Kubernetes Authors.

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

package util

import (
	"net"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

// ConfigPoolSlotClaim identifies the machine config pool slot that already owns
// an address.
type ConfigPoolSlotClaim struct {
	PoolName string
	Hostname string
}

// FindConfigPoolSlotClaimingIP returns the first slot of clusterName's machine
// config pools that already declares ip as a node address. A control plane VIP
// that collides with such a slot cannot work: keepalived would be handed an
// address a node already owns statically.
//
// It is used both by the VSphereCluster webhook and, defensively, by the
// reconciler, so the two never disagree about what counts as a conflict.
func FindConfigPoolSlotClaimingIP(pools []infrav1.VSphereMachineConfigPool, clusterName, ip string) (ConfigPoolSlotClaim, bool) {
	if ip == "" {
		return ConfigPoolSlotClaim{}, false
	}
	for i := range pools {
		pool := &pools[i]
		if pool.Spec.ClusterRef.Name != clusterName {
			continue
		}
		for j := range pool.Spec.Configs {
			slot := &pool.Spec.Configs[j]
			if slot.Network == nil {
				continue
			}
			if SlotAddressEquals(slot.Network.Primary.IP, ip) {
				return ConfigPoolSlotClaim{PoolName: pool.Name, Hostname: slot.Hostname}, true
			}
			for _, additional := range slot.Network.Additional {
				if SlotAddressEquals(additional.IP, ip) {
					return ConfigPoolSlotClaim{PoolName: pool.Name, Hostname: slot.Hostname}, true
				}
			}
		}
	}
	return ConfigPoolSlotClaim{}, false
}

// SlotAddressEquals reports whether a machine config pool slot address refers to
// ip. Slot addresses may carry a prefix length (they end up in
// VSphereVM.spec.network.devices[].ipAddrs, which is CIDR-formatted); ip never
// does.
func SlotAddressEquals(slotAddress, ip string) bool {
	if slotAddress == "" {
		return false
	}
	if host, _, err := net.ParseCIDR(slotAddress); err == nil {
		return host.String() == ip
	}
	return slotAddress == ip
}
