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

import (
	"fmt"
	"strings"

	utilversion "k8s.io/apimachinery/pkg/util/version"
)

const (
	legacySystemImageRepository = "tkestack"
	coreDNSImageRepositoryV44   = "acp"
	kubeProxyRepositoryV135     = "acp/k8s"
	kubeOvnLegacyChartName      = "acp/chart-cpaas-kube-ovn"
	kubeOvnChartNameV44         = "acp/chart-kube-ovn"
)

func kubeProxyRepositoryForKubernetesVersion(kubernetesVersion string) (string, error) {
	useNewRepository, err := versionAtLeastMinor(kubernetesVersion, 1, 35)
	if err != nil {
		return "", fmt.Errorf("invalid Kubernetes version %q: %w", kubernetesVersion, err)
	}
	if useNewRepository {
		return kubeProxyRepositoryV135, nil
	}
	return legacySystemImageRepository, nil
}

func coreDNSImageRepositoryForTag(imageTag string) string {
	suffixIndex := strings.LastIndex(imageTag, "-v")
	if suffixIndex < 0 {
		return legacySystemImageRepository
	}

	distributionVersion := imageTag[suffixIndex+1:]
	useNewRepository, err := versionAtLeastMinor(distributionVersion, 4, 4)
	if err != nil || !useNewRepository {
		return legacySystemImageRepository
	}
	return coreDNSImageRepositoryV44
}

func kubeOvnChartNameForVersion(kubeOvnVersion string) (string, error) {
	useNewChart, err := versionAtLeastMinor(kubeOvnVersion, 4, 4)
	if err != nil {
		return "", fmt.Errorf("invalid kube-ovn version %q: %w", kubeOvnVersion, err)
	}
	if useNewChart {
		return kubeOvnChartNameV44, nil
	}
	return kubeOvnLegacyChartName, nil
}

func versionAtLeastMinor(rawVersion string, minimumMajor, minimumMinor uint) (bool, error) {
	parsedVersion, err := utilversion.ParseGeneric(rawVersion)
	if err != nil {
		return false, err
	}
	return parsedVersion.Major() > minimumMajor ||
		(parsedVersion.Major() == minimumMajor && parsedVersion.Minor() >= minimumMinor), nil
}
