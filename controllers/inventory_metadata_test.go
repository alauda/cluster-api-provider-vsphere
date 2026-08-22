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
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/inventory"
)

func TestInventoryMetadataFromObjects(t *testing.T) {
	regionName := inventory.AnnotationPrefix + "region-name"
	regionUID := inventory.AnnotationPrefix + "region-uid"
	displayName := inventory.AnnotationPrefix + "display-name"
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		regionName:                           "cluster-region",
		displayName:                          "cluster-display",
		inventory.AnnotationPrefix + "empty": "",
		"cpaas.io/unrelated":                 "ignored",
	}}}
	vsphereCluster := &infrav1.VSphereCluster{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		regionName:           "infrastructure-region",
		regionUID:            "region-uid",
		"cpaas.io/unrelated": "ignored",
	}}}

	got := inventoryMetadataFromObjects(cluster, vsphereCluster)
	require.Equal(t, map[string]string{
		regionName:  "cluster-region",
		regionUID:   "region-uid",
		displayName: "cluster-display",
	}, got)
}

func TestInventoryMetadataChanged(t *testing.T) {
	regionName := inventory.AnnotationPrefix + "region-name"
	oldCluster := &infrav1.VSphereCluster{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{regionName: "old"}}}
	newCluster := oldCluster.DeepCopy()
	require.False(t, inventoryMetadataChanged(oldCluster, newCluster))
	newCluster.Annotations[regionName] = "new"
	require.True(t, inventoryMetadataChanged(oldCluster, newCluster))

	newCluster = oldCluster.DeepCopy()
	newCluster.Annotations["cpaas.io/unrelated"] = "changed"
	require.False(t, inventoryMetadataChanged(oldCluster, newCluster))
}
