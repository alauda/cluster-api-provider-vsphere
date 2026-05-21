/*
Copyright The Kubernetes Authors.

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

package ovn

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const nbStatus = `88e1
Name: OVN_Northbound
Cluster ID: fcf0 (fcf07c8f-1dc4-4871-9d6a-7f3e9bdac04d)
Server ID: 88e1 (88e1bd75-ff2f-4797-8811-6fae7eda9312)
Address: tcp:[192.168.129.242]:6643
Status: cluster member
Role: leader
Servers:
    88e1 (88e1 at tcp:[192.168.129.242]:6643) (self) next_index=956 match_index=1078
`

const sbStatusWithSecondMember = `e3e4
Name: OVN_Southbound
Address: tcp:[192.168.129.242]:6644
Servers:
    e3e4 (e3e4 at tcp:[192.168.129.242]:6644) (self) next_index=916 match_index=1038
    abcd (abcd at tcp:[192.168.129.39]:6644) next_index=916 match_index=1038
`

func TestFindMatchingRaftMemberIP(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		statuses   []string
		want       string
		wantErr    bool
	}{
		{
			name:       "single exact match",
			candidates: []string{"192.168.129.242", "192.168.164.13"},
			statuses:   []string{nbStatus},
			want:       "192.168.129.242",
		},
		{
			name:       "external only reported IP matches",
			candidates: []string{"192.168.129.242"},
			statuses:   []string{nbStatus},
			want:       "192.168.129.242",
		},
		{
			name:       "candidate order does not matter",
			candidates: []string{"192.168.164.13", "192.168.129.242"},
			statuses:   []string{nbStatus},
			want:       "192.168.129.242",
		},
		{
			name:       "no substring false positive",
			candidates: []string{"10.0.0.1"},
			statuses:   []string{"Servers:\n    abcd (abcd at tcp:[10.0.0.10]:6643)"},
			want:       "",
		},
		{
			name:       "invalid candidates ignored",
			candidates: []string{"", "node-1", "192.168.129.242"},
			statuses:   []string{nbStatus},
			want:       "192.168.129.242",
		},
		{
			name:       "multiple matches are ambiguous",
			candidates: []string{"192.168.129.242", "192.168.129.39"},
			statuses:   []string{nbStatus, sbStatusWithSecondMember},
			wantErr:    true,
		},
		{
			name:       "no matching raft member",
			candidates: []string{"192.168.164.13"},
			statuses:   []string{nbStatus},
			want:       "",
		},
		{
			name:       "address header alone is not a member match",
			candidates: []string{"192.168.129.242"},
			statuses:   []string{"Address: tcp:[192.168.129.242]:6643\nStatus: cluster member\nServers:\n"},
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := findMatchingRaftMemberIP(tt.candidates, tt.statuses...)

			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}

func TestSelectPodsExcludesCandidateHostIPs(t *testing.T) {
	g := NewWithT(t)
	clientset := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ovn-central-deleting-node",
				Namespace: CentralNamespace,
				Labels:    map[string]string{"app": "ovn-central"},
			},
			Status: corev1.PodStatus{
				Phase:  corev1.PodRunning,
				HostIP: "192.168.137.39",
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "ovn-central",
					Ready: true,
				}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ovn-central-healthy-node",
				Namespace: CentralNamespace,
				Labels:    map[string]string{"app": "ovn-central"},
			},
			Status: corev1.PodStatus{
				Phase:  corev1.PodRunning,
				HostIP: "192.168.142.106",
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "ovn-central",
					Ready: true,
				}},
			},
		},
	)

	targets, err := selectPods(context.Background(), clientset, CentralNamespace, CentralSelector, []string{"192.168.137.39"})

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(targets).To(Equal([]podExecTarget{{podName: "ovn-central-healthy-node", containerName: "ovn-central"}}))
}

func TestServerIDForMemberIP(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		memberIP string
		want     string
		wantErr  bool
	}{
		{
			name:     "resolves server ID for exact IP",
			status:   nbStatus,
			memberIP: "192.168.129.242",
			want:     "88e1",
		},
		{
			name:     "does not match Address header",
			status:   "Address: tcp:[192.168.129.242]:6643\nServers:\n",
			memberIP: "192.168.129.242",
			want:     "",
		},
		{
			name:     "does not match non Servers fields",
			status:   "Leader: 192.168.129.242\nServers:\n",
			memberIP: "192.168.129.242",
			want:     "",
		},
		{
			name:     "does not match substring IP",
			status:   "Servers:\n    abcd (abcd at tcp:[10.0.0.10]:6643)",
			memberIP: "10.0.0.1",
			want:     "",
		},
		{
			name:     "multiple matching server IDs errors",
			status:   "Servers:\n    abcd (abcd at tcp:[10.0.0.10]:6643)\n    efgh (efgh at tcp:[10.0.0.10]:6644)",
			memberIP: "10.0.0.10",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := serverIDForMemberIP(tt.status, tt.memberIP)

			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}
