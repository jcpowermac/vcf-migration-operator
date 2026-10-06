# Research: Rollback feasibility when the new MachineSet / CPMS fails on a single master

Status: research only — no implementation. Open for review; no code changes
are proposed by this document.

Question: if the target (new) MachineSet or the ControlPlaneMachineSet (CPMS) rollout fails
on a single master, can the migration be rolled back, and how?

Note: there are no single-master clusters; "single master" means one master in an
N-master (3/5) control plane. "CPMS" in this repo = ControlPlaneMachineSet
(`machine.openshift.io/v1`, `cluster` in `openshift-machine-api`). Worker Machines
and MachineSets are `machine.openshift.io/v1beta1` (the CPMS embeds a v1beta1
machine template, which is easy to conflate with the CPMS's own version).

## Answer

Yes, rollback is feasible — but only while the migration is still in the
`WorkloadMigrated` phase, and today it is a manual process, not an operator feature.
`SourceCleaned` (source vCenter removed from cluster config) ends the pre-cleanup,
operator-supported rollback path. Recovery afterwards is manual, high-risk, and
unsupported (Section C) — not a true point of no return, but the end of anything
the operator can help with. A sketch of an operator-native `Rollback` state —
what it would take to make rollback an operator feature — follows the gaps list.

## Where the failure lands

Condition flow (`vmwarecloudfoundationmigration_controller.go`):

```
InfrastructurePrepared -> DestinationInitialized -> MultiSiteConfigured
  -> WorkloadMigrated -> SourceCleaned -> Ready
```

Within `ensureWorkloadMigrated` / `ensureWorkloadMigratedRolloutAndScaleDown`:

1. Target worker MachineSets created (`CreateWorkerMachineSet`), workers awaited.
2. CPMS spec re-pointed to target failure domains, state set to Active
   (`UpdateCPMSFailureDomain`, `internal/openshift/machines.go:235`). In-place update —
   the CPMS operator resolves topology (vCenter, datacenter, datastore, ...) from the
   Infrastructure resource.
3. Rollout awaited (`CheckControlPlaneRolloutStatus`, `machines.go:281`):
   complete only when `replicas > 0 && updatedReplicas == replicas &&
   readyReplicas == replicas`.
4. Source worker MachineSets scaled to 0, machines/nodes awaited deleted, empty
   source MachineSets deleted (steps 6–8).

A failure of the target worker MachineSets themselves (steps 1–2) is the easy
case: it stalls the migration before anything control-plane-related has changed.
Resume by fixing the root cause and deleting the stuck `Machine` (the MachineSet
controller recreates it to meet replica count), or roll back by deleting the
target worker MachineSets — nothing else has been changed at that point. Neither
is analyzed further here; the rest of this document is about the CPMS rollout
failure, which is the case where rollback becomes an actual question.

### Single-master failure state

The CPMS operator uses quorum-safe rolling replacement: create replacement machine,
wait for its node Ready, then delete the old machine. `UpdateCPMSFailureDomain`
enforces only the `RollingUpdate` strategy type (`machines.go:254` — other
strategies would never roll the control plane); the replacement ordering, the
node-Ready gate, and the old-machine deletion all come from the CPMS operator's
implementation, not from this operator (evidence and version caveats in
"Open questions / caveats").

Consequence: if one new master fails to become Ready, the old source master for that
machine is **not** deleted. If the failure hits the k-th of the N replacements, the
resulting cluster state is:

- (k−1) masters on target failure domains (completed replacements)
- (N−k+1) old masters still on source, including the stuck replacement's old machine
- 1 stuck new machine on target (provisioning failed / node NotReady / CPMS rollout stuck)
- etcd quorum intact

(Worst case, a failure on the final replacement — k=N — leaves N−1 masters on
target and 1 old master on source.)

Operator behavior on a stuck rollout: `CheckControlPlaneRolloutStatus` returns
incomplete → `RequeueAfter(30s)` forever with condition
`WorkloadMigrated=Progressing, "Control plane rolling out (updated/replicas,
ready/replicas)"`. No timeout, no failure reason, no auto-rollback. (The only stall
detection in the operator is for old-worker deletion: `oldWorkerStallDetail`
Warning events, rate-limited.)

## What is reversible at the failure moment

| Change made by migration | Reversible? |
|---|---|
| Target vCenter added to Infrastructure spec, cloud-provider-config, vsphere-creds (`ensureMultiSiteConfigured`) | Yes — additive. Source failure domains (index 0, `GetSourceFailureDomain`) remain present, so CPMS can still resolve source topology. |
| Target worker MachineSets + machines + VMs in target vCenter | Yes — scale to 0, delete; machine deletion removes VMs. Source worker MachineSets untouched until steps 6–8. |
| CPMS spec re-pointed to target FDs | Yes — spec-only change; can be re-pointed back to source FD names. |
| Already-replaced masters (old source machines deleted by CPMS operator) | Old VMs gone, but source vCenter is still fully configured → new source masters provision fine. |
| After `SourceCleaned` (`RemoveSourceVCenter` + config + creds removal) | **Not via the supported path** — source vCenter and its failure domains are removed from Infrastructure/cloud-provider-config/creds, so CPMS can no longer resolve source FD topology. End of the pre-cleanup rollback window; manual restoration (Section C) remains possible but is unsupported and high-risk. |
| After steps 6–8 (source workers scaled down + source MachineSets deleted) | Worker rollback window closes: source worker MachineSets must be recreated manually (source vCenter still configured until `SourceCleaned`). |

## Recovery options

### A. Resume (recommended; works today)

Fix the target vCenter root cause (template, storage, network, resource pool, quota),
then delete the stuck `Machine`. The CPMS operator recreates it to meet replica count.
The operator is idempotent — it re-derives phase progress from cluster state every
reconcile (`IsCPMSUpdatedForFailureDomains`, `CheckControlPlaneRolloutStatus`) and
continues. No CR change, no rollback. This is the designed path for transient
target-side failures.

### B. True rollback to source (manual; feasible before `SourceCleaned`)

1. Set `spec.state: Paused` **first**. While `Running`, step 3
   (`UpdateCPMSFailureDomain`) re-applies target FDs to the CPMS on any reconcile
   where `IsCPMSUpdatedForFailureDomains` no longer matches, so a manual re-point
   would be reverted. `Paused` makes the reconciler early-return.
   (Note: pausing the migration operator does NOT pause the CPMS rollout itself —
   the CPMS operator keeps working.)
2. Re-point CPMS `spec.template...failureDomains.vSphere` back to the source FD
   names (state stays `Active`). CPMS operator rolls the target masters back to
   source, quorum-safe, one at a time.
3. Scale down + delete target worker MachineSets (or leave them for forensics).
   Source worker MachineSets are still at full replicas.
4. Optionally strip the target vCenter from Infrastructure spec,
   cloud-provider-config, and vsphere-creds. No target-removal code path exists —
   manual today (the source-removal methods are server-parameterized and could be
   reused for the target server, but nothing wires them for it).
5. Delete the migration CR (or leave it Paused).

Caveats:

- Slow: a full second control-plane roll (one master replacement at a time).
- If the failure cause also breaks source-side provisioning, the rollback stalls the
  same way — but never worse than forward, because old machines persist until their
  replacements are Ready (caveat: an old machine whose own node has gone NotReady
  may be deleted to make room for a further replacement — see open questions).
- Edge case: if the failed machine's node was briefly Ready (so the old source
  machine was deleted) and then became unhealthy, the master is missing from source;
  quorum still holds (2/3 or 4/5). CPMS re-pointing to source still replaces it.

