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
	"testing"

	. "github.com/onsi/gomega"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
)

func TestPersistentDiskAttachmentsFromVM(t *testing.T) {
	g := NewWithT(t)
	vm := mo.VirtualMachine{
		ManagedEntity: mo.ManagedEntity{
			Name: "attached-vm",
			ExtensibleManagedObject: mo.ExtensibleManagedObject{
				Self: types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-42"},
			},
		},
		Config: &types.VirtualMachineConfigInfo{
			Hardware: types.VirtualHardware{
				Device: []types.BaseVirtualDevice{
					newTestVirtualDisk(2000, "[datastore] old-master/var-lib-etcd.vmdk"),
					newTestVirtualDisk(2001, "[datastore] other/disk.vmdk"),
					&types.VirtualDisk{
						VirtualDevice: types.VirtualDevice{
							Key:     2002,
							Backing: &types.VirtualDiskRawDiskMappingVer1BackingInfo{},
						},
					},
				},
			},
		},
	}

	paths := persistentDiskVolumePaths([]infrav1.PersistentDisk{
		{Name: "empty"},
		{Name: "etcd", VolumePath: "[datastore] old-master/var-lib-etcd.vmdk"},
	})
	attachments := persistentDiskAttachmentsFromVM(vm, paths)

	g.Expect(attachments).To(HaveLen(1))
	g.Expect(attachments[0].VolumePath).To(Equal("[datastore] old-master/var-lib-etcd.vmdk"))
	g.Expect(attachments[0].VMName).To(Equal("attached-vm"))
	g.Expect(attachments[0].VMRef).To(Equal("VirtualMachine:vm-42"))
	g.Expect(attachments[0].DiskKey).To(Equal(int32(2000)))
}

func TestPersistentDiskAttachmentsFromVMNoMatches(t *testing.T) {
	g := NewWithT(t)
	vm := mo.VirtualMachine{
		ManagedEntity: mo.ManagedEntity{Name: "attached-vm"},
		Config: &types.VirtualMachineConfigInfo{
			Hardware: types.VirtualHardware{
				Device: []types.BaseVirtualDevice{newTestVirtualDisk(2000, "[datastore] other/disk.vmdk")},
			},
		},
	}

	paths := persistentDiskVolumePaths([]infrav1.PersistentDisk{{Name: "etcd", VolumePath: "[datastore] old-master/var-lib-etcd.vmdk"}})
	attachments := persistentDiskAttachmentsFromVM(vm, paths)

	g.Expect(attachments).To(BeEmpty())
}

func newTestVirtualDisk(key int32, fileName string) *types.VirtualDisk {
	return &types.VirtualDisk{
		VirtualDevice: types.VirtualDevice{
			Key: key,
			Backing: &types.VirtualDiskFlatVer2BackingInfo{
				VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: fileName},
			},
		},
	}
}
