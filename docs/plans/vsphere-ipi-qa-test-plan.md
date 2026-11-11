# vSphere IPI migration QA/QE test plan

**Scope:** Test `vcf-migration-operator` against OpenShift clusters installed on vSphere with the installer-provisioned infrastructure (IPI) workflow. The installer research defines realistic cluster shapes and fixtures; this is not a test plan for `../installer`.

**Research snapshot:** 2026-10-01. Local installer checkout: `../installer` at `6d44cca7d8` (2026-09-28). OpenShift installation references below are 4.22 documentation. Re-check release-specific support and feature-gate availability before selecting a CI image.

## 1. What the installer can create

The vSphere IPI install configuration describes placement through failure domains. A failure domain includes a vCenter `server`, region and zone, and topology such as datacenter, compute cluster, datastore, network(s), and optional resource pool/folder/template. The checked-out installer schema also models `regionType`/`zoneType`; the supported pairings include datacenter → compute-cluster and compute-cluster → host-group. `VCenters` has a schema maximum of three. A schema field alone does not establish that every combination is a supported release configuration, so CI must use the support and feature-gate rules for its OCP release.

The installer code creates MachineSets for a machine pool's selected zones; when no zones are specified it uses the configured failure domains. The installer-provisioned topologies relevant to migration QE are:

| Install shape | IPI characteristics | Migration value |
|---|---|---|
| **Standard HA, one zone** | Conventional baseline: three control-plane nodes plus worker nodes, all in one failure domain. One vCenter/datacenter/compute cluster. | Lowest-cost live migration smoke test; validates the end-to-end path without placement complexity. |
| **Three-node compact** | Three control-plane nodes, zero worker replicas; control-plane nodes are schedulable. Documented vSphere installation shape. | High-value test: exercises a real control-plane-only cluster, where an accidental worker/CPMS assumption can strand the migration. |
| **Multiple failure domains / zones** | Machine pools can target more than one zone. Failure domains may span compute clusters and datacenters; the vSphere guide documents multi-region/zone configuration. | Validates source MachineSets in more than one placement domain and destination mapping/tagging. |
| **Host-group zones** | Installer schema has `regionType: ComputeCluster`, `zoneType: HostGroup`, and `topology.hostGroup`. Verify release support/feature gate before provisioning. | Candidate only: the operator currently has no host-group-specific validation or tag attachment path (see §2). Do not count as supported migration coverage yet. |
| **Multiple vCenters at install time** | Installer schema permits up to three vCenter entries and failure domains carry a server. Whether a particular cross-vCenter initial install is supported is release/feature-gate dependent. | Separate from this operator's main source shape. Resolve the product support boundary before adding it to the positive source matrix. |

The IPI network and storage configuration can add further dimensions (DHCP/static addressing, one or more networks, load-balancer/DNS setup, CSI). Do not multiply the topology matrix by all of them. Use the ordinary supported IPI network configuration for the baseline; add a focused static/multi-network case only if this operator promises it. The operator currently blocks migration when vSphere CSI-backed PVs exist, and preflight hard-requires `ClusterCSIDriver/csi.vsphere.vmware.com` managementState to be `Removed` (while `Storage/cluster` managementState is advisory only, warning at most), so a successful baseline must not quietly depend on a vSphere CSI volume.

## 2. Compatibility boundary in this repository

These are findings from the current code, not assumptions to bake into an idealized test fixture:

