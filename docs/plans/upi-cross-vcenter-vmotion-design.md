# UPI Cross-vCenter VMotion: First-Release Design

**Status:** Consolidated engineering design draft, 2026-10-16. This revision records the discussion decisions and replaces the earlier exported draft. Design direction is agreed; the implementation and qualification gates in section 13 remain open. This is not customer-facing support guidance, an implementation claim, or a lab qualification report.

**Intended repository path:** `docs/plans/upi-cross-vcenter-vmotion-design.md`.

**Document authority:** This is the working implementation contract. [The companion document](upi-node-migration-design.md) is background only and must not introduce competing requirements. Interim review/checklist proposals are superseded where they conflict with this revision.

## 1. Goal and delivery boundary

Relocate existing unmanaged UPI node VMs to a different vCenter without creating replacement cluster-node VMs. Preserve existing Node and verified VM identity, addressing, and address families.

The first delivery retains the narrow baseline:

- Three dedicated control-plane nodes plus workers; no SNO, compact/dual-role, or unclassified-node expansion.
- One source vCenter/datacenter and one destination vCenter, with exactly one destination failure domain (FD) for the plan.
- One NIC per node VM; administrator-prepared destination networking that supports the existing configuration. No readdressing, address-family conversion, subnet cutover, or network repair.
- Explicit `VMotion` selection and the same explicitly selected `Hot` or `Cold` mode for workers and control plane. No `Auto`, mixed modes, or automatic fallback.
- One node workflow and one mutating VM action in flight cluster-wide. Workers precede control-plane nodes; use a stable order within each role.
- Explicit target host/pool/datastore in the execution specification. Use qualified native compatibility checks, not a custom placement optimizer or exhaustive hardware validator.
- No MachineSet/CPMS lifecycle operations on this path. Excluding Machine-managed VMotion is a first-release policy, not a claim that vSphere cannot move those VMs.

**No mandatory temporary probe VM for UPI VMotion.** No probe template, probe DHCP/static address configuration, or image-import step is required for this path. Do not remove or repurpose legacy template fields or change the existing probe in the recreation path.

**Address-family-neutral preservation:** there is no arbitrary IPv4-only restriction. Qualify the IPv4, IPv6, and dual-stack combinations intended for support; do not claim untested combinations work.

**Intended initial targets:** OCP 5.0 and the vSphere 8/9 families, using one API-driven workflow where applicable. Discover actual source/selected destination ESXi versions/builds and record them in qualification. This intent does not establish every patch level, cross-major direction, DRS/pool combination, or ESXi pairing as supported.

## 2. Compatibility and safety invariants

Retain `migration.openshift.io/v1alpha1`, `VmwareCloudFoundationMigration`, and Namespaced scope. Only `openshift-vcf-migration/cluster` is executable. Check both namespace and name before migration side effects, including preparation. Other CR locations receive an actionable unsupported-location result. This does not require restricting the manager cache.

An omitted optional `nodeMigration` selection retains the existing legacy path. Unsupported explicit VMotion requests must not fall through to MachineSet/CPMS replacement. The API-only schema baseline, static defaults/validation, status fields, and wire decisions are recorded in [the Node Migration API reference](../dev/node-migration-api.md). This API foundation does not implement dispatch or runtime authorization; VMotion execution remains unsupported until the backend work and qualification gates below are complete.

The invariants are:

1. Bind execution to CR UID plus stable execution/plan/action identities; resource generation alone is not action identity.
2. Freeze source/destination identities, intended placement/mode, and Node identities before configuration mutation. Pause/resume does not create another execution. Reject unapproved plan changes.
3. Durably acknowledge a single-winner submission claim before a mutating VM request. Missing task history or process restart never authorizes blind replay.
4. Retain separate shutdown, relocation, and power-on outcomes. A new power-on task must not erase relocation success. Task references are interpreted only on their owning vCenter.
5. Hot never shuts down or powers off the VM. Cold uses the selected graceful-only procedure; no hard power-off escalation or automatic mode fallback.
6. Preserve eviction/PDB/quorum protections and role-specific health gates. Admission of another disruption is distinct from observation or explicitly authorized recovery of the current node.
7. Preserve pre-existing cordons. Restore only migration-owned scheduling changes, while mutation authorization permits it.
8. No source cleanup while VM action outcome, intervention, identity, relevant inventory drift, or required completion evidence is unresolved.
9. No automatic source recovery power-on, VM deletion, network repair, or rollback engine in the first release.
10. Do not store credentials, TLS private material, ServiceLocator secrets, or full task output in status/logs. Fingerprint only sanitized non-secret operation inputs.