### C. After `SourceCleaned` (manual only, unsupported)

Restore the source vCenter entries **and** the original source failure-domain
entries in the Infrastructure spec (both are removed by `RemoveSourceVCenter`),
re-add the source vCenter entry to cloud-provider-config and the source credentials
to vsphere-creds, restart MCO/vSphere pods, then re-point CPMS to the source FD
names and recreate source worker MachineSets. High-risk manual surgery; not an
operator feature.

## Gaps if an operator-native rollback is ever wanted

1. No failure/timeout detection for a stalled CPMS rollout — infinite `Progressing`.
2. No rollback state in the CRD (only `Pending`/`Running`/`Paused`); the condition
   walk is forward-only and conditions are never un-set.
3. No rollback wiring for removing the target vCenter. The remove methods that
   exist (`RemoveSourceVCenter`, `RemoveSourceVCenterCreds`,
   `RemoveSourceVCenterFromConfig`) are parameterized by server and would
   mechanically work for the target — the gap is orchestration, not primitives.
4. No rollback handler; `Paused` is the only freeze point (pause/resume already
   covered by the QA test plan).
5. No webhooks guard the spec — helps manual rollback, but nothing prevents deleting
   a mid-migration CR.

## Proposed design: operator-native `Rollback` state

Research-level proposal — nothing below is implemented. It adds a `Rollback` value
to `MigrationState` (`Pending`/`Running`/`Paused` today) so that
`spec.state: Rollback` means "walk the migration backward". Every step reuses
manager primitives that already exist; the new work is orchestration, status
handling, and guards. It closes gaps 2–4 fully, gap 5 partially (webhook), and
gap 1 for the backward walk only.

