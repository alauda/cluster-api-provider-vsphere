/*
Copyright 2025.

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

package v1beta1

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPersistentDiskStatusHelpers(t *testing.T) {
	t.Run("upsert inserts then updates in place, keyed by hostname+name", func(t *testing.T) {
		g := NewWithT(t)
		pool := &VSphereMachineConfigPool{}

		UpsertDiskStatus(pool, PersistentDiskStatus{Hostname: "h1", Name: "d1", Phase: PersistentDiskPhaseCreating})
		UpsertDiskStatus(pool, PersistentDiskStatus{Hostname: "h1", Name: "d2", Phase: PersistentDiskPhaseCreating})
		g.Expect(pool.Status.PersistentDiskStatuses).To(HaveLen(2))

		UpsertDiskStatus(pool, PersistentDiskStatus{Hostname: "h1", Name: "d1", Phase: PersistentDiskPhaseAttached, VolumePath: "[ds] d1.vmdk"})
		g.Expect(pool.Status.PersistentDiskStatuses).To(HaveLen(2))
		rec, idx := FindDiskStatus(pool, "h1", "d1")
		g.Expect(idx).To(Equal(0))
		g.Expect(rec.Phase).To(Equal(PersistentDiskPhaseAttached))
		g.Expect(rec.VolumePath).To(Equal("[ds] d1.vmdk"))
	})

	t.Run("LastTransitionTime refreshes only on phase change", func(t *testing.T) {
		g := NewWithT(t)
		pool := &VSphereMachineConfigPool{}
		old := metav1.NewTime(time.Now().Add(-time.Hour))
		UpsertDiskStatus(pool, PersistentDiskStatus{Hostname: "h1", Name: "d1", Phase: PersistentDiskPhaseAttached, LastTransitionTime: old})

		// Same phase, different observed field: timestamp preserved.
		UpsertDiskStatus(pool, PersistentDiskStatus{Hostname: "h1", Name: "d1", Phase: PersistentDiskPhaseAttached, VolumePath: "[ds] d1.vmdk"})
		rec, _ := FindDiskStatus(pool, "h1", "d1")
		g.Expect(rec.LastTransitionTime.Time).To(BeTemporally("==", old.Time))

		// Phase change: timestamp advances.
		UpsertDiskStatus(pool, PersistentDiskStatus{Hostname: "h1", Name: "d1", Phase: PersistentDiskPhaseReclaiming})
		rec, _ = FindDiskStatus(pool, "h1", "d1")
		g.Expect(rec.LastTransitionTime.Time).To(BeTemporally(">", old.Time))
	})

	t.Run("remove drops the entry", func(t *testing.T) {
		g := NewWithT(t)
		pool := &VSphereMachineConfigPool{}
		UpsertDiskStatus(pool, PersistentDiskStatus{Hostname: "h1", Name: "d1", Phase: PersistentDiskPhaseAttached})
		UpsertDiskStatus(pool, PersistentDiskStatus{Hostname: "h1", Name: "d2", Phase: PersistentDiskPhaseAttached})
		RemoveDiskStatus(pool, "h1", "d1")
		g.Expect(pool.Status.PersistentDiskStatuses).To(HaveLen(1))
		rec, _ := FindDiskStatus(pool, "h1", "d1")
		g.Expect(rec).To(BeNil())
		remaining, _ := FindDiskStatus(pool, "h1", "d2")
		g.Expect(remaining).NotTo(BeNil())
	})
}
