# Failure Domain Mapping Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add explicit source↔destination failure domain mapping to the migration CRD so migrations with more than one failure domain on either side work correctly, while single-FD-to-single-FD migrations keep their current implicit behavior.

**Architecture:** A new optional `spec.failureDomainMappings` list (name pairs) is resolved and validated at preflight into an effective mapping recorded in `status.failureDomainMappings`; later phases consume the recorded mapping (never re-resolving it from Infrastructure, which legitimately changes mid-migration) for MachineSet identity, provider-spec/replica inheritance, and source-vCenter selection. The mapping is immutable once preflight has passed, and generated MachineSet name collisions are detected at preflight — before the mapping freezes — so the "rename the destination" remedy is still available. Source-side lookups gain a per-failure-domain MachineSet matcher that provably assigns each source MachineSet to at most one failure domain. VM templates always come from the destination failure domain's `topology.template`.

**Tech Stack:** Go, controller-runtime, kubebuilder codegen (`make manifests`, `make generate`), envtest (Ginkgo/Gomega), standard `testing.T` table tests for pure functions.

**Spec:** `docs/failure-domain-mapping-analysis.md` (scope, API design, validation split, semantics)

## Review corrections incorporated

All findings from the plan review are addressed in the tasks below:

1. Destination-uniqueness CEL uses `distinct()` (the old `size() == size()` projection was vacuously true); corrected in both this plan and the analysis doc.
2. Immutability compares requested spec pairs against recorded status pairs only — never re-resolves from Infrastructure (which gains the destination vCenter and later loses source FDs during a normal migration).
3. Multi-vCenter rejection is a preflight-time check (initial state), not a permanent property of `GetSourceVCenter`; during the transition the source vCenter is selected by server name derived from the recorded mapping.
4. The MachineSet matcher requires unambiguous topology matching; overlapping source assignments are rejected at preflight and guarded at rollout.
5. MachineSet name-collision detection runs at preflight, before mappings are recorded as immutable.
6. Replica inheritance is exact — an explicit source total of zero produces zero destination replicas; only "no matching MachineSets" is an error.
7. Mapping comparison is order-insensitive (keyed by source).
8. Empty-list rejection is tested at the wire level (raw JSON / unstructured), because `omitempty` drops typed empty slices before schema validation.

**Design decision (settled with the user):** when a source failure domain contains MachineSets with heterogeneous provider specs, each source MachineSet is copied to its own destination MachineSet with the correct sizing; the VM template comes from the destination failure domain's `topology.template` and is never inherited from the source.

## Global Constraints

- Release scope: exactly one source vCenter and one destination vCenter server; destination failure domains must all reference the same server.
- Omitted `failureDomainMappings` means exactly one source FD and one destination FD, mapped implicitly. Anything else with the field omitted is a preflight error.
- When present, mappings are a complete bijection: every source FD (in Infrastructure) and every destination FD (in `spec.failureDomains`) covered exactly once.
- Effective mappings are immutable for the lifetime of the migration (recorded in status at preflight; drift afterwards is an error). Immutability compares the requested spec pairs against the recorded pairs; Infrastructure changes do not participate.
- Field markers per the spec doc: `+listType=map`, `+listMapKey=source`, `MinItems=1`, `MaxItems=64`, `MinLength=1`, `MaxLength=256` (matching `configv1.VSpherePlatformFailureDomainSpec.Name`), XValidation for destination uniqueness via `distinct()`.
- Project conventions (AGENTS.md): errors via `fmt.Errorf` with `%w`, lowercase gerund messages; klog from context; three import groups; unit tests standard `testing.T` table-driven, controller integration tests Ginkgo/Gomega envtest.

## Review Focus

Each failure mode below is pinned by a test in the owning task.