### Entry guards

- Admitted only while `SourceCleaned=False` — the pre-cleanup window defined by
  the reversibility table. With `SourceCleaned=True` the reconciler sets
  `Ready=False`/reason `RollbackUnsupported`, emits a Warning event, and no-ops;
  post-cleanup recovery stays the manual Section C path.
- Pre-rollback gate before any mutation: source vCenter and failure domains still
  resolvable in Infrastructure, source credentials still in vsphere-creds (both
  hold by definition pre-`SourceCleaned`), CPMS strategy still `RollingUpdate`,
  cluster health acceptable. A failed gate blocks with a condition instead of a
  partial rewind.
- While `state=Rollback` the reconciler takes the rollback walk instead of the
  forward condition walk, so nothing re-applies target FDs — the Paused-first
  workaround from Section B step 1 becomes unnecessary. `Paused` still freezes
  both directions; the existing early-return in `Reconcile` precedes both walks.

### Rollback walk

Same philosophy as the forward `ensure*` functions: every step re-derives progress
from cluster state, so the walk is idempotent, restart-safe, and self-selecting —
a migration that died at forward Step 2 (workers never ready, CPMS never
re-pointed) finds R1/R2 already satisfied and moves straight to teardown.

- **R0 — recreate source worker MachineSets** (only when forward steps 6–8 already
  deleted them, i.e. `WorkloadMigrated=True` at failure): the mirror of forward
  Step 1 with the roles swapped. List surviving target worker MachineSets
  (`GetMachineSetsByVCenter` on the target server), take one as template, and
  `CreateWorkerMachineSet` re-pointed at the source failure domains
  (`updateMachineSetProviderSpec` accepts any FD spec). Replica splitting mirrors
  the forward math. Recreated sets get operator-style names
  (`workerMachineSetName`); the original installer names are gone with the deleted
  MachineSets — harmless, the Machines carry the identity that matters. Wait for
  machines and nodes Ready (`CheckMachinesReady`/`CheckNodesReady`) before
  touching masters: capacity first, for the same reason forward Step 2 gates
  Step 3.
- **R1 — re-point CPMS to source**: `UpdateCPMSFailureDomain` with the source FD
  names — the failure domains in Infrastructure that reference the source server
  (the code assumes a single source FD; `GetSourceFailureDomain` returns index 0).
  Idempotence via `IsCPMSUpdatedForFailureDomains(sourceFDNames)`. The CPMS
  operator then rolls the masters back with the quorum-safe semantics verified in
  "Open questions": replacement created first, old machine deleted only once the
  replacement is Ready, and an outdated NotReady machine deleted to make room —
  so a stuck target replacement does not block the backward roll.
- **R2 — wait for the backward control-plane rollout**: the same completion
  predicate as forward Step 5 (`CheckControlPlaneRolloutStatus`: replicas > 0,
  updated == replicas, ready == replicas), 30s requeues.
- **R3 — tear down target workers**: scale target worker MachineSets to 0, await
  machine and node deletion (`CheckMachinesDeleted`, `CheckNodesDeletedForMachines`),
  then delete the MachineSets (`DeleteMachineSetsByVCenter` on the target server) —
  the mirror of forward steps 6–8.
- **R4 — "TargetCleaned" (full rewind)**: strip the target vCenter entries and
  failure domains from Infrastructure, the vCenter entry from cloud-provider-config,
  and the target credentials from vsphere-creds. The existing remove methods are
  server-parameterized and work as-is for the target server
  (`RemoveSourceVCenter`/`RemoveSourceVCenterFromConfig`/`RemoveSourceVCenterCreds`;
  rename to `...ByServer` for clarity while wiring). Restart MCO + vSphere pods and
  wait for vSphere pod readiness — the mirror of the `ensureMultiSiteConfigured`
  tail.
- **R5 — completion**: `Ready=True`, reason `RolledBack`, event `MigrationRolledBack`.

Ordering rationale: masters roll back while both worker pools exist (R0 restores
source workers first when needed); target workers are deleted only after the
control plane is back on source; platform config is stripped only after the
workload is back. Each resource is undone only while it is the redundant copy.

### Status model