## 3. Existing phases and UPI dispatch

Retain the reviewed phase sequence:

```text
InfrastructurePrepared -> DestinationInitialized -> MultiSiteConfigured
    -> WorkloadMigrated -> SourceCleaned -> Ready
```

Do not introduce `DestinationImageImported` for UPI VMotion. An earlier assessment's phase claim did not describe the reviewed branch.

Dispatch UPI VMotion before legacy-only assumptions, template/probe requirements, MachineSet/CPMS creation/scaling/deletion, and Machine-derived progress checks. Reuse helpers only after checking their inputs and lifecycle assumptions. Existing phases do not imply their current helper bodies already implement all requirements below.

The reviewed source baselines were PR #211 head `472571fdb5d2b1db38749ce64bdc0308848058a9`, its base `005663725efddde6cef12d77c3ac002a3f606125`, and namespace/finalizer PR #217 head `756e491a85ffa923dc520b23dbab14e73eb511c8`. Reconcile the implementation checkout with those foundations; avoid duplicating fixes already present upstream.

## 4. Shared preparation: add the new FD before either mode

```text
Persist source identities and the frozen plan
  -> initialize the destination as applicable
  -> add destination credentials, vCenter entry and new FD
  -> update multi-vCenter cloud-provider configuration
  -> establish qualified component readiness and operator availability
  -> begin Hot or Cold node execution
```

Add the destination; do not replace the source. Preserve source configuration/credentials until guarded cleanup. Verify same-name existing FD entries match the intended topology; presence of a name alone is not success.

Reuse/adapt `MultiSiteConfigured` preparation and `AddTargetVCenter`, which already adds vCenter/FD entries. Do not create a new phase solely for this shared prerequisite. Read-only feasibility analysis can use the intended target specification before Infrastructure mutation; disruptive node execution cannot bypass shared readiness.

### Configuration readiness

Use a small consumer set qualified for the intended OCP 5.0 build:

1. Confirm intended source-plus-destination writes/readback and the preparation/configuration revision being validated.
2. Apply the qualified affected-consumer reload/restart/rollout procedure, using existing helpers/owning operators where suitable.
3. Checkpoint preparation so readiness retries do not continually restart replacement pods without a configuration change.
4. Require the relevant components to exist, complete their applicable rollout/replacement checks, and have expected Ready instances.
5. Treat alternative selectors as discovery variants. Zero for an obsolete selector is harmless if a qualified alternative finds the component; zero across required-component discovery cannot pass. Errors are not absence.
6. Allow intentional absence. Under the retained storage boundary, vSphere CSI is `Removed` and vSphere CSI PVs are not supported; running CSI pods must not be required.

This is a qualified rollout/readiness proxy, not runtime configuration inspection inside every process. Qualification must demonstrate that the procedure applies configuration. Do not add a generic inspection framework or arbitrary sleep-as-proof. Recheck required health before nodes without restarting consumers per node.

## 5. Eligibility: plan-scoped ownership, not an empty inventory

Use three outcomes for the explicit UPI path:

- **Managed:** supported associations/verified identity establish Machine lifecycle ownership of a planned Node/VM. Block this first-release VMotion path as a scope policy.
- **VerifiedUnmanaged:** complete relevant reads and unique planned identities establish absence of Machine lifecycle ownership under the supported checks.
- **Indeterminate:** required reads fail, relevant associations cannot be resolved, or identity/ownership evidence conflicts. Block with specific evidence and a resolution path.

Do not require a pristine cluster or an empty Machine inventory. Clearly unrelated/inactive historical objects, zero-replica MachineSets and inactive CPMS are not automatically disqualifying. Use ordinary supported references and identities, not guesses based on age/name or a generic stale-resource detector.

Separately gate provisioning/replacement that can change the cluster's Node/VM inventory. A positive MachineSet desired count alone does not prove that current planned UPI nodes are Machine-managed, but potentially conflicting outstanding provisioning cannot be ignored. Require relevant activity settled/quiesced under a supported procedure.

