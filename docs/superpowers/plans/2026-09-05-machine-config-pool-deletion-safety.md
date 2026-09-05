# MachineConfigPool Deletion Safety Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent VSphereCluster deletion while related VSphereMachineConfigPools exist, and preserve pool finalizers until machines and persistent-disk backings are safely gone.

**Architecture:** Reuse the existing pool `spec.clusterRef` association and per-disk reclaim state. Add a small cluster-reconciler helper that lists associated pools before existing cluster cleanup; keep pool reclaim ordering in the existing pool controller and strengthen it with focused regression tests.

**Tech Stack:** Go, controller-runtime fake client, Gomega, govmomi-backed service interfaces.

---

### Task 1: Lock pool deletion safety with regression tests

**Files:**
- Modify: `controllers/vspheremachineconfigpool_controller_test.go`
- Inspect/modify: `controllers/vspheremachineconfigpool_controller.go:845-961`

- [x] **Step 1: Write the failing test for an attached persistent disk**

Add a test exercising `reconcileDelete` with a released slot and a persistent disk whose `VolumePath` is still attached. The test must use the existing fake vCenter/session setup or the smallest injectable seam available, assert that the pool finalizer remains, and assert that the reconcile returns a retry result or error rather than claiming deletion is complete.

- [x] **Step 2: Run the focused test and verify the failure**

Run:

```bash
go test ./controllers -run 'TestMachineConfigPoolReconcileDelete.*Attached' -count=1
```

Expected: the new test fails because the current test seam does not yet prove the required deletion gate, or because the implementation removes the finalizer without the expected attachment behavior.

- [x] **Step 3: Write the smallest implementation adjustment**

Keep the existing order in `reconcileDelete`: resolve vCenter whenever any slot has reclaimable backing, check all live `VSphereMachine` references, call `reclaimSlotDisks` for every slot without a live machine (including stale `Available` slots), and remove `MachineConfigPoolFinalizer` only when no requeue is requested and no blocking machine remains. Do not change normal slot allocation.

- [x] **Step 4: Run pool deletion tests**

Run:

```bash
go test ./controllers -run 'TestMachineConfigPoolReconcileDelete|TestVmReconciler_Delete' -count=1
```

Expected: PASS.

### Task 2: Add VSphereCluster deletion blocking for related pools

**Files:**
- Modify: `controllers/vspherecluster_reconciler.go:223-299`
- Modify: `controllers/vspherecluster_reconciler_test.go`

- [x] **Step 1: Write failing tests**

Add focused unit tests for a helper or deletion path covering:

```go
// related pool exists, including a pool with DeletionTimestamp: block cluster deletion
// pool list returns an error: return the error and keep the cluster finalizer
// no related pool: continue to existing cluster cleanup
```

Use a fake client with `Cluster`, `VSphereCluster`, and `VSphereMachineConfigPool` objects. Match pools by namespace and `pool.Spec.ClusterRef.Name` (and namespace when set) against `clusterCtx.Cluster`. Assert the result is requeued or errors, and that `ClusterFinalizer` is still present while a pool exists.

- [x] **Step 2: Run the new tests and verify they fail**

Run:

```bash
go test ./controllers -run 'TestClusterReconciler_.*MachineConfigPool|TestClusterReconciler_ReconcileDelete' -count=1
```

Expected: FAIL because the current `reconcileDelete` proceeds after the `VSphereMachine` check without listing machine config pools.

- [x] **Step 3: Implement the pool deletion gate**

Add a helper near the existing `computeAvailableDatacenters` / mapping helpers:

```go
func (r *clusterReconciler) getMachineConfigPoolsForCluster(ctx context.Context, cluster *clusterv1.Cluster) ([]infrav1.VSphereMachineConfigPool, error)
```

The helper lists pools in the cluster namespace, filters exact `ClusterRef` matches, and returns list errors unchanged (wrapped with context). Call it in `reconcileDelete` after the existing VSphereMachine check and before cluster-module or Secret cleanup. If the result is non-empty, log the pool names and return `reconcile.Result{RequeueAfter: 10 * time.Second}` without removing `ClusterFinalizer`. Treat pools with `DeletionTimestamp` as existing until the API list no longer returns them.

- [x] **Step 4: Run focused and package tests**

Run:

```bash
go test ./controllers -run 'TestClusterReconciler_.*MachineConfigPool|TestClusterReconciler_ReconcileDelete|TestMachineConfigPoolReconcileDelete' -count=1
go test ./controllers -count=1
```

Expected: PASS with no new failures.

- [x] **Step 5: Verify the final diff**

Run:

```bash
gofmt -w controllers/vspherecluster_reconciler.go controllers/vspherecluster_reconciler_test.go controllers/vspheremachineconfigpool_controller_test.go
git diff --check
git status --short
```

Expected: only the intended controller/test files and the existing proposal/plan documents are changed.