| Source or destination shape | Current repo evidence | QA disposition |
|---|---|---|
| One source vCenter with exactly one source datacenter | `runPreflightChecks` / `validatePreflightVSphere` in `internal/controller/preflight.go` requires one source datacenter. | **Positive baseline.** Keep the initial source topology inside this boundary. |
| Source uses multiple datacenters on its source vCenter | Preflight returns an error when `sourceVC.Datacenters` has more than one entry. | **Negative boundary test.** Confirm failure occurs in `InfrastructurePrepared`, before destination setup or other mutation. Do not describe this as supported migration. |
| Source Infrastructure already has multiple vCenters | `InfrastructureManager.GetSourceVCenter` in `internal/openshift/infrastructure.go` returns `VCenters[0]`; preflight checks datacenter count, but does not reject multiple source vCenter entries. Cleanup is keyed to a source server. | **Open compatibility risk.** Add a test that exposes the ambiguity. Until product intent and code agree, the safe expected behavior is fail closed before mutation; a passing happy-path test must not imply all existing source vCenters are migrated. |
| One or more target failure domains on one or more target vCenters | The migration spec accepts a slice of failure domains; preflight checks the `VSphereMultiVCenterDay2` gate and validates target inventory/privileges per FD. | **Positive coverage.** Test multiple target FDs and, on an eligible OCP release, more than one target vCenter. |
| Target host-group zones | `internal/vsphere/tags.go` defines region/zone tag categories for datacenters and compute clusters; current target validation checks datacenter, compute cluster, datastore, networks, optional resource pool/folder, and template, but not `topology.hostGroup`. | **Not supported by evidence today.** Keep out of positive migration tests. Add an explicit fail-fast test or first implement host-group-aware validation/tagging. |
| Single-node OpenShift (SNO) on vSphere IPI | The reviewed vSphere IPI guide documents the three-node compact shape; this research did not establish a vSphere IPI SNO support procedure. | **Unconfirmed; not a baseline.** Only add after confirming the exact supported installation method and operator CPMS/etcd behavior. |
| Multi-network failure domains (`VSphereMultiNetworks`) | The installer/config schema accepts up to 10 networks per FD plus `VSpherePlatformNodeNetworking` for internal/external addressing; `updateMachineSetProviderSpec` in `internal/openshift/machines.go` wires only `Networks[0]` into target machines and silently drops the rest. | **Silent-truncation risk.** Baseline uses one network per FD. If a multi-network source is in scope, either pin the expected single-network behavior or add a fail-closed preflight test. |
| Load-balancer/DNS/VIP configuration (`loadBalancer`, `dnsRecordsType`, dual-stack `apiVIPs`/`ingressVIPs`) | The installer supports UserManaged load balancers, external DNS records, and dual-stack VIPs; the operator never reads or reconfigures any of them — migration only re-points the vSphere workspace fields of the machine provider specs. | **Post-migration health, not a migration feature.** Fold into the L1/L2 oracle: VIPs still resolve, ingress is reachable, and the LB object is unchanged. |
| Static per-host networking (`hosts[]`, TechPreview) | The installer applies per-host network configuration at install time; the operator has no model for it. | **Out of scope.** Do not build baseline runs on TechPreview host networking. |
| Per-machine disk settings (`diskType`, up to 29 `dataDisks` per machine pool with provisioning mode) | The operator clones the source MachineSet provider spec, so disks travel verbatim; nothing validates target datastore capacity or provisioning for them. | **Add to the L1 oracle.** If the source worker pool uses data disks, verify they attach and survive the rollout. |

The operator also requires the `VSphereMultiVCenterDay2` feature gate during preflight. Run live tests only on a release where the gate is supported and enabled. Capture cluster version and `FeatureGate/cluster` state with every result. Do not silently substitute an older release or TechPreview configuration and call that equivalent coverage.

## 3. Existing test coverage and the main gap

Coverage is not starting from zero:

- `internal/controller/` has focused tests for the reconciliation phases, preflight, destination initialization, workload/CPMS rollout, and final readiness. `internal/controller/suite_test.go` runs the Ginkgo suite against envtest; vCenter is not provisioned by envtest.
- `internal/openshift/` and `internal/vsphere/` have manager/session/tag/folder tests, including simulator- or fake-backed cases. These establish component behavior, not a real OpenShift/vCenter migration.
- `test/e2e/e2e_test.go` runs on Kind and currently checks that the manager starts and serves metrics. It does **not** create a migration resource or migrate an IPI cluster.
- `docs/plans/control-plane-migration-coverage-plan.md` records control-plane migration acceptance coverage work. Keep its CPMS/quorum assertions as the detailed source of truth when maintaining those tests.
- `../release/ci-operator/step-registry/vcf-migration/` registers an actual Bash-based live migration workflow. It provisions a source vSphere IPI cluster, deploys the CI-built operator, creates a migration CR with target failure domains, and waits for all six migration conditions in order. Its final verifier checks ClusterOperator health, all nodes Ready, Infrastructure references only target vCenters, all non-deleting Machines use the target vCenter, and migration `Ready=True`. This is substantive positive-path integration coverage; Bash is only its implementation language.
- `../release/ci-operator/config/openshift/vcf-migration-operator/` configures that workflow for OCP 4.20, 4.21, 4.22, 5.0, and 5.1, all with `VSphereMultiVCenterDay2=true`. These are CI configurations, not proof of customer support or successful qualification on every release. The workflow selects exactly one target vCenter; it does not declare an explicit topology-cardinality matrix.
- Prow evidence checked 2026-10-04: the 5.1 job's latest three runs all failed during cluster installation, before migration steps; two share a `machine-config` degraded/cluster-initialization timeout signature, below the automated permafail threshold. The 4.21 job reports one successful run; no public job history was available for 4.20, 4.22, or 5.0. The recent 5.1 failures are not evidence of an operator migration regression, but those runs do not qualify migration behavior.

