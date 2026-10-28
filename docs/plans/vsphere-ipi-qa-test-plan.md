# vSphere IPI: pre-release test checklist

Run in the order below. Record **pass, fail, or not run** and an evidence link for each test and each listed variant. A skipped test or failed cluster installation is not a pass. P0 is highest priority; lower priority does not mean optional. Conditional live variants apply only to configurations claimed supported for this release.

**Test build:** use the pre-PR #105 implementation/API baseline `c70df0965` and its CRD and sample, not current HEAD. VMotion is excluded. Run live tests on every customer-supported OpenShift release with `VSphereMultiVCenterDay2` supported and enabled. If the release list or test build is not confirmed, get it confirmed before starting.

**Safety:** use disposable clusters for migration and failure tests. Do not use customer workloads or vSphere CSI volumes. Redact credentials from all evidence. Automated scenarios below require individual results; a green suite alone does not prove that an unimplemented case was tested. Ask the test owner for missing fixtures rather than marking those cases passed.

## P0 — safety and core migration

### 1. Run the automated regression tests — Automated

**Setup:** check out the test build and use the repository's envtest setup for unit/controller tests.

1. Run `make test` and save the output.
2. Check that the required tests actually ran, rather than being skipped.

**Pass:** the regression tests pass without skipped required cases. These tests use test APIs, fake clients and simulators; they do not replace the live migrations in tests 2–5.

### 2. Migrate a standard cluster — Live (L1)

**Setup:** three control-plane nodes and at least two workers; one source vCenter, datacenter and failure domain (FD: a named placement location); one FD on a different target vCenter. Use healthy nodes/operators, no vSphere CSI-backed PVs, and `ClusterCSIDriver/csi.vsphere.vmware.com` set to `Removed`. Deploy a small stateless application with an external route. Agree on a timeout before starting.

1. Save the OpenShift version, feature gates, operator image digest/source revision, CRD revision, installer revision, vCenter versions, source topology and node counts.
2. Create the target credentials Secret with `<target-server>.username` and `<target-server>.password`. Ensure source credentials exist in `kube-system/vsphere-creds`.
3. Create the baseline migration sample as `openshift-vcf-migration/cluster`, set `state: Running`, and configure the target inventory paths and an accessible template. For this single FD pair, omit mappings or explicitly map the source name to the target name.
4. Watch `oc get vcfm cluster -n openshift-vcf-migration -o yaml`. Verify conditions succeed in order: `InfrastructurePrepared`, `DestinationInitialized`, `MultiSiteConfigured`, `WorkloadMigrated`, `SourceCleaned`, `Ready`.
5. Check nodes, ClusterOperators, MachineConfigPools, Machines, MachineSets and the ControlPlaneMachineSet (CPMS). Verify placement in vCenter and the checks below.

**Pass — all must hold:**

- All six conditions are true within the agreed timeout. Save transition times, events and logs.
- Expected nodes are Ready; operators are Available, not Progressing or Degraded; MachineConfigPools are Updated and not Degraded.
- The application stays available and can schedule on destination nodes. No source control-plane machine is removed before its replacement is ready.
- Every destination machine uses the intended server, datacenter, compute cluster, datastore, networks and FD. Region/zone node labels match destination tags.
- From outside the cluster, API and route DNS resolve correctly, API readiness succeeds, and the route returns the expected response. External load-balancer configuration remains correct and its API/ingress backends are healthy.
- Infrastructure and cloud-provider configuration contain the intended destination configuration; migrated source entries are removed. No source MachineSet retains desired replicas. Target and unrelated resources survive cleanup.

**On failure:** keep the migration object, events/logs, node/Machine/MachineSet/CPMS snapshots, Infrastructure/cloud-provider configuration and vCenter tasks/events. A timeout is a failure, even if nodes later recover.

### 3. Migrate a compact cluster — Live, when supported (L2)

**Setup:** three schedulable control-plane nodes, zero workers, one source FD and one target FD on a different vCenter.

1. Repeat test 2 on this cluster.
2. Check control-plane availability throughout replacement and confirm the operator does not create worker capacity.

**Pass:** all test 2 checks pass with exactly the intended control-plane capacity and no added workers.

### 4. Migrate workers across mapped locations — Live, when supported (L3)

**Setup:** two source FDs in one datacenter, two target FDs on one different vCenter, uneven worker replica counts, and more than one source MachineSet in an FD. Source resource pools must not overlap.

