# Remove OVA Import Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Completely remove operator-managed RHCOS OVA import, requiring a user-supplied vSphere template in every failure domain.

**Architecture:** Remove the image API, importer and workflow phase rather than retaining compatibility shims. Keep the upstream embedded OpenShift failure-domain Go type; require its optional `topology.template` via CRD CEL and a runtime preflight guard. Preserve all non-image migration behavior and update generated CRDs/bundle, tests, metrics and docs.

**Tech Stack:** Go, Kubebuilder/controller-gen, OpenShift API, Kubernetes CRD CEL, controller-runtime, govmomi, Ginkgo/envtest, operator-sdk bundle.

**Spec:** `docs/superpowers/specs/2026-09-30-remove-ova-import-design.md`

## Global Constraints

- Work only in `.worktrees/remove-ova-feature` on `remove/ova-feature` from `upstream/main` `6cb35bf331c1d50e529954a18b0a6b7595e70e2f`.
- No customer release exists; do not retain `spec.image`, `status.image`, `DestinationImageImported`, deprecated aliases, or upgrade shims.
- Require `failureDomains[*].topology.template` at admission and preflight; retain the embedded `configv1.VSpherePlatformFailureDomainSpec` Go type.
- Do not delete existing vSphere templates. Keep destination initialization, template existence checks, shared ConfigMap RBAC, MCO pod restart, and generic container images.
- Use TDD for behavior changes, regenerate source-of-truth artifacts, avoid unrelated refactors; do not push or implement until plan review.
- Baseline: `go test ./internal/vsphere/... ./api/v1alpha1/...` passed; controller test passed with `KUBEBUILDER_ASSETS=/Users/jcallen/Development/vcf-migration-operator/.worktrees/fix-splat-2960/bin/k8s/1.33.0-darwin-arm64`. Without envtest assets controller suite fails before running tests.

## Review Focus

1. A CR with a *later* failure domain lacking a template must fail admission and preflight naming that FD (Task 1 tests).
2. Both absent and explicitly empty template values must fail CRD validation, while an existing valid path is accepted (Task 1 tests).
3. A user-supplied template must be validated against vCenter and never deleted/replaced by removal code (Tasks 1 and 2 tests and deletion audit).
4. Shared ConfigMap permissions and MCO pod restart must remain functional when removing `coreos-bootimages` access (Task 2 tests and RBAC audit).
5. Image-specific phase labels and OVA scratch volumes must vanish from both generated and packaged surfaces (Tasks 3–4 checks).

## File responsibility map

- `api/v1alpha1/vmwarecloudfoundationmigration_types.go`: feature API removal and list-level validation marker; `zz_generated.deepcopy.go`: regenerate.
- `internal/controller/preflight.go`, `preflight_test.go`: unconditional user-template preflight and vCenter existence check; remove only import privileges.
- `internal/controller/vmwarecloudfoundationmigration_controller.go`, `image_import_test.go`, `image_import_helpers_test.go`: remove import phase/handlers and dedicated tests, retain other conditions.
- `internal/vsphere/image.go`, `image_test.go`: delete image-only implementation/tests; retain other vSphere files.
- `internal/metrics/metrics.go`, `metrics_test.go`: remove phase and change expectation to next phase.
- `config/crd/bases/migration.openshift.io_vmwarecloudfoundationmigrations.yaml`, `bundle/manifests/migration.openshift.io_vmwarecloudfoundationmigrations.yaml`: generated/public CRD schema.
- `config/manager/manager.yaml`, `bundle/manifests/vcf-migration-operator.clusterserviceversion.yaml`: remove scratch from deployed and packaged manager.
- `go.mod`, `go.sum`, `vendor/`, `vendor/modules.txt`: remove now-unused stream-metadata dependency through supported vendoring workflow.
- `README.md`, `docs/dev/api.md`, `docs/dev/architecture.md`, `docs/user/spec-examples.md`, `docs/user/install-with-olm.md`, `docs/user/install-without-olm.md`, `docs/vcenter-privileges.md`: remove current OVA instructions, document required templates.