**Largest QE gap:** not the absence of a live end-to-end workflow. It is the lack of sustained green, version-by-version qualification and negative-path/recovery/topology coverage. The existing workflow exercises a positive-path migration to a distinct target vCenter, but its PR job is optional and the latest 5.1 runs stopped before migration. Keep the working Bash workflow; close the evidence and scenario gaps rather than converting it to another language.

## 4. Prioritized test matrix

### Tier 0 — required PR coverage (fast, deterministic)

| ID | Scenario | Required assertions |
|---|---|---|
| U1 | Baseline preflight: one source vCenter/datacenter; one valid target FD | Each prerequisite is checked; target inventory and privileges are validated; no later phase runs before `InfrastructurePrepared=True`. |
| U2 | Target topology validation, table-driven by FD | Empty/duplicate FD names, missing template, bad datacenter/cluster/datastore/network/resource pool/folder/template, invalid credentials, and missing privileges fail with the FD named in the error. No destination tags, MachineSets, Infrastructure edits, or source deletion occur on failure. |
| U3 | Unsupported source topology boundaries | Multiple source datacenters fails before mutation. Multiple source vCenters are explicitly rejected (or a separately approved multi-source contract is tested); never silently test only the first vCenter. |
| U4 | OCP readiness blockers | Missing/disabled `VSphereMultiVCenterDay2`, missing feature-gate status, active rollout blockers, and vSphere CSI PVs stop preflight as hard failures; upgrade in progress and unhealthy ClusterOperators are transient (requeue with `Progressing` until healthy), so QE assertions must distinguish stuck-in-progress from terminal failure. Non-vSphere PVs alone do not trigger the CSI-PV blocker. |
| U5 | Destination failure-domain composition | One FD; two FDs on one target server; two FDs on separate target servers. Verify vCenter entries are deduplicated by server, datacenters and FD names are retained, and unrelated/source FDs are not accidentally removed. |
| U6 | Idempotency and resume | Reconcile the same phase repeatedly and restart from persisted status. Existing tags/configuration are reused; repeated reconciliation creates no duplicate or destructive resources. Pause/resume is covered. |
| U7 | Worker migration guardrails | Replacement capacity becomes ready before source scale-down/deletion; a source MachineSet with positive or nil replicas cannot be deleted; transient errors and stuck deletions preserve safe state and report useful conditions/events. |
| U8 | Control-plane migration contract | CPMS is Active and targets exactly the requested destination FDs; non-RollingUpdate strategy, missing CPMS/template, stale observed generation, and incomplete readiness do not advance/clean up source. Keep quorum-safe replacement-before-removal assertions. |
| U9 | Final readiness | `Ready=True` only after target-only Infrastructure configuration, all expected nodes ready, operators healthy, MCPs converged, and the stability window completes; a health regression resets/blocks completion. |
| U10 | Unsupported host-group target | Until supported, a host-group FD must be rejected before any destination mutation. If host-group support is added, replace this with positive tag-placement and migration assertions. |

Run the existing controller/component suite for these changes; do not require a vCenter in every PR job. Any new coverage should fit the existing package's test style instead of creating a parallel test framework.

### Tier 1 — live IPI migration, required before release

