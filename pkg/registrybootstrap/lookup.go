// Copyright 2026 Alauda. Licensed under the Apache License, Version 2.0.

package registrybootstrap

import (
	"context"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

// LoadForVSphereVM resolves the owning CAPI Cluster from the VM's standard
// cluster label and loads its versioned bootstrap snapshot.
func LoadForVSphereVM(ctx context.Context, c client.Client, vm *infrav1.VSphereVM) (Config, error) {
	if vm == nil || c == nil {
		return Config{}, nil
	}
	clusterName := vm.Labels[clusterv1.ClusterNameLabel]
	if clusterName == "" {
		return Config{}, nil
	}
	cluster := &clusterv1.Cluster{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: clusterName}, cluster); err != nil {
		return Config{}, err
	}
	return LoadForCluster(ctx, c, cluster)
}
