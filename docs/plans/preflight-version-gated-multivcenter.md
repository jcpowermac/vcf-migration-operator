# Preflight And Version-Gated Multi-vCenter Plan

## Overview

Update the operator so Phase 1 is a real preflight gate that surfaces blockers before any disruptive cluster mutation begins. Use the latest `openshift/api` support for Day 2 vSphere multi-vCenter changes on OpenShift `>= 5.0.0`, and keep the existing CVO pause plus CRD-modification path only as a compatibility fallback for clusters `< 5.0.0`.

## Current Baseline

- `go.mod` already depends on `github.com/openshift/api`, but the controller still assumes the legacy workflow for all clusters.
- `internal/controller/vmwarecloudfoundationmigration_controller.go` currently ends `InfrastructurePrepared` by disabling CVO, and later `MultiSiteConfigured` / `SourceCleaned` always use the CRD-modification wrappers in `internal/openshift/infrastructure.go`.
- There is no helper today for reading the cluster OpenShift version, so the operator cannot choose between the native 5.0+ path and the older compatibility path.

## Plan

### 1. Update dependency and add version + feature gate detection

- Bump `github.com/openshift/api` in `go.mod` to the latest commit that contains the vSphere Day 2 / multi-vCenter API behavior needed for OpenShift 5.0.
- Add a version helper under `internal/openshift/` that:
  - reads `ClusterVersion/version` and parses `status.desired.version`
  - reads `FeatureGate/cluster` (`config.openshift.io/v1`) and checks whether `VSphereMultiVCenterDay2` appears in `status.featureGates[].enabled[]` for the current version
- **Pattern note:** This operator is a standalone Kubebuilder controller, not a child operand managed by another operator. It cannot receive feature gates as `--feature-gates` CLI flags the way MAO's child controllers do ([machine-api-operator#1275](https://github.com/openshift/machine-api-operator/pull/1275)). Instead, it must read the `FeatureGate/cluster` CR directly via the controller-runtime client.
  - Key imports: `apifeatures "github.com/openshift/api/features"` for gate name constants (e.g., `apifeatures.FeatureGateVSphereMultiVCenterDay2`), `configv1 "github.com/openshift/api/config/v1"` for the `FeatureGate` type
  - RBAC: add `+kubebuilder:rbac:groups=config.openshift.io,resources=featuregates,verbs=get;list` to the controller or a dedicated helper
  - Read `FeatureGate/cluster`, iterate `status.featureGates` for the entry matching the current cluster version, check if `VSphereMultiVCenterDay2` is in `enabled[]`
- Centralize the compatibility rule — **both** conditions must be true for the native path:
  - `>= 5.0.0` (major.minor parse, pre-release suffixes like `5.0.0-0.nightly-*` qualify) **AND** `VSphereMultiVCenterDay2` gate enabled: direct `Infrastructure` updates, no forced CVO stop, no CRD schema surgery. Set `status.migrationPath = "Native"`.
  - Either condition not met: retain the legacy compatibility path that pauses CVO and temporarily relaxes the `Infrastructure` CRD validations. Set `status.migrationPath = "Legacy"`.
  - unparsable version or unreadable gate: fail preflight with an actionable status message.

### 2. Turn `InfrastructurePrepared` into a pure preflight phase

- Refactor `ensureInfrastructurePrepared()` in `internal/controller/vmwarecloudfoundationmigration_controller.go` so it only validates and reports results; it should no longer disable CVO (move that to `ensureMultiSiteConfigured` on the legacy path only).
- Extract preflight checks into `internal/controller/preflight.go`. `ensureInfrastructurePrepared()` delegates to focused preflight functions defined there.
- Keep the existing checks for credentials, source connectivity, target inventory, and failure-domain topology.
- Add the missing enforced preflight checks from the story:
  - supported OpenShift version and `VSphereMultiVCenterDay2` feature gate state → workflow selection
  - required source and target credentials
  - source and destination vCenter connectivity
  - destination inventory objects and required privileges
  - failure-domain completeness and consistency
  - immediate failure when any vSphere CSI-backed persistent volumes are present; there can be no `PersistentVolume` objects with `spec.csi.driver == csi.vsphere.vmware.com`
  - hard-block when `MachineHealthCheck`, `ClusterAutoscaler`, or `MachineAutoscaler` resources exist — user must remove them manually; operator re-checks on each reconcile and unblocks automatically once removed

