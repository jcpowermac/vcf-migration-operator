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

1. Call `DeleteMachineSetsByVCenter(sourceVC)` (refuses nil/`>0` replicas; NotFound is success)
2. If any were deleted, emit event `SourceWorkersDeleted`
3. Re-list source MachineSets via `GetMachineSetsByVCenter`
4. While any remain, keep `WorkloadMigrated=False` with message `"Deleting source MachineSets"` and requeue after 10s
5. Only when the list is empty, set `ConditionWorkloadMigrated=True` with `"Workload migrated to target vCenter"`

Implemented empty-list confirmation flow:

```text
all machines/nodes gone
→ DeleteMachineSetsByVCenter(sourceVC)
→ if delete API failed → return error
→ event SourceWorkersDeleted when any deleted
→ GetMachineSetsByVCenter(sourceVC)
→ if any remain → WorkloadMigrated=False, requeue 10s
→ set WorkloadMigrated=True
```

Safety: refuse to delete a MachineSet whose `spec.replicas` is nil or `> 0`. Empty `vcenterServer` is rejected so destination MachineSets cannot be matched.

### 3. Tests

- Unit: `DeleteMachineSet` success, NotFound success, other error propagated (`internal/openshift/machines_test.go` with fake client)
- Controller/unit coverage for Step 8: after scaled-to-0 and no machines, source MS objects are deleted; target MS remain

### 4. Docs / status messaging

- While source MachineSets remain after delete, condition message is `"Deleting source MachineSets"` with a 10s requeue
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
