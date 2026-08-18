# E2E Failure: DestinationInitialized Ownership Tag Race

## Purpose

This document is a **review packet** for an independent agent. It describes a failed periodic CI job, lists verifiable evidence, states hypotheses with confidence levels, and proposes a fix. The authoring agent's conclusions should **not** be trusted without cross-checking the artifacts and code cited below.

## CI Job Under Review

| Field | Value |
|-------|-------|
| Job | `periodic-ci-openshift-vcf-migration-operator-main-e2e-vsphere-vcf-migration-periodic` |
| Build ID | `2088015435248177152` |
| Date | 2026-08-13 |
| Failed step | `vcf-migration-execute` |
| Artifact log | `artifacts/e2e-vsphere-vcf-migration-periodic/vcf-migration-execute/build-log.txt` |

**GCSweb base URL:**

```text
https://gcsweb-ci.apps.ci.l2s4.p1.openshiftapps.com/gcs/test-platform-results/logs/periodic-ci-openshift-vcf-migration-operator-main-e2e-vsphere-vcf-migration-periodic/2088015435248177152/
```

## Observed Failure (verbatim from build log)

**User-visible error:**

```text
ensuring cluster ownership tag for "ci-op-yp303g06-0fd03-5lkqg" on vcenter-1.ci.ibmc.devcluster.openshift.com:
creating cluster ownership tag category: existing tag category "openshift-ci-op-yp303g06-0fd03-5lkqg" is incompatible:
missing required associable types urn:vim25:VirtualMachine, urn:vim25:Folder;
update the category in the vSphere UI or delete it and let the operator recreate it
```

**Condition:** `DestinationInitialized` → `status=False`, `reason=Failed`

**Earlier in the same run:** `InfrastructurePrepared` passed.

**Environment (from same log):**

| Role | vCenter server |
|------|----------------|
| Source (cluster install) | `vcenter.ci.ibmc.devcluster.openshift.com` |
| Target (migration destination) | `vcenter-1.ci.ibmc.devcluster.openshift.com` |
| Infra ID | `ci-op-yp303g06-0fd03-5lkqg` |
| Target datacenter | `cidatacenter-2` |

**Reviewer note:** Source and target are **separate vCenter servers**. There are **no linked vCenters** in this environment. Any hypothesis that depends on shared tag catalogs across linked vCenters is **out of scope** and was rejected by the operator owner.

## Verifiable Timeline (same artifact)

Reviewer should grep the build log for reconcile IDs and confirm this ordering:

| Time (UTC) | Reconcile ID | Event |
|------------|--------------|-------|
| 22:28:34 | `a7970fbc` | `processing condition DestinationInitialized` |
| 22:28:35 | `a7970fbc` | `created VM folder` path `/cidatacenter-2/vm/ci-op-yp303g06-0fd03-5lkqg` |
| 22:28:36 | `a7970fbc` | `ensuring cluster ownership tag` |
| 22:28:37 | `a7970fbc` | `ensured cluster ownership tag` tagID `urn:vmomi:InventoryServiceTag:7fd976d6-b215-4048-bc61-81ada36ad03a:GLOBAL` |
| 22:28:37 | `a7970fbc` | `ensured cluster ownership tag attached to folder` |
| 22:28:38 | `a7970fbc` | `failure domain initialized` |
| 22:28:38 | (event) | Normal `DestinationInitialized`: "VM folders and tags created on target vCenter" |
| 22:28:38 | `8357ac0e` | `processing condition DestinationInitialized` (overlapping reconcile) |
| 22:28:38 | `8357ac0e` | `VM folder already exists` |
| 22:28:39 | `8357ac0e` | `ensuring cluster ownership tag` |
| 22:28:39 | `8357ac0e` | **ERROR** — incompatible category (same message as above) |

**Key fact for reviewers:** The first reconcile **completed** folder creation, ownership tag ensure, and folder attachment. A **second concurrent reconcile** re-entered the ownership-tag path and failed. The E2E step then observed `DestinationInitialized=False`.

## Code Paths (reviewer should read these files)

### Controller: `ensureDestinationInitialized`

File: `internal/controller/vmwarecloudfoundationmigration_controller.go`

**Ownership tags (no skip-before-create):**

- Per-reconcile `folderCreated` map deduplicates only within a single reconcile loop.
- On each reconcile that enters the block, the controller calls:
  1. `vsphere.CreateVMFolder` / `GetVMFolder`
  2. `vsphere.EnsureClusterOwnershipTag`
  3. `vsphere.AttachClusterOwnershipTag`
