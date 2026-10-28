# vSphere IPI QA reference

Background only. The release checklist is [vsphere-ipi-qa-test-plan.md](vsphere-ipi-qa-test-plan.md) (also available as HTML). This file does not define additional release tests.

**Scope:** Test `vcf-migration-operator` against OpenShift clusters installed on vSphere with the installer-provisioned infrastructure (IPI) workflow. The installer research defines realistic cluster shapes and fixtures; this is not a test plan for `../installer`.

**Review baseline:** Operator `e5b4a4610`, excluding PR #105 (merge `1a8e5c7ba` and its five commits). The applicable implementation/API baseline is its first parent `c70df0965`; subsequent non-#105 changes do not modify `internal/`, `test/`, or the Makefile. Do not use #105's `nodeMigration` fields, VMotion modes, status records, or examples as acceptance criteria. Use the pre-#105 CRD and sample when qualifying that baseline; do not deploy current HEAD and describe it as an excluded-#105 build.

**Installer reference snapshot:** 2026-10-01, `../installer` at `6d44cca7d8`. Installation references below are 4.22 documentation, not a newly verified support matrix. Re-check release-specific support and feature-gate availability before selecting a CI image.


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
| Source Infrastructure already has multiple vCenters | `validateSourceVCenterCount` requires exactly one source vCenter during preflight. | **Negative boundary test.** Reject before destination setup; do not confuse the temporary multi-vCenter configuration during migration with a supported multi-vCenter source. |
| Multiple source/target failure domains | `resolveFailureDomainMappings` requires a complete one-to-one mapping; omission is accepted only for one source and one destination FD. `validateFailureDomainServers` requires all target FDs to use one vCenter. | **Positive:** multiple FDs with equal source/target counts and explicit mappings on one target vCenter. **Negative:** fan-in, fan-out, incomplete mappings, and multiple target vCenters. |
| Source or target host-group zones / compute-cluster regions | `validateFailureDomainTypes` rejects `HostGroup` zones and `ComputeCluster` regions on both sides. | **Negative boundary test.** Reject during preflight before destination setup; do not require unsupported tag placement. |
| Single-node OpenShift (SNO) on vSphere IPI | The reviewed vSphere IPI guide documents the three-node compact shape; this research did not establish a vSphere IPI SNO support procedure. | **Unconfirmed; not a baseline.** Only add after confirming the exact supported installation method and operator CPMS/etcd behavior. |
| Multi-network failure domains (`VSphereMultiNetworks`) | `updateMachineSetProviderSpec` rebuilds the network device list from every target network; `TestUpdateMachineSetProviderSpecNetworks` covers this behavior. | **Component coverage plus conditional live overlay.** Verify network count/order/names and node connectivity on an eligible release; this does not establish support for static per-host addressing. |
| Load-balancer/DNS/VIP configuration (`loadBalancer`, `dnsRecordsType`, dual-stack `apiVIPs`/`ingressVIPs`) | The installer supports UserManaged load balancers, external DNS records, and dual-stack VIPs; the operator never reads or reconfigures any of them — migration only re-points the vSphere workspace fields of the machine provider specs. | **Post-migration health, not a migration feature.** Fold into the L1/L2 oracle: VIPs still resolve, ingress is reachable, and the LB object is unchanged. |
| Static per-host networking (`hosts[]`, TechPreview) | The installer applies per-host network configuration at install time; the operator has no model for it. | **Out of scope.** Do not build baseline runs on TechPreview host networking. |
| Per-machine disk settings (`diskType`, `dataDisks`) | Replacement workers clone source provider settings; VM recreation does not migrate disk contents. | **Configuration oracle only:** verify requested disk settings on new VMs. Do not expect local data to survive replacement; use stateless fixtures, with no vSphere CSI PVs. |

The operator also requires the `VSphereMultiVCenterDay2` feature gate during preflight. Run live tests only on a release where the gate is supported and enabled. Capture cluster version and `FeatureGate/cluster` state with every result. Do not silently substitute an older release or TechPreview configuration and call that equivalent coverage.


## 3. Existing test coverage and the main gap

Coverage is not starting from zero:

