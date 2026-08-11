# Remove Source Cluster MachineSets After Migration

**Jira:** [SPLAT-2877](https://issues.redhat.com/browse/SPLAT-2877) (Epic [SPLAT-2644](https://issues.redhat.com/browse/SPLAT-2644))

## Overview

After worker cutover, the operator scales source-vCenter MachineSets to `0` and waits for Machines/Nodes to disappear, but leaves the empty MachineSet objects in `openshift-machine-api`. Those stale objects should be deleted once machines and nodes are gone, before `WorkloadMigrated` becomes True.

This closes the gap against SPLAT-2655 acceptance (“scaled down and removed”) and keeps source cleanup focused on vCenter configuration removal.

## Current Behavior

In `ensureWorkloadMigratedRolloutAndScaleDown`:

1. Wait for CPMS generation observed + control plane rollout complete
2. Scale source MachineSets (`GetMachineSetsByVCenter(sourceVC)`) to `0`
3. Wait until Machines and Nodes for those MachineSets are deleted
4. Mark `ConditionWorkloadMigrated=True`

There is no `DeleteMachineSet` helper today (`internal/openshift/machines.go` only has get/list/create/scale).

## Design Decisions

1. Delete source MachineSets as **Step 8** of workload migration — after scale-down and machine/node deletion, before marking `WorkloadMigrated`
2. Identify candidates the same way as scale-down: `GetMachineSetsByVCenter(ctx, sourceVC.Server)`
3. Deletion is idempotent: treat `NotFound` as success
4. Do **not** delete target MachineSets (those use destination failure-domain servers)
5. Do **not** delete CPMS; control-plane cutover already updated it in place
6. Record a Normal event when any source MachineSet is deleted
7. Fail the reconcile (surface via condition) if delete fails for a non-NotFound reason

## Implementation

### 1. `MachineManager` helpers (`internal/openshift/machines.go`)

```go
// DeleteMachineSet deletes the named MachineSet from openshift-machine-api.
// NotFound is treated as success (idempotent).
func (m *MachineManager) DeleteMachineSet(ctx context.Context, name string) error

// DeleteMachineSetsByVCenter deletes zero-replica MachineSets for a vCenter.
// Refuses MachineSets with nil or positive replicas.
func (m *MachineManager) DeleteMachineSetsByVCenter(ctx context.Context, vcenterServer string) ([]string, error)
```

- Call typed client `MachineSets(MachineAPINamespace).Delete`
- Wrap errors with gerund context: `deleting machineset %q: %w`
- Log at `V(2)` on delete attempt / success

### 2. Controller Step 8 (`ensureWorkloadMigratedRolloutAndScaleDown`)

After Step 7 (`allDeleted == true`), before setting `WorkloadMigrated=True`:

1. Re-list source MachineSets via `GetMachineSetsByVCenter`
2. For each remaining MS, call `DeleteMachineSet`
3. If any were present, set progressing message `"Deleting source MachineSets"` and event `SourceWorkersDeleted`
4. Requeue briefly (10s) if deletes were issued, then confirm list is empty on next pass (or confirm NotFound per name in the same reconcile — prefer same-reconcile delete + empty check)
5. Only then set `ConditionWorkloadMigrated=True` with `"Workload migrated to target vCenter"`

Recommended same-reconcile flow (implemented):

```text
all machines/nodes gone
→ DeleteMachineSetsByVCenter(sourceVC) (refuses replicas > 0; ignore NotFound)
→ if delete API failed → return error
→ event SourceWorkersDeleted when any deleted
→ set WorkloadMigrated=True
```

Safety: refuse to delete a MachineSet whose `spec.replicas` is nil or `> 0`.

### 3. Tests

- Unit: `DeleteMachineSet` success, NotFound success, other error propagated (`internal/openshift/machines_test.go` with fake client)
- Controller/unit coverage for Step 8: after scaled-to-0 and no machines, source MS objects are deleted; target MS remain

### 4. Docs / status messaging

- Progress message may briefly show `"Deleting source MachineSets"` if we requeue; otherwise completion message stays unchanged
- Event: `SourceWorkersDeleted` — `"Source worker MachineSets deleted"`

## Out of Scope

- Deleting MachineAutoscalers / MachineHealthChecks that referenced source MachineSets
- Deleting destination MachineSets or CPMS
- Removing source failure domains from Infrastructure (already `ensureSourceCleaned`)
- Cluster Autoscaler policy changes

## Test Plan

```bash
go test ./internal/openshift/ -run 'TestDeleteMachineSet' -v
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" \
  go test ./internal/controller/ -v -ginkgo.focus="source MachineSet"
```

Manual:

1. Run migration to `WorkloadMigrated=True`
2. Confirm `oc get machineset -n openshift-machine-api` shows only destination MachineSets
3. Confirm no Machines remain for deleted source MachineSet names