The forward walk is monotonic and conditions are never un-set today; rollback
becomes the first writer of False-after-True. `WorkloadMigrated` flips to
`False`/reason `RollingBack` when the walk starts undoing it;
`MultiSiteConfigured` flips when R4 begins. `DestinationInitialized` and
`InfrastructurePrepared` stay `True` — the destination vCenter objects they
describe still exist (see scope boundaries). Completion is reported through the
`Ready` condition; `spec.state` stays user-owned intent — `Rollback` is the ask,
`RolledBack` the answer. Setting `state: Running` after a completed rollback
restarts the forward walk from whatever conditions remain — a full re-migration,
by design.

### Stall handling

The backward walk would inherit the forward walk's wait-forever risk (gap 1), so
it ships with detection rather than the bug:

- Rate-limited Warning events with stall detail for a non-converging backward
  CPMS rollout or worker teardown, mirroring `oldWorkerStallDetail`.
- A `RollbackStalled` reason on `Ready=False` when nothing observable changes
  (CPMS status counters, machine phases) for a configurable deadline (default
  e.g. 30m). On stall the operator stops issuing changes and parks in the safest
  reached state; a human decides to fix-and-resume, extend the deadline, or take
  over manually. It never auto-resumes and never flips to forward.

### Scope boundaries

- Post-`SourceCleaned` stays unsupported (Section C remains the manual path).
- vCenter-side objects from the forward migration (VM folders, region/zone tags,
  cluster-ownership tags on the target) are not removed — they are inert without
  machines; a later destination-cleanup step could reuse the `internal/vsphere`
  primitives if wanted.
- The metadata secret that `ensureSourceCleaned` generates never exists on this
  path (`SourceCleaned` never ran).
- A webhook should validate state transitions (reject `Rollback` when
  `SourceCleaned=True`); the entry guard above is the belt, the webhook the
  suspenders (gap 5).

## Open questions / caveats

- CPMS replacement semantics come from the upstream
  cluster-control-plane-machine-set-operator, not this repo. Verified against its
  `main` at commit b438274f (2026-09-23), `pkg/controllers/controlplanemachineset/updates.go`:
  `reconcileMachineRollingUpdate` processes one machine index at a time with the
  surge capped at one machine; `createRollingUpdateReplacementMachines` creates the
  replacement first, observing the surge budget; `deleteReplacedMachines` deletes
  the outdated machine only once an updated (Ready, spec-matching) replacement
  exists for that index; `waitForReadyMachine` gates on node-level readiness
  (`MachineInfo.Ready`). Two edge-case caveats from the same code: (a) an
  **outdated** machine whose node has gone NotReady is deleted to make room for a
  further replacement even before its own replacement is Ready, so old machines
  persist only while they themselves stay Ready; and (b) a replacement whose node
  goes NotReady *after* its old machine was deleted is merely "pending" to the
  rollout logic and is not itself replaced (deleting it, as in option A, triggers a
  replacement). This evidence comes from `main`, not from OCP 4.18–4.21 release
  builds — validate against the specific target release before relying on option B.
- Multi-vCenter day-2 support requires the `VSphereMultiVCenterDay2` feature gate
  (preflight hard-fails without it); rollback to source assumes the multi-vCenter
  platform state remains valid.

## Key code references

- `internal/controller/vmwarecloudfoundationmigration_controller.go`
  - `Reconcile` (:175) — state checks, Paused early-return, condition walk
  - `ensureMultiSiteConfigured` (:543) — add target vCenter/FDs
  - `ensureWorkloadMigrated` (:649) — worker MS creation, CPMS update
  - `ensureWorkloadMigratedRolloutAndScaleDown` (:780) — rollout wait, source scale-down
  - `ensureSourceCleaned` (:917) — source removal (ends the operator-supported
    rollback window)
  - `oldWorkerStallDetail` (:1500s) — only existing stall detection
- `internal/openshift/machines.go`
  - `GetControlPlaneMachineSet` (:219)
  - `UpdateCPMSFailureDomain` (:235) — RollingUpdate enforcement, in-place CPMS update
  - `CheckControlPlaneRolloutStatus` (:281)
  - `IsCPMSUpdatedForFailureDomains` (:302)
- `internal/openshift/infrastructure.go` — `AddTargetVCenter` (:71),
  `RemoveSourceVCenter` (:140s), `GetSourceFailureDomain` (:55)
- `api/v1alpha1/vmwarecloudfoundationmigration_types.go` — `MigrationState`
  (Pending/Running/Paused), `FailureDomains` spec
