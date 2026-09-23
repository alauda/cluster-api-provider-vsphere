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
	gocontext "context"
	"crypto/tls"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"

	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

const (
	testDatacenter  = "DC0"
	testCluster     = "cl1"
	testHostname    = "node-a"
	testPrimaryIP   = "10.0.0.5"
	testSlotDirName = testCluster + "-" + testHostname + "-" + testPrimaryIP
	testSlotDir     = "[LocalDS_0] " + testSlotDirName
)

func TestSlotDiskDirectory(t *testing.T) {
	g := NewWithT(t)

	// The directory this package would have created is the only one it may delete.
	g.Expect(util.DeterministicDiskDirectoryName(testHostname, testPrimaryIP, testCluster)).To(Equal(testSlotDirName),
		"guard must stay in step with the path writer")

	directory, ok := SlotDiskDirectory(testSlotDir+"/data.vmdk", testCluster, testHostname, testPrimaryIP)
	g.Expect(ok).To(BeTrue())
	g.Expect(directory).To(Equal(testSlotDir))

	// Clone reads the cluster name from a VM label that can be empty, so the
	// unprefixed form it then produces is ours too.
	directory, ok = SlotDiskDirectory("[LocalDS_0] "+testHostname+"-"+testPrimaryIP+"/data.vmdk", "", testHostname, testPrimaryIP)
	g.Expect(ok).To(BeTrue())
	g.Expect(directory).To(Equal("[LocalDS_0] " + testHostname + "-" + testPrimaryIP))

	// A slot with no address still has a directory of its own.
	_, ok = SlotDiskDirectory("[LocalDS_0] "+testCluster+"-"+testHostname+"/data.vmdk", testCluster, testHostname, "")
	g.Expect(ok).To(BeTrue())

	// A CIDR-formatted address normalizes to the bare address clone used.
	_, ok = SlotDiskDirectory(testSlotDir+"/data.vmdk", testCluster, testHostname, testPrimaryIP+"/24")
	g.Expect(ok).To(BeTrue())

	for name, volumePath := range map[string]string{
		"VM home directory":     "[LocalDS_0] some-vm/data.vmdk",
		"another slot":          "[LocalDS_0] " + testCluster + "-node-b-10.0.0.6/data.vmdk",
		"another cluster":       "[LocalDS_0] cl2-" + testHostname + "-" + testPrimaryIP + "/data.vmdk",
		"nested path":           "[LocalDS_0] disks/" + testSlotDirName + "/data.vmdk",
		"datastore root":        "[LocalDS_0] data.vmdk",
		"not a datastore path":  "/vmfs/volumes/LocalDS_0/data.vmdk",
		"empty":                 "",
		"no datastore":          "[] " + testSlotDirName + "/data.vmdk",
		"directory is the disk": testSlotDir,
	} {
		_, ok := SlotDiskDirectory(volumePath, testCluster, testHostname, testPrimaryIP)
		g.Expect(ok).To(BeFalse(), "must not claim %s (%q) as a slot disk directory", name, volumePath)
	}
}