---

### Task 1: Require supplied templates and remove the image API

**Files:** Modify `api/v1alpha1/vmwarecloudfoundationmigration_types.go`, `internal/controller/preflight.go`, `internal/controller/preflight_test.go`; regenerate `api/v1alpha1/zz_generated.deepcopy.go` and `config/crd/bases/migration.openshift.io_vmwarecloudfoundationmigrations.yaml`; add admission validation coverage in `api/v1alpha1/types_test.go` or an appropriate CRD-schema test. Defer removal of import privilege code until Task 2.

**Interfaces:** Keep `fdTemplateMissing(fds []configv1.VSpherePlatformFailureDomainSpec) error`; no `ImageSpec`, `ImageStatus`, `ImageURLSource`, `DiskProvisioningMode`, or image condition after coordinated Task 2 edits. CEL marker goes on `FailureDomains` field (`self.all(fd, has(fd.topology.template) && fd.topology.template != '')`). The existing `validateFailureDomain` continues to look up `fd.Topology.Template` using `session.Finder.VirtualMachine(ctx, path)`.

- [ ] **Step 1: Add failing tests.** Change preflight tests so a missing/empty template on FD 0 and FD 1 is rejected, with FD index/name in the error, before vCenter calls. Add a Ginkgo envtest admission test in `internal/controller/preflight_test.go` (the existing suite installs generated CRDs) proving absent and empty `topology.template` fail on either FD while valid inventory paths are accepted; use `client.Create` on a new named migration with `state: Pending` and an otherwise valid spec so preflight cannot be mistaken for admission. Also assert the generated schema positions the CEL rule on `spec.failureDomains`. Preserve existing test of a configured but nonexistent vCenter template.
- [ ] **Step 2: Run targeted tests and see the expected failure.** `KUBEBUILDER_ASSETS=<existing assets dir> go test ./internal/controller/... ./api/v1alpha1/... -run 'TestControllers|Test.*Template' -count=1` (target the new tests by their actual names); missing-template acceptance/rule absence must fail before implementation.
- [ ] **Step 3: Make preflight unconditional.** Remove the `migration.Spec.Image == nil` guard around `fdTemplateMissing`, update its error to `spec.failureDomains[%d].topology.template is required (failure domain %q)`, and keep the vCenter finder check for the supplied template. In Task 2 remove `validateFailureDomain`'s unused `migration` parameter and image privilege branch atomically with image controller references.
- [ ] **Step 4: Remove API feature and generate.** Delete spec/status image fields, dedicated types and condition from `api/v1alpha1/vmwarecloudfoundationmigration_types.go`; update its comments; add `+kubebuilder:validation:XValidation:rule="self.all(fd, has(fd.topology.template) && fd.topology.template != '')",message="topology.template is required for every failure domain"` on the failure-domain list. Coordinate Tasks 1 and 2 in one working-tree transition so temporary Go compile failures do not become commits. Run `make generate && make manifests` and check that the CEL rule is under the correct list schema, no image fields remain and the controller-gen output is stable. If controller-gen cannot attach the marker correctly, stop and bring this alternative to the owner for approval rather than introducing a new generator or manual patches.
- [ ] **Step 5: Re-run targeted tests.** With envtest assets set, expect preflight and admission tests to pass; commit the coherent Task 1–2 Go/API change only after Task 2 finishes and the tree compiles (`git add api/v1alpha1 internal/controller internal/vsphere config/crd/bases; git commit -m 'Remove OVA import and require failure-domain templates'`).

### Task 2: Remove image workflow, importer and image-only vSphere privileges

**Files:** Modify `internal/controller/vmwarecloudfoundationmigration_controller.go`, `internal/controller/preflight.go`, controller tests as needed; delete `internal/controller/image_import_test.go`, `internal/controller/image_import_helpers_test.go`, `internal/vsphere/image.go`, `internal/vsphere/image_test.go`.