An unhealthy Machine API or zero Running Machines alone is never proof of UPI. Do not automatically delete/adopt/disable Machine resources or clear Node associations.

Resolve all planned Nodes to their unique expected source VMs using the qualified providerID/BIOS-UUID identity contract, not an assumption that Node and VM names match. Freeze Node UID/providerID, VM identity, role, source placement, intended target mapping, and original scheduling state. Exact association fields/resource scope remain implementation-review work.

## 6. Native feasibility and per-node sequences

Use `CheckRelocate_Task` against the intended VM/specification; omit `testType` to request all native tests. The documented groups cover source/host checks, destination datastore visibility, NIC-target network visibility, and resource-pool support. Do not promote those group descriptions into an exhaustive version-independent compatibility guarantee.

Choose an explicit target host with a simple deterministic procedure and native checks. Do not interpret another candidate's pass as approval for the selected host. Revalidate changed identity, placement, or specifications. Do not build a custom capacity/placement optimizer.

### Result handling

- Applicable native fatal compatibility errors block.
- Nonfatal warnings are surfaced without automatically blocking merely because a warning exists. Independent supported-scope and safety requirements still apply.
- API/task failures and unavailable, mismatched, or uninterpretable evidence never authorize relocation.
- Inspect per-VM/host findings, not just task success or a nil request error. Qualify empty-result and optional-identity semantics for supported APIs.
- No warning catalog/approval engine or generic ignore-errors switch.

### Permissions and trust

Before disruption, establish credentials/trust and applicable rights on the relevant source/destination entities using the existing authorization-checking pattern. Check permission alone is insufficient: `CheckRelocate_Task` documents `System.View`, while actual relocation/shutdown/power-on require additional rights, including conditional target placement permissions.

Use the documented rights for the actual selected APIs/specification, not an Administrator-role requirement or role-management framework. Qualify rights dependent on destination registration/inheritance and recheck against actual entities where needed; do not claim unavailable future-entity checks have already succeeded.

### Hot

1. Resolve/verify identity, intended target specification, supported scope, credentials/trust and required rights.
2. Obtain native feasibility findings for that specification while the VM is powered on.
3. Complete protected drain and the independent OpenShift admission gates.
4. Revalidate identity/placement and rerun the same checker before the submission claim and relocation.
5. Observe the action and verify recovery. Never issue shutdown or power-off.

### Cold

1. Before shutdown, verify identity/current power, intended placement, graceful-shutdown prerequisites, credentials/trust/rights, and OpenShift admission gates.
2. Complete protected drain, use `ShutdownGuest`, and observe actual `poweredOff`.
3. Run the qualified native relocation check against the final intended specification in that actual state.
4. Claim/submit relocation, establish the authoritative destination VM, perform qualified normal destination power-on, and verify recovery.

Do not use a powered-on check as a Cold pass/fail gate without qualified applicability. `CheckRelocate_Task` has no hypothetical-power-state argument; `CheckMigrate_Task.state` is not a simulation flag. Do not build a hypothetical Cold/boot simulator or require an unqualified cross-vCenter pre-move power-on checker.

Qualify the Cold/boot procedure for the supported combinations. This design accepts a documented residual risk: a native check can fail after shutdown. The response is administrator recovery, not automatic source power-on. If a usable sequence cannot be qualified for a combination, do not enable Cold for it.

Normal destination power-on after successful Cold relocation remains automatic. Qualify the chosen API and observed-placement acceptance rules; a placement hint is not proof of the final host. Successful power-on is not guest/Node readiness.

## 7. Durable execution, pause and deadlines

Use the existing CR and leader election; no separate workflow service, per-VM lock resources, or unbounded journal. Leader election alone is not external-operation fencing.

### Bounded execution data

Persist:

- CR UID, stable execution/plan identity and sanitized immutable-plan fingerprint; frozen source/destination and Node/VM identities.
- Per-node bounded progress/outcomes for shutdown, relocation and power-on, plus original scheduling state.
- One active mutating action: stable ID/kind, node/VM identity, owning vCenter, intended operation, task reference where one exists, timestamps/deadline and sanitized outcome/reason.

Preserve relocation success when power-on is recorded. `ShutdownGuest` supplies no task reference: track its intent/uncertainty and actual power state. Keep detailed diagnostics in logs, not an ever-growing status history. Final field names, sizes and state labels require API review.

### Claim once, then observe