### 3. Enforced preflight blockers

- Treat all controller-enforced failures as hard blockers that keep `InfrastructurePrepared=False`.
- Manual prerequisites (external DNS, load balancers, proxy, firewall, IPAM coordination) are documented in user-facing docs only — the operator does not attempt to validate or surface them at runtime. Actual validation would require deploying an agent VM on the target network, which is out of scope.
- Ensure the status output clearly distinguishes:
  - blocked by cluster-enforced checks (version/gate, CSI PVs, MHC/autoscaler)
  - requires operator action before proceeding (e.g., remove MHC resources)

### 4. Version-and-gate the disruptive infrastructure mutation flow

- In `internal/controller/vmwarecloudfoundationmigration_controller.go`, branch `MultiSiteConfigured` and `SourceCleaned` based on `status.migrationPath` (set during preflight).
- When native path is available (version `>= 5.0.0` **AND** `VSphereMultiVCenterDay2` gate enabled):
  - call direct `AddTargetVCenter()` and `RemoveSourceVCenter()` in `internal/openshift/infrastructure.go`
  - do not modify the `Infrastructure` CRD
  - keep CVO running for the full migration lifecycle because the CRD does not need to be changed on this path
- When `status.migrationPath == "Legacy"`:
  - disable CVO at the start of `ensureMultiSiteConfigured()` before the first CRD mutation
  - preserve `AddTargetVCenterWithCRDModification()` and `RemoveSourceVCenterWithCRDModification()`
  - keep CVO paused through `WorkloadMigrated`
  - re-enable CVO at the end of `ensureSourceCleaned()` after source removal and CRD restoration
- Update condition messages so the workflow makes the selected path explicit, for example:
  - `Using native path (VSphereMultiVCenterDay2 gate enabled on 5.x)`
  - `Using legacy compatibility path (gate not enabled or cluster < 5.0)`

### 5. Keep status results actionable in the API and UI

- Continue using `status.conditions` as the primary reporting mechanism.
- Tighten reason/message values so users can tell whether the issue is:
  - a hard stop
  - a compatibility fallback
  - an operator-action requirement
  - a documented manual prerequisite
### 6. Add focused TDD coverage

- Add unit tests first for:
  - version parsing, feature gate detection, and combined `>= 5.0.0` + `VSphereMultiVCenterDay2` gating
  - direct vs legacy `Infrastructure` update path selection (all four combinations: version yes/no × gate yes/no)
  - vSphere CSI PV detection
  - `MachineHealthCheck`, `ClusterAutoscaler`, and `MachineAutoscaler` detection
  - preflight status message composition
- Add controller/envtest coverage for:
  - successful preflight on 5.0+ without CVO pause, and confirmation that CVO remains running on the native path
  - legacy `< 5.0` path selection
  - immediate Phase 1 failure when any vSphere CSI-backed PV exists
  - blocking behavior for interfering autoscaler / health-check resources
- Add govmomi simulator coverage under `internal/vsphere/` if privilege validation requires vSphere-specific behavior that cannot be covered well with fake clients.

## Key Files

- `go.mod` — bump `openshift/api`, add `openshift/library-go`
- `internal/controller/vmwarecloudfoundationmigration_controller.go` — refactor `ensureInfrastructurePrepared()`, version-gate `MultiSiteConfigured`/`SourceCleaned`
- `internal/openshift/infrastructure.go` — direct vs CRD-modification path selection
- `internal/openshift/version.go` (new) — ClusterVersion parsing + FeatureGate detection
- `internal/controller/helpers.go` — CVO disable/enable scoping (currently lives here, not `operators.go`)
- `internal/controller/preflight.go` (new) — extracted preflight check functions
- `api/v1alpha1/vmwarecloudfoundationmigration_types.go` — add `status.migrationPath` field (`"Native"` / `"Legacy"`)
- `config/rbac/role.yaml` — add `featuregates.config.openshift.io` get/list, `clusterversions.config.openshift.io` get

## Reference PRs

