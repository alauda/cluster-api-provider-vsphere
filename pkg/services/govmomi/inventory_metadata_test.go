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
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/internal/test/helpers/vcsim"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/inventory"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
)

func TestInventoryMetadataChanges(t *testing.T) {
	keys := map[string]int32{
		inventory.AnnotationPrefix + "region-name": 1,
		inventory.AnnotationPrefix + "region-uid":  2,
	}
	current := map[int32]string{1: "region-a", 2: "uid-a"}

	if changes := inventoryMetadataChanges(keys, map[string]string{
		inventory.AnnotationPrefix + "region-name": "region-a",
		inventory.AnnotationPrefix + "region-uid":  "uid-a",
	}, current); len(changes) != 0 {
		t.Fatalf("inventoryMetadataChanges() unchanged values = %#v, want no changes", changes)
	}

	changes := inventoryMetadataChanges(keys, map[string]string{
		inventory.AnnotationPrefix + "region-name": "region-b",
	}, current)
	if len(changes) != 2 {
		t.Fatalf("inventoryMetadataChanges() changed values = %#v, want two changes", changes)
	}
	if changes[inventory.AnnotationPrefix+"region-name"].value != "region-b" {
		t.Errorf("changed value = %q, want %q", changes[inventory.AnnotationPrefix+"region-name"].value, "region-b")
	}
	if changes[inventory.AnnotationPrefix+"region-uid"].value != "" {
		t.Errorf("stale value = %q, want empty", changes[inventory.AnnotationPrefix+"region-uid"].value)
	}

	current[2] = ""
	changes = inventoryMetadataChanges(keys, map[string]string{
		inventory.AnnotationPrefix + "region-name": "region-b",
	}, current)
	if _, ok := changes[inventory.AnnotationPrefix+"region-uid"]; ok {
		t.Errorf("empty stale value produced an unnecessary change: %#v", changes)
	}
}

func TestReconcileInventoryMetadata(t *testing.T) {
	ctx := context.Background()
	model := simulator.VPX()
	model.Host = 0
	simr, err := vcsim.NewBuilder().WithModel(model).Build()
	if err != nil {
		t.Fatalf("unable to create simulator: %v", err)
	}
	defer simr.Destroy()

	authSession, err := session.GetOrCreate(ctx, session.NewParams().
		WithServer(simr.ServerURL().Host).
		WithUserInfo(simr.Username(), simr.Password()).
		WithDatacenter("*"))
	if err != nil {
		t.Fatalf("unable to create vSphere session: %v", err)
	}

	vm := model.Map().Any("VirtualMachine").(*simulator.VirtualMachine)
	vmCtx := &virtualMachineContext{
		VMContext: capvcontext.VMContext{
			VSphereVM: &infrav1.VSphereVM{ObjectMeta: metav1.ObjectMeta{Name: "metadata-vm"}},
			Session:   authSession,
			InventoryMetadata: map[string]string{
				inventory.AnnotationPrefix + "region-name": "region-a",
				inventory.AnnotationPrefix + "region-uid":  "uid-a",
			},
		},
		Ref: vm.Reference(),
	}

	if err := reconcileInventoryMetadata(ctx, vmCtx); err != nil {
		t.Fatalf("reconcileInventoryMetadata() error = %v", err)
	}
	if err := reconcileInventoryMetadata(ctx, vmCtx); err != nil {
		t.Fatalf("second reconcileInventoryMetadata() error = %v", err)
	}

	manager, err := object.GetCustomFieldsManager(authSession.Client.Client)
	if err != nil {
		t.Fatalf("unable to get custom fields manager: %v", err)
	}
	for name := range vmCtx.InventoryMetadata {
		if _, err := manager.FindKey(ctx, name); err != nil {
			t.Errorf("custom field %q was not created: %v", name, err)
		}
	}

	var properties mo.VirtualMachine
	if err := object.NewVirtualMachine(authSession.Client.Client, vm.Reference()).Properties(ctx, vm.Reference(), []string{"customValue"}, &properties); err != nil {
		t.Fatalf("unable to read VM custom values: %v", err)
	}
	values := make(map[int32]string)
	for _, value := range properties.CustomValue {
		if stringValue, ok := value.(*types.CustomFieldStringValue); ok {
			values[stringValue.Key] = stringValue.Value
		}
	}
	for name, expected := range vmCtx.InventoryMetadata {
		key, err := manager.FindKey(ctx, name)
		if err != nil {
			t.Fatalf("unable to find custom field %q: %v", name, err)
		}
		if values[key] != expected {
			t.Errorf("custom field %q = %q, want %q", name, values[key], expected)
		}
	}

	changedName := inventory.AnnotationPrefix + "region-name"
	vmCtx.InventoryMetadata[changedName] = "region-b"
	if err := reconcileInventoryMetadata(ctx, vmCtx); err != nil {
		t.Fatalf("reconcileInventoryMetadata() after update error = %v", err)
	}
	properties = mo.VirtualMachine{}
	if err := object.NewVirtualMachine(authSession.Client.Client, vm.Reference()).Properties(ctx, vm.Reference(), []string{"customValue"}, &properties); err != nil {
		t.Fatalf("unable to read VM custom values after update: %v", err)
	}
	changedKey, err := manager.FindKey(ctx, changedName)
	if err != nil {
		t.Fatalf("unable to find changed custom field %q: %v", changedName, err)
	}
	values = make(map[int32]string)
	for _, value := range properties.CustomValue {
		if stringValue, ok := value.(*types.CustomFieldStringValue); ok {
			values[stringValue.Key] = stringValue.Value
		}
	}
	if values[changedKey] != "region-b" {
		t.Errorf("changed custom field %q = %q, want %q", changedName, values[changedKey], "region-b")
	}

	removedName := inventory.AnnotationPrefix + "region-uid"
	delete(vmCtx.InventoryMetadata, removedName)
	if err := reconcileInventoryMetadata(ctx, vmCtx); err != nil {
		t.Fatalf("reconcileInventoryMetadata() after removal error = %v", err)
	}
	properties = mo.VirtualMachine{}
	if err := object.NewVirtualMachine(authSession.Client.Client, vm.Reference()).Properties(ctx, vm.Reference(), []string{"customValue"}, &properties); err != nil {
		t.Fatalf("unable to read VM custom values after removal: %v", err)
	}
	removedKey, err := manager.FindKey(ctx, removedName)
	if err != nil {
		t.Fatalf("unable to find removed custom field %q: %v", removedName, err)
	}
	for _, value := range properties.CustomValue {
		if stringValue, ok := value.(*types.CustomFieldStringValue); ok && stringValue.Key == removedKey && stringValue.Value != "" {
			t.Errorf("removed custom field %q = %q, want empty", removedName, stringValue.Value)
		}
	}
}
