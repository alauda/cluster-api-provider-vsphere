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
	"path"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"

	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
)

// directoryDeleteTimeout bounds a directory delete so a slow or hung datastore
// operation requeues the slot instead of pinning a reconcile worker.
const directoryDeleteTimeout = 2 * time.Minute

// SlotDiskDirectory returns the datastore directory holding volumePath, and
// whether that directory is the per-slot disk directory clone created for this
// slot. Only a directory this provider named itself may be deleted wholesale,
// so the name is re-derived from the slot identity rather than trusted from the
// observed path.
//
// It reports false for anything else: a disk in a VM home directory (what a
// disk-level storagePolicy without a datastore produces), a VolumePath a user
// set by hand in spec, a nested path, or a disk sitting in a datastore root.
// Those disks are reclaimed one at a time through VirtualDiskManager instead.
func SlotDiskDirectory(volumePath, clusterName, hostname, primaryIP string) (string, bool) {
	var dsPath object.DatastorePath
	if !dsPath.FromString(volumePath) || dsPath.Datastore == "" {
		return "", false
	}
	directory := path.Dir(dsPath.Path)
	if directory == "" || directory == "." || directory == "/" {
		// The disk sits in the datastore root; there is no directory to reclaim.
		return "", false
	}
	if strings.Contains(directory, "/") {
		// Clone only ever creates a single-segment directory. A nested path came
		// from somewhere else and is not ours to delete.
		return "", false
	}
	// Clone reads the cluster name from the VM cluster label, so the name is
	// re-derived the same way. The bare form this yields for an empty cluster
	// name is deliberately not accepted when the pool has one: without a primary
	// IP it collapses to the plain hostname, which is the shape of a VM home
	// folder, and a directory refused here still has its disk reclaimed one at a
	// time through VirtualDiskManager.
	if directory != util.DeterministicDiskDirectoryName(hostname, primaryIP, clusterName) {
		return "", false
	}
	return (&object.DatastorePath{Datastore: dsPath.Datastore, Path: directory}).String(), true
}

// DeleteDatastoreDirectory deletes directory and everything inside it, and waits
// for the delete to finish. FileManager removes a folder recursively, so this
// releases every descriptor in the directory together with its extents, without
// having to guess at vmdk file naming.
//
// Callers must have established that the directory is a slot disk directory (see
// SlotDiskDirectory) and that nothing in it is still attached to a VM. A
// directory that is already gone counts as deleted.
func DeleteDatastoreDirectory(ctx context.Context, s *session.Session, datacenter, directory string) error {
	if s == nil || s.Client == nil {
		return errors.New("vSphere session is required to delete a datastore directory")
	}
	var dsPath object.DatastorePath
	if !dsPath.FromString(directory) {
		return errors.Errorf("invalid datastore directory %q", directory)
	}
	if dsPath.Path == "" || dsPath.Path == "." || dsPath.Path == "/" {
		// Never touch a datastore root.
		return errors.Errorf("refusing to delete datastore root %q", directory)
	}
	dc, err := s.Finder.Datacenter(ctx, datacenter)
	if err != nil {
		return errors.Wrapf(err, "failed to find datacenter %q to delete directory %q", datacenter, directory)
	}

	ctx, cancel := context.WithTimeout(ctx, directoryDeleteTimeout)
	defer cancel()

	// A directory that is already gone counts as deleted, but that has to be
	// settled before the delete rather than read out of its fault:
	// CannotDeleteFile is vCenter's generic delete failure (a privilege the user
	// lacks, a file the host cannot remove), so treating it as "already gone"
	// would report a real failure as a successful reclaim and leak the extents.
	exists, err := datastoreDirectoryExists(ctx, s, dsPath)
	if err != nil {
		return errors.Wrapf(err, "failed to check whether directory %q exists", directory)
	}
	if !exists {
		return nil
	}

	task, err := object.NewFileManager(s.Client.Client).DeleteDatastoreFile(ctx, directory, dc)
	if err != nil {
		return errors.Wrapf(err, "failed to start deletion of directory %q", directory)
	}
	if err := task.Wait(ctx); err != nil {
		return errors.Wrapf(err, "failed to delete directory %q", directory)
	}
	return nil
}

// datastoreDirectoryExists reports whether dsPath still resolves on the
// datastore. Searching a path that is not there fails with FileNotFound; every
// other failure is reported as such rather than being read as "already gone".
func datastoreDirectoryExists(ctx context.Context, s *session.Session, dsPath object.DatastorePath) (bool, error) {
	ds, err := s.Finder.Datastore(ctx, dsPath.Datastore)
	if err != nil {
		return false, errors.Wrapf(err, "failed to find datastore %q", dsPath.Datastore)
	}
	browser, err := ds.Browser(ctx)
	if err != nil {
		return false, errors.Wrapf(err, "failed to browse datastore %q", dsPath.Datastore)
	}
	task, err := browser.SearchDatastore(ctx, dsPath.String(), &types.HostDatastoreBrowserSearchSpec{})
	if err != nil {
		return false, err
	}
	if _, err := task.WaitForResult(ctx); err != nil {
		if types.IsFileNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// DeletePersistentDiskBacking starts deletion of the virtual disk at volumePath
// and returns the vCenter task tracking it. It uses VirtualDiskManager, which
// understands that a virtual disk is a descriptor plus its extents; deleting
// through FileManager would drop the descriptor and leak the extents.
//
// This is the path for disks that do not live in a per-slot directory, where
// deleting the whole directory is not an option.
func DeletePersistentDiskBacking(ctx context.Context, s *session.Session, datacenter, volumePath string) (*object.Task, error) {
	if s == nil || s.Client == nil {
		return nil, errors.New("vSphere session is required to delete persistent disk backing")
	}
	dc, err := s.Finder.Datacenter(ctx, datacenter)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find datacenter %q to delete persistent disk %q", datacenter, volumePath)
	}
	return object.NewVirtualDiskManager(s.Client.Client).DeleteVirtualDisk(ctx, volumePath, dc)
}