- [openshift/api#2783](https://github.com/openshift/api/pull/2783) — `VSphereMultiVCenterDay2` feature gate definition
- [openshift/machine-api-operator#1275](https://github.com/openshift/machine-api-operator/pull/1275) — feature gate flag pattern (operator→operand; this operator reads the CR directly instead)

## Risks

- The 5.0+ path depends on both the updated `openshift/api` dependency and the actual cluster payload behavior, so tests should verify the branch logic independently from compile-time type changes.
- The `< 5.0` fallback path still carries recovery risk because it mutates CRD and CVO state; the implementation should narrow that path to only the operations that still require it.
- Manual prerequisites need to be surfaced clearly without being accidentally turned into controller-enforced blockers.

## TODOs

- Update `openshift/api` and add helpers for ClusterVersion parsing and `VSphereMultiVCenterDay2` feature gate detection; path selection requires both version `>= 5.0.0` and gate enabled.
- Refactor `InfrastructurePrepared` into a true preflight phase and add storage, autoscaler, MHC, privilege, and manual-prerequisite reporting.
- Version-and-gate `MultiSiteConfigured` and `SourceCleaned` so only clusters without the native path (version < 5.0 or gate disabled) use CVO pause plus CRD mutation wrappers.
- Add focused tests first, then adjust status messaging and UI only where needed to keep results actionable.

## Review Notes

### Inaccuracies in the plan

1. **CVO helpers location:** The plan (section 1 and Key Files) originally referenced `internal/openshift/operators.go` for CVO management. CVO scaling (`disableCVO`, `enableCVO`, `scaleCVO`) actually lives in `internal/controller/helpers.go` (lines 82-137). `operators.go` contains `OperatorManager` which checks `ClusterOperator` health — unrelated to CVO lifecycle. The Key Files section has been corrected.

2. **`library-go` is not a current dependency but should be added.** The plan lists it under Key Files as "possibly add `openshift/library-go`." It's not in `go.mod` today. While a direct CR read using `configv1.FeatureGate` from `openshift/api` is possible, `library-go` provides tested helpers for feature gate lookup that handle the version-matching logic in `status.featureGates` and avoid reimplementing that iteration. Use `library-go` for feature gate detection.

3. **RBAC for `clusterversions` may already be partially covered.** The existing marker at line 79 of `vmwarecloudfoundationmigration_types.go` covers `config.openshift.io` for `infrastructures;clusteroperators` with get/list/watch/update/patch. Adding `clusterversions` and `featuregates` to this group requires either extending that marker or adding a new one. The plan should call out that `clusterversions` needs **get** only (not update/patch) and `featuregates` needs **get;list** only.

### Gaps in the plan

4. **No mechanism to persist the selected path across reconcile loops.** The reconciler is stateless — each `Reconcile()` call walks `conditionOrder`, finds the first non-True condition, runs its handler, and returns. There is no shared in-memory state between phases. The version + gate check runs in `ensureInfrastructurePrepared()`, but two later handlers called on separate reconcile loops need the result:
   - `ensureMultiSiteConfigured()` must choose `infraMgr.AddTargetVCenterWithCRDModification()` (legacy) vs `infraMgr.AddTargetVCenter()` (native).
   - `ensureSourceCleaned()` must choose `infraMgr.RemoveSourceVCenterWithCRDModification()` (legacy) vs `infraMgr.RemoveSourceVCenter()` (native). It also conditionally calls `enableCVO()`, which must be skipped on the native path since CVO was never disabled.

   Today `VmwareCloudFoundationMigrationStatus` has no field to carry this forward. Without one, each phase re-reads `ClusterVersion` + `FeatureGate`, adding API calls and risking a path switch if the cluster is upgraded or the gate is toggled mid-migration. Options:
   - Add `status.migrationPath` (e.g., `"Native"` / `"Legacy"`) — set once during preflight, read by later phases to branch. Visible in `kubectl get`.
   - Re-evaluate on every reconcile — simpler, but the operator could switch paths mid-migration if conditions change between phases.

5. **Missing RBAC for new preflight checks.** The plan adds checks for `PersistentVolume` (CSI PV detection), `MachineHealthCheck`, `ClusterAutoscaler`, and `MachineAutoscaler`. Current RBAC markers don't cover:
   - `persistentvolumes` (core group, cluster-scoped) — needs get/list
   - `machinehealthchecks.machine.openshift.io` — needs get/list
   - `clusterautoscalers.autoscaling.openshift.io` — needs get/list
   - `machineautoscalers.autoscaling.openshift.io` — needs get/list
   These must be added to the RBAC markers and `config/rbac/role.yaml`.

6. **File organization for preflight logic is unspecified.** `ensureInfrastructurePrepared()` is currently large enough that version detection, gate detection, CSI PV checks, MHC/autoscaler checks, privilege validation, and advisory message composition should be extracted into `internal/controller/preflight.go`.

7. **`ensureMultiSiteConfigured` idempotency guard.** This function currently uses the condition message string (`"Waiting for vSphere pods to become ready"`) as a state marker to avoid re-running the entire configuration on re-reconcile. If the condition messages change, this guard will break. **Proposed alternative:** replace the message-based guard with an actual state check — read the `Infrastructure` CR and check whether the target vCenter servers and failure domains from `migration.Spec.FailureDomains` already exist in `infra.Spec.PlatformSpec.VSphere`. This is self-healing, costs one API read that `ensureMultiSiteConfigured` already performs on the non-waiting path, and works identically on both the native and legacy paths.

8. **RESOLVED: CVO disable timing on the legacy path.** CVO must be paused because the Infrastructure CRD validations are temporarily removed. On the legacy path, disable CVO at the start of `ensureMultiSiteConfigured()` (before the first CRD mutation), keep it paused through `WorkloadMigrated`, and re-enable at the end of `ensureSourceCleaned()` (after the source vCenter is removed and the CRD is restored). This moves the disable out of `ensureInfrastructurePrepared()` so preflight is purely non-disruptive, while keeping the same pause-through-completion semantics for the actual mutation window.

9. **RESOLVED: Advisory messages deferred — documentation only.** Validating manual prerequisites (DNS, load balancers, proxy, firewall, IPAM) at runtime would require deploying an agent VM on the target network to query infrastructure state — a significant undertaking out of scope for this work. These remain documented prerequisites in user-facing docs only; the operator does not attempt to check or surface them. Remove the advisory message category from section 3 of the plan.

### Existing plan strengths

- The dual version + gate check is correct and well-reasoned.
- Direct and CRD-modification methods both exist in `infrastructure.go` already — the version-gate is purely about calling the right one, no new vCenter manipulation code needed.
- The 2×2 test matrix (version × gate) is the right coverage model.
- CSI PV detection as a hard blocker with no override is the right call — vSphere CSI volumes cannot survive a cross-vCenter migration.

## Follow-Up Questions (all resolved)

1. **RESOLVED: Path persistence.** Persist in `status.migrationPath` (e.g., `"Native"` / `"Legacy"`). Set once during preflight in `ensureInfrastructurePrepared()`, read by `ensureMultiSiteConfigured()` and `ensureSourceCleaned()` to branch. Prevents mid-migration path switches. Visible in `kubectl get`. Requires adding the field to `VmwareCloudFoundationMigrationStatus` in `api/v1alpha1/vmwarecloudfoundationmigration_types.go`.

2. **RESOLVED: CVO pause window on legacy path.** Disable CVO at the start of `ensureMultiSiteConfigured()` (before the first CRD mutation), keep paused through `WorkloadMigrated`, re-enable at the end of `ensureSourceCleaned()` (after source removal and CRD restoration). Move the `disableCVO()` call out of `ensureInfrastructurePrepared()` so preflight is purely non-disruptive.

3. **RESOLVED: MHC/autoscaler disposition.** Hard blockers that keep `InfrastructurePrepared=False`. User must manually delete or pause `MachineHealthCheck`, `ClusterAutoscaler`, and `MachineAutoscaler` resources. Operator re-checks on each reconcile so it unblocks automatically once the user removes them.

4. **RESOLVED: Advisory message delivery.** Deferred — documentation only. No runtime validation of manual prerequisites.

5. **RESOLVED: Pre-release version handling.** Treat pre-release builds as `>= 5.0.0`. Parse `major.minor` and ignore pre-release suffixes. Any `5.0.0-*` build qualifies. Matches how CI and dev clusters work in practice.

6. **RESOLVED: Preflight file organization.** New file `internal/controller/preflight.go`. Keeps preflight checks close to the reconciler. `ensureInfrastructurePrepared()` delegates to preflight functions defined there.

7. **RESOLVED: `openshift/api` commit.** Pin to `github.com/openshift/api@v0.0.0-20260511191110-9b69e5fa27e9`, which includes `VSphereMultiVCenterDay2` and the feature-gated Infrastructure validation updates. When intentionally bumping later, update this plan entry alongside the corresponding upstream PR/reference that motivated the dependency change.