1. Read the authoritative CR and observe any known action/VM state.
2. Validate current ownership, state, plan and action gates.
3. Persist and acknowledge a conflict-protected single-winner action claim meaning submission may have begun.
4. Only the reconciliation that newly wins and acknowledges that claim may send the mutating VM request.
5. Persist the task reference/observation for that same action, then observe and verify its outcome.

A later reconcile that sees a claimed action without a task reference must not submit again. Establish the outcome from authoritative evidence or require intervention. Accept conservative intervention if the crash was after claim but before the call reached vCenter. There is no atomic Kubernetes/vCenter transaction or exactly-once guarantee.

Use resourceVersion conflict handling and CR UID/execution/action identity to merge results. Do not discard an active result solely because pause changed generation; do not let stale writers overwrite terminal outcomes or a newer action. Conditions still represent observations/spec freshness, not commands to replay steps. Keep the frozen plan distinct from spec generation and completion-bound spec evidence.

Audit SDK/transport retry behavior for mutating operations as part of qualification; it must not silently reintroduce ambiguous-request replay. Read-only feasibility checks and idempotent configuration updates do not require a new per-VM lock system.

### Pause policy A

- `Paused` authorizes no new VM-action claims or other new migration mutations, but permits observation/status persistence for already claimed/submitted actions.
- A claim that wins before pause can still reach vCenter; pause is not instantaneous cancellation.
- If Cold relocation finishes before destination power-on is claimed, the VM may remain off until resume. No finish-current-node exception is implied.
- `Running` allows eligible normal progression; it does not clear intervention or authorize replay of an unknown action.
- Preserve mutation guards while separating read-only observation/action-result persistence. Do not loosen all existing guards globally to implement UPI observation.

### Shutdown and deadlines

Use `ShutdownGuest` only for first-release graceful shutdown, with prerequisites verified before Cold disruption. Observe actual power state; do not treat request return as shutdown completion. Never escalate automatically to hard power-off or another shutdown mechanism.

Use a bounded operator-defined shutdown deadline, with its value established during qualification rather than a new per-migration setting. On expiry, block further actions and report intervention while continuing observation. A late completion is evidence to record, not automatic reset/retry authorization. Timeout does not prove cancellation/inactivity. Other exact action/deadline policies and values remain implementation work.

## 8. Control-plane integration

Preserve the existing three voting members and bind expected member IDs to planned Node identities through a qualified mapping. Relocation must not add/remove/recreate/promote members or become a Machine-replacement workflow.

Direct authenticated etcd health reads are in scope, subject to integration/security review. Use one in-process member-specific read checker following the native health-read pattern; no new health service, helper Job, pod exec or log/message scraping. Internal CEO code is a precedent, not a supported remote interface or a promise that private controller packages can be imported unchanged.

Take fresh bounded observations at action boundaries; an old pass after pause/restart does not authorize another action:

- Before control-plane disruption: three expected Nodes and voting members healthy, working API access, no conflicting rollout/maintenance.
- After drain and before shutdown/relocation: fresh evidence still permits the action.
- During selected Cold absence: the selected member is expected absent; the remaining two must be healthy.
- After return: same Node/member recovery and full three-member health before another node.
- Additional failure: block new disruption and cleanup; keep observing and use only explicitly authorized current-node recovery. Do not confuse new-node admission with recovery authorization or override pause A.

Use standard cordon/eviction and respect PDB/quorum guards. Do not write MCO private drain annotations, borrow Machine-deletion hooks, or bypass guards. Static-pod mirror objects are handled by the normal drain mechanism; drain itself is not etcd shutdown.

`EtcdMembersAvailable=True` alone can mean majority health, not all three healthy. Node Ready, aggregate ClusterOperator Available and administrator checkboxes do not replace required member evidence. Unavailable/ambiguous health evidence blocks control-plane admission actionably.

Read-only RPCs do not imply a read-only credential. Client credential authority, delivery/rotation, precise RBAC, member binding, freshness and supported-version integration require review/qualification. This design does not grant access to etcd secrets or control-plane pod execution.

## 9. Manual recovery and deletion handoff

First-release failure recovery is administrator-performed VM recovery in vCenter, followed by one explicit execution/action-bound recheck/resume request on the existing CR. No operator recovery power-action engine, new recovery CR, approval service, automatic rollback, or automatic source power-on.

