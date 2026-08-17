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

package util_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

func Test_SlotAddressEquals(t *testing.T) {
	tests := []struct {
		slotAddress string
		ip          string
		want        bool
	}{
		{"10.0.0.10/24", "10.0.0.10", true},
		{"10.0.0.10", "10.0.0.10", true},
		{"10.0.0.11/24", "10.0.0.10", false},
		{"", "10.0.0.10", false},
		{"not-an-address", "10.0.0.10", false},
	}
	for _, tt := range tests {
		if got := util.SlotAddressEquals(tt.slotAddress, tt.ip); got != tt.want {
			t.Fatalf("SlotAddressEquals(%q, %q) = %v, want %v", tt.slotAddress, tt.ip, got, tt.want)
		}
	}
}

func Test_FindConfigPoolSlotClaimingIP(t *testing.T) {
	pools := []infrav1.VSphereMachineConfigPool{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "other-cluster-pool"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "other-cluster"},
				Configs: []infrav1.MachineConfigSlot{{
					Hostname: "other-1",
					Network:  &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{IP: "10.0.0.10/24"}},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pool"},
			Spec: infrav1.VSphereMachineConfigPoolSpec{
				ClusterRef: corev1.ObjectReference{Name: "test-cluster"},
				Configs: []infrav1.MachineConfigSlot{
					{
						Hostname: "cp-1",
						Network:  &infrav1.MachineConfigSlotNetwork{Primary: infrav1.NetworkConfig{IP: "10.0.0.20/24"}},
					},
					{
						Hostname: "cp-2",
						Network: &infrav1.MachineConfigSlotNetwork{
							Primary:    infrav1.NetworkConfig{IP: "10.0.0.21/24"},
							Additional: []infrav1.NetworkConfig{{IP: "10.0.0.30/24"}},
						},
					},
					{Hostname: "cp-3"},
				},
			},
		},
	}

	// A slot in another cluster's pool is not a conflict.
	if _, found := util.FindConfigPoolSlotClaimingIP(pools, "test-cluster", "10.0.0.10"); found {
		t.Fatal("expected no conflict for an address owned by another cluster's pool")
	}
	if _, found := util.FindConfigPoolSlotClaimingIP(pools, "test-cluster", "10.0.0.99"); found {
		t.Fatal("expected no conflict for an unused address")
	}

	claim, found := util.FindConfigPoolSlotClaimingIP(pools, "test-cluster", "10.0.0.20")
	if !found || claim.PoolName != "pool" || claim.Hostname != "cp-1" {
		t.Fatalf("expected the primary address of cp-1 to conflict, got %+v (found=%v)", claim, found)
	}

	claim, found = util.FindConfigPoolSlotClaimingIP(pools, "test-cluster", "10.0.0.30")
	if !found || claim.Hostname != "cp-2" {
		t.Fatalf("expected an additional address of cp-2 to conflict, got %+v (found=%v)", claim, found)
	}
}

func Test_GetBootstrapVIPCloudConfig(t *testing.T) {
	actual, err := util.GetBootstrapVIPCloudConfig("10.0.0.10", "10.0.0.21", "ens192")
	if err != nil {
		t.Fatal(err)
	}

	var actualObj interface{}
	if err := yaml.Unmarshal(actual, &actualObj); err != nil {
		t.Fatalf("failed to parse the generated cloud-config: %v", err)
	}
	actualMap := actualObj.(map[interface{}]interface{})

	writeFiles, ok := actualMap["write_files"].([]interface{})
	if !ok || len(writeFiles) != 2 {
		t.Fatalf("expected 2 write_files entries (script, unit), got: %#v", actualMap["write_files"])
	}

	scriptEntry := writeFiles[0].(map[interface{}]interface{})
	if scriptEntry["path"] != "/etc/capv/bootstrap-vip.sh" || scriptEntry["permissions"] != "0755" {
		t.Fatalf("unexpected script entry: %#v", scriptEntry)
	}
	decodedScript, err := base64.StdEncoding.DecodeString(scriptEntry["content"].(string))
	if err != nil {
		t.Fatalf("failed to decode the bootstrap VIP script: %v", err)
	}
	scriptText := string(decodedScript)
	for _, expected := range []string{
		`VIP="10.0.0.10"`,
		`NODE_IP="10.0.0.21"`,
		`INTERFACE="ens192"`,
		"ip addr add",
		"arping",
	} {
		if !strings.Contains(scriptText, expected) {
			t.Fatalf("expected the bootstrap VIP script to contain %q, got: %s", expected, scriptText)
		}
	}

	unitEntry := writeFiles[1].(map[interface{}]interface{})
	if unitEntry["path"] != "/etc/systemd/system/capv-bootstrap-vip.service" {
		t.Fatalf("unexpected unit entry: %#v", unitEntry)
	}
	decodedUnit, err := base64.StdEncoding.DecodeString(unitEntry["content"].(string))
	if err != nil {
		t.Fatalf("failed to decode the bootstrap VIP unit: %v", err)
	}
	unitText := string(decodedUnit)
	// The missing [Install] section is the whole guard against a rebooted node
	// racing keepalived for the VIP: without it systemd cannot enable the unit.
	if strings.Contains(unitText, "[Install]") || strings.Contains(unitText, "WantedBy") {
		t.Fatalf("the bootstrap VIP unit must not be enableable, got: %s", unitText)
	}
	if !strings.Contains(unitText, "Type=oneshot") || !strings.Contains(unitText, "RemainAfterExit=yes") {
		t.Fatalf("unexpected bootstrap VIP unit: %s", unitText)
	}

	runcmd, ok := actualMap["runcmd"].([]interface{})
	if !ok || len(runcmd) != 2 {
		t.Fatalf("expected 2 runcmd entries, got: %#v", actualMap["runcmd"])
	}
	// The start has to abort the whole cloud-init script, otherwise kubeadm runs
	// against an endpoint nothing answers.
	start, ok := runcmd[1].(string)
	if !ok || !strings.Contains(start, "|| exit 1") {
		t.Fatalf("expected the unit start to abort cloud-init on failure, got: %#v", runcmd[1])
	}
}

