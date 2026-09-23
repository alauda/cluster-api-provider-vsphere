/*
Copyright 2021 The Kubernetes Authors.

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

package webhooks

import (
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

// AggregateObjErrors aggregates a list of field errors into a single Invalid API error.
func AggregateObjErrors(gk schema.GroupKind, name string, allErrs field.ErrorList) error {
	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		gk,
		name,
		allErrs,
	)
}

// isVSphereMachineTemplateRef reports whether ref points at a govmomi VSphereMachineTemplate.
//
// KubeadmControlPlane and MachineDeployment are provider-agnostic Cluster API objects, so these
// webhooks see every one of them in the cluster, including those of clusters backed by another
// infrastructure provider. The machine config pool rules only apply to our own machine templates:
// anything else must be left alone rather than resolved by name, which would either reject a
// perfectly valid object or silently read the pool reference off an unrelated same-named template.
// The group is compared as well as the kind, because the supervisor API shares the kind name.
func isVSphereMachineTemplateRef(ref *corev1.ObjectReference) bool {
	if ref == nil || ref.Kind != "VSphereMachineTemplate" {
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return false
	}
	return gv.Group == infrav1.GroupVersion.Group
}