// TestDeleteDatastoreDirectoryReclaimsEverything pins the bug this code exists to
// fix. Deleting a virtual disk through FileManager removes only the descriptor
// and leaves the full-size extent behind, where VirtualDiskManager can no longer
// reach it because the descriptor that named the extent is gone. Deleting the
// slot's directory releases the live disk, that orphan, and the directory, with
// no dependency on how vSphere happens to name extent files.
func TestDeleteDatastoreDirectoryReclaimsEverything(t *testing.T) {
	g := NewWithT(t)
	ctx := gocontext.Background()
	s, dc := newReclaimTestSession(t)

	// A disk released by the old descriptor-only delete: the extent survives.
	orphanPath := testSlotDir + "/orphan.vmdk"
	createTestDisk(t, s, dc, orphanPath)
	task, err := object.NewFileManager(s.Client.Client).DeleteDatastoreFile(ctx, orphanPath, dc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(task.Wait(ctx)).To(Succeed())
	expectDatastoreFile(t, s, testSlotDirName+"/orphan-flat.vmdk", true)

	// A disk still fully present.
	volumePath := testSlotDir + "/data.vmdk"
	createTestDisk(t, s, dc, volumePath)
	expectDatastoreFile(t, s, testSlotDirName+"/data-flat.vmdk", true)

	g.Expect(DeleteDatastoreDirectory(ctx, s, testDatacenter, testSlotDir)).To(Succeed())

	expectDatastoreFile(t, s, testSlotDirName+"/orphan-flat.vmdk", false)
	expectDatastoreFile(t, s, testSlotDirName+"/data.vmdk", false)
	expectDatastoreFile(t, s, testSlotDirName+"/data-flat.vmdk", false)
	expectDatastoreFile(t, s, testSlotDirName, false)

	// A retried reclaim converges instead of wedging on the missing directory.
	g.Expect(DeleteDatastoreDirectory(ctx, s, testDatacenter, testSlotDir)).To(Succeed())
}

func TestDeleteDatastoreDirectoryRefusesDatastoreRoot(t *testing.T) {
	g := NewWithT(t)
	ctx := gocontext.Background()
	s, _ := newReclaimTestSession(t)

	for _, directory := range []string{"[LocalDS_0]", "[LocalDS_0] ", "[LocalDS_0] .", "[LocalDS_0] /"} {
		g.Expect(DeleteDatastoreDirectory(ctx, s, testDatacenter, directory)).NotTo(Succeed(), "must refuse %q", directory)
	}
	// The directory that does exist is untouched by the refusals above.
	expectDatastoreFile(t, s, testSlotDirName, true)
}

func TestDeletePersistentDiskBackingRemovesExtents(t *testing.T) {
	g := NewWithT(t)
	ctx := gocontext.Background()
	s, dc := newReclaimTestSession(t)

	volumePath := testSlotDir + "/data.vmdk"
	createTestDisk(t, s, dc, volumePath)

	task, err := DeletePersistentDiskBacking(ctx, s, testDatacenter, volumePath)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(task.Wait(ctx)).To(Succeed())

	expectDatastoreFile(t, s, testSlotDirName+"/data.vmdk", false)
	expectDatastoreFile(t, s, testSlotDirName+"/data-flat.vmdk", false, "descriptor and extent must go together")
}

func newReclaimTestSession(t *testing.T) (*session.Session, *object.Datacenter) {
	t.Helper()
	g := NewWithT(t)
	ctx := gocontext.Background()

	model := simulator.VPX()
	model.Host = 0
	g.Expect(model.Create()).To(Succeed())
	t.Cleanup(model.Remove)
	model.Service.TLS = new(tls.Config)
	model.Service.RegisterEndpoints = true

	server := model.Service.NewServer()
	t.Cleanup(server.Close)
	pass, _ := server.URL.User.Password()

	s, err := session.GetOrCreate(ctx, session.NewParams().
		WithServer(server.URL.Host).
		WithUserInfo(server.URL.User.Username(), pass).
		WithDatacenter(testDatacenter))
	g.Expect(err).NotTo(HaveOccurred())

	dc, err := s.Finder.Datacenter(ctx, testDatacenter)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(object.NewFileManager(s.Client.Client).MakeDirectory(ctx, testSlotDir, dc, true)).To(Succeed())

	return s, dc
}

func createTestDisk(t *testing.T, s *session.Session, dc *object.Datacenter, volumePath string) {
	t.Helper()
	g := NewWithT(t)
	ctx := gocontext.Background()

	task, err := object.NewVirtualDiskManager(s.Client.Client).CreateVirtualDisk(ctx, volumePath, dc, &types.FileBackedVirtualDiskSpec{
		VirtualDiskSpec: types.VirtualDiskSpec{AdapterType: "lsiLogic", DiskType: "thick"},
		CapacityKb:      1024,
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(task.Wait(ctx)).To(Succeed())
}

// expectDatastoreFile asserts whether path (relative to LocalDS_0) exists.
func expectDatastoreFile(t *testing.T, s *session.Session, path string, want bool, description ...any) {
	t.Helper()
	g := NewWithT(t)
	ctx := gocontext.Background()

	datastore, err := s.Finder.Datastore(ctx, "LocalDS_0")
	g.Expect(err).NotTo(HaveOccurred())

	_, err = datastore.Stat(ctx, path)
	if want {
		g.Expect(err).NotTo(HaveOccurred(), append([]any{"expected %q to exist"}, path)...)
		return
	}
	if len(description) == 0 {
		description = []any{"expected %q to be gone", path}
	}
	g.Expect(err).To(HaveOccurred(), description...)
}