1. Empty-but-present mappings list (`failureDomainMappings: []`), applied at the wire level, must be rejected by the schema (`MinItems`), not silently treated as omitted — Task 1 test.
2. A mapping naming a failure domain that does not exist (in Infrastructure for `source`, in `spec.failureDomains` for `destination`) must produce a preflight error naming it, not a panic or silent skip — Task 2 test.
3. Editing `spec.failureDomainMappings` after InfrastructurePrepared=True must error, even if Infrastructure has since changed (destination vCenter/FDs added, source FDs removed) — Task 3 test.
4. A destination FD whose generated MachineSet name collides with an existing *source* MachineSet must fail preflight with an error telling the user to rename, and renaming must let preflight pass — Task 2 test.
5. A mapped source FD with no matching source MachineSet (provider spec topology mismatch) must error clearly at creation time — Task 4 test.
6. Two source FDs in the same datacenter with ambiguous resource pools must be rejected at preflight — Task 2 test.
7. `spec.failureDomains` entries spanning two servers must be rejected at preflight per release scope — Task 2 test.
8. A source FD deliberately scaled to zero must produce zero destination replicas (not one) — Task 4 test.

---

### Task 1: API types — `FailureDomainMapping`, spec and status fields

**Files:**
- Modify: `api/v1alpha1/vmwarecloudfoundationmigration_types.go` (spec field near `FailureDomains` at line 122; status field near `Conditions` at line 146)
- Test: `internal/controller/failure_domain_mapping_api_test.go` (new, Ginkgo — joins the existing envtest suite)

**Interfaces:**
- Consumes: existing `VmwareCloudFoundationMigrationSpec`/`Status` types.
- Produces: `migrationv1alpha1.FailureDomainMapping` struct with `Source string` / `Destination string`; `Spec.FailureDomainMappings []FailureDomainMapping`; `Status.FailureDomainMappings []FailureDomainMapping`.

- [ ] **Step 1: Write the failing Ginkgo spec rejection tests**

Note: duplicate/empty-list fixtures must be applied at the wire level — with `omitempty` on the field, a typed empty slice in a Go struct disappears during serialization and never exercises `MinItems`. Use `unstructured.Unstructured` with `Spec` set to raw JSON (`"failureDomainMappings": []`), or `json.Unmarshal` into `map[string]interface{}` before creating; separately assert that omitting the field entirely is accepted (implicit 1:1 case).

```go
// It("rejects a mapping list with duplicate destinations")
//   Via unstructured/raw JSON: two spec.failureDomains ("dst-a", "dst-b") and
//   failureDomainMappings:
//     {source: "src-a", destination: "dst-a"}, {source: "src-b", destination: "dst-a"}
//   Expect creation to fail with an error whose message contains
//   "destination must be unique".
// It("rejects an empty mappings list")
//   Wire-level fixture with "failureDomainMappings": [] (present but empty).
//   Expect creation to fail (MinItems=1).
// It("rejects duplicate source names")
//   Two mappings with the same source, different destinations.
//   Expect creation to fail (listMapKey=source).
// It("accepts a valid mapping list")
//   Two mappings, bijective. Expect creation to succeed.
// It("accepts omission of the field")
//   No failureDomainMappings key at all. Expect creation to succeed.
```

- [ ] **Step 2: Run test to verify it fails**

Run: `KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" go test ./internal/controller/ -v -ginkgo.focus="failure domain mapping"`
Expected: FAIL — unknown field, CRD accepts everything.

- [ ] **Step 3: Add the types**

Add to `api/v1alpha1/vmwarecloudfoundationmigration_types.go` exactly the shapes from the spec doc's "Chosen design" section (type `FailureDomainMapping`, spec field with `+optional`, `+listType=map`, `+listMapKey=source`, `+kubebuilder:validation:MinItems=1`, `+kubebuilder:validation:MaxItems=64`, XValidation below). The uniqueness rule must use `distinct()` — projecting with `map()` preserves list length, so `self.map(m, m.destination).size() == self.size()` is always true:

```go
// +kubebuilder:validation:XValidation:rule="self.map(m, m.destination).distinct().size() == self.size()",message="destination must be unique across mappings"
```

(`MaxItems=64` bounds the CEL cost of the rule.) Status field:

```go
// failureDomainMappings is the effective source-to-destination failure domain
// mapping resolved at preflight. When spec.failureDomainMappings is omitted it
// holds the single inferred pair. Immutable once set.
// +optional
// +listType=atomic
FailureDomainMappings []FailureDomainMapping `json:"failureDomainMappings,omitempty"`
```

