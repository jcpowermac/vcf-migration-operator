# Preflight network validation via probe VMs — design

Tracked by SPLAT-2958 and SPLAT-2961. New preflight capability: for each
destination failure domain, clone a throwaway probe VM from the FD template on
the destination vCenter, boot it, read its guest-tools network info, and compare
it against the guest-tools network info of the current source node VMs. If the
destination network would not preserve IP connectivity, preflight hard-blocks.

## Decisions (confirmed with user)

- Compared fields: **IPv4 address (as subnet, via prefixLength) + prefixLength +
  default gateway**, read from guest tools on **both sides**. The source side
  is read from the real source node VMs on the source vCenter
  (`vm.Guest()`), not from Kubernetes Node objects — same data source as the
  probe, so no Node-object limitations apply.
- Mismatch action: **hard block** (non-transient preflight failure;
  `InfrastructurePrepared` never becomes True until the networks match).
- Scope of this iteration: **full script-injection capability** (arbitrary
  ignition, afterburner kargs, extra VMX keys — i.e. kubeconfig files and bash
  scripts via ignition). The load-balancer path tests that will consume this
  capability (API 6443 / MCS 22623 / Ingress 80,443 from the probe) are **not**
  wired into preflight yet; they are the next iteration.

## Mechanism

### Probe lifecycle (per destination failure domain, sequential)

1. **Reap** stale probes first: on each target datacenter, destroy any VM whose
   name has the prefix `netcheck-<infraID>-` (idempotent re-runs, crash
   cleanup).
2. **Clone** the FD template (`fd.Topology.Template`) with
   `VirtualMachineCloneSpec{PowerOn: true}` (power on with the clone):
   - `Location`: Folder = `fd.Topology.Folder` or the DC VM folder; Datastore =
     `fd.Topology.Datastore`; Pool = `fd.Topology.ResourcePool` or the cluster
     root pool; `DiskMoveType: moveAllMergeBackup`. No host specified — vCenter
     selects via DRS (same behavior as machine-api-operator).
   - `Config.ExtraConfig`: the injection payload (below).
   - Single NIC on `fd.Topology.Networks[0]`.
3. **Wait** for guest tools: poll `vm.Guest(ctx)` every 5s until
   `ToolsStatus == toolsGuestToolsRunning` and at least one IPv4 + default
   gateway is present. Bounded by the phase context (10 min total for all FDs).
   Timeout → destroy probe, hard error with guidance (open-vm-tools must be
   running in the template; the FD network must provide DHCP or the kargs
   must configure static networking).
4. **Read** `NetworkInfo` from `GuestInfo`:
   - IPv4s from `GuestNicInfo.IpAddress` (filter `netip.Is4`)
   - gateway + prefix from the matching `GuestStackInfo`
     (`DefaultGateway`, `GuestIpAddrInfo.PrefixLength`)
5. **Compare** against the aggregated source networks (below).
6. **Destroy** the probe (`DestroyTask`) — always, via defer, on every path.

VM name: `netcheck-<infraID>-<sanitized fd name>` (deterministic; max 80
chars).

### Source networks

List Kubernetes nodes; for each node, find the VM **by node name** on the
source datacenter (vSphere IPI convention: node name == VM name) and read one
`vm.Guest()` snapshot → all `NetworkInfo`. Per-node problems (VM not found,
tools not running) are logged at V(1) and skipped; if **zero** source networks
are collected from all nodes, that is a hard error (open-vm-tools must be
running on nodes; VM names must match node names).

### Comparison

A probe network `n` **matches** when its masked network
(`ip.Prefix(prefix).Masked()`) **and** its default gateway equal those of at
least one source node network. If any probe network of any FD fails to match,
preflight fails with a combined message, one line per offending FD, e.g.:

```
failure domain "fd-a": probe network 10.0.1.0/24 (gateway 10.0.1.1) does not
match any source node network (source networks: 192.168.5.0/24 (gateway
192.168.5.1)); IP connectivity would not be preserved when recreating source
machines on the destination vCenter
```

A Warning event is recorded with the same detail.

### Script injection (extraConfig keys)

Keys mirror exactly what machine-api-operator / CAPV / the installer set on
real machine VMs (verified against
`machine-api-operator/pkg/controller/vsphere/reconciler.go`,
`cluster-api-provider-vsphere/pkg/services/govmomi/extra/config.go`,
`installer/pkg/asset/machines/vsphere/capimachines.go`):