**Interfaces:** Controller `conditionOrder` flows `ConditionDestinationInitialized` directly to `ConditionMultiSiteConfigured`; dispatch map no longer contains image condition. Preflight calls `validateFailureDomain(ctx context.Context, fd *configv1.VSpherePlatformFailureDomainSpec, creds credentials) error`, without `migration` argument once import-only branch is removed. Keep `validateTargetPrivileges`, `missingPrivileges` and shared `configmaps` RBAC marker.

- [ ] **Step 1: Add a failing workflow test.** Assert destination initialization is followed by multi-site configuration in `conditionOrder` and in the handler dispatch; ensure existing template finder validation is exercised for an explicitly supplied path. Run the controller package tests with envtest assets and expect the new adjacency assertion to fail.
- [ ] **Step 2: Remove image-only controller code.** Delete image stage/order/map entry and `ovaDownloadTimeout`; delete `ensureDestinationImageImported`, `importOVATemplate`, `needsOVAReresolution`, `populateTopologyTemplates`, and imports/constants used only by these. Do not alter destination folder/tag setup, cloud-provider-config mutations, source cleanup or MCO pod restart.
- [ ] **Step 3: Remove import-only preflight and vSphere code.** Delete `validateImageImportPrivileges`, `imageImportPrivileges`, `checkPrivilegesOnEntity` if no other caller exists; remove the conditional import branch from `validateFailureDomain` and unconditionally validate the user template. Update its call sites/signature and drop only now-unused imports. Delete `internal/vsphere/image.go` and its dedicated tests; keep other govmomi/vSphere helpers.
- [ ] **Step 4: Remove obsolete dedicated tests and verify.** Delete the two image-only controller test files; run `KUBEBUILDER_ASSETS=<assets> go test ./internal/controller/... ./internal/vsphere/... ./api/v1alpha1/... -count=1`. Expect all to pass and no first-party Go references to removed types/functions. Finish the joint Task 1–2 commit after generation/admission tests pass.

### Task 3: Remove metrics phase and unused dependency

**Files:** Modify `internal/metrics/metrics.go`, `internal/metrics/metrics_test.go`, `go.mod`, `go.sum`, `vendor/modules.txt` and tracked `vendor/` files via the repo's vendoring workflow.

**Interfaces:** `PhaseFromConditions` advances from `DestinationInitialized` to `MultiSiteConfigured`; no `DestinationImageImported` phase label. `github.com/coreos/stream-metadata-go` must not remain in direct/indirect dependencies if unused.

- [ ] **Step 1: Change metric expectations and run red.** Replace the image-in-progress case with a test asserting an initialized destination yields `MultiSiteConfigured` as the next phase; assert no phase label for the removed condition. Run `go test ./internal/metrics/... -count=1` and expect the old stage to break the new expectation.
- [ ] **Step 2: Remove phase and clean dependencies.** Delete the image phase from `phaseOrder`/`allPhases`, retaining all other labels. Run `hack/go-mod.sh` (inspect it first for side effects) or `go mod tidy && go mod vendor` as appropriate for the repo; inspect diff to avoid unrelated module churn. Do not drop govmomi.
- [ ] **Step 3: Verify and commit.** `go test ./internal/metrics/... -count=1`; `rg 'github.com/coreos/stream-metadata-go|DestinationImageImported' --glob '!docs/plans/**' --glob '!docs/superpowers/**' --glob '!vendor/**' .` should have no matches after remaining tasks. Commit coherent metrics/dependency cleanup (`git add internal/metrics go.mod go.sum vendor; git commit -m 'Remove image phase metrics and stream metadata dependency'`).

### Task 4: Synchronize deployable artifacts and current documentation

