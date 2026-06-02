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

	"github.com/pkg/errors"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/services"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
)

// FindAttachedPersistentDisks returns persistent disk backings still attached to vCenter VMs in the resolved slot datacenter.
func (vms *VMService) FindAttachedPersistentDisks(ctx context.Context, vmCtx *capvcontext.VMContext, datacenter string, disks []infrav1.PersistentDisk) ([]services.PersistentDiskAttachment, error) {
	if vmCtx == nil || vmCtx.Session == nil {
		return nil, errors.New("vSphere session is required to find persistent disk attachments")
	}
	return FindAttachedPersistentDisks(ctx, vmCtx.Session, datacenter, disks)
}

// FindAttachedPersistentDisks scans visible vCenter VMs in datacenter for exact persistent disk backing matches.
func FindAttachedPersistentDisks(ctx context.Context, s *session.Session, datacenter string, disks []infrav1.PersistentDisk) ([]services.PersistentDiskAttachment, error) {
	volumePaths := persistentDiskVolumePaths(disks)
	if len(volumePaths) == 0 {
		return nil, nil
	}
	if s == nil || s.Client == nil {
		return nil, errors.New("vSphere session is required to find persistent disk attachments")
	}

	if datacenter == "" {
		return nil, errors.New("datacenter is required for persistent disk attachment scan")
	}
	dc, err := s.Finder.Datacenter(ctx, datacenter)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find datacenter %q for persistent disk attachment scan", datacenter)
	}
	folders, err := dc.Folders(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to find datacenter VM folder for persistent disk attachment scan")
	}

	viewManager := view.NewManager(s.Client.Client)
	vmView, err := viewManager.CreateContainerView(ctx, folders.VmFolder.Reference(), []string{"VirtualMachine"}, true)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create virtual machine view")
	}
	defer vmView.Destroy(ctx) //nolint:errcheck // best-effort cleanup of inventory view.

	var vms []mo.VirtualMachine
	if err := vmView.Retrieve(ctx, []string{"VirtualMachine"}, []string{"name", "config.hardware.device"}, &vms); err != nil {
		return nil, errors.Wrap(err, "failed to retrieve virtual machine disk devices")
	}

	attachments := []services.PersistentDiskAttachment{}
	for i := range vms {
		attachments = append(attachments, persistentDiskAttachmentsFromVM(vms[i], volumePaths)...)
	}
	return attachments, nil
}

func persistentDiskVolumePaths(disks []infrav1.PersistentDisk) map[string]struct{} {
	volumePaths := map[string]struct{}{}
	for i := range disks {
		if disks[i].VolumePath == "" {
			continue
		}
		volumePaths[disks[i].VolumePath] = struct{}{}
	}
	return volumePaths
}

func persistentDiskAttachmentsFromVM(vm mo.VirtualMachine, volumePaths map[string]struct{}) []services.PersistentDiskAttachment {
	if vm.Config == nil || vm.Config.Hardware.Device == nil {
		return nil
	}

	attachments := []services.PersistentDiskAttachment{}
	for _, device := range vm.Config.Hardware.Device {
		disk, ok := device.(*types.VirtualDisk)
		if !ok {
			continue
		}
		volumePath := diskBackingFileName(disk)
		if volumePath == "" {
			continue
		}
		if _, ok := volumePaths[volumePath]; !ok {
			continue
		}
		attachments = append(attachments, services.PersistentDiskAttachment{
			VolumePath: volumePath,
			VMName:     vm.Name,
			VMRef:      vm.Reference().String(),
			DiskKey:    disk.Key,
		})
	}
	return attachments
}