| Key | Value | Source |
|---|---|---|
| `guestinfo.ignition.config.data` | base64(ignition) | machine-api-operator |
| `guestinfo.ignition.config.data.encoding` | `base64` | machine-api-operator |
| `guestinfo.afterburn.initrd.network-kargs` | afterburner kargs (e.g. static IP) | installer / machine-api-operator |
| `guestinfo.hostname` | VM name | machine-api-operator |
| `stealclock.enable` | `TRUE` | machine-api-operator |
| `<arbitrary>` | caller-supplied VMX/guestinfo keys | — |

Default ignition when none is supplied: a minimal no-op ignition
(`{"ignition":{"version":"3.4.0"},"passwd":{},"storage":{"files":[]},"systemd":{"units":[]},"networkd":{"units":[]}}`)
so afterburner completes and RHCOS boots with default (DHCP) networking and
guest tools reports the network. Callers can supply full ignition documents
(including files — e.g. a kubeconfig — and systemd units — e.g. bash scripts)
for later iterations' LB path tests. No ignition library dependency is added;
ignition is an opaque JSON string.

## Code layout

- `internal/vsphere/probe.go` (+ `probe_test.go`) — OpenShift-free:
  - `type ProbeSpec struct { Name, Datacenter, Cluster, Datastore, ResourcePool, Folder, Template, Network, Ignition, NetworkKargs string; ExtraConfig map[string]string }`
  - `type NetworkInfo struct { IP string; Prefix int; Gateway string }`
  - `(s *Session) CreateProbeVM(ctx, spec) (*object.VirtualMachine, error)`
  - `WaitForGuestNetworks(ctx, vm) ([]NetworkInfo, error)`
  - `(s *Session) GetVMNetworks(ctx, vmName) ([]NetworkInfo, error)` (single read, for source nodes)
  - `DestroyProbeVM(ctx, vm) error`
  - `(s *Session) ReapProbeVMs(ctx, namePrefix) ([]string, error)`
  - `(n NetworkInfo) MatchesAny(sources []NetworkInfo) (bool, string)` (pure)
- `internal/controller/preflight_netcheck.go` (+ unit tests for pure helpers):
  - `probeName(infraID, fdName) string`, `describeFDMismatch(...)` (pure, tested)
  - `func (r *Reconciler) checkNetworkingViaProbeVMs(ctx, migration, sourceVC) error`
  - wired in `runPreflightChecks` after `validatePreflightVSphere`, with its
    own context: `preflightNetcheckTimeout = 10 * time.Minute` (the existing
    2-min `preflightVSphereTimeout` stays for the topology/privilege validation)

No CRD changes, no new conditions, no RBAC changes (nodes get/list/watch
already granted), no new dependencies.

## Failure semantics

- Network mismatch, zero source networks, zero probe networks, guest-tools
  timeout: all **non-transient** preflight errors → `InfrastructurePrepared`
  False/Failed, migration does not proceed. Re-runs are idempotent (reaper +
  deterministic names).
- vSphere connectivity/permission failures surface as their wrapped errors,
  same as existing preflight checks.

## Assumptions / preconditions (documented in the error messages)

1. Node name == VM name on the source vCenter (vSphere IPI convention).
2. open-vm-tools (vmtoolsd) runs in the FD template and on source nodes.
3. The FD network provides DHCP, or the caller supplies `NetworkKargs` for
   static configuration.
4. `fd.Topology.Networks[0]` is the primary network (a single NIC is attached).

## Test plan

- `internal/vsphere/probe_test.go` (govmomi simulator, same pattern as
  `folder_test.go`; the simulator supports `CloneVM` and exposes
  `vm.Guest.Net`/`vm.Guest.IpStack` for direct setup):
  - CreateProbeVM: clone succeeds; relocate spec (folder/datastore/pool/
    network) correct; extraConfig carries base64-roundtripped ignition, kargs,
    hostname, custom keys; VM powered on.
  - WaitForGuestNetworks: seeded guest info → correct `NetworkInfo` (ip,
    prefix, gateway); not-ready → keeps polling until deadline error.
  - `MatchesAny`: table-driven (match; different subnet; different gateway;
    same subnet different gateway; multiple sources).
  - ReapProbeVMs / DestroyProbeVM: destroyed; non-matching names untouched.
- `internal/controller/preflight_netcheck_test.go`: `probeName` sanitization /
  length, mismatch message formatting. The vSphere glue itself is not
  unit-testable in envtest (same as existing `validatePreflightVSphere`, which
  is covered by simulator-level tests instead).

## Out of scope (next iteration)

- LB path validation (API 6443, MCS 22623, Ingress 80/443) executed from the
  probe via injected scripts — the injection capability this spec adds is the
  enabler for it.
- Multiple NICs on the probe; host placement control; per-FD source-FD
  mapping (comparison is against all current source node networks).