- `internal/controller/` has focused tests for the reconciliation phases, preflight, destination initialization, workload/CPMS rollout, and final readiness. `internal/controller/suite_test.go` runs the Ginkgo suite against envtest; vCenter is not provisioned by envtest.
- `internal/openshift/` and `internal/vsphere/` have manager/session/tag/folder tests, including simulator- or fake-backed cases. These establish component behavior, not a real OpenShift/vCenter migration.
- `test/e2e/e2e_test.go` runs on Kind and currently checks that the manager starts and serves metrics. It does **not** create a migration resource or migrate an IPI cluster.
- `docs/plans/control-plane-migration-coverage-plan.md` records control-plane migration acceptance coverage work. Keep its CPMS/quorum assertions as the detailed source of truth when maintaining those tests.
- `../release/ci-operator/step-registry/vcf-migration/` registers an actual Bash-based live migration workflow. It provisions a source vSphere IPI cluster, deploys the CI-built operator, creates a migration CR with target failure domains, and waits for all six migration conditions in order. Its final verifier checks ClusterOperator health, all nodes Ready, Infrastructure references only target vCenters, all non-deleting Machines use the target vCenter, and migration `Ready=True`. This is substantive positive-path integration coverage; Bash is only its implementation language.
- `../release/ci-operator/config/openshift/vcf-migration-operator/` configures that workflow for OCP 4.20, 4.21, 4.22, 5.0, and 5.1, all with `VSphereMultiVCenterDay2=true`. These are CI configurations, not proof of customer support or successful qualification on every release. The workflow selects exactly one target vCenter; it does not declare an explicit topology-cardinality matrix.
- Prow results quoted by the original plan were checked on 2026-10-04, not in this review. See the historical-evidence limitation in section 6; do not report those results as current job health.

**Largest QE evidence gap:** current, retained version-by-version live results and explicit negative-path/recovery/topology qualification. A live workflow already exists, and the current component suite includes substantial recovery/safety tests. Neither test-source presence nor historical CI summaries establish current live qualification. Keep the existing workflow and attach fresh scenario-specific evidence.


## 7. Decisions needed to make the matrix authoritative

1. Which OCP release is the minimum supported live-test version, and is `VSphereMultiVCenterDay2` stable or gated for that release?
2. Any proposed expansion beyond one source vCenter/datacenter and one target vCenter requires an implementation change and separate qualification; current rejection is not an unresolved test expectation.
3. HostGroup zones and ComputeCluster regions remain rejected; do not include them in positive release coverage without a contract change.
4. Is SNO supported through vSphere IPI and by this operator, or should it remain explicitly out of scope?
5. Which of CI-configured OCP versions 4.20, 4.21, 4.22, 5.0, and 5.1 are customer-supported, and what per-version green-run cadence is required for release qualification? The release CI is configured under `../release`; configured variants alone do not establish the product support matrix.

Until these are settled, the **implementation envelope to qualify** is a healthy, feature-gate-eligible source with one source vCenter/datacenter, an IPI HA or three-node compact shape, and one distinct target vCenter with complete one-to-one source/target FD mappings (implicit only for a single pair). This describes the test contract, not a product support certification.


## 10. Automated coverage and remaining qualification

The former 64.7% aggregate and 0% stage/session coverage claims are stale. The applicable baseline includes expanded tests in PR #222; this review inspected test sources but did not rerun a coverage profile. Do not substitute a commit-message percentage for measured evidence.

| Area | Existing evidence | Remaining live oracle |
|---|---|---|
| Multi-site configuration and source cleanup | `source_cleanup_test.go`, `mutation_deny_test.go`, `reconcile_errors_test.go` | Correct credentials/config retention, pod readiness and safe partial-failure recovery on a real cluster. |
| Mapping and destination/source safety | `failuredomains_test.go`, `recorded_destination_safety_test.go`, `destination_provider_validation_test.go`, `retention_safety_test.go` | Per-source replica preservation, no unrelated deletion, and recovery without adopting foreign capacity. |
| Rollout, restart and stall bounds | `workload_migration_rollout_test.go`, `workload_migration_recovery_test.go`, `execution_safety_test.go`, `stall_bounds_test.go` | CPMS/etcd availability, bounded failure reporting, safe retry and convergence after recovery. |
| Worker provider specs and vSphere helpers | `internal/openshift/machines_gaps_test.go`, `machines_test.go`; `internal/vsphere/session_gaps_test.go` | Real placement, networking, privileges and session behavior. |

These files demonstrate automated scenarios exist, not that every behavior or branch is covered. Envtest/fake clients and simulator tests do not qualify live migration. Kind E2E remains manager smoke coverage.

Refresh coverage on the selected **pre-#105 baseline**, recording the commit and command. Exclude live E2E packages and provide envtest assets (as `make test` does):

```sh
make test  # sets up envtest and writes cover.out; also runs code generation
go tool cover -func=cover.out
```

Do not use `go test ./...` as a unit-only command: it includes E2E packages. Keep the raw profile and test output with the result; a failed or unexecuted run is not coverage evidence.