- There is **no** call to `ObjectHasTagInCategory` for ownership tags before ensure.

**Region/zone tags (skip-before-create — contrast):**

- Uses `ObjectHasTagInCategory` for `openshift-region` / `openshift-zone` before calling `EnsureTagCategory`.
- See lines ~306–326 in the same function.

### vSphere: category validation

File: `internal/vsphere/tags.go`

- `EnsureClusterOwnershipTag` → `ensureTagCategory` with `requiredClusterOwnershipAssociableTypes`:
  - `urn:vim25:VirtualMachine`
  - `urn:vim25:Folder`
- `validateExistingCategory` returns an error if an existing category is missing any required associable type (exact string match via `missingAssociableTypes`).
- `ObjectHasTagInCategory` checks whether an object already has a tag from a named category (used for region/zone skip, **not** used for ownership).

### Reconcile error handling

File: `internal/controller/vmwarecloudfoundationmigration_controller.go` (~lines 151–175)

- On handler error: `setCondition(..., ReasonFailed, err.Error())` then `updateStatus`.
- A later failing reconcile can overwrite a condition that a concurrent successful reconcile had set to `True` moments earlier.

## Hypotheses (for reviewer to validate or reject)

### H1 — Concurrent reconcile re-runs ownership ensure (HIGH confidence)

**Claim:** The E2E failure is caused by a second reconcile re-calling `EnsureClusterOwnershipTag` after the first reconcile already succeeded, not by an inability to create tags on the first attempt.

**Evidence:**

- Log timeline above shows success on `a7970fbc` then failure on `8357ac0e` within ~1 second.
- `folderCreated` is not shared across reconciles.
- Region/zone tags already use vSphere state checks; ownership tags do not.

**How to falsify:**

- If reviewer finds only one reconcile ever called `EnsureClusterOwnershipTag` in the log, H1 is wrong.
- If reviewer finds first reconcile also failed with incompatible category, H1 is incomplete (see H2).

### H2 — Why second `ensureTagCategory` fails validation (MEDIUM confidence, needs investigation)

**Claim:** The second call to `ensureTagCategory` hits `GetCategory` for `openshift-ci-op-yp303g06-0fd03-5lkqg` and `validateExistingCategory` rejects it for missing `urn:vim25:VirtualMachine` and `urn:vim25:Folder`.

**Open question for reviewer:** If the first reconcile created the category via `CreateCategory` with `clusterOwnershipAssociableTypes` (which includes both missing types), why does the second reconcile see a category missing those types?

**Possible explanations (reviewer should investigate, not assume):**

1. **Concurrent create race:** Two reconciles both call `CreateCategory`; one wins; the loser hits `already_exists` and `GetCategory` returns a different or incomplete view. Reviewer should check vSphere/govmomi behavior under concurrent category creation.
2. **Pre-existing category on target:** Something before the operator (e.g. `ipi-install-vsphere-registry`, template upload script in `vcf-migration-execute`) created `openshift-<infraID>` on the target vCenter with incomplete associable types. **Counter-evidence:** First reconcile succeeded, which would require first `ensureTagCategory` to pass or create fresh — reviewer must reconcile this.
3. **Validation / API mismatch:** `GetCategory` returns associable type strings in a different format than `CreateCategory` sent (e.g. `VirtualMachine` vs `urn:vim25:VirtualMachine`). Reviewer can compare `AssociableTypes` on create vs get in simulator tests or CI must-gather if available.

**Explicitly rejected hypothesis:** Linked vCenters sharing a tag catalog between `vcenter.ci` and `vcenter-1`. Operator owner confirmed this is not the deployment model.

### H3 — Tag/category should only be created once (REQUIREMENT)

**Claim:** Ownership tag category, tag, and folder attachment should be created **once** per migration on the target vCenter. Re-running ensure on every reconcile is incorrect even when validation would pass.

**Basis:** Operator owner requirement; mirrors region/zone skip pattern; installer creates ownership category once per cluster.

## Proposed Fix

### Primary change (controller)

**Goal:** Create folder + ownership category + tag + folder attachment once; subsequent reconciles skip when vSphere state shows work is done.

**Location:** `ensureDestinationInitialized`, inside the `if !folderCreated[key]` block, **before** `CreateVMFolder` / `EnsureClusterOwnershipTag`.

**Logic (pseudocode):**

