# Node Migration API (SPLAT-2949)

**Kind:** `VmwareCloudFoundationMigration` · **Group/Version:** `migration.openshift.io/v1alpha1`

`spec.nodeMigration` and `status.nodeMigration` control and report how nodes
move to the target vCenter. When `spec.nodeMigration` is omitted, the operator
keeps the current MachineSet/CPMS replacement path.

## What `type` vs `vmotion` vs `mode` mean

Each role (`workers`, `controlPlane`) independently selects its migration
engine and, when VMotion, its relocation mode. The shape mirrors a
Deployment's `strategy`, but per role:

```yaml
nodeMigration:
  workers:
    type: VMotion              # which engine for this role
    vmotion:                   # settings that exist only for VMotion
      mode: Cold
    maxUnavailable: 25%
  controlPlane:
    type: VMotion
    vmotion:
      mode: Hot
    maxUnavailable: 1
```

Two knobs per role:

| Field | Question it answers | Where it lives |
|-------|---------------------|----------------|
| `type` | Move existing VMs (`VMotion`) or build new ones (`Recreate`)? | On each role (`workers`, `controlPlane`). |
| `vmotion.mode` | If VMotion: try live, force live, or always power off? | On each role, under `vmotion`. Optional; omit = `Auto`. |

`type: Recreate` with a `vmotion:` block is invalid — `vmotion` belongs
to the VMotion engine. CEL on `RoleMigrationSpec` enforces this.

`vmotion` itself is optional when `type` is `VMotion`. You do not have to
write `vmotion: {}`; omitted means `mode: Auto`.

Two engines only. `Recreate` is the MachineSet/CPMS path.

## Full Example (`oc get vcfm cluster -o yaml`)

```yaml
apiVersion: migration.openshift.io/v1alpha1
kind: VmwareCloudFoundationMigration
metadata:
  name: cluster
spec:
  # ... state, targetVCenterCredentialsSecret, failureDomains, image ...
  nodeMigration:
    workers:
      type: VMotion            # per-role engine (default: VMotion)
      vmotion:                 # optional; omitted => mode Auto
        mode: Cold
      maxUnavailable: 25%      # count or percent of this role
    controlPlane:
      type: VMotion
      vmotion:
        mode: Hot
      maxUnavailable: 1
status:
  nodeMigration:
    type: VMotion            # frozen when WorkloadMigrated started
    progress: "control plane 2/3, workers 4/10"
    controlPlane:
      requestedMode: Auto    # spec policy
      observedMode: Hot      # what actually ran (never Auto)
      total: 3
      pending: 0
      inProgress: 1
      succeeded: 2
      failed: 0
      nodes:
      - name: master-1
        phase: Migrating
        requestedMode: Auto
        observedMode: Hot
        instanceUUID: 420e5e2e-c9eb-4a76-9623-725885785001
        targetFailureDomain: fd1
        sourceInventoryPath: /SourceDC/vm/openshift-master-1
        startedAt: "2026-09-09T14:09:40Z"
        lastTransitionTime: "2026-09-09T14:10:15Z"
    workers:
      requestedMode: Cold
      observedMode: Cold
      total: 10
      pending: 5
      inProgress: 1
      succeeded: 4
      failed: 0
      nodes:
      - name: worker-0
        phase: Succeeded
        requestedMode: Cold
        observedMode: Cold
        instanceUUID: 420e5e2e-c9eb-4a76-9623-725885785002
        targetFailureDomain: fd1
        sourceInventoryPath: /SourceDC/vm/openshift-worker-0
        targetInventoryPath: /TargetDC/openshift/fd1/worker-0
        startedAt: "2026-09-09T14:03:21Z"
        lastTransitionTime: "2026-09-09T14:11:02Z"
        completedAt: "2026-09-09T14:11:02Z"
      - name: worker-7
        phase: Failed
        requestedMode: Cold
        observedMode: Cold
        message: "RelocateVM_Task failed: unsupported disk type"
        instanceUUID: 420e5e2e-c9eb-4a76-9623-725885785007
        startedAt: "2026-09-09T14:05:00Z"
        lastTransitionTime: "2026-09-09T14:07:30Z"
        completedAt: "2026-09-09T14:07:30Z"
```

`requestedMode` is spec policy and may be `Auto`. `observedMode` is what
vCenter actually ran (`Hot` or `Cold` only). If `Auto` fell back on some
nodes and not others, role `observedMode` stays empty and
`controlPlane.nodes` / `workers.nodes` `observedMode` is the source of truth.

Invalid (rejected by CEL): `type: Recreate` together with `vmotion`.