- [ ] **Step 4: Regenerate and run tests**

Run: `make generate manifests` then `KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" go test ./internal/controller/ -v -ginkgo.focus="failure domain mapping"`
Expected: PASS; `git status` shows updated `api/v1alpha1/zz_generated.deepcopy.go` and CRD yaml under `config/crd/bases/`. Also run `api_lint_types` on `api/v1alpha1/` — expect 0 errors.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/ config/crd/bases/ internal/controller/failure_domain_mapping_api_test.go
git commit -m "Add failure domain mappings to migration API"
```

---

### Task 2: Mapping resolution, preflight validation, and name-collision detection

**Files:**
- Create: `internal/controller/failuredomains.go`
- Create: `internal/controller/failuredomains_test.go` (standard `testing.T`, table-driven, style of `preflight_test.go`)
- Modify: `internal/controller/preflight.go` (`runPreflightChecks`, after the existing `validateFailureDomainTypes` call)
- Modify: `internal/openshift/infrastructure.go` (add `GetVCenterByServer`)
- Test: `internal/openshift/infrastructure_test.go` (add cases to existing table test)

**Interfaces:**
- Consumes: `migrationv1alpha1.FailureDomainMapping`, `Spec.FailureDomainMappings` (Task 1).
- Produces:
  - `func resolveFailureDomainMappings(mappings []migrationv1alpha1.FailureDomainMapping, sourceFDs, destFDs []configv1.VSpherePlatformFailureDomainSpec) ([]migrationv1alpha1.FailureDomainMapping, error)`
  - `func validateFailureDomainServers(fds []configv1.VSpherePlatformFailureDomainSpec) error`
  - `func validateSourceVCenterCount(infra *configv1.Infrastructure) error`
  - `func validateSourceFDTopologyDisjoint(sourceFDs []configv1.VSpherePlatformFailureDomainSpec) error`
  - `func (r *VmwareCloudFoundationMigrationReconciler) validateMachineSetNameCollisions(ctx context.Context, infraID string, mappings []migrationv1alpha1.FailureDomainMapping, destFDs []configv1.VSpherePlatformFailureDomainSpec) error`
  - `func (m *InfrastructureManager) GetVCenterByServer(ctx context.Context, server string) (*configv1.VSpherePlatformVCenterSpec, error)`

- [ ] **Step 1: Write the failing table tests**

```go
// TestResolveFailureDomainMappings — cases:
//   "explicit bijection"        -> 2 mappings, 2 source FDs, 2 dest FDs; want mappings back, no error
//   "omitted 1:1"               -> nil mappings, 1 source FD, 1 dest FD; want [{src, dst}], no error
//   "omitted with 2 dest FDs"   -> nil mappings, 1 source FD, 2 dest FD; want error "provide mappings"
//   "omitted with 2 source FDs" -> nil mappings, 2 source FDs, 1 dest FD; want error "provide mappings"
//   "unknown source name"       -> mapping source "nope"; want error naming "nope"
//   "unknown destination name"  -> mapping destination "nope"; want error naming "nope"
//   "partial coverage"          -> 2 source/dest FDs but 1 mapping; want error "must cover"
//   "wrong cardinality"         -> 2 source FDs, 3 dest FDs (mapping set of size 2); want error
// TestValidateFailureDomainServers — cases:
//   "single server"      -> no error
//   "two servers"        -> error naming both servers
// TestValidateSourceVCenterCount — cases:
//   "one vCenter"        -> no error
//   "two vCenters"       -> error naming the count and the supported limit
//   "zero vCenters"      -> error
// TestValidateSourceFDTopologyDisjoint — cases:
//   "same datacenter, distinct pools" -> no error
//   "same datacenter, one pool empty"  -> error "overlapping" naming the datacenter
//   "same datacenter, same pool"       -> error "overlapping"
//   "different datacenters"            -> no error (pools may be empty)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run 'TestResolveFailureDomainMappings|TestValidateFailureDomainServers|TestValidateSourceVCenterCount|TestValidateSourceFDTopologyDisjoint' -v`
Expected: FAIL — `undefined: resolveFailureDomainMappings`.