1. Map every source FD name to exactly one target FD name.
2. Repeat test 2. Compare each source MachineSet's desired replicas and placement with its replacement.

**Pass:** all test 2 checks pass; each MachineSet's replica count is preserved and its replacement follows the named mapping, not array order. Source cleanup removes only migrated resources.

### 5. Migrate with empty worker locations — Live, when supported (L4)

**Setup:** multiple mapped source/target FDs; target FDs use separate compute clusters/zones on one target vCenter. Include a zero-replica source MachineSet and an FD with no worker MachineSet.

1. Repeat test 2 with complete one-to-one mappings, including the empty FD.
2. Check destination tags, node labels and every machine's placement.

**Pass:** all test 2 checks pass. Empty locations do not gain invented worker replicas. Each existing source MachineSet keeps its intended count.

### 6. Resolve mappings by name — Automated (F01, F08)

**Setup:** controller fixtures with source FDs `s1`, `s2`, `s3` and target FDs `d1`, `d2`, `d3`.

1. For one pair, test an omitted mapping and explicit `s1 -> d1`.
2. For two and three pairs, swap destinations and reorder both FD and mapping lists before preflight. Include several MachineSets in one source FD.

**Pass:** both single-pair forms record the same effective mapping. Valid multi-pair permutations succeed; worker and CPMS placement follows names, and status records the actual pairs.

### 7. Reject duplicate or incomplete mappings — Automated (F05, F07)

**Setup:** baseline CRD in envtest and matching controller fixtures.

1. Submit duplicate sources, duplicate destinations, and one source mapped to two destinations.
2. Test partial mappings, unused source/target FDs and unequal source/target counts. Include a source FD without workers.

**Pass:** admission rejects duplicate sources/destinations. Preflight rejects incomplete coverage that passes admission, before source mutation. Every FD must be covered exactly once.

### 8. Reject overlapping source placement — Automated (F09)

**Setup:** two source FDs in the same datacenter.

1. Test equal explicit resource pools, omitted pools resolving to the same pool, and distinct pools.
2. Test omitted pools under distinct compute-cluster paths.

**Pass:** overlapping effective pools are rejected. Distinct pools are eligible; different FD names or zone labels alone do not make shared pools safe.

### 9. Protect worker capacity and existing MachineSets — Automated (F10, F11)

**Setup:** controller fixtures; never create hundreds of live VMs for this test.

1. Add a source MachineSet outside all mapped FDs. Test positive, zero and omitted replicas, plus a malformed provider spec.
2. Test destination names that collide after sanitizing/truncating and a destination name already owned by an unrelated MachineSet.
3. Test otherwise valid fixtures producing 256 and 257 destination MachineSets.

**Pass:** positive or unresolved source capacity is never silently abandoned. Zero-replica/empty FDs do not gain capacity; omitted replicas follow Machine API defaults, not an assumed zero. Collisions, foreign ownership and more than 256 generated sets fail closed. The 256-set boundary succeeds when all other requirements are met.

### 10. Freeze recorded mappings — Automated (F13)

**Setup:** a migration that has completed preflight and stored effective mappings in status.

1. Change a pair, remove a pair, add a pair and substitute a source name, using separate cases.
2. Reconcile each accepted edit, then restore the recorded mappings.

**Pass:** edits that pass admission are blocked before phase side effects with `Accepted=False`, reason `MappingRejected`. Recorded mappings stay unchanged. Restoration clears the rejection; repeated reconciliation does not duplicate the rejection event for the same generation.

### 11. Prevent unsafe topology edits and identity overlap — Automated (F15, G01–G03)

**Setup:** isolated controller fixtures; use no production vCenter.

1. Set the target server equal to the source server.
2. Reuse a source FD name for a destination FD.
3. After preflight, separately change destination name, server, template, network, datastore and pool. Repeat before and after worker creation, retaining mappings and omitting them.

**Pass:** no case causes destructive source action or wrong-target mutation. Source identity/topology stays recoverable through cutover. Save the exact rejection/failure stage; do not assume preflight rejects every case. Unsafe acceptance blocks release.

### 12. Execute only the authoritative migration — Automated (S01)

**Setup:** baseline operator/controller fixture.

1. Create the correct name in a wrong namespace, then a wrong name in the correct namespace.
2. Create `openshift-vcf-migration/cluster` with a valid spec.

