# Node Migration API (SPLAT-2949)

**Kind:** `VmwareCloudFoundationMigration` · **Group/Version:** `migration.openshift.io/v1alpha1`

This API retains #105's per-role progress and per-node map list, adding bounded
execution identity, action outcomes and recovery-request data to that structure.
Counts and summary strings describe observations; they are not authority to
replay an operation.

> **API foundation only.** The controller does not implement the VMotion
> execution contract. Schema support does not enable migration execution or
> establish runtime ownership, health, action safety, task outcome, or recovery
> authorization. Do not use an explicit VMotion selection as evidence that the
> backend supports it. Runtime dispatch, durable action claims, status merging,
> recovery enforcement and qualification remain follow-up work.

## Selection and validation

When `spec.nodeMigration` is omitted, the existing legacy MachineSet/CPMS
replacement selection and template requirement are retained. When present, both
roles are required and must select the same engine. `type` continues to default
to `Recreate` for legacy compatibility.

```yaml
nodeMigration:
  workers:
    type: VMotion
    vmotion:
      mode: Hot
    maxUnavailable: 1
  controlPlane:
    type: VMotion
    vmotion:
      mode: Hot
    maxUnavailable: 1
```

`VMotion` requires an explicit `vmotion.mode` of `Hot` or `Cold`. Both roles
must use the same mode. There is no `Auto` mode, default, or automatic fallback.
VMotion limits worker `maxUnavailable` to `1` and requires exactly one failure
domain. A VMotion failure domain does not require `topology.template`. Legacy
and Recreate configurations continue to require templates, and Recreate worker
availability may exceed one. `maxUnavailable` is an integer node count; percent
strings are invalid.

The `vmotion` block is required for VMotion and forbidden for Recreate. Mixed
VMotion/Recreate role selections, mixed Hot/Cold modes, and multiple failure
domains for VMotion are rejected by structural schema/CEL validation.

## Examples

### Legacy selection

Omit `nodeMigration` to retain legacy behavior. The failure domain must include
the template used by the existing replacement path.

```yaml
spec:
  failureDomains:
    - name: target-fd-1
      region: target-region
      zone: target-zone-1
      server: vcenter-target.example.com
      topology:
        datacenter: TargetDC
        computeCluster: /TargetDC/host/TargetCluster
        datastore: /TargetDC/datastore/TargetDS
        networks: ["VM Network"]
        resourcePool: /TargetDC/host/TargetCluster/Resources
        template: /TargetDC/vm/rhcos-template
        folder: /TargetDC/vm/my-cluster
```

### Recreate selection

Both roles may explicitly select Recreate; omitted `type` also defaults to
Recreate. The template remains required. Worker concurrency greater than one
remains valid for this engine.

```yaml
nodeMigration:
  workers:
    type: Recreate
    maxUnavailable: 3
  controlPlane:
    type: Recreate
    maxUnavailable: 1
```

### Hot and Cold API selections

For VMotion, choose one explicit mode and use it for both roles. The target
failure domain can omit `topology.template`.

Hot selection:

```yaml
nodeMigration:
  workers: {type: VMotion, vmotion: {mode: Hot}, maxUnavailable: 1}
  controlPlane: {type: VMotion, vmotion: {mode: Hot}, maxUnavailable: 1}
```

Cold selection:

```yaml
nodeMigration:
  workers: {type: VMotion, vmotion: {mode: Cold}, maxUnavailable: 1}
  controlPlane: {type: VMotion, vmotion: {mode: Cold}, maxUnavailable: 1}
```

These examples describe valid API shapes only; they do not assert that the
current controller executes either mode.

### Recovery request shape

A request identifies a request, frozen execution, and action. It is a request
for runtime rechecking, not proof that the action is inactive or authorization
to replay it. CEL validates the bounded shape, not equality to the live action.

```yaml
nodeMigration:
  workers:
    type: VMotion
    vmotion: {mode: Cold}
    maxUnavailable: 1
  controlPlane:
    type: VMotion
    vmotion: {mode: Cold}
    maxUnavailable: 1
  recoveryRequest:
    requestID: 11111111-1111-4111-8111-111111111111
    executionID: 22222222-2222-4222-8222-222222222222
    actionID: 33333333-3333-4333-8333-333333333333
    reason: Administrator completed manual inspection
```

Changing `spec.state` to `Running`, timing out, or changing a request does not
clear intervention, prove inactivity, or authorize replay. Pause A permits
observation of existing work but starts no new actions; after a Cold relocation,
the VM may remain off until resume.

## Status model

`status.nodeMigration` retains `type`, human-readable `progress`, `controlPlane`,
and `workers`. Each role retains requested/observed mode, counts, and `nodes` as
a map list keyed by Kubernetes Node `name`. Counts and strings are observations.

### Execution and frozen plan

The optional status fields `executionID`, `migrationUID`, `planFingerprint`,
and `frozenPlan` identify the CR UID and sanitized immutable plan. The execution
and action IDs are independent of generation and remain stable across pause
generation changes. The frozen plan contains source/destination vCenter
identifiers, source datacenter, selected mode, and target failure domain. It
must not contain credentials, TLS material, ServiceLocator secrets, SDK objects,
full task results, or an event journal.

Each existing per-node entry adds `nodeUID`, `providerID`, `vmBIOSUUID`,
`originalUnschedulable`, and frozen target host/resource-pool/datastore/network fields.
Existing source/target inventory paths, failure-domain, mode and timestamps
remain available. `instanceUUID` remains the legacy wire name for the VM BIOS
UUID and is deprecated; it does **not** mean vSphere `config.instanceUuid`.
New records should use `vmBIOSUUID`; if both names occur, they must agree.

### Action and outcomes

`activeAction` carries one stable action ID/kind, execution ID, Node/VM identity,
owning vCenter, non-secret operation identity, submission and observation
states, optional task reference, timestamps/deadline, and a sanitized reason.
Task IDs are scoped to their `ownerVCenter`. `ShutdownGuest` has no task
reference. A durable claim without a task reference means submission may have
begun and is not safe to retry.

Each per-node entry has separate `shutdownOutcome`, `relocationOutcome`, and
`powerOnOutcome` fields. A later power-on result must not overwrite relocation
success. Outcome/count/phase fields do not independently authorize another
action. `intervention`, `recoveryAcknowledgement`, and `cleanup` are bounded
records for future runtime enforcement; their presence or values are not
runtime proof or authorization.

`Preparing` is a coarse pre-action/scheduling-preparation phase and does not
imply that only Cold mode drains. `Succeeded` means required action, identity,
placement, and recovery verification completed; a stale Node Ready condition
alone is insufficient. These semantics are API documentation, not implemented
controller guarantees.

## Validation and runtime boundary

The CRD enforces static enums, required fields, the engine/mode pairing,
conditional template and failure-domain limits, bounded list/string sizes,
task-reference ownership shape, outcome-kind pairing, UUID/identity formats,
and recovery-request shape. It intentionally does not freeze all spec fields,
bind task results to current generation, require a retained recovery request to
match whichever action later becomes active, or claim live ownership/health was
checked.

Runtime work still needs frozen-plan execution, live action ownership and
authorization, conflict-safe status copying, safe pause observation, action
claim/replay prevention, recovery evidence and acknowledgement consumption,
cleanup enforcement, dispatch isolation from legacy replacement, and the
version/security/operational qualification described in
[`upi-cross-vcenter-vmotion-design.md`](../plans/upi-cross-vcenter-vmotion-design.md).
