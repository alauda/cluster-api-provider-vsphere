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

package govmomi

import (
	"context"
	"strings"

	"github.com/pkg/errors"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/inventory"
)

const virtualMachineManagedObjectType = string(types.ManagedObjectTypesVirtualMachine)

// reconcileInventoryMetadata makes the allowlisted environment metadata
// queryable on the vSphere VM. Custom fields are used instead of dynamically
// creating tags because they retain key/value semantics and do not require a
// tag/category lifecycle for every environment value.
func reconcileInventoryMetadata(ctx context.Context, vmCtx *virtualMachineContext) error {
	// A nil map means this context was not built from an owning Cluster (for
	// example, the deletion fallback path). A non-nil empty map is meaningful:
	// clear provider-owned values that are no longer present in annotations.
	if vmCtx.InventoryMetadata == nil {
		return nil
	}

	manager, err := object.GetCustomFieldsManager(vmCtx.Session.Client.Client)
	if err != nil {
		return errors.Wrap(err, "failed to get vSphere custom fields manager")
	}

	fields, err := manager.Field(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to list vSphere custom field definitions")
	}
	keys := make(map[string]int32, len(vmCtx.InventoryMetadata))
	for _, field := range fields {
		if !isVMInventoryField(field) {
			continue
		}
		keys[field.Name] = field.Key
	}
	for desired := range vmCtx.InventoryMetadata {
		if _, ok := keys[desired]; ok {
			continue
		}
		field, err := ensureCustomField(ctx, manager, fields, desired)
		if err != nil {
			return err
		}
		keys[desired] = field
		fields = append(fields, types.CustomFieldDef{Name: desired, Key: field, ManagedObjectType: virtualMachineManagedObjectType})
	}

	var properties mo.VirtualMachine
	vm := vmCtx.Obj
	if vm == nil {
		vm = object.NewVirtualMachine(vmCtx.Session.Client.Client, vmCtx.Ref)
	}
	if err := vm.Properties(ctx, vmCtx.Ref, []string{"customValue"}, &properties); err != nil {
		return errors.Wrapf(err, "failed to read vSphere custom values on VM %s", vmCtx.VSphereVM.Name)
	}
	currentValues := make(map[int32]string, len(properties.CustomValue))
	for _, value := range properties.CustomValue {
		if stringValue, ok := value.(*types.CustomFieldStringValue); ok {
			currentValues[stringValue.Key] = stringValue.Value
		}
	}

	for name, change := range inventoryMetadataChanges(keys, vmCtx.InventoryMetadata, currentValues) {
		if err := manager.Set(ctx, vmCtx.Ref, change.key, change.value); err != nil {
			return errors.Wrapf(err, "failed to set vSphere custom field %q on VM %s", name, vmCtx.VSphereVM.Name)
		}
	}

	ctrl.LoggerFrom(ctx).V(4).Info("Reconciled vSphere VM inventory metadata", "fields", len(keys))
	return nil
}

type inventoryMetadataChange struct {
	key   int32
	value string
}

func inventoryMetadataChanges(keys map[string]int32, desired map[string]string, current map[int32]string) map[string]inventoryMetadataChange {
	changes := make(map[string]inventoryMetadataChange)
	for name, key := range keys {
		value := desired[name]
		if current[key] != value {
			changes[name] = inventoryMetadataChange{key: key, value: value}
		}
	}
	return changes
}

func isVMInventoryField(field types.CustomFieldDef) bool {
	return strings.HasPrefix(field.Name, inventory.AnnotationPrefix) &&
		(field.ManagedObjectType == "" || field.ManagedObjectType == virtualMachineManagedObjectType)
}

func ensureCustomField(ctx context.Context, manager *object.CustomFieldsManager, fields []types.CustomFieldDef, name string) (int32, error) {
	for _, field := range fields {
		if field.Name == name && isVMInventoryField(field) {
			return field.Key, nil
		}
	}

	field, err := manager.Add(ctx, name, virtualMachineManagedObjectType, nil, nil)
	if err == nil {
		return field.Key, nil
	}

	// Another controller may have created the global definition between Field
	// and Add. Re-read before returning the original error so creation is
	// idempotent under concurrent VM reconciles.
	fields, lookupErr := manager.Field(ctx)
	if lookupErr != nil {
		return 0, errors.Wrapf(err, "failed to create vSphere custom field %q", name)
	}
	for _, existing := range fields {
		if existing.Name == name && isVMInventoryField(existing) {
			return existing.Key, nil
		}
	}
	return 0, errors.Wrapf(err, "failed to create vSphere custom field %q", name)
}
