# UPI Node Migration — Background and Deferred Work

**Status:** Proposed replacement companion, consolidated 2026-10-16. Engineering background only; not a second implementation contract, product-support statement or qualification report.

**Intended repository path:** `docs/plans/upi-node-migration-design.md`.

## 1. One authoritative contract

The [UPI cross-vCenter Hot/Cold design](upi-cross-vcenter-vmotion-design.md) defines the first delivery, its accepted safety policies, acceptance cases and remaining implementation gates. This companion intentionally removes duplicate orchestration/API requirements from the earlier broad draft so implementers do not have to resolve conflicting instructions.

The [original broader draft remains available in source history](https://github.com/openshift/vcf-migration-operator/blob/472571fdb5d2b1db38749ce64bdc0308848058a9/docs/plans/upi-node-migration-design.md). Treat historical PoC notes and analyses as observations for their tested setup, not universal guarantees or evidence of unimplemented API features.

## 2. First delivery in brief

The first delivery relocates the same unmanaged UPI node VMs, preserving identity and existing networking, rather than provisioning replacement nodes.

- Existing namespaced API, canonical `openshift-vcf-migration/cluster`, and compatible omitted-option legacy path.
- Explicit Hot or Cold for both roles, same mode, one node/action in flight, workers before the dedicated three-node control plane.
- One source/destination pair, one destination FD, one NIC per VM; destination FD/vCenter/credentials/configuration prepared while retaining source.
- Native per-VM intended-spec feasibility, normal OpenShift disruption protections, durable action tracking and observed recovery.
- No mandatory UPI probe VM/template or probe-addressing configuration. Administrator supplies network continuity; legacy recreation probe remains unchanged.
- Address-family preservation, not an IPv4-only limit or family conversion.
- Conservative manual administrator recovery after failure, action-bound recheck/resume, and distinct deletion handoff.
- Two operator replicas with native placement/PDB protections and durable restart handling.
- Live inventory and frozen-source guards before retryable reference cleanup; completion does not authorize retiring external infrastructure.

The main document contains the precise requirements and qualifications. This summary does not add exceptions or relax its guards.

## 3. Superseded claims and requirements

The following statements from earlier drafts/assessments are not requirements or established guarantees for this delivery:

### CCM and inventory safety

Correct MultiSite configuration does not, by this document, establish that CCM can never delete a Node. Do not rely on a universal 200 ms stun bound, a fixed grace-period argument, simultaneous inventory visibility, or registration ordering as a production safety proof. Mode/version-specific qualification, unchanged Node identity and post-move health remain required. Lab confirmation is blocking qualification work, not optional reassurance.

### Network validation

The earlier temporary-VM probe observes guest-reported IPv4 subnet/default-gateway information; it does not establish arbitrary traffic/L2 continuity. Do not carry its template/DHCP/static-addressing requirements or IPv4-oriented implementation into the UPI relocation contract. Native network visibility is also not guest connectivity proof. The chosen boundary is administrator-prepared continuity plus qualified native checks and observed recovery, with the residual risk documented in the main design.

### Cold scope and recovery

Cold is part of the first design, not future-only. The operator performs normal destination power-on after successful relocation, but does not automatically power the source back on after failed relocation. Unknown outcomes never authorize replay or booting presumed copies. Graceful shutdown is not synchronous task completion; do not invent a ShutdownGuest task reference or hard-power fallback.

### Native findings and placement

Fatal compatibility findings and unavailable/uninterpretable checks block; native nonfatal warnings are surfaced without automatically blocking. This does not make an unresolved destination object or a violated hard scope requirement a harmless warning. Do not retain warn-only handling for unresolved host/pool/datastore/network mappings.

Explicit placement parameters and a passing check do not establish every Hot/Cold/DRS/pool/version combination or guarantee the observed power-on host. Use the qualified execution/result contract in the main document, not a blanket non-DRS support assertion or custom placement engine.

### API and phase status

Do not say proposed node-migration validation/status fields are already implemented merely because they appear in a design example. Retain the reviewed phase flow; there is no UPI `DestinationImageImported` stage or image-import requirement. Reconcile implementation foundations with PR #217 and current source rather than duplicating namespace/finalizer fixes.

### Ownership and eligibility

Namespaced singleton authority, process leader election and durable external action identity are different concerns. Leader election is not external fencing or exactly-once execution. A pause-related generation change must not discard the matching action's result.

The agreed eligibility check is plan-scoped ownership plus a conflicting-provisioning gate, not an empty Machine inventory. Do not infer unmanaged status from an unhealthy Machine API or zero Running Machines, and do not label current nodes Machine-managed from a desired replica count alone.

## 4. Deferred topics, not first-release commitments

Potential future work includes automatic source recovery after narrowly verified failures, more destination FDs or concurrency, managed-node VMotion, automatic mode selection/fallback, and broader topology/device support.

None is required to implement the current contract. None has a committed release, approval or API here. Each needs a separate scope, authorization/recovery policy and qualification plan. Address-family neutrality is preservation of the existing configuration, not permission to introduce a network conversion feature.

## 5. Acceptance and qualification ownership

Keep the authoritative acceptance cases and implementation gates in the main document; do not maintain a competing historical test matrix here.

In particular, retain tests for fatal findings inside a successful check task, submission/persistence crash windows, strict pause, graceful shutdown timeout/late completion, manual action-bound recovery, operator leader eviction, additional etcd-member failure, stale Ready state, inventory drift, and partial source-reference cleanup.

Remove mandatory UPI probe/template/address-allocation cases, arbitrary IPv4-only admission, automatic source power-on, and tests that expect native network visibility to prove L2 continuity. Preserve legacy recreation coverage. A deliberately bad network can pass inventory visibility; qualification must demonstrate honest failure handling, not pretend the operator certifies all infrastructure.

No code or lab execution is implied by this replacement. Source history remains the record for older investigation details; the primary contract and its qualification evidence govern enablement.