**Pass:** only the authoritative name/namespace executes. Others report `Accepted=False` with `UnsupportedName` or `UnsupportedNamespace`, without cluster or vCenter mutation.

### 13. Pause and resume without restarting migration — Automated (S02)

**Setup:** a multi-phase controller fixture.

1. Move through `Pending -> Running -> Paused -> Running`.
2. In a separate run, change `Running -> Pending` during work.

**Pass:** non-Running states stop new phase work; pause is visible in status. Resume continues from saved progress; start time is set once. Pending does not roll back. Already-submitted external machine operations may continue.

### 14. Stop stale execution — Automated and isolated live restart (S03)

**Setup:** a fake-client fixture that can interrupt a mutation, plus a disposable live migration.

1. During a multi-step phase, independently pause, delete, change generation, and delete/recreate the CR with a different UID.
2. Reconcile each phase repeatedly. Restart separately during worker creation, rollout and cleanup; verify persisted destination identity and provider specs before reuse.
3. In the live run, restart the operator mid-phase and verify progress after restart.

**Pass:** guarded mutations re-read permission from the current CR. Stale work cannot mutate resources or publish success. A recreated CR does not inherit the old execution identity. Restart resumes safely, reusing tags/configuration without duplicate resources, foreign MachineSet adoption or unsafe source deletion.

### 15. Report actual progress and stable completion — Automated (S04)

**Setup:** controller fixtures with controllable node/operator health and status-update conflicts.

1. Complete a normal migration, make health regress during the readiness window, and inject a status conflict in separate cases.

**Pass:** phase order, observed generations, counters and timestamps match resources. Completion time/hash appear only for completed work. Readiness waits for stable health; unrelated status updates survive conflict retries.

### 16. Retain incomplete records on deletion — Automated (S06, S07)

**Setup:** disposable fixtures for deletion before, during and after migration. Save evidence before testing the deletion override.

1. Delete incomplete migrations and completed migrations with matching, missing and mismatched completion hashes.
2. For an incomplete record, test annotation `migration.openshift.io/allow-deletion` absent, `false`, different casing, and exactly `"true"`.
3. Check the metadata Secret's expected name, labels and keys without printing values. Verify it has no owner reference, remains after CR deletion, and access is restricted; redact its data from artifacts.

**Pass:** `migration.openshift.io/record-retention` retains incomplete records with `Ready=False/DeletionBlocked`. Deletion stops new actions but does not roll back or cancel submitted tasks. Automatic release requires Ready, completion time and a hash matching the typed spec; deletion's generation change alone must not block a matching record. Only exact `"true"` overrides incomplete-record retention. Removing the record does not remove VMs or undo migration.

### 17. Block source cleanup until replacements are ready — Automated (U7)

**Setup:** worker migration fixtures with recorded destination MachineSets.

1. Test incomplete mappings, altered destination provider specs, unready replacement workers and incomplete control-plane rollout.
2. Attempt deletion of source MachineSets with positive and omitted replicas. Inject transient errors and stuck deletions.

**Pass:** source scale-down waits for complete mappings, validated destination identity/specs, ready replacement capacity and completed control-plane rollout. Source MachineSets with positive or omitted replicas are not deleted. Errors/stalls preserve safe capacity and report useful conditions/events.

### 18. Block incomplete control-plane replacement — Automated (U8)

**Setup:** CPMS controller fixtures.

1. Test a missing CPMS/template, non-RollingUpdate strategy, stale observed generation and incomplete readiness.
2. Restore a valid Active CPMS targeting exactly the requested destination FDs.

**Pass:** invalid or incomplete control-plane rollout cannot advance to source cleanup. Valid rollout uses the requested locations and preserves quorum by making replacements ready before removal.

### 19. Validate networking with temporary probe VMs — Automated and live inspection (U11)

**Setup:** vSphere simulator/controller fixtures; inspect both vCenters during the live runs in tests 2–5.

1. Test successful probes, clone/power-on/guest-network failures, missing address evidence, incompatible networks, timeout and cleanup failure.
2. After live preflight, check both vCenters for leftover probe VMs.

**Pass:** network failures produce actionable evidence and block destination initialization. Successful probes are removed; cleanup failures are visible, not silent leaks. Temporary probe creation is expected preflight activity, not permission to mutate source capacity.

## P1 — input validation, recovery and configuration

### 20. Reject unsupported topology before mutation — Automated (L5, A08, A09, G04)