Expose the blocked node/action, known location/power, owning vCenter/task, known/unknown outcome and missing evidence. A recovery request is an instruction to revalidate, not proof or a force-clear flag.

On recheck:

1. Validate that the request refers to the blocked execution/action; reject stale or consumed acknowledgement for another action.
2. Establish authoritative action outcome/inactivity and unique expected VM identity/location/power.
3. Verify preserved Node identity, applicable health and unchanged plan/relevant inventory. Unresolved uncertainty stays blocked.
4. A source VM restored after failed relocation can enter an explicitly authorized retry with a new action ID and normal admission gates. A healthy destination is verified without relocating again. Preserve prior evidence.
5. Pause A still prohibits new mutations until `Running`; changing state alone, editing status, or deleting conditions is not intervention reset.

The exact optional typed request and authorization require API/security review. Reuse the migration-CR RBAC boundary for the request and administrator vCenter permissions for manual repair; do not add a separate approval system by default.

The finalizer retains the CR/execution record until resolution or deliberate administrator deletion handoff. Deletion does not automatically cancel relocation, power VMs, delete VMs or roll back. PR #217's `migration.openshift.io/allow-deletion: "true"` is a deletion-handoff mechanism, not recovery/reset or proof of success. Completion/spec-bound deletion protection must account for the actual final API.

Keep normal deletion handling separate from paused observation. The reviewed PR #217 deletion path does not run normal readiness/progress/probe-cleanup progression; do not silently generalize new UPI pause behavior to legacy/deletion behavior. Necessary task evidence and safe handoff details remain qualification work.

Future automatic source recovery is a desired deferred direction, not an implementation commitment or release schedule.

## 10. Operator survivability

Use native Kubernetes availability mechanisms:

- Two operator replicas, using existing leader election plus the durable action guard.
- Required pod anti-affinity across distinct Nodes, with correct labels and existing authorized placement. Do not silently add control-plane tolerations or force placement.
- A matching PDB with `minAvailable: 1`, respected by normal eviction/drain.
- Before disrupting a Node, a nonterminating Ready operator replica on another healthy Node; otherwise wait/block before disruption.

Do not build a scheduler/capacity simulator. This costs an extra operator pod and capacity on two eligible Nodes, not a blanket two-worker rule. With one eligible Node, provide suitable capacity/placement before its disruption.

Qualify standby readiness, leader eviction/takeover, persisted-action resume without duplicate submission, install/OLM packaging, and a rolling strategy compatible with the placement/availability boundary. A PDB is not a guarantee against involuntary node/hypervisor/network failure or every rollout/direct deletion. Durable restart handling and independent component disruption protections remain required.

## 11. Completion and source cleanup

### Node completion

Require:

- Known required VM action outcomes; authoritative expected VM powered on at intended FD/qualified placement.
- Preserved VM identity, Node UID/providerID, intended mapping and required addressing.
- Node Ready plus fresh post-operation kubelet/API activity. A new Node Lease renewal after a post-operation observation baseline is the native candidate; an unchanged pre-move Ready condition is insufficient. Qualify identity/freshness/deadline semantics without assuming clock comparisons alone prove freshness.
- Restoration of migration-owned scheduling changes only, preserving pre-existing cordons and respecting pause/mutation authorization.
- For control plane, same member participation/full three-member recovery and required API health before another node.

This is minimal operational recovery evidence, not certification of every network path. Administrator-provided continuity remains a prerequisite. Failure/timeout follows the observation/intervention/recovery contract; no extra probe or network-repair scope.

### Before source cleanup

- Every frozen entry is verified complete; no active/unknown VM action or unresolved intervention.
- Re-list live in-scope Nodes and relevant ownership/provisioning evidence. Added, replaced, removed, or unexpectedly changed relevant identities/roles/relationships are drift: block, do not expand the plan automatically.
- Reverify intended destination placement and no live in-scope source node dependency. This is not an exhaustive audit of all non-node vCenter devices/VMs.
- Use frozen persisted source vCenter/FD identity and configured-destination exclusion guards, never discover source from mutated `VCenters[0]`. The reviewed base already persists source identity before cleanup and guards destinations; adapt/reuse that fix.
- Require current execution, `Running`, and non-deletion mutation authorization.

### Retryable cleanup, not rollback

