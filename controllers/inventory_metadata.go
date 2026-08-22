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
	"reflect"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/inventory"
)

func inventoryMetadataFromObjects(cluster *clusterv1.Cluster, vsphereCluster *infrav1.VSphereCluster) map[string]string {
	metadata := map[string]string{}
	if vsphereCluster != nil {
		copyInventoryMetadata(metadata, vsphereCluster.Annotations)
	}
	if cluster != nil {
		// The portable CAPI Cluster source takes precedence when both objects
		// provide the same non-empty annotation.
		copyInventoryMetadata(metadata, cluster.Annotations)
	}
	return metadata
}

func copyInventoryMetadata(destination, annotations map[string]string) {
	for key, value := range annotations {
		if strings.HasPrefix(key, inventory.AnnotationPrefix) && value != "" {
			destination[key] = value
		}
	}
}

func inventoryMetadataChanged(oldCluster, newCluster *infrav1.VSphereCluster) bool {
	oldMetadata := map[string]string{}
	newMetadata := map[string]string{}
	copyInventoryMetadata(oldMetadata, oldCluster.Annotations)
	copyInventoryMetadata(newMetadata, newCluster.Annotations)
	return !reflect.DeepEqual(oldMetadata, newMetadata)
}