## spec.nodeMigration — `NodeMigrationSpec`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `workers` | `RoleMigrationSpec` | No | Worker-node engine selection, VMotion mode, and rolling limits. |
| `controlPlane` | `RoleMigrationSpec` | No | Control-plane engine selection, VMotion mode, and rolling limits. `maxUnavailable` is validated to 1. |

### RoleMigrationSpec (tagged union)

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `type` | `NodeMigrationType` | No | `VMotion` | Migration engine for this role and tagged-union discriminator: `VMotion` (relocate existing VMs) or `Recreate` (MachineSet/CPMS replacement). |
| `vmotion` | `*VMotionSpec` | No | | VMotion-specific behavior. Allowed only when `type` is `VMotion` (omission means `mode: Auto`). Forbidden when `type` is `Recreate`. |
| `maxUnavailable` | `intstr.IntOrString` | No | | Rolling window as a count or percent of this role (e.g. `2` or `"25%"`). Control plane is validated to 1. |

### VMotionSpec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `mode` | `VMotionMode` | No | `Auto` | `Auto` tries Hot and falls back to Cold; `Hot` performs live relocation; `Cold` relocates powered off. |

## status.nodeMigration — `NodeMigrationStatus`

| Field | Type | Description |
|-------|------|-------------|
| `type` | `NodeMigrationType` | Engine selected when `WorkloadMigrated` started. |
| `progress` | `string` | Human-readable rollup for `kubectl get`, e.g. `"control plane 2/3, workers 4/10"`. |
| `controlPlane` | `*RoleMigrationStatus` | Control-plane counts and nested per-node progress. |
| `workers` | `*RoleMigrationStatus` | Worker counts and nested per-node progress. |

There is no top-level `nodes` list. A node belongs to one role; its progress lives under that role.

### RoleMigrationStatus

| Field | Type | Description |
|-------|------|-------------|
| `requestedMode` | `VMotionMode` | vMotion mode from spec for this role (inherited from `vmotion.mode` when the role omits `mode`). May be `Auto`. |
| `observedMode` | `VMotionMode` | Relocation that actually ran for this role: `Hot` or `Cold` (never `Auto`). Empty until a node starts, and empty if `Auto` produced a mix (then see `nodes[].observedMode`). |
| `total` | `int32` | Number of discovered nodes in this role. Derived from `nodes`. |
| `pending` | `int32` | Nodes not yet started. Derived from `nodes`. |
| `inProgress` | `int32` | Nodes in `Preparing`, `Migrating`, or `WaitingForNode`. Derived from `nodes`. |
| `succeeded` | `int32` | Nodes in `Succeeded`. Derived from `nodes`. |
| `failed` | `int32` | Nodes in `Failed`. Derived from `nodes`. |
| `nodes` | `[]NodeMigrationProgress` | Per-node progress for this role, list merge key `name`. |

### NodeMigrationProgress (per node)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Kubernetes Node name. List merge key. |
| `phase` | `NodeMigrationPhase` | Yes | Node-local relocation state (below). |
| `requestedMode` | `VMotionMode` | No | Mode in force when this node started. Frozen for in-flight nodes if spec changes later. |
| `observedMode` | `VMotionMode` | No | Relocation that actually ran: `Hot` or `Cold`. Empty until RelocateVM starts; `Auto` resolves here, never in spec. |
| `instanceUUID` | `string` | No | VM BIOS UUID (node `spec.providerID` without `vsphere://`). Stable identity across vCenters; MoRef is not. |
| `targetFailureDomain` | `string` | No | `spec.failureDomains[].name` chosen for placement. |
| `sourceInventoryPath` | `string` | No | VM path before relocation. |
| `targetInventoryPath` | `string` | No | VM path after relocation. |
| `message` | `string` | No | Short human-readable explanation of phase or failure. |
| `lastTransitionTime` | `*metav1.Time` | No | When `phase` last changed. |
| `startedAt` | `*metav1.Time` | No | When `Preparing` began. |
| `completedAt` | `*metav1.Time` | No | When `phase` became `Succeeded` or `Failed`. |

## Enums

### NodeMigrationPhase (lifecycle)

```
Pending → Preparing → Migrating → WaitingForNode → Succeeded
              ↘ Failed (from any phase)
```

| Value | Meaning |
|-------|---------|
| `Pending` | Discovered, not started |
| `Preparing` | Drain/cordon (Cold) or live-compat checks (Hot) |
| `Migrating` | `RelocateVM_Task` running |
| `WaitingForNode` | VM on the target; kubelet not Ready yet |
| `Succeeded` | Node Ready on the target |
| `Failed` | Terminal or currently-failed node |

### Other enums

| Type | Values |
|------|--------|
| `NodeMigrationType` | `VMotion`, `Recreate` |
| `VMotionMode` | `Auto`, `Hot`, `Cold` |