1. Remove only intended operator-managed source configuration/credential references through adapted existing helpers, preserving destination and unrelated entries. Do not delete node VMs.
2. Retain frozen source identity and bounded progress. Read actual state on retry; already absent source entries are complete only with expected destination state intact. Retry incomplete idempotent configuration changes, not unknown VM actions.
3. Apply the qualified final reload/rollout/readiness procedure after configuration changes; do not repeatedly restart consumers on every retry.
4. Set `SourceCleaned`/`Ready` only after intended cleanup and final required health/readiness complete for the current execution. Partial removals or successful API writes alone do not mean success.

Recovery acknowledgement and deletion handoff do not independently authorize cleanup. Finalizer release follows its separate completion/handoff policy.

`SourceCleaned` means intended cluster references were removed. It does not certify that source vCenter or external LB/DNS infrastructure can be decommissioned. Administrator-managed API/MCS/Ingress dependencies remain documented prerequisites; no generic external dependency-discovery, port-scanning or repair framework is introduced.

## 12. Acceptance and failure qualification

### API, ownership and legacy compatibility

- Omitted selection retains legacy recreation behavior, including its probe/template handling. Explicit UPI VMotion never creates replacement node VMs or drives MachineSet/CPMS lifecycle changes.
- UPI admission does not require a probe/template/DHCP/static-probe option; no arbitrary IPv4-only check or network-family conversion.
- Only canonical namespace/name can execute; a competing CR cannot produce a second mutating workflow.
- Round-trip all bounded action/status fields; pause-generation changes preserve matching action observations; stale writers cannot overwrite terminal/newer action state.

### Native checks and submission uncertainty

- Successful check task with applicable fatal findings blocks; warning-only findings follow nonfatal policy; uninterpretable/mismatched results do not pass. Qualify empty results.
- Another host/spec passing cannot authorize the selected host/spec; changed placement/mapping invalidates old evidence.
- Missing known action privileges/trust blocks before disruption. Qualify registration-dependent permissions and selected power-on API behavior.
- Exercise claim/status-ack/call/task-persistence crash windows, leader handoff, and SDK retry behavior. Claimed/no-task uncertainty never triggers blind replay.
- Combined drill: submit, pause before result persistence, restart, and introduce another CR; observe/intervene on the original action, not a duplicate.

### Hot, Cold, pause and recovery

- Hot never issues shutdown/power-off. Cold observes actual off and performs the qualified actual-state check; a running check is not mislabeled a Cold/boot certificate.
- Graceful shutdown timeout/late completion never causes hard escalation or implicit reset.
- Pause before a claim prevents it; a prior winning claim may complete; results remain observable. Cold VM may remain off if power-on was not claimed.
- Relocation succeeds but power-on/recovery fails: retain relocation outcome and block further nodes/cleanup.
- Stale recovery acknowledgement cannot authorize a newer action. Source retry gets a new ID; already healthy destination is not relocated again. Recheck cannot bypass pause or become deletion/cleanup proof.

### Network prerequisite and readiness boundaries

- Qualification covers intended address families without conversion. Native network visibility is not presented as guest/L2 proof.
- Deliberately misconfigured continuity can remain inventory-visible; document that preflight does not universally detect it. If a moved node fails recovery, block completion/next-node/cleanup and use administrator intervention, not network repair.
- Missing required consumer, stale/failed rollout or read error blocks; alternate selector absence alone does not. Removed CSI is allowed absent. Retries do not endlessly restart consumers.
- Relevant ownership ambiguity/provisioning blocks; clearly unrelated inactive historical resources do not automatically block.

### Operator, control plane and cleanup

- Evict the active operator during protected drain; successor reconciles the same action. Missing eligible off-node Ready replica blocks before shutdown; PDB refusal is not bypassed.
- Verify fresh member-ID-bound health before/during/after Cold; inject an additional member failure while the selected member is off. Preserve observation and explicit recovery rules without new disruption or membership mutation.
- VM power-on with stale/no Node heartbeat, wrong identity or failed member recovery cannot complete the node.
- Add/replace a source-side Node after freeze; cleanup blocks. Expected migration updates must be distinguished from unapproved drift.
- Crash between source-reference removals; frozen source identity survives, destination remains intact, only remaining safe steps retry. Final consumer failure means no `Ready`.
- Pause/deletion/ownership change between cleanup steps blocks new mutations. No node VM deletion or implicit infrastructure retirement.