**Setup:** controller fixtures and baseline CRD. Use valid inventory for everything except the variant being tested.

1. Test multiple source vCenters, multiple source datacenters and target FDs on different vCenters.
2. Test HostGroup zones and ComputeCluster regions separately on source and target.
3. At admission, test omitted affinity objects, valid Datacenter-region/ComputeCluster-zone objects, invalid enum values, missing HostGroup details and HostGroup details on a non-HostGroup zone. Check each HostGroup detail's 1–80 character boundary.

**Pass:** unsupported topology is rejected by preflight before destination setup/source mutation, even when admission accepts it. Malformed affinity unions fail admission. Supported affinity forms are accepted.

### 21. Validate state, required fields and strict unknown fields — Automated (A01, A10)

**Setup:** baseline CRD in envtest; do not use HEAD's VMotion schema.

1. Submit a minimal valid single-FD spec with state omitted, each valid state, an invalid string and an explicit empty string.
2. Submit `{}` and an object with `spec` omitted.
3. Submit an unknown field, including a PR #105-only field, with strict field validation. Also read back a non-strict request.

**Pass:** omitted state defaults to Pending; only Pending, Running and Paused are valid, and only Running executes. Empty/missing spec is rejected, not treated as a usable migration. Strict requests reject unknown fields; a pruned field in a non-strict request is not supported functionality.

### 22. Validate and recover credentials — Automated (A02, S08)

**Setup:** envtest/controller fixtures with non-secret placeholder credentials and controllable access errors.

1. Omit the credential reference; use an empty Secret object; test Secret name lengths 0, 1, 253 and 254.
2. Test omitted/empty namespace, explicit namespace, missing namespace/Secret, denied access and missing server-specific username/password keys.
3. During migration, rotate Secret data, change the reference, revoke access and restore it.

**Pass:** the reference is required and names are 1–253 characters. Omitted/empty namespace resolves to the CR namespace; explicit namespace works with access. Runtime credential errors fail safely and recover after correction. Reference edits honor generation guards. No credential values appear in logs or saved metadata.

### 23. Validate failure-domain names and inventory — Automated (A04–A07, G04)

**Setup:** baseline CRD plus controller/vSphere simulator fixtures. Change only one field at a time.

1. Omit each required FD field: name, region, zone, server and topology. Test empty strings and the length limits: name 256, region/zone 80, server 255; test each limit and limit+1.
2. Omit each required topology field: computeCluster, datacenter, datastore, networks and template. Test empty networks, empty template, duplicate FD names, whitespace/non-resolving servers and nonexistent inventory.
3. Test paths at 2048 and 2049 characters and malformed prefixes. Required prefixes are `/…/host/…` for clusters, `/…/datastore/…` for datastores, `/…/vm/…` for folders/templates, and `/…/host/…/Resources…` for pools. Test datacenter length 80 and 81, and an empty name.
4. Test omitted folder/pool, valid explicit values, and explicit paths to missing inventory.

**Pass:** schema-required fields and length/path constraints are enforced. Values that pass schema but do not resolve are rejected during runtime validation; duplicate FD names are rejected by preflight. Omitted optional folder/pool uses the operator's defaults. Every target FD requires an accessible template; schema acceptance alone never proves inventory exists.

### 24. Preserve networks and disk configuration — Automated; live overlay when supported (A05)

**Setup:** source provider specs with disk settings and targets with one and multiple networks. Use stateless workloads; local disk contents are not migrated.

1. Verify replacement worker specs retain requested `diskType` and `dataDisks` settings.
2. Verify all target networks appear in the original order. Test an empty network string and a nonexistent network.
3. Where multi-network migration is claimed supported, repeat test 2 with that network configuration and verify node/application connectivity and actual VM disk settings.

**Pass:** requested disk configuration and network count/order/names are correct. Invalid networks fail safely at runtime if admitted. Eligible live nodes and applications remain reachable. Do not expect replacement VMs to retain local data.

### 25. Reject ambiguous and unknown mapping inputs — Automated (F02–F03, F06)

**Setup:** source FDs `s1`, `s2` and target FDs `d1`, `d2`.

1. Omit mappings with multiple sources and one destination.
2. Omit mappings with multiple destinations before status mappings exist; separately submit `[]`.
3. Submit an unknown destination name, then an unknown source name.

