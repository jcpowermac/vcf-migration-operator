# Plan: Singleton Namespace Enforcement + Deletion Finalizer

## Problem

Two gaps confirmed against the design requirements:

1. **Namespace not enforced.** The controller rejects any CR not named `cluster`
   (`SingletonName`), but reconciles `cluster` from *any* namespace. Requirement:
   the authoritative CR must be `openshift-vcf-migration/cluster`.
2. **No finalizer.** Deleting the CR is unguarded. Requirement: deletion must
   stop new actions but retain the CR/execution record until outstanding work
   is resolved or an administrator explicitly completes a handoff. Deletion
   must never cancel relocation, power VMs, delete VMs, or roll back.

## Changes

### 1. Namespace + name enforcement (controller)

`api/v1alpha1/vmwarecloudfoundationmigration_types.go`:

- Add `SingletonNamespace = "openshift-vcf-migration"` constant next to
  `SingletonName`, documented the same way (matches
  `config/default/kustomization.yaml` and the manager deployment namespace).
- Add reason constant `ReasonUnsupportedNamespace = "UnsupportedNamespace"`.

`internal/controller/vmwarecloudfoundationmigration_controller.go`:

- Extend the existing name guard (line ~195) to a combined check:
  - wrong name OR wrong namespace → set `Accepted=False` with the matching
    reason (`UnsupportedName` / `UnsupportedNamespace`), emit event, return.
    No side effects run. Same idempotent "alreadyRecorded" short-circuit the
    name guard already uses.
- No webhook, no CRD-level validation — reconcile-time enforcement only,
  consistent with the existing singleton pattern.

### 2. Deletion finalizer (controller)

Constants (api/v1alpha1):

- `FinalizerName = "migration.openshift.io/record-retention"`
- `AnnotationAllowDeletion = "migration.openshift.io/allow-deletion"`
- Reason `ReasonDeletionBlocked = "DeletionBlocked"` (on `Accepted` condition).

Reconcile logic, before the Running-state check:

1. **Add finalizer early:** if `migration.Name == SingletonName &&
   namespace == SingletonNamespace` and the finalizer is absent, patch it on
   and return (requeue via update event).
2. **Handle deletion:** if `DeletionTimestamp != nil`:
   - Do NOT run any migration step (deletion stops new actions; nothing is
     cancelled, powered off, deleted, or rolled back).
   - Set `Accepted=False` / `ReasonDeletionBlocked` with message explaining
     retention.
   - **Release the finalizer only when either:**
     a. migration is complete — `completionTime` set (or `Ready=True`), i.e.
        outstanding work is resolved; or
     b. administrator handoff — annotation
        `migration.openshift.io/allow-deletion: "true"` is set (documented
        admin escape hatch).
   - Otherwise leave the finalizer in place and return (object stays in
     terminating state, record retained).

The existing `/finalizers,verbs=update` RBAC marker already covers the patch.

### 3. Docs

- `docs/dev/api.md`: document the singleton pair `openshift-vcf-migration/cluster`,
  the finalizer, and the deletion semantics (stops new actions, retains record,
  admin handoff annotation).

## Test changes (`internal/controller/`)

- Existing tests that create a singleton in `default` namespace: switch to
  `openshift-vcf-migration` (envtest accepts any namespace).
- New Ginkgo specs:
  - CR named `cluster` in wrong namespace → `Accepted=False`, reason
    `UnsupportedNamespace`, no side effects.
  - Deleting an incomplete migration → CR remains (finalizer held),
    no reconcile actions; annotation alone without completion → still held.
  - Deleting after completion (or with annotation) → CR is removed.
- Unit test for the finalizer add path.

## Verification

```bash
make generate manifests
make test
make lint
```