Run repository-required generation, formatting, lint, unit/simulator and bundle validation when implementing. Unit/simulator success does not replace cross-vCenter lab qualification.

## 13. Implementation and qualification gates still open

The design choices above are settled directions, not completed engineering work. Before enablement, record evidence for:

- **G1 — API/status:** the API-only engine/mode/recovery/status schema baseline is recorded in the [Node Migration API reference](../dev/node-migration-api.md) and exercised through API-server tests. Runtime compatibility, immutable-plan enforcement, status merge/conflict rules and supported object-size bounds remain open.
- **G2 — vSphere integration:** actual `CheckRelocate_Task` result handling/empty semantics, source/target references, deterministic host selection, SDK retry behavior, selected power-on API/result/placement contract, and applicable action/inheritance permissions.
- **G3 — Supported combinations:** exact OCP build, vCenter/ESXi versions and cross-major directions, DRS/pool/boot-device combinations and existing address families qualified for Hot/Cold. No exclusions or support guarantees inferred from an API's minimum version alone.
- **G4 — etcd integration/security:** credential authority/delivery/rotation, precise RBAC, expected member-to-Node binding, bounded reads/polling/freshness, guard-respecting drain and supported OCP integration.
- **G5 — Configuration/availability:** exact OCP 5.0 consumers and reliable reload/readiness proxy; operator standby readiness, label/anti-affinity/PDB/rolling strategy, OLM/direct-install manifests and eviction/failover evidence.
- **G6 — Runtime evidence/deadlines:** qualified shutdown and other action deadlines, lease/identity/freshness implementation, topology/address verification and late-completion behavior. No invented universal timeout values.
- **G7 — Recovery/handoff:** final action-bound API/permissions, acknowledgement consumption/audit, authoritative outcome/inactivity evidence, supported recovery states, and deletion/spec-bound completion integration.
- **G8 — Cleanup:** precise source references, actual-state idempotency/partial-failure handling, final readiness and live dependency/drift checks, with focused failure tests.

Do not silently replace any missing evidence with success, a checkbox, a generic ignore switch, or a claim of universal safety.

## References

- [Original design PR](https://github.com/openshift/vcf-migration-operator/pull/211)
- [Namespace/finalizer foundation](https://github.com/openshift/vcf-migration-operator/pull/217)
- [Reviewed source cleanup foundation](https://github.com/openshift/vcf-migration-operator/blob/005663725efddde6cef12d77c3ac002a3f606125/internal/controller/vmwarecloudfoundationmigration_controller.go)
- [Reviewed preflight/authorization foundation](https://github.com/openshift/vcf-migration-operator/blob/005663725efddde6cef12d77c3ac002a3f606125/internal/controller/preflight.go)
- [vSphere provisioning checker](https://developer.broadcom.com/xapis/vsphere-web-services-api/latest/vim.vm.check.ProvisioningChecker.html)
- [Native findings](https://developer.broadcom.com/xapis/vsphere-web-services-api/latest/vim.vm.check.Result.html)
- [Native test groups](https://developer.broadcom.com/xapis/vsphere-web-services-api/latest/vim.vm.check.TestType.html)
- [VM relocation, shutdown and power APIs](https://developer.broadcom.com/xapis/vsphere-web-services-api/latest/vim.VirtualMachine.html)
- [Relocation specification](https://developer.broadcom.com/xapis/vsphere-web-services-api/latest/vim.vm.RelocateSpec.html)
- [Reviewed etcd member-health reporting](https://github.com/openshift/cluster-etcd-operator/blob/04b4210b9c8681cbbb3849682a5966ed383d3376/pkg/operator/etcdmemberscontroller/etcdmemberscontroller.go)
- [Reviewed etcd health-read pattern](https://github.com/openshift/cluster-etcd-operator/blob/04b4210b9c8681cbbb3849682a5966ed383d3376/pkg/etcdcli/health.go)
- [Leader election/fencing limitation](https://raw.githubusercontent.com/kubernetes/client-go/master/tools/leaderelection/leaderelection.go)
- [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#spec-and-status)
- [Node Lease heartbeats](https://kubernetes.io/docs/concepts/architecture/leases/)
- [Disruptions/PDBs](https://kubernetes.io/docs/concepts/workloads/pods/disruptions/)
- [Pod placement/anti-affinity](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/)