- [ ] **Step 3: Implement the pure validators in `internal/controller/failuredomains.go`**

Rules (from the spec doc's "Validation split"): explicit list must reference only names present on the given side, must contain exactly `len(sourceFDs)` entries, and must cover each side exactly once (uniqueness within the list is already schema-enforced; completeness is not). Omitted list requires exactly one FD on each side and returns the inferred pair. Destination-server validation: all `fds[i].Server` equal and non-empty. Source FD topology disjointness: two source FDs sharing a datacenter must both have non-empty, distinct resource pools — a machineset can then match at most one FD.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestResolve|TestValidate' -v`
Expected: PASS.

- [ ] **Step 5: Add `GetVCenterByServer` to `InfrastructureManager`**

Return the `VCenters` entry whose `Server` equals the argument; `fmt.Errorf` naming the server when absent. This is the source-vCenter lookup used after the destination vCenter has been added — `GetSourceVCenter`'s `VCenters[0]` is only valid at preflight time, when `validateSourceVCenterCount` guarantees exactly one entry. Add "found by server", "server not found" cases to `TestGetSourceVCenter` in `internal/openshift/infrastructure_test.go`. Do **not** change `GetSourceVCenter`'s existing behavior (finding 3: a blanket `len(VCenters) > 1` rejection would reject the expected source-plus-destination transitional state).

- [ ] **Step 6: Write the failing name-collision preflight test**

```go
// It("fails preflight when a destination MachineSet name collides with a source MachineSet")
//   Destination FD named like a source FD so workerMachineSetName(infraID, name)
//   matches an existing source machineset in the fake client.
//   Expect preflight error containing "collides with existing" and "rename".
// It("passes preflight after the destination FD is renamed")
//   Same fixture, destination FD renamed so no collision. Expect preflight success.
```

- [ ] **Step 7: Implement `validateMachineSetNameCollisions`**

For each destination FD in the resolved mapping, compute `workerMachineSetName(infraID, fd.Name)` (and the suffixed variants `-2`, `-3`, … that Task 4 generates for additional copied machinesets, bounded by the source machineset count per FD) and list MachineSets in `openshift-machine-api`; if any existing MachineSet has a generated name, return `fmt.Errorf("generated MachineSet name %q collides with existing MachineSet; rename the destination failure domain", name)`. This runs at preflight, before mappings are recorded as immutable, so the rename remedy is still available (finding 5).

- [ ] **Step 8: Wire all validations into `runPreflightChecks`**

After `validateFailureDomainTypes`: call `validateFailureDomainServers(migration.Spec.FailureDomains)`; fetch Infrastructure once; call `validateSourceVCenterCount` (initial state has exactly one source vCenter), `validateSourceFDTopologyDisjoint` on the Infrastructure FDs, `resolveFailureDomainMappings(migration.Spec.FailureDomainMappings, infraFDs, migration.Spec.FailureDomains)`, then `validateMachineSetNameCollisions` with the resolved pairs. Extend `TestRunPreflightChecks` in `preflight_test.go` with a case per new error path (fixtures already build fake Infrastructure objects).

- [ ] **Step 9: Run tests and commit**

Run: `go test ./internal/controller/ ./internal/openshift/ -v`
Expected: PASS.

```bash
git add internal/controller/failuredomains.go internal/controller/failuredomains_test.go internal/controller/preflight.go internal/controller/preflight_test.go internal/openshift/infrastructure.go internal/openshift/infrastructure_test.go
git commit -m "Validate failure domain mappings and MachineSet name collisions at preflight"
```

---

### Task 3: Record effective mappings in status and enforce immutability

**Files:**
- Modify: `internal/controller/preflight.go` (success path of `runPreflightChecks`, where `message := "Preflight validation passed"` is assembled)
- Modify: `internal/controller/vmwarecloudfoundationmigration_controller.go` (Reconcile, after the migration object is fetched and its status refreshed)
- Test: `internal/controller/failuredomains_test.go` (extend)

**Interfaces:**
- Consumes: `resolveFailureDomainMappings` (Task 2), `Status.FailureDomainMappings` (Task 1).
- Produces: `func (r *VmwareCloudFoundationMigrationReconciler) ensureMappingImmutable(migration *migrationv1alpha1.VmwareCloudFoundationMigration) error`

- [ ] **Step 1: Write the failing tests**

```go
// TestRecordFailureDomainMappings (standard, fake clients) — runPreflightChecks
//   success path populates migration.Status.FailureDomainMappings with the
//   resolved pairs (explicit case and inferred 1:1 case).
// TestEnsureMappingImmutable — cases:
//   "status unset"          -> no error (pre-preflight)
//   "unchanged spec"        -> same pairs; no error
//   "reordered spec"        -> same pairs in different order; no error
//   "edited spec"           -> spec lists a different pair than status; want error
//     "failureDomainMappings are immutable"
//   "omitted then edited"   -> status holds inferred pair, user adds an explicit
//     list that differs; want error
//   "destination FD added"  -> Infrastructure gains a destination-side change;
//     spec unchanged; want NO error (Infrastructure drift does not participate)
//   "source FD removed"     -> Infrastructure drops a source FD (post-cleanup);
//     spec unchanged; want NO error
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run 'TestRecordFailureDomainMappings|TestEnsureMappingImmutable' -v`
Expected: FAIL — functions undefined / status never populated.

- [ ] **Step 3: Implement**

In the preflight success path, store the pairs returned by `resolveFailureDomainMappings` into `migration.Status.FailureDomainMappings` (the caller persists status as it already does for conditions).

`ensureMappingImmutable` must **not** re-resolve from Infrastructure (finding 2): Infrastructure legitimately changes during the migration (destination vCenter/FDs added, source FDs removed), and re-resolving against it would reject those normal transitions. Instead:

- Return nil while `Status.FailureDomainMappings` is unset (pre-preflight).
- Derive requested pairs from the spec alone: `Spec.FailureDomainMappings` when present; when omitted, the recorded status pairs remain the effective (inferred) mapping.
- Compare requested vs recorded **order-insensitively** (finding 7): key both lists by `Source` into `map[string]string` and compare the maps — `reflect.DeepEqual` on the raw lists would reject reordered-but-equivalent entries.
- Return `fmt.Errorf("failureDomainMappings are immutable after preflight; ...")` on drift.

Call it in Reconcile before phase dispatch.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestRecordFailureDomainMappings|TestEnsureMappingImmutable' -v`
Expected: PASS. Then `go test ./internal/controller/ -v` — full controller suite PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/
git commit -m "Record resolved failure domain mappings and enforce immutability"
```

---

### Task 4: Mapping-driven worker MachineSet creation

**Files:**
- Modify: `internal/openshift/machines.go` (new method near `GetMachineSetsByVCenter`)
- Test: `internal/openshift/machines_test.go` (table test for the matcher)
- Modify: `internal/controller/vmwarecloudfoundationmigration_controller.go` (`ensureWorkloadMigrated` Step 1, ~lines 680–735)
- Test: `internal/controller/workload_migration_test.go` (Ginkgo; extend the existing workload migration suite — see `workload_migration_rollout_test.go` for fixtures)

**Interfaces:**
- Consumes: `Status.FailureDomainMappings` (Task 3), `FailureDomainMapping` (Task 1), `GetVCenterByServer` (Task 2).
- Produces: `func (m *MachineManager) GetMachineSetsByFailureDomain(ctx context.Context, vcenterServer string, fd *configv1.VSpherePlatformFailureDomainSpec) ([]*machinev1beta1.MachineSet, error)`

- [ ] **Step 1: Write the failing matcher test**

```go
// TestGetMachineSetsByFailureDomain — fake machine client with machinesets whose
// provider specs have Workspace {Server, Datacenter, ResourcePool}:
//   "matches datacenter and resource pool" -> only the machineset with both matching returned
//   "empty resource pool in FD matches by datacenter" -> all machinesets in that datacenter returned
//   "no match" -> empty result, no error
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/openshift/ -run TestGetMachineSetsByFailureDomain -v`
Expected: FAIL — method undefined.

- [ ] **Step 3: Implement the matcher**

Wrap `GetMachineSetsByVCenter` (server match already handled there); for the returned set, `extractVSphereProviderSpec` and keep machinesets whose `Workspace.Datacenter == fd.Topology.Datacenter` and (`fd.Topology.ResourcePool == ""` or `Workspace.ResourcePool == fd.Topology.ResourcePool`). machinev1beta1 Machine/MachineSet specs have no `FailureDomain` field, so the provider-spec Workspace is the only linkage.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/openshift/ -run TestGetMachineSetsByFailureDomain -v`
Expected: PASS.

- [ ] **Step 5: Write the failing Ginkgo scenarios**

```go
// Context("with multiple failure domains and mappings")
//   Preflight passes with 2 source FDs / 2 destination FDs and a bijection.
//   Expect: for each destination FD, one destination MachineSet per source
//   MachineSet in its mapped source FD (so heterogeneous provider specs are
//   preserved — settled design decision), each copying that source MachineSet's
//   provider spec (VM size) with the template from the destination FD's
//   topology.template; the FD's destination replicas equal the exact summed
//   replica count of its mapped source FD (not the even split).
// It("keeps source failure domain assignments disjoint")
//   Two source FDs in the same datacenter with distinct resource pools, each
//   owning different machinesets. Expect each destination FD to inherit exactly
//   its own machinesets' provider specs and replica counts (no double counting).
// It("preserves an explicit zero replica total")
//   Mapped source FD's machinesets are scaled to 0. Expect the destination FD's
//   total replicas to be 0 (no MachineSet forced to 1).
// It("errors when a mapped source FD has no source MachineSets")
//   Mapping references a source FD with no machinesets in the fake client.
//   Expect reconcile error containing "no source MachineSets found for failure domain".
```

- [ ] **Step 6: Run tests to verify they fail**

Run: `KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" go test ./internal/controller/ -v -ginkgo.focus="failure domain mapping"`
Expected: FAIL — even split still used; collision still silently skipped.

- [ ] **Step 7: Rework `ensureWorkloadMigrated` Step 1**

Replace the even-split block: read the effective mapping from `migration.Status.FailureDomainMappings`; for each destination FD, look up its mapping and the source FD object (from Infrastructure); `GetMachineSetsByFailureDomain(ctx, sourceServer, sourceFD)` where `sourceServer` is the source FD's own `.Server` (select the vCenter with `GetVCenterByServer` when a client is needed — never `GetSourceVCenter` post-preflight). Error `"no source MachineSets found for failure domain %q"` when empty — distinct from a matched-but-zero replica total, which is preserved exactly with no minimum-1 floor (finding 6).

Create one destination MachineSet per matched source MachineSet (settled design): each copies its source MachineSet's provider spec (VM size, disks, etc.); the VM template and target topology always come from the destination FD (`fd.Topology`, as `CreateWorkerMachineSet` already takes). Naming: the first is `workerMachineSetName(infraID, fd.Name)`; subsequent copies append `-2`, `-3`, … (these names are the same ones collision-checked at preflight in Task 2). Track claimed source MachineSet names across destination FDs and error `"source MachineSet %q assigned to multiple failure domains"` on overlap (runtime guard for finding 4).

On the existing-MachineSet path, extract its provider spec: if `Workspace.Server == fd.Server` keep skipping (idempotent); otherwise error with the collision message.

- [ ] **Step 8: Run tests to verify they pass**

Run: `KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path)" go test ./internal/controller/ -v`
Expected: full controller suite PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/openshift/machines.go internal/openshift/machines_test.go internal/controller/
git commit -m "Create worker MachineSets from failure domain mappings"
```

---

### Task 5: Full verification

**Files:**
- Modify: `docs/failure-domain-mapping-analysis.md` (already corrected for the CEL expression and template/replica semantics; add one line under the design section noting it is implemented)

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Verify the whole repo**

Run: `make fmt vet lint` then `make test`
Expected: all PASS, no lint errors.

- [ ] **Step 2: Note implementation in the analysis doc and commit**

```bash
git add docs/failure-domain-mapping-analysis.md
git commit -m "Mark failure domain mapping design as implemented"
```
