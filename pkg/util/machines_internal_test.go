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
	"fmt"
	"testing"

	kubeadmtypes "sigs.k8s.io/cluster-api/bootstrap/kubeadm/types"
)

// TestKubeadmAPIVersionMinKubeVersionMapping validates that every entry in
// kubeadmAPIVersionMinKubeVersion produces the expected kubeadm API
// GroupVersion when passed through the upstream
// KubeVersionToKubeadmAPIGroupVersion function. If the upstream mapping
// changes, this test will fail and remind us to update the local table.
func TestKubeadmAPIVersionMinKubeVersionMapping(t *testing.T) {
	for apiSuffix, minVersion := range kubeadmAPIVersionMinKubeVersion {
		t.Run(apiSuffix, func(t *testing.T) {
			gv, err := kubeadmtypes.KubeVersionToKubeadmAPIGroupVersion(minVersion)
			if err != nil {
				t.Fatalf("KubeVersionToKubeadmAPIGroupVersion(%s) returned error: %v", minVersion, err)
			}
			expected := fmt.Sprintf("kubeadm.k8s.io/%s", apiSuffix)
			actual := gv.String()
			if actual != expected {
				t.Fatalf("kubeadmAPIVersionMinKubeVersion[%q] = %s maps to GroupVersion %s, expected %s — the local mapping table needs updating",
					apiSuffix, minVersion, actual, expected)
			}
		})
	}
}