| ID | Source IPI shape | Destination | Why this earns a live run |
|---|---|---|---|
| L1 | Standard HA, one source vCenter + one datacenter + one FD, three control-plane and at least two workers | One target FD on a distinct target vCenter | End-to-end happy path for the operator's primary use. |
| L2 | Three-node compact, three control-plane and zero workers | One target FD on a distinct target vCenter | Detects hidden assumptions about worker MachineSets and exercises CPMS/etcd replacement under the smallest documented vSphere HA shape. |
| L3 | Standard HA with source workers assigned across two failure domains in one datacenter | One target FD | Proves source MachineSets are selected and drained/deleted correctly when placement is not represented by one source cluster/zone. |
| L4 | Standard HA, one source FD | Two target FDs on the **same** target vCenter, separate compute clusters/zones | Proves multi-FD generation, zone tags and target provider specs without also changing credentials/server. Use both an even and an odd worker replica count over scheduled runs; assert the operator's declared distribution contract rather than assuming equal replicas. |
| L5 | Standard HA, one source FD | Target FDs on two target vCenters (same or different target regions, using supported feature-gate config) | Proves the product's multi-vCenter destination behavior: credentials keyed per server, Infrastructure entries, provider specs, tags, and source cleanup. |

**Recommended order:** make L1 stable first, add L2 next, then L4/L5. L3 is valuable but lower priority than exercising the target-side multi-FD behavior. Run L1 on every eligible PR or merge queue if capacity permits; run L2/L4/L5 nightly or before release until their cost and flake rate justify a tighter cadence. The registered CI workflow is closest to L1, but its scripts do not pin/assert the source node counts or target-FD count; inspect retained run artifacts before claiming exact L1 equivalence. It selects one target vCenter, so it does not cover L5.

### Tier 2 — conditional topology / resilience runs

Run only when the matching install shape is explicitly supported by both the selected OCP release and this operator:

- Source with multiple datacenters on one source vCenter: currently expected to fail preflight; convert to a positive scenario only after that guard/contract changes.
- Source spanning multiple vCenters: currently unresolved due to first-vCenter selection; gate on a defined source-migration contract.
- Host-group failure domains: positive test only after hostGroup is validated and region/zone tags attach to the correct vSphere objects.
- Static IP or multi-network IPI: one overlay run to verify that replacement Machine provider specs and node readiness preserve required addressing. Keep it out of the base matrix unless the operator owns that configuration.
- Target vCenter unavailable, invalid credentials, privilege revoked, transient Kubernetes API errors, controller restart mid-phase, and stalled worker/control-plane rollout. Verify no unsafe source removal and that retry/resume converges once the dependency recovers.

## 5. Live-test procedure and pass/fail oracle

1. Install a fresh source cluster using the official vSphere IPI workflow and a version with `VSphereMultiVCenterDay2` supported/enabled. Save the install-config (redacted), cluster version, failure domains, vCenter/datacenter topology, and control-plane/worker counts as run artifacts.
2. Verify the starting cluster is healthy and stable. For a successful migration scenario, ensure there are no vSphere CSI-backed PVs and the vSphere CSI management state satisfies preflight. Add a small stateless workload so post-migration API and scheduling checks are meaningful.
3. Deploy the operator and create its credentials Secret plus `VmwareCloudFoundationMigration` resource using the destination FDs. Keep source and target vCenters distinct in the mainline case.
4. Wait for the ordered conditions: `InfrastructurePrepared`, `DestinationInitialized`, `MultiSiteConfigured`, `WorkloadMigrated`, `SourceCleaned`, then `Ready`. Record condition transition times, events, controller logs, and resource snapshots. A timeout is a failure with phase-specific diagnostics, not a pass based only on eventual node health.
5. Verify all of the following before pass:
   - Migration CR `Ready=True` and every required phase condition succeeded.
   - Every expected control-plane and worker node is Ready; no unexpected node loss; the stateless workload remains available and can schedule.
   - ClusterOperators are Available and not Progressing/Degraded; MCPs are Updated and not Degraded.
   - From a client outside the cluster network, resolve the API and application-route DNS names to their expected addresses; probe the external API for readiness and the application route for its expected HTTP response. Verify the external load balancer's VIP/listener/backend configuration targets the intended, healthy API and ingress endpoints.
   - Control-plane rollout is complete and the source control-plane machines were not removed before their replacements were ready.
   - Destination MachineSets/Machines reference the intended vCenter, datacenter, compute cluster, datastore, network, and failure-domain names in their provider specs; node region/zone labels agree with the destination tags.
   - Infrastructure contains the intended destination vCenters/failure domains and no migrated source vCenter/failure domains after `SourceCleaned`; multi-target scenarios retain **all** targets.
   - No source MachineSet remains with desired replicas, and cleanup does not delete a target MachineSet or unrelated infrastructure entry.