func Test_GetBootstrapVIPCloudConfigRejectsAnInvalidVIP(t *testing.T) {
	for _, vip := range []string{"", "not-an-ip"} {
		if _, err := util.GetBootstrapVIPCloudConfig(vip, "10.0.0.21", ""); err == nil {
			t.Fatalf("expected GetBootstrapVIPCloudConfig(%q) to fail", vip)
		}
	}
}

func Test_GetBootstrapVIPCloudConfigMergeIsIdempotent(t *testing.T) {
	base := []byte("## template: jinja\n#cloud-config\n\nruncmd:\n  - kubeadm init --config /run/kubeadm/kubeadm.yaml\n")
	vipConfig, err := util.GetBootstrapVIPCloudConfig("10.0.0.10", "10.0.0.21", "")
	if err != nil {
		t.Fatal(err)
	}

	merged, err := util.MergeCloudConfigUserData(base, vipConfig)
	if err != nil {
		t.Fatal(err)
	}
	// reconcileBootstrapUserData re-merges the already merged data on every
	// reconcile, so a second merge must not accumulate entries.
	remerged, err := util.MergeCloudConfigUserData(merged, vipConfig)
	if err != nil {
		t.Fatal(err)
	}
	if string(merged) != string(remerged) {
		t.Fatalf("merging the bootstrap VIP config twice changed the result:\nfirst:\n%s\nsecond:\n%s", merged, remerged)
	}

	var mergedObj interface{}
	if err := yaml.Unmarshal(remerged, &mergedObj); err != nil {
		t.Fatalf("failed to parse the merged cloud-config: %v", err)
	}
	runcmd := mergedObj.(map[interface{}]interface{})["runcmd"].([]interface{})
	if len(runcmd) != 3 {
		t.Fatalf("expected the VIP entries to be prepended once ahead of kubeadm, got: %#v", runcmd)
	}
	if last, ok := runcmd[2].(string); !ok || !strings.Contains(last, "kubeadm init") {
		t.Fatalf("expected kubeadm to stay last, got: %#v", runcmd)
	}
}

func Test_IsKubeadmInitUserData(t *testing.T) {
	tests := []struct {
		name     string
		userData string
		want     bool
	}{
		{
			name:     "init node",
			userData: "#cloud-config\nwrite_files:\n  - path: /run/kubeadm/kubeadm.yaml\n    content: |\n      apiVersion: kubeadm.k8s.io/v1beta3\n",
			want:     true,
		},
		{
			name:     "joining node",
			userData: "#cloud-config\nwrite_files:\n  - path: /run/kubeadm/kubeadm-join-config.yaml\n    content: |\n      apiVersion: kubeadm.k8s.io/v1beta3\n",
			want:     false,
		},
		{
			name:     "unparsable data",
			userData: "\tnot yaml at all: [",
			want:     false,
		},
		{
			name:     "no write_files",
			userData: "#cloud-config\nruncmd:\n  - echo hi\n",
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := util.IsKubeadmInitUserData([]byte(tt.userData)); got != tt.want {
				t.Fatalf("IsKubeadmInitUserData() = %v, want %v", got, tt.want)
			}
		})
	}
}
