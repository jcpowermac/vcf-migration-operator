# Node Migration API (SPLAT-2949)

**Kind:** `VmwareCloudFoundationMigration` · **Group/Version:** `migration.openshift.io/v1alpha1`

`spec.nodeMigration` and `status.nodeMigration` control and report how nodes
move to the target vCenter. When `spec.nodeMigration` is omitted, the operator
keeps the current MachineSet/CPMS replacement path.

## Full Example (`oc get vcfm cluster -o yaml`)

```yaml
apiVersion: migration.openshift.io/v1alpha1
kind: VmwareCloudFoundationMigration
metadata:
  name: cluster
spec:
  # ... state, targetVCenterCredentialsSecret, failureDomains, image ...
  nodeMigration:
    strategy: VMotion        # VMotion | Recreate (default: VMotion)
    vmotion:
      mode: Auto             # Auto | Hot | Cold (default: Auto)
    workers:
      mode: Hot              # optional per-role override of vmotion.mode
      maxUnavailable: 25%    # count or percent of this role
    controlPlane:
      maxUnavailable: 1      # validated to 1
status:
  nodeMigration:
    strategy: VMotion        # frozen when WorkloadMigrated started
    progress: "control plane 2/3, workers 4/10"
    controlPlane:
      requestedMode: Auto
      total: 3
      pending: 0
      inProgress: 1
      succeeded: 2
      failed: 0
    workers:
      requestedMode: Hot
      total: 10
      pending: 5
      inProgress: 1
      succeeded: 4
      failed: 0
    nodes:
    - name: worker-0
      role: Worker
      phase: Succeeded
      requestedMode: Hot
      observedMode: Hot
      instanceUUID: 420e5e2e-c9eb-4a76-9623-725885785002
      targetFailureDomain: fd1
      sourceInventoryPath: /SourceDC/vm/openshift-worker-0
      targetInventoryPath: /TargetDC/openshift/fd1/worker-0
      startedAt: "2026-09-09T14:03:21Z"
      lastTransitionTime: "2026-09-09T14:11:02Z"
      completedAt: "2026-09-09T14:11:02Z"
    - name: master-1
      role: ControlPlane
      phase: Migrating
      requestedMode: Auto
      instanceUUID: 420e5e2e-c9eb-4a76-9623-725885785001
      targetFailureDomain: fd1
      sourceInventoryPath: /SourceDC/vm/openshift-master-1
      startedAt: "2026-09-09T14:09:40Z"
      lastTransitionTime: "2026-09-09T14:10:15Z"
    - name: worker-7
      role: Worker
      phase: Failed
      requestedMode: Hot
      observedMode: Cold    # Auto/Hot fell back to cold relocation
      message: "RelocateVM_Task failed: unsupported disk type; retried cold"
      instanceUUID: 420e5e2e-c9eb-4a76-9623-725885785007
      startedAt: "2026-09-09T14:05:00Z"
      lastTransitionTime: "2026-09-09T14:07:30Z"
      completedAt: "2026-09-09T14:07:30Z"
```

## spec.nodeMigration — `NodeMigrationSpec`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `strategy` | `NodeMigrationStrategy` | No | `VMotion` | Engine type: `VMotion` (Hot/Cold) or `Recreate` (Day-2). The `vmotion` field applies only when `strategy` is `VMotion`. |
| `vmotion` | `VMotionSpec` | No | | vMotion-specific behavior. Ignored unless `strategy` is `VMotion`. |
| `workers` | `RoleMigrationSpec` | No | | Worker-node rolling limits. |
| `controlPlane` | `RoleMigrationSpec` | No | | Control-plane rolling limits. `maxUnavailable` is validated to 1. |

### VMotionSpec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `mode` | `VMotionMode` | No | `Auto` | `Auto` tries Hot and falls back to Cold; `Hot` performs live relocation; `Cold` relocates powered off. |

### RoleMigrationSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `mode` | `*VMotionMode` | No | Per-role override of `spec.nodeMigration.vmotion.mode` (`Auto`, `Hot`, or `Cold`). |
| `maxUnavailable` | `intstr.IntOrString` | No | Rolling window as a count or percent of this role (e.g. `2` or `"25%"`). Control plane is validated to 1. |

## status.nodeMigration — `NodeMigrationStatus`

| Field | Type | Description |
|-------|------|-------------|
| `strategy` | `NodeMigrationStrategy` | Engine selected when `WorkloadMigrated` started. |
| `progress` | `string` | Human-readable rollup for `kubectl get`, e.g. `"control plane 2/3, workers 4/10"`. |
| `controlPlane` | `*RoleMigrationStatus` | Rollup for control-plane nodes. |
| `workers` | `*RoleMigrationStatus` | Rollup for worker nodes. |
| `nodes` | `[]NodeMigrationProgress` | Per-node progress, list merge key `name`. |

### RoleMigrationStatus

| Field | Type | Description |
|-------|------|-------------|
| `requestedMode` | `VMotionMode` | vMotion mode from spec for this role (inherited from `vmotion.mode` when the role omits `mode`). |
| `total` | `int32` | Number of discovered nodes in this role. |
| `pending` | `int32` | Nodes not yet started. |
| `inProgress` | `int32` | Nodes in `Preparing`, `Migrating`, or `WaitingForNode`. |
| `succeeded` | `int32` | Nodes in `Succeeded`. |
| `failed` | `int32` | Nodes in `Failed`. |

### NodeMigrationProgress (per node)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | Yes | Kubernetes Node name. List merge key. |
| `role` | `NodeMigrationRole` | Yes | `ControlPlane` or `Worker`. |
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
| `NodeMigrationStrategy` | `VMotion`, `Recreate` |
| `VMotionMode` | `Auto`, `Hot`, `Cold` |
| `NodeMigrationRole` | `ControlPlane`, `Worker` |