6. On failure, retain the migration object/status, operator and cluster events/logs, all nodes/Machines/MachineSets/CPMS, Infrastructure and cloud-provider configuration, and vCenter task/event records. Preserve enough artifacts to determine whether the failure was install, preflight, operator, control-plane quorum, worker rollout, cleanup, or CI infrastructure.

## 6. CI gates and commands

- **PR gate:** existing `make test` (unit/controller envtest coverage); add focused deterministic regression tests with each bug fix. Keep vCenter credentials out of PR tests.
- **Kind smoke:** `make test-e2e` validates manager deployment/metrics, but is not the migration release gate.
- **Existing live integration job:** `../release/ci-operator/step-registry/vcf-migration/e2e/vcf-migration-e2e-workflow.yaml` runs the source IPI install, operator deployment, ordered condition waits, and final health/vCenter assertions described above. The 5.1 PR job is `optional: true` and manually triggerable. Periodic configs cover 4.20–5.1, but the 4.20, 4.21, and 5.0 schedules are annual; 4.22 is weekly; 5.1 is three times daily. Treat configured schedules as intent, not proof a run passed.
- **Current qualification evidence (2026-10-04):** Prow reports 3/3 latest 5.1 periodic runs failed before the migration workflow, during IPI cluster initialization; two show the same machine-config degraded timeout. Permafail detection returned false (2/3 matches; 3/3 required). Prow reports one 4.21 success and no matching public history for 4.20, 4.22, or 5.0. Investigate/restore green 5.1 install runs and collect at least one successful full migration per customer-supported OCP release before counting that release as qualified.
- **Strict release gate:** require successful L1 migration plus the final cluster/vCenter assertions on every customer-supported OCP version. Require L2 and multi-failure-domain/multi-target scenarios (L4/L5) on every version for which those configurations are claimed supported, or record an explicit approved exception. A skipped, optional, pre-migration install failure, or merely configured periodic is not a pass. Record OCP version, feature gates, installer release, operator image digest, vCenter versions, topology, and job link with each result.

Repository commands:

```bash
make test
make test-e2e  # Kind manager smoke only; not a live vSphere migration test
```

## 7. Decisions needed to make the matrix authoritative

1. Which OCP release is the minimum supported live-test version, and is `VSphereMultiVCenterDay2` stable or gated for that release?
2. Are multi-datacenter or multi-vCenter **source** clusters supported inputs, or must preflight reject them? Current code rejects multiple source datacenters but selects only the first source vCenter.
3. Is HostGroup topology in scope for migration? The installer schema supports it, but this repo has no host-group-specific validation/tagging evidence.
4. Is SNO supported through vSphere IPI and by this operator, or should it remain explicitly out of scope?
5. Which of CI-configured OCP versions 4.20, 4.21, 4.22, 5.0, and 5.1 are customer-supported, and what per-version green-run cadence is required for release qualification? The release CI is configured under `../release`; configured variants alone do not establish the product support matrix.

Until these are settled, report the supported positive envelope as: **a healthy, feature-gate-eligible source with one source vCenter/datacenter; a documented vSphere IPI HA or three-node compact shape; and explicit target failure domains whose topology is validated by this operator.** Do not infer support for every topology the installer schema can express.

## 8. Operator-stage execution and status oracle

Use this as the live QE runbook after installing the operator and preparing the disposable source/target vSphere environment. Start the migration only after reviewing the initial Infrastructure, MachineSets, vSphere credentials, and recovery plan. The CR must be named `cluster`, set to `Running`, and reference a target credentials Secret with `{server}.username` and `{server}.password` keys for every target vCenter. Source credentials must be available in `kube-system/vsphere-creds`.

