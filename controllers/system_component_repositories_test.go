/*
Copyright 2026 The Kubernetes Authors.

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

package controllers

import "testing"

func TestKubeProxyRepositoryForKubernetesVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
		wantErr bool
	}{
		{name: "Kubernetes 1.34 uses legacy repository", version: "v1.34.9", want: legacySystemImageRepository},
		{name: "Kubernetes 1.35 uses new repository", version: "v1.35.0", want: kubeProxyRepositoryV135},
		{name: "Kubernetes 1.35 prerelease uses new repository", version: "v1.35.0-rc.1", want: kubeProxyRepositoryV135},
		{name: "newer Kubernetes minor uses new repository", version: "v1.36.2", want: kubeProxyRepositoryV135},
		{name: "invalid Kubernetes version fails", version: "not-a-version", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := kubeProxyRepositoryForKubernetesVersion(tt.version)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("kubeProxyRepositoryForKubernetesVersion: %v", err)
			}
			if got != tt.want {
				t.Fatalf("kube-proxy repository = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCoreDNSImageRepositoryForTag(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want string
	}{
		{name: "ACP 4.3 suffix uses legacy repository", tag: "1.14.2-v4.3.11", want: legacySystemImageRepository},
		{name: "ACP 4.4 suffix uses new repository", tag: "1.14.2-v4.4.0", want: coreDNSImageRepositoryV44},
		{name: "ACP 4.4 prerelease suffix uses new repository", tag: "1.14.2-v4.4.0-rc.1", want: coreDNSImageRepositoryV44},
		{name: "newer ACP suffix uses new repository", tag: "1.14.2-v5.0.1", want: coreDNSImageRepositoryV44},
		{name: "tag without ACP suffix uses legacy repository", tag: "1.14.2", want: legacySystemImageRepository},
		{name: "standalone ACP version is not a suffix", tag: "v4.4.0", want: legacySystemImageRepository},
		{name: "invalid ACP suffix uses legacy repository", tag: "1.14.2-vinvalid", want: legacySystemImageRepository},
		{name: "empty tag uses legacy repository", tag: "", want: legacySystemImageRepository},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coreDNSImageRepositoryForTag(tt.tag); got != tt.want {
				t.Fatalf("CoreDNS repository = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestKubeOvnChartNameForVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
		wantErr bool
	}{
		{name: "Kube-OVN 4.3 uses legacy chart", version: "v4.3.9", want: kubeOvnLegacyChartName},
		{name: "Kube-OVN 4.4 uses new chart", version: "v4.4.0", want: kubeOvnChartNameV44},
		{name: "Kube-OVN 4.4 prerelease uses new chart", version: "v4.4.0-rc.1", want: kubeOvnChartNameV44},
		{name: "Kube-OVN 5 uses new chart", version: "v5.0.1", want: kubeOvnChartNameV44},
		{name: "invalid Kube-OVN version fails", version: "invalid", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := kubeOvnChartNameForVersion(tt.version)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("kubeOvnChartNameForVersion: %v", err)
			}
			if got != tt.want {
				t.Fatalf("chart name = %q, want %q", got, tt.want)
			}
		})
	}
}