```go
folder, folderErr := vsphere.GetVMFolder(ctx, session, infraID)
if folderErr == nil {
    hasOwnership, err := vsphere.ObjectHasTagInCategory(
        ctx, session, vsphere.ClusterOwnershipCategoryName(infraID), folder)
    if err != nil {
        return ctrl.Result{}, fmt.Errorf(
            "checking ownership tag on folder %q on %s/%s: %w",
            infraID, fd.Server, fd.Topology.Datacenter, err)
    }
    if hasOwnership {
        log.V(1).Info("VM folder and ownership tag already configured",
            "path", folder.InventoryPath, "server", fd.Server, "datacenter", fd.Topology.Datacenter)
        folderCreated[key] = true
        // fall through to region/zone handling — do not call EnsureClusterOwnershipTag
    }
}
// existing CreateVMFolder + EnsureClusterOwnershipTag block only if folderCreated[key] still false
```

**Design notes:**

- Reuses existing `ObjectHasTagInCategory` (same as region/zone).
- Skip condition is **attachment on folder**, not merely category existence — folder without tag still needs ensure.
- `folderCreated[key] = true` after skip prevents re-entry in the same reconcile loop.

**Optional hardening (lower priority):**

- Early return at top of `ensureDestinationInitialized` if `DestinationInitialized` is already `True` on the in-memory object (may not help concurrent reconciles with stale reads; vSphere check is the real fix).
- Refactor folder block so skip and create paths share structure with region/zone (reviewer judgment).

### Out of scope for this fix

| Item | Reason |
|------|--------|
| `UpdateCategory` to append associable types | Not required if skip prevents re-validation; owner wants create-once semantics |
| CI teardown of tag categories | Hygiene only; does not fix operator race |
| Changes to `validateExistingCategory` | Keep strict validation on first create; skip avoids repeat calls |

## Test Plan

### Unit / package tests

1. **`internal/vsphere`** — existing tests should remain green:
```bash
   go test ./internal/vsphere/ -run 'TestEnsureClusterOwnership|TestObjectHasTagInCategory|TestEnsureTagCategory' -v
   ```

2. **New controller or vsphere test (implementer adds):**
   - Scenario: folder exists, ownership tag attached → `ObjectHasTagInCategory` returns true → implementer verifies controller does not call `EnsureClusterOwnershipTag` (mock or envtest).

### Integration / envtest

```bash
KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" \
  go test ./internal/controller/ -v -ginkgo.focus="DestinationInitialized"
```

Add or extend Ginkgo case:

1. Apply migration CR (or trigger `ensureDestinationInitialized`).
2. Simulate or mock vSphere: folder + ownership tag already present.
3. Reconcile again.
4. Assert `DestinationInitialized=True` and no error; assert ownership ensure not invoked twice if test harness supports it.

### Manual / CI verification

Re-run periodic job after merge:

```text
periodic-ci-openshift-vcf-migration-operator-main-e2e-vsphere-vcf-migration-periodic
```

Success criteria:

- `vcf-migration-execute` completes.
- `DestinationInitialized=True` on `VmwareCloudFoundationMigration/cluster`.
- Build log shows at most one `ensured cluster ownership tag` per server/datacenter (subsequent reconciles may log skip).

## Reviewer Checklist

Use this checklist independently; do not rely on this document's narrative alone.

- [ ] Download `build-log.txt` from artifact URL and confirm reconcile timeline matches table above.
- [ ] Confirm source vCenter (`vcenter.ci`) ≠ target vCenter (`vcenter-1`) in log.
- [ ] Read `ensureDestinationInitialized` and confirm ownership path lacks `ObjectHasTagInCategory` skip.
- [ ] Read region/zone path in same function and confirm skip pattern exists there.
- [ ] Read `ensureTagCategory` / `validateExistingCategory` and confirm failure message matches CI log.
- [ ] Decide whether H2 (why second validation fails) needs a separate fix beyond skip logic.
- [ ] Review proposed skip logic: is folder+tag attachment the correct "done" signal?
- [ ] Review test plan adequacy.
- [ ] Approve, reject, or request changes to proposed fix.

## Related Documents

- `docs/plans/destination-cluster-ownership-tags.md` — ownership tag design (installer reference)
- `docs/plans/destination-topology-tags.md` — region/zone skip and category validation policy
- `docs/plans/ci-e2e-testing.md` — E2E workflow (source vs target vCenter)

## Document History

| Date | Author | Note |
|------|--------|------|
| 2026-08-14 | Cursor agent | Initial review packet for build 2088015435248177152 |
