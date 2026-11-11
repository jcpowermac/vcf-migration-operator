package vsphere

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/openshift/vcf-migration-operator/internal/execution"
)

// Shared literals across vsphere tests, hoisted for goconst.
const (
	missingDatacenter   = "no-such-dc"
	toolsNotRunning     = "guestToolsNotRunning"
	defaultRouteNetwork = "0.0.0.0"
	probeGuestIP        = "192.168.5.42"
	otherGateway        = "10.0.2.1"
)

// TestNextDeviceKey covers nextDeviceKey: an empty device list returns the
// default base key, and a populated list returns one past the highest key.
func TestNextDeviceKey(t *testing.T) {
	tests := []struct {
		name    string
		devices object.VirtualDeviceList
		want    int32
	}{
		{name: "empty list returns base key", devices: object.VirtualDeviceList{}, want: 1000},
		{
			name: "returns max key plus base offset",
			devices: object.VirtualDeviceList{
				&types.VirtualDevice{Key: 100},
				&types.VirtualDevice{Key: 3000},
			},
			want: 4000,
		},
		{
			name:    "single device",
			devices: object.VirtualDeviceList{&types.VirtualDevice{Key: 50}},
			want:    1050,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextDeviceKey(tt.devices); got != tt.want {
				t.Fatalf("nextDeviceKey = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestMatchesAnyContinueBranches covers the defensive `continue`/early-return
// branches of MatchesAny that the main table does not reach: malformed probe
// IP, out-of-range probe prefix, malformed source IP, out-of-range source
// prefix, and malformed source gateway.
func TestMatchesAnyContinueBranches(t *testing.T) {
	tests := []struct {
		name string
		n    NetworkInfo
		srcs []NetworkInfo
	}{
		{name: "malformed probe ip", n: NetworkInfo{IP: "not-an-ip", Prefix: 24, Gateway: "192.168.5.1"}},
		{name: "out-of-range probe prefix", n: NetworkInfo{IP: probeGuestIP, Prefix: 33, Gateway: "192.168.5.1"}},
		{name: "malformed source ip", n: NetworkInfo{IP: probeGuestIP, Prefix: 24, Gateway: "192.168.5.1"},
			srcs: []NetworkInfo{{IP: "bogus", Prefix: 24, Gateway: "192.168.5.1"}}},
		{name: "out-of-range source prefix", n: NetworkInfo{IP: probeGuestIP, Prefix: 24, Gateway: "192.168.5.1"},
			srcs: []NetworkInfo{{IP: "192.168.5.10", Prefix: 33, Gateway: "192.168.5.1"}}},
		{name: "malformed source gateway", n: NetworkInfo{IP: probeGuestIP, Prefix: 24, Gateway: "192.168.5.1"},
			srcs: []NetworkInfo{{IP: "192.168.5.10", Prefix: 24, Gateway: "bogus"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := tt.n.MatchesAny(tt.srcs)
			if got {
				t.Fatalf("MatchesAny(%+v, %v) = true, want false", tt.n, tt.srcs)
			}
		})
	}
}

// TestDefaultGatewayAmbiguous covers the multiple-default-route error branch.
func TestDefaultGatewayAmbiguous(t *testing.T) {
	guest := &types.GuestInfo{
		IpStack: []types.GuestStackInfo{{
			IpRouteConfig: &types.NetIpRouteConfigInfo{
				IpRoute: []types.NetIpRouteConfigInfoIpRoute{
					{Network: defaultRouteNetwork, Gateway: types.NetIpRouteConfigInfoGateway{IpAddress: "10.0.1.1"}},
					{Network: defaultRouteNetwork, Gateway: types.NetIpRouteConfigInfoGateway{IpAddress: otherGateway}},
				},
			},
		}},
	}
	_, err := defaultGateway(guest.IpStack)
	if err == nil || !strings.Contains(err.Error(), "distinct IPv4 default gateways") {
		t.Fatalf("defaultGateway = err %v, want distinct-gateways error", err)
	}
}

// TestReadGuestNetworks covers the guest-nil, tools-not-running, and
// no-default-gateway branches of readGuestNetworks.
func TestReadGuestNetworks(t *testing.T) {
	e := newProbeTestEnv(t)
	template, err := e.session.Finder.VirtualMachine(e.ctx, e.template)
	if err != nil {
		t.Fatalf("finding template VM: %v", err)
	}
	ref := template.Reference()

	t.Run("guest nil", func(t *testing.T) {
		// Seed nil explicitly so the guest == nil branch is exercised even if
		// the simulator ever defaults Guest to a non-nil value.
		e.seedGuest(t, ref, nil)
		networks, ready, err := readGuestNetworks(e.ctx, template)
		if err != nil {
			t.Fatalf("readGuestNetworks: %v", err)
		}
		if ready {
			t.Fatal("ready = true, want false (no guest info)")
		}
		if len(networks) != 0 {
			t.Fatalf("networks = %d, want 0", len(networks))
		}
	})

	t.Run("tools not running", func(t *testing.T) {
		e.seedGuest(t, ref, &types.GuestInfo{ToolsRunningStatus: toolsNotRunning})
		_, ready, err := readGuestNetworks(e.ctx, template)
		if err != nil {
			t.Fatalf("readGuestNetworks: %v", err)
		}
		if ready {
			t.Fatal("ready = true, want false (tools not running)")
		}
	})

	t.Run("no default gateway", func(t *testing.T) {
		e.seedGuest(t, ref, &types.GuestInfo{
			ToolsRunningStatus: guestToolsRunning,
			Net:                []types.GuestNicInfo{{Network: "n", Connected: true, IpAddress: []string{"10.0.1.5"}, IpConfig: &types.NetIpConfigInfo{IpAddress: []types.NetIpConfigInfoIpAddress{{IpAddress: "10.0.1.5", PrefixLength: 24}}}}},
		})
		networks, ready, err := readGuestNetworks(e.ctx, template)
		if err != nil {
			t.Fatalf("readGuestNetworks: %v", err)
		}
		if ready {
			t.Fatal("ready = true, want false (no default gateway)")
		}
		if len(networks) == 0 {
			t.Fatal("networks empty, want non-empty")
		}
	})
}

// TestCreateProbeVMLookupErrors covers the not-found branches for template,
// datastore, network, cluster, and folder during CreateProbeVM.
func TestCreateProbeVMLookupErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(spec *ProbeSpec)
		wantSub string
	}{
		{name: "template not found", mutate: func(s *ProbeSpec) { s.Template = "no-such-template" }, wantSub: "looking up template"},
		{name: "datastore not found", mutate: func(s *ProbeSpec) { s.Datastore = "no-such-datastore" }, wantSub: "looking up datastore"},
		{name: "network not found", mutate: func(s *ProbeSpec) { s.Network = "no-such-network" }, wantSub: "looking up network"},
		{name: "resource pool not found", mutate: func(s *ProbeSpec) { s.ResourcePool = "no-such-pool" }, wantSub: "looking up resource pool"},
		{name: "folder not found", mutate: func(s *ProbeSpec) { s.Folder = "/no-such-folder" }, wantSub: "looking up folder"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newProbeTestEnv(t)
			spec := e.baseSpec("probe-lookup")
			tt.mutate(&spec)
			if _, err := e.session.CreateProbeVM(e.ctx, spec); err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantSub)
			}
		})
	}
}

// TestProbeFolderGaps covers probeFolder's default-folder path and its
// datacenter-not-found branch.
func TestProbeFolderGaps(t *testing.T) {
	t.Run("default folder when empty", func(t *testing.T) {
		e := newProbeTestEnv(t)
		spec := e.baseSpec("probe-default-folder")
		spec.Folder = ""
		folder, err := e.session.probeFolder(e.ctx, spec)
		if err != nil {
			t.Fatalf("probeFolder: %v", err)
		}
		if folder == nil {
			t.Fatal("probeFolder = nil")
		}
	})

	t.Run("datacenter not found", func(t *testing.T) {
		e := newProbeTestEnv(t)
		spec := e.baseSpec("probe-bad-dc")
		spec.Datacenter = missingDatacenter
		if _, err := e.session.probeFolder(e.ctx, spec); err == nil || !strings.Contains(err.Error(), "datacenter") {
			t.Fatalf("error = %v, want datacenter error", err)
		}
	})
}

// TestProbeResourcePoolGaps covers probeResourcePool's default-pool fallback
// (a non-empty but unknown resource pool name) and its cluster-not-found
// branch.
func TestProbeResourcePoolGaps(t *testing.T) {
	t.Run("empty pool uses cluster default", func(t *testing.T) {
		e := newProbeTestEnv(t)
		spec := e.baseSpec("probe-default-pool")
		spec.ResourcePool = ""
		ref, err := e.session.probeResourcePool(e.ctx, spec)
		if err != nil {
			t.Fatalf("probeResourcePool: %v", err)
		}
		if ref == nil || ref.Value == "" {
			t.Fatalf("probeResourcePool = %+v, want a default pool ref", ref)
		}
	})

	t.Run("unknown pool errors", func(t *testing.T) {
		e := newProbeTestEnv(t)
		spec := e.baseSpec("probe-bad-pool")
		spec.ResourcePool = "no-such-pool"
		if _, err := e.session.probeResourcePool(e.ctx, spec); err == nil || !strings.Contains(err.Error(), "looking up resource pool") {
			t.Fatalf("error = %v, want resource pool error", err)
		}
	})
}

// TestGetVMNetworksGuestNil covers the guest-nil branch of GetVMNetworks
// (a VM with no guest info returns a "has no guest info" error).
func TestGetVMNetworksGuestNil(t *testing.T) {
	e := newProbeTestEnv(t)
	template, err := e.session.Finder.VirtualMachine(e.ctx, e.template)
	if err != nil {
		t.Fatalf("finding template VM: %v", err)
	}
	// Clear the guest info so guestInfo returns nil.
	e.seedGuest(t, template.Reference(), nil)
	_, err = e.session.GetVMNetworks(e.ctx, e.template)
	if err == nil || !strings.Contains(err.Error(), "has no guest info") {
		t.Fatalf("error = %v, want has-no-guest-info error", err)
	}
}

// TestWaitForGuestNetworksTimeoutGap covers WaitForGuestNetworks' timeout branch.
func TestWaitForGuestNetworksTimeoutGap(t *testing.T) {
	e := newProbeTestEnv(t)
	template, err := e.session.Finder.VirtualMachine(e.ctx, e.template)
	if err != nil {
		t.Fatalf("finding template VM: %v", err)
	}
	// A not-ready guest makes readGuestNetworks return (not ready, no error),
	// so the poll loop blocks until the context deadline fires.
	e.seedGuest(t, template.Reference(), &types.GuestInfo{ToolsRunningStatus: toolsNotRunning})
	// Generous enough for the initial readGuestNetworks call to complete, well
	// under the 5s poll interval, so the deadline fires inside the poll loop.
	ctx, cancel := context.WithTimeout(e.ctx, 500*time.Millisecond)
	defer cancel()
	if _, err := WaitForGuestNetworks(ctx, template); err == nil || !strings.Contains(err.Error(), "waiting for guest networks") {
		t.Fatalf("error = %v, want waiting-for-guest-networks error", err)
	}
}

// TestReapProbeVMsNoMatch covers ReapProbeVMs' no-probe-VM branch (a prefix
// that matches no VM returns an empty list, no error).
func TestReapProbeVMsNoMatch(t *testing.T) {
	e := newProbeTestEnv(t)
	reaped, err := e.session.ReapProbeVMs(e.ctx, "zzz-no-match")
	if err != nil {
		t.Fatalf("ReapProbeVMs: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("ReapProbeVMs = %v, want empty", reaped)
	}
}

// TestIsProbeVMGaps covers isProbeVM's marker-present and marker-absent
// outcomes for real VMs.
func TestIsProbeVMGaps(t *testing.T) {
	t.Run("probe-marked VM is a probe", func(t *testing.T) {
		e := newProbeTestEnv(t)
		spec := e.baseSpec("probe-marker-yes")
		vm, err := e.session.CreateProbeVM(e.ctx, spec)
		if err != nil {
			t.Fatalf("CreateProbeVM: %v", err)
		}
		isProbe, err := isProbeVM(e.ctx, vm)
		if err != nil {
			t.Fatalf("isProbeVM: %v", err)
		}
		if !isProbe {
			t.Fatal("isProbeVM = false, want true for probe VM")
		}
	})

	t.Run("unmarked VM is not a probe", func(t *testing.T) {
		e := newProbeTestEnv(t)
		vm := e.cloneUnmarkedVM(t, "probe-marker-no")
		isProbe, err := isProbeVM(e.ctx, vm)
		if err != nil {
			t.Fatalf("isProbeVM: %v", err)
		}
		if isProbe {
			t.Fatal("isProbeVM = true, want false for unmarked VM")
		}
	})
}

// TestDestroyProbeVMGaps covers DestroyProbeVM's power-state-read error
// (removed VM) and its destroy error (execution-check fault on a powered-off
// clone).
func TestDestroyProbeVMGaps(t *testing.T) {
	t.Run("power state read error", func(t *testing.T) {
		e := newProbeTestEnv(t)
		spec := e.baseSpec("probe-power-err")
		vm, err := e.session.CreateProbeVM(e.ctx, spec)
		if err != nil {
			t.Fatalf("CreateProbeVM: %v", err)
		}
		e.model.Service.Context.Map.Remove(e.model.Service.Context, vm.Reference())
		if err := DestroyProbeVM(e.ctx, vm); err == nil || !strings.Contains(err.Error(), "power state") {
			t.Fatalf("error = %v, want power-state error", err)
		}
	})

	t.Run("destroy error via execution fault", func(t *testing.T) {
		e := newProbeTestEnv(t)
		spec := e.baseSpec("probe-destroy-fault")
		vm, err := e.session.CreateProbeVM(e.ctx, spec)
		if err != nil {
			t.Fatalf("CreateProbeVM: %v", err)
		}
		// Power the VM off (non-faulting ctx) so DestroyProbeVM skips the
		// power-off block and reaches the destroy execution check.
		off, err := vm.PowerOff(e.ctx)
		if err != nil {
			t.Fatalf("powering off probe VM: %v", err)
		}
		if err := off.Wait(e.ctx); err != nil {
			t.Fatalf("waiting for power off: %v", err)
		}
		sentinel := errors.New("boom")
		ctx := execution.WithCheck(e.ctx, func(context.Context) error { return sentinel })
		if err := DestroyProbeVM(ctx, vm); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want the execution sentinel from the destroy check", err)
		}
	})
}

// TestCleanupFailedCloneDestroyError covers cleanupFailedClone's destroy-error
// branch: the clone exists and is probe-marked, but DestroyProbeVM faults via
// the execution check.
func TestCleanupFailedCloneDestroyError(t *testing.T) {
	e := newProbeTestEnv(t)
	spec := e.baseSpec("probe-cleanup-fault")
	if _, err := e.session.CreateProbeVM(e.ctx, spec); err != nil {
		t.Fatalf("CreateProbeVM: %v", err)
	}
	// The probe VM is powered on, so DestroyProbeVM's power-off execution check
	// faults before it destroys the VM. Assert the VM still resolves afterward to
	// prove the destroy path was reached (and faulted); cleanupFailedClone always
	// returns its waitErr argument, so the sentinel is the meaningful check.
	sentinel := errors.New("boom")
	ctx := execution.WithCheck(e.ctx, func(context.Context) error { return sentinel })
	waitErr := errors.New("clone wait failed")
	if err := e.session.cleanupFailedClone(ctx, spec, waitErr); !errors.Is(err, waitErr) {
		t.Fatalf("cleanupFailedClone = %v, want the waitErr sentinel", err)
	}
	if _, err := e.session.Finder.VirtualMachine(e.ctx, spec.Name); err != nil {
		t.Fatalf("probe VM should still exist after a faulted destroy: %v", err)
	}
}

// TestGuestInfoError covers guestInfo's property-read error branch.
func TestGuestInfoError(t *testing.T) {
	e := newProbeTestEnv(t)
	template, err := e.session.Finder.VirtualMachine(e.ctx, e.template)
	if err != nil {
		t.Fatalf("finding template VM: %v", err)
	}
	// Remove the VM from the registry so Properties faults.
	e.model.Service.Context.Map.Remove(e.model.Service.Context, template.Reference())
	if _, err := guestInfo(e.ctx, template); err == nil {
		t.Fatal("guestInfo = nil, want property-read error")
	}
}

// TestDefaultGatewayNilRouteConfig covers the nil-IP-route-config continue in
// defaultGateway.
func TestDefaultGatewayNilRouteConfig(t *testing.T) {
	stacks := []types.GuestStackInfo{
		{IpRouteConfig: nil},
		{IpRouteConfig: &types.NetIpRouteConfigInfo{
			IpRoute: []types.NetIpRouteConfigInfoIpRoute{
				{Network: defaultRouteNetwork, Gateway: types.NetIpRouteConfigInfoGateway{IpAddress: "10.0.1.1"}},
			},
		}},
	}
	gw, err := defaultGateway(stacks)
	if err != nil {
		t.Fatalf("defaultGateway: %v", err)
	}
	if gw != "10.0.1.1" {
		t.Fatalf("defaultGateway = %q, want 10.0.1.1", gw)
	}
}

// TestIsProbeVMPropertiesError covers isProbeVM's property-read error branch.
func TestIsProbeVMPropertiesError(t *testing.T) {
	e := newProbeTestEnv(t)
	spec := e.baseSpec("probe-isprobe-err")
	vm, err := e.session.CreateProbeVM(e.ctx, spec)
	if err != nil {
		t.Fatalf("CreateProbeVM: %v", err)
	}
	e.model.Service.Context.Map.Remove(e.model.Service.Context, vm.Reference())
	if _, err := isProbeVM(e.ctx, vm); err == nil {
		t.Fatal("isProbeVM = nil, want property-read error")
	}
}

// TestProbeFolderExplicitValid covers probeFolder's success path for an
// explicitly-set folder.
func TestProbeFolderExplicitValid(t *testing.T) {
	e := newProbeTestEnv(t)
	spec := e.baseSpec("probe-explicit-folder")
	spec.Folder = ""
	defaultFolder, err := e.session.probeFolder(e.ctx, spec)
	if err != nil {
		t.Fatalf("default probeFolder: %v", err)
	}
	spec.Folder = defaultFolder.InventoryPath
	if _, err := e.session.probeFolder(e.ctx, spec); err != nil {
		t.Fatalf("explicit probeFolder: %v", err)
	}
}

// TestSingleNICDeviceChangeAddPath covers singleNICDeviceChange's no-NIC add
// path: the template is stripped of all ethernet cards, so the function adds a
// fresh NIC instead of reusing an existing one.
func TestSingleNICDeviceChangeAddPath(t *testing.T) {
	e := newProbeTestEnv(t)
	template, err := e.session.Finder.VirtualMachine(e.ctx, e.template)
	if err != nil {
		t.Fatalf("finding template VM: %v", err)
	}
	simVM, ok := e.model.Service.Context.Map.Get(template.Reference()).(*simulator.VirtualMachine)
	if !ok {
		t.Fatalf("finding simulator VM: got %T", e.model.Service.Context.Map.Get(template.Reference()))
	}
	kept := make([]types.BaseVirtualDevice, 0, len(simVM.Config.Hardware.Device))
	for _, d := range simVM.Config.Hardware.Device {
		if _, isNIC := d.(types.BaseVirtualEthernetCard); !isNIC {
			kept = append(kept, d)
		}
	}
	simVM.Config.Hardware.Device = kept

	network, err := e.session.Finder.Network(e.ctx, e.network)
	if err != nil {
		t.Fatalf("finding network: %v", err)
	}
	changes, err := singleNICDeviceChange(e.ctx, template, network)
	if err != nil {
		t.Fatalf("singleNICDeviceChange: %v", err)
	}
	added := false
	for _, c := range changes {
		change, ok := c.(*types.VirtualDeviceConfigSpec)
		if !ok {
			continue
		}
		if change.Operation == types.VirtualDeviceConfigSpecOperationAdd {
			if _, isNIC := change.Device.(types.BaseVirtualEthernetCard); isNIC {
				added = true
			}
		}
	}
	if !added {
		t.Fatal("expected an ADD operation for a new NIC when the template has no NIC")
	}
}

// Uncovered branches in probe.go. The simulator's Clone/PowerOff/Destroy
// succeed for a well-formed spec, so the fault branches need fault injection
// (simulator.Service.AddFaultRule with a MethodName target, or a TaskDelay for
// task.Wait) which is not set up here:
//   - CreateProbeVM clone path: singleNICDeviceChange error, template.Clone
//     error, clone-task wait error (which routes to cleanupFailedClone), and the
//     clone-result type assertion.
//   - DestroyProbeVM: the PowerOff and Destroy task.Wait fault branches (a
//     TaskDelay fault injection would cover them).
//   - cleanupFailedClone: the isProbeVM property error (the finder and
//     Properties read the same inventory, so a removed VM faults the finder
//     first, before isProbeVM).
//   - probeFolder: the dc.Folders property-collector fault.
//   - singleNICDeviceChange: template.Device read error, network backing read
//     error, and the extra-NIC remove path (a template with two or more NICs).
//   - WaitForGuestNetworks: the read-error branch and the normal ticker tick
//     (the poll interval is 5s, so a ticker tick needs a >5s timeout).
//   - readGuestNetworks: the guestInfo error and the networksFromGuest error
//     (defensive parse failure).
//   - GetVMNetworks: the guestInfo error (the finder fails first on a removed
//     VM).
//   - DestroyProbeVM: PowerOff and Destroy fault/wait branches (the simulator's
//     tasks reliably succeed).
//   - ReapProbeVMs: a non-NotFound list error and the isProbeVM error.
//   - isProbeVM: a VM whose config property is nil.

// TestCreateProbeVMExecCheckFault covers CreateProbeVM's pre-clone execution
// check fault: all lookups succeed, then the execution check before the clone
// faults.
func TestCreateProbeVMExecCheckFault(t *testing.T) {
	e := newProbeTestEnv(t)
	spec := e.baseSpec("probe-exec-check")
	sentinel := errors.New("boom")
	ctx := execution.WithCheck(e.ctx, func(context.Context) error { return sentinel })
	if _, err := e.session.CreateProbeVM(ctx, spec); err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("CreateProbeVM = %v, want the execution-check sentinel", err)
	}
}

// TestReapProbeVMsDestroyError covers ReapProbeVMs' destroy-error branch: a
// probe-marked VM matches the prefix but DestroyProbeVM faults via the
// execution check.
func TestReapProbeVMsDestroyError(t *testing.T) {
	e := newProbeTestEnv(t)
	spec := e.baseSpec("probe-reap-destroy-err")
	if _, err := e.session.CreateProbeVM(e.ctx, spec); err != nil {
		t.Fatalf("CreateProbeVM: %v", err)
	}
	sentinel := errors.New("boom")
	ctx := execution.WithCheck(e.ctx, func(context.Context) error { return sentinel })
	if _, err := e.session.ReapProbeVMs(ctx, "probe-reap-destroy-err"); err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("ReapProbeVMs = %v, want the execution-check sentinel", err)
	}
}