**Pass:** ambiguous source cardinality fails preflight without mutation. Multiple destinations without mappings and explicit empty mappings fail admission. Unknown destination fails admission; unknown source fails preflight against Infrastructure.

### 26. Reuse and recover persisted mappings — Automated (F12, F14)

**Setup:** one fixture with recorded mappings and another older object past preflight without recorded mappings.

1. Reorder equivalent mappings, omit the mapping field, and restore the same entries. Separately set it to `[]`.
2. Recover the older object's mappings; restart the controller. Repeat with ambiguous Infrastructure and after source cleanup.

**Pass:** equivalent edits/omission keep using recorded pairs; `[]` fails admission. Older objects recover only unambiguous non-destination source mappings and persist them across restart. Ambiguous recovery fails closed and never treats a destination as a source.

### 27. Round-trip status without corrupting it — Automated (S05)

**Setup:** baseline CRD in envtest, using the status subresource.

1. Write/read conditions, worker/CPMS progress, mappings, sourceVCenter, workerMachineSetNames, timestamps and completionSpecHash; update spec and read status again.
2. Test zero/omitted counters, duplicate condition types, malformed conditions/timestamps, atomic replacement of status mappings/worker names and more than 256 worker names.
3. Test valid 64-character lowercase hexadecimal hashes and malformed hashes.

**Pass:** spec updates preserve status. Status schema enforces condition, timestamp, hash and worker-name bounds. Status mappings/names replace atomically. Zero counters may be omitted; the controller must produce meaningful progress even though the schema does not reject all negative counters.

### 28. Reject unsafe storage and missing feature gates — Automated

**Setup:** preflight fixtures, with an otherwise valid migration.

1. Add a vSphere CSI-backed PV.
2. Test missing/disabled `VSphereMultiVCenterDay2` and `ClusterCSIDriver/csi.vsphere.vmware.com` not set to Removed.
3. With those required checks satisfied, vary `Storage/cluster` management state and add a non-vSphere PV.
4. Test an upgrade in progress, unhealthy ClusterOperators and other rollout blockers; then restore health.

**Pass:** CSI PVs, an unavailable feature gate or the wrong ClusterCSIDriver state block preflight before mutation. Storage/cluster management state is advisory, not a substitute for the required CSI checks. Non-vSphere PVs alone do not trigger the CSI blocker. Upgrade/unhealthy-operator blockers wait safely and report transient errors as Retrying or ordinary waits as Progressing; no later phase runs early.

### 29. Recover from service failures without losing source capacity — Automated; isolated live fault run

**Setup:** disposable fixtures with recoverable faults and a timeout agreed before testing. For live faults, have an engineer able to restore access immediately.

1. Independently make the target vCenter unavailable, supply invalid credentials, revoke a required privilege and inject transient Kubernetes API errors. Restore each dependency.
2. Stall worker readiness and control-plane rollout separately; then allow them to recover.

**Pass:** no unsafe source removal or duplicate replacement capacity. Status/events identify the failing phase and distinguish retries, failure, ordinary waits and MappingRejected. Recoverable faults resume after correction. Invalid inputs require correction; `Failed` alone is not proof execution has stopped.

### 30. Place target FDs in multiple datacenters — Live, only when supported (A09)

**Setup:** one source datacenter, complete one-to-one mappings, and target FDs in different datacenters on one target vCenter. Run only if this target shape is claimed supported.

1. Repeat test 2 and verify each mapped target's inventory and actual VM placement.

**Pass:** all test 2 checks pass. The source-only single-datacenter restriction is not incorrectly applied to the destination.

## P2 — API size and update boundaries

### 31. Enforce failure-domain list limits — Automated (A03)

**Setup:** baseline CRD in envtest, unique FD names and valid mappings so unrelated rules do not mask results.

1. Submit an absent list and lists containing 0, 1, 64 and 65 FDs.
2. Patch/apply a replacement FD list and read the stored object.

**Pass:** the list is required and permits 1–64 entries. Replacement is atomic; entries are not silently merged by name.

### 32. Enforce mapping list and name limits — Automated (F04)

**Setup:** baseline CRD in envtest with matching valid destination lists.

1. Submit 1, 64 and 65 mapping entries.
2. Omit source/destination names individually; test empty, 1-character, 256-character and 257-character names.

**Pass:** mappings permit 1–64 entries; both names are required and must contain 1–256 characters. Invalid cases fail admission without starting migration.