| Gate | Expected status and cluster evidence |
|---|---|
| Start / admission | `Pending` does not start work. `Paused` reports `Ready=False`, reason `Paused`; a non-`cluster` object is ignored with `Accepted=False`, reason `UnsupportedName`. On first `Running` reconcile, `startTime` is set. |
| `InfrastructurePrepared` | Preflight requires the multi-vCenter feature gate, healthy/non-upgrading cluster, source vCenter connectivity and exactly one source datacenter, valid target credentials/topology/privileges, no vSphere CSI PVs, CSI driver `Removed`, and no interfering user autoscaler/MHC resources. A blocker must prevent destination setup; restore transient blockers and verify progress resumes. |
| `DestinationInitialized` | Target VM folder has the cluster-ownership tag; datacenter and cluster have the requested region/zone tags. Repeat reconcile/operator restart must not duplicate or damage them. |
| `MultiSiteConfigured` | Target entries exist in `Infrastructure/cluster`, `cloud-provider-config` (`openshift-config`), and `kube-system/vsphere-creds`; MCO and vSphere pods restart. While vSphere pods are unready the condition stays false with a waiting message; it completes after readiness. |
| `WorkloadMigrated` | Target MachineSets and their Machines/Nodes become ready before CPMS cutover. CPMS generation must be observed and rollout complete before source MachineSets scale to zero. Source Machines/Nodes must disappear before source MachineSets are deleted. Use status progress and events to check each wait. |
| `SourceCleaned` | Source vCenter/failure domains and credentials are removed from Infrastructure/configuration/secrets; vSphere pods restart; `{migration-name}-metadata` Secret is written in the migration namespace. |
| `Ready` | All ClusterOperators are healthy, all MachineConfigPools are converged, and only target vCenters remain. `Ready=True` requires six stable observations at least 30 seconds apart (about three minutes), not one healthy snapshot. |

Capture the status and non-secret cluster evidence at each gate:

```sh
NS=<migration-namespace>
oc get vcfm cluster -n "$NS" -o yaml
oc get infrastructure.config.openshift.io cluster -o yaml
oc get configmap cloud-provider-config -n openshift-config -o yaml
oc get machinesets -n openshift-machine-api -o wide
oc get controlplanemachineset.machine.openshift.io cluster -n openshift-machine-api -o yaml
oc get clusteroperators
oc get machineconfigpools
oc get events -n "$NS" --field-selector involvedObject.name=cluster --sort-by=.lastTimestamp
```

## 9. QA safety and edge acceptance

- **Replica distribution:** Run an uneven source-worker count over multiple target failure domains and compare actual per-domain replicas with the approved policy. Explicitly test fewer source replicas than target failure domains: the current implementation enforces at least one replica per target domain, which can increase the total target worker count. Resolve whether that is intended before release sign-off.
- **Pause is not rollback:** Verify pause/resume only in a disposable cluster. Pausing stops this controller from advancing; it does not undo already-applied changes or guarantee that external cluster operators stop an in-flight rollout. Never use it as a recovery or rollback procedure.
- **Sensitive metadata:** The metadata Secret contains vSphere credentials, has no owner reference, and is intentionally retained for teardown after the migration CR is deleted. Verify its name/label/key presence without printing values; restrict access and redact Secret data from logs, screenshots, and bug attachments.
- **Recovery evidence:** For each injected blocker, record the failing condition/message, event, external resource state, and requeue/recovery behavior. Confirm the next stage does not become complete before its prerequisites pass. Keep all disruptive fault injection and operator restarts on the isolated test cluster.

## 10. Coverage-profile findings and release-test priorities

A local `go test ./... -coverprofile=...` run passed all packages. The combined profile after the readiness tests below was 64.7%; package coverage was API 30.0%, controller 72.8%, metadata 92.3%, metrics 96.7%, OpenShift managers 76.8%, and vSphere 59.4%. Use these figures to target behavioral tests, not as a standalone release gate: generated API code and the uninstrumented manager process affect aggregate percentages.

Reproduce and refresh the baseline with:

```sh
go test ./... -coverprofile=/tmp/vcf-qa-cover.out
go tool cover -func=/tmp/vcf-qa-cover.out
```

The following unexecuted code paths are higher priority because they mutate cluster configuration or control destructive cutover. Coverage figures below are from that profile; they are not claims about every possible live/manual test.