**Files:** Modify `config/manager/manager.yaml`, `bundle/manifests/vcf-migration-operator.clusterserviceversion.yaml`, `bundle/manifests/migration.openshift.io_vmwarecloudfoundationmigrations.yaml`, `README.md`, `docs/dev/api.md`, `docs/dev/architecture.md`, `docs/user/spec-examples.md`, `docs/user/install-with-olm.md`, `docs/user/install-without-olm.md`, `docs/vcenter-privileges.md`; review `config/samples/migration_v1alpha1_vmwarecloudfoundationmigration.yaml` (already supplies template). Do not rewrite historical `docs/plans/`.

**Interfaces:** Deployment and CSV have no `/tmp/ova-scratch` mount/emptyDir; both CRD copies match for spec/status schema and validation; docs call `topology.template` required in every failure domain.

- [ ] **Step 1: Add artifact contract checks (red).** Write a small Go or script-level validation for both CRD schemas: image fields absent, template CEL present in failure-domain schema, base/bundle rules identical; assert no scratch volume/mount in deployment/CSV. Run it against old artifacts and see expected failure (or use explicit grep/YAML checks with documented expected results if no durable test is warranted).
- [ ] **Step 2: Update deployment and docs.** Remove OVA-specific scratch `volumeMounts`/`volumes` from `config/manager/manager.yaml`; update public/current docs and vCenter privilege matrix to remove import-only privileges, calls and phase. Keep general VM template finder privileges, required path examples and generic container-image references. Make no changes to historical plans unless they falsely purport to be current docs.
- [ ] **Step 3: Regenerate and compare.** Run `make manifests`; copy/produce the generated base CRD into the bundle according to existing repo workflow. Regenerate CSV from manager source through `make bundle` only if tooling is available and inspect its image/tag/timestamp diffs; otherwise make the minimal CSV scratch-only edit and compare its pod spec to manager source. Re-run artifact contract checks and confirm sample template paths are present. Commit generated manifests and docs when checks pass (`git add config/manager bundle/manifests README.md docs/dev docs/user docs/vcenter-privileges.md; git commit -m 'Update manifests and docs for manual templates'`).

### Task 5: Whole-feature verification and review

**Files:** No planned edits beyond fixes arising from checks. Preserve the user's unrelated root-checkout untracked plan.

**Interfaces:** All first-party OVA import behavior/surface removed; `topology.template` required at admission/preflight; non-image migration workflow remains functional.

- [ ] **Step 1: Run complete tests with envtest.** `KUBEBUILDER_ASSETS=/Users/jcallen/Development/vcf-migration-operator/.worktrees/fix-splat-2960/bin/k8s/1.33.0-darwin-arm64 go test ./... -count=1` (if tool-generated assets differ, use `bin/setup-envtest use -p path`); report any external test prerequisite separately. Run `go build ./...`, `go vet ./...` and `git diff --check`.
- [ ] **Step 2: Audit feature deletion and regeneration.** Scoped `rg -n 'DestinationImageImported|spec\.image|status\.image|coreos-bootimages|ova-scratch|ImageSpec|ImageStatus|stream-metadata-go|OVA import' api internal config bundle docs README.md go.mod go.sum` should return no *current* first-party references (historical plans and vendored/unrelated container image references may remain). Inspect `git diff --stat`, `git status --short`, `make manifests`/`make generate` repeatability, matching CRDs and CSV/deployment volume specs; do not claim lint passes unless executed successfully.
- [ ] **Step 3: Obtain independent review of the diff against the spec.** Focus on admission CEL placement, missing/empty template tests, vCenter template validation, preserved shared permissions, generated bundle parity and dependency churn. Fix actionable findings and re-run affected checks. Commit only coherent verified results; no push or PR unless separately requested.

## Execution handoff

This plan is for review, not authorization to implement. Once approved, use subagent-driven development with a fresh reviewer per coherent task (Tasks 1–2 are one atomic Go/API compile-and-commit unit); alternatively use native execution if the operator prefers lower overhead. The worktree is already isolated. Keep the complete-removal decision and generated-schema validation non-negotiable.