| Priority | Code path (profile coverage) | Required automated coverage before customer rollout |
|---|---|---|
| P0 | `ensureMultiSiteConfigured` (0%) | Controller-level tests for target credentials, Infrastructure and cloud-provider ConfigMap updates, MCO/vSphere pod restarts, waiting while pods are unready, retry after partial failure, and idempotent re-reconcile. |
| P0 | `ensureSourceCleaned` (0%) | Tests that remove only source entries, retain target credentials/config, restart vSphere pods, create the metadata Secret, and recover safely from a failure partway through cleanup. |
| P0 | Worker creation/scale-down methods in `internal/openshift/machines.go` (`CreateWorkerMachineSet`, `ScaleMachineSet`, `updateMachineSetProviderSpec`, `CheckMachinesDeleted`, and `CheckNodesDeletedForMachines`: 0%) | `CheckMachinesReady` and `CheckNodesReady` now have table-driven fake-client coverage (100% function coverage) for complete, partial, empty, missing-resource, and list-error cases. Add corresponding tests for provider-spec/selector mapping, worker creation/scaling, and old-machine/node deletion. Retain an explicit safety oracle: source scale-down must not happen before target workers and control-plane rollout pass their readiness gates. |
| P1 | vSphere session lifecycle (`NewSession`, `GetOrCreate`, `Close`, `ClearSessions`) and `ListDatacenters` (0%) | Simulator-backed tests for connection/cache reuse, distinct-server credentials, cleanup after preflight, invalid sessions, and lookup errors. |
| P1 | `cmd/main.go` startup functions (0% in this profile) | Keep executable startup/health/metrics smoke coverage; do not add brittle unit tests for `main` solely to raise coverage. The current Kind E2E suite is a smoke check, not a migration-flow test. |

Controller phase coverage is uneven: preflight 75.0%, destination initialization 88.1%, worker migration 76.7%, rollout/scale-down 79.8%, final readiness 92.9%; the two P0 configuration/cleanup stages above are not exercised by the Go test profile. The release CI workflow supplies positive-path integration coverage, but the latest 5.1 runs failed before migration and only one 4.21 pass is currently reported. Until the P0 stage tests and reliable green live runs on each customer-supported release are in place, treat the suite as a useful component baseline rather than customer-release qualification.

### Tool evidence limitations

Defect prediction returned no files above its risk threshold; that is not evidence of defect absence. The coverage-gap scan supplied with the Go profile ranked uncovered `cmd/main.go`, vSphere session, and MachineManager code, consistent with the profile, but its target filter did not isolate the requested controller directory. A separate sublinear scan surfaced `.worktrees/` paths, so it is excluded from release decisions. Review tool suggestions against the checked-out test suite and measured profile before accepting them.

## References

### OpenShift / installer

- [OCP 4.22: Installer-provisioned infrastructure on vSphere](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/installing_on_vmware_vsphere/installer-provisioned-infrastructure)
- [OCP 4.22: vSphere installation configuration parameters](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/installing_on_vmware_vsphere/installation-config-parameters-vsphere)
- [OCP 4.22: multiple regions and zones on vSphere](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/installing_on_vmware_vsphere/post-install-vsphere-zones-regions-configuration)
- [OCP 4.22: three-node vSphere cluster](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/installing_on_vmware_vsphere/installing-vsphere-three-node)
- Installer checkout inspected: `../installer` commit `6d44cca7d8`, especially `pkg/types/vsphere/platform.go`, `pkg/types/vsphere/validation/platform.go`, and `pkg/asset/machines/vsphere/machinesets.go`.

### This repository

- `internal/controller/preflight.go` — feature gate, source-vCenter boundary, target inventory and CSI checks.
- `internal/openshift/infrastructure.go` — source selection and target/source Infrastructure updates.
- `internal/vsphere/tags.go` — destination topology and cluster-ownership tag behavior.
- `internal/openshift/machines.go` and `internal/controller/workload_migration_rollout_test.go` — MachineSet and CPMS rollout behavior.
- `internal/controller/suite_test.go`, `test/e2e/e2e_test.go` — envtest and current Kind E2E boundary.
- `docs/plans/control-plane-migration-coverage-plan.md`, `docs/plans/ci-e2e-testing.md` — existing coverage and live-CI planning notes.
- `docs/user/spec-examples.md` — current single- and multi-target-FD migration specs.

---

**QA summary:** Prioritize two real positive paths (standard HA and three-node compact) plus multi-target-failure-domain coverage. Treat installer-expressible but operator-unimplemented source/target shapes as compatibility boundaries, not as assumed coverage.

**Research caveat:** Release-level support for multi-vCenter IPI installation, HostGroup, SNO, and the multi-vCenter feature gate must be checked against the specific OpenShift release selected for a live run; the installer schema alone is not a support statement.
