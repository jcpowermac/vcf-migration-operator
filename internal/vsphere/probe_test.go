package vsphere

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	_ "github.com/vmware/govmomi/vapi/simulator"
)

// probeTestEnv bundles a simulator model, session, and discovered inventory for
// probe VM tests.
type probeTestEnv struct {
	model     *simulator.Model
	ctx       context.Context
	session   *Session
	dcName    string
	cluster   string
	datastore string
	dsRef     types.ManagedObjectReference
	pool      string
	network   string
	template  string
}

func newProbeTestEnv(t *testing.T) *probeTestEnv {
	t.Helper()

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("creating simulator model: %v", err)
	}
	t.Cleanup(model.Remove)

	model.Service.TLS = new(tls.Config)
	model.Service.RegisterEndpoints = true
	server := model.Service.NewServer()
	t.Cleanup(server.Close)

	ctx := model.Service.Context
	username := simulator.DefaultLogin.Username()
	password, ok := simulator.DefaultLogin.Password()
	if !ok {
		t.Fatal("simulator default login missing password")
	}

	client, err := govmomi.NewClient(ctx, server.URL, true)
	if err != nil {
		t.Fatalf("creating govmomi client: %v", err)
	}
	t.Cleanup(func() { _ = client.Logout(ctx) })

	finder := find.NewFinder(client.Client, true)
	dc, err := finder.DefaultDatacenter(ctx)
	if err != nil {
		t.Fatalf("finding default datacenter: %v", err)
	}
	finder.SetDatacenter(dc)

	clusters, err := finder.ClusterComputeResourceList(ctx, "*")
	if err != nil || len(clusters) == 0 {
		t.Fatalf("listing clusters: %v", err)
	}
	datastores, err := finder.DatastoreList(ctx, "*")
	if err != nil || len(datastores) == 0 {
		t.Fatalf("listing datastores: %v", err)
	}
	networks, err := finder.NetworkList(ctx, "*")
	if err != nil || len(networks) == 0 {
		t.Fatalf("listing networks: %v", err)
	}
	networkPath := networks[0].GetInventoryPath()
	networkName := networkPath[strings.LastIndex(networkPath, "/")+1:]
	pools, err := finder.ManagedObjectList(ctx, "*", "ResourcePool")
	if err != nil || len(pools) == 0 {
		t.Fatalf("listing resource pools: %v", err)
	}
	poolPath := ""
	for _, pool := range pools {
		if strings.Contains(pool.Path, "Resources") {
			poolPath = pool.Path
			break
		}
	}
	if poolPath == "" {
		t.Fatal("no resource pool found in simulator inventory")
	}
	vms, err := finder.VirtualMachineList(ctx, "*")
	if err != nil || len(vms) == 0 {
		t.Fatalf("listing VMs: %v", err)
	}

	s, err := NewSession(ctx, Params{
		Server:     server.URL.Host,
		Datacenter: dc.Name(),
		Username:   username,
		Password:   password,
		Insecure:   true,
	})
	if err != nil {
		t.Fatalf("creating session: %v", err)
	}
	t.Cleanup(func() { ClearSessions(ctx) })

	return &probeTestEnv{
		model:     model,
		ctx:       ctx,
		session:   s,
		dcName:    dc.Name(),
		cluster:   clusters[0].Name(),
		datastore: datastores[0].Name(),
		dsRef:     datastores[0].Reference(),
		pool:      poolPath,
		network:   networkName,
		template:  vms[0].Name(),
	}
}

// seedGuest sets the guest info snapshot served for the simulator VM with ref.
func (e *probeTestEnv) seedGuest(t *testing.T, ref types.ManagedObjectReference, guest *types.GuestInfo) {
	t.Helper()
	simVM, ok := e.model.Service.Context.Map.Get(ref).(*simulator.VirtualMachine)
	if !ok {
		t.Fatalf("finding simulator VM %s: got %T", ref.Value, e.model.Service.Context.Map.Get(ref))
	}
	simVM.Guest = guest
}

// readyGuest returns a guest info snapshot with one IPv4 address, a /24 prefix,
// and an IPv4 default route.
func readyGuest() *types.GuestInfo {
	return &types.GuestInfo{
		ToolsRunningStatus: guestToolsRunning,
		Net: []types.GuestNicInfo{{
			Network:   "test-network",
			Connected: true,
			IpAddress: []string{"10.0.1.5", "fd00::5"},
			IpConfig: &types.NetIpConfigInfo{
				IpAddress: []types.NetIpConfigInfoIpAddress{
					{IpAddress: "10.0.1.5", PrefixLength: 24},
				},
			},
		}},
		IpStack: []types.GuestStackInfo{{
			IpRouteConfig: &types.NetIpRouteConfigInfo{
				IpRoute: []types.NetIpRouteConfigInfoIpRoute{{
					Network: "0.0.0.0",
					Gateway: types.NetIpRouteConfigInfoGateway{IpAddress: "10.0.1.1"},
				}},
			},
		}},
	}
}

// probeExtraConfig reads the clone's extra config as a string map.
func probeExtraConfig(t *testing.T, ctx context.Context, vm *object.VirtualMachine) map[string]string {
	t.Helper()
	var moVM mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"config"}, &moVM); err != nil {
		t.Fatalf("reading VM config: %v", err)
	}
	if moVM.Config == nil {
		t.Fatal("reading VM config: nil config")
	}
	out := make(map[string]string)
	for _, base := range moVM.Config.ExtraConfig {
		ov := base.GetOptionValue()
		out[ov.Key] = fmt.Sprint(ov.Value)
	}
	return out
}

func (e *probeTestEnv) baseSpec(name string) ProbeSpec {
	return ProbeSpec{
		Name:         name,
		Datacenter:   e.dcName,
		Cluster:      e.cluster,
		Datastore:    e.datastore,
		ResourcePool: e.pool,
		Template:     e.template,
		Network:      e.network,
	}
}

// cloneUnmarkedVM clones the template into the probe folder without the
// probe marker, simulating an unrelated VM that shares the probe name
// prefix, and returns the clone.
func (e *probeTestEnv) cloneUnmarkedVM(t *testing.T, name string) *object.VirtualMachine {
	t.Helper()
	template, err := e.session.Finder.VirtualMachine(e.ctx, e.template)
	if err != nil {
		t.Fatalf("finding template VM: %v", err)
	}
	folder, err := e.session.probeFolder(e.ctx, e.baseSpec(name))
	if err != nil {
		t.Fatalf("resolving folder for %s: %v", name, err)
	}
	folderRef := folder.Reference()
	dsRef := e.dsRef
	task, err := template.Clone(e.ctx, folder, name, types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Folder:    &folderRef,
			Datastore: &dsRef,
		},
	})
	if err != nil {
		t.Fatalf("cloning VM %s: %v", name, err)
	}
	taskInfo, err := task.WaitForResult(e.ctx)
	if err != nil {
		t.Fatalf("cloning VM %s: %v", name, err)
	}
	ref, ok := taskInfo.Result.(types.ManagedObjectReference)
	if !ok {
		t.Fatalf("cloning VM %s: unexpected result %T", name, taskInfo.Result)
	}
	return object.NewVirtualMachine(e.session.Client.Client, ref)
}

func TestSingleNICDeviceChangePreservesVMXNET3Subtype(t *testing.T) {
	e := newProbeTestEnv(t)
	template, err := e.session.Finder.VirtualMachine(e.ctx, e.template)
	if err != nil {
		t.Fatalf("finding template VM: %v", err)
	}
	simVM, ok := e.model.Service.Context.Map.Get(template.Reference()).(*simulator.VirtualMachine)
	if !ok {
		t.Fatalf("finding simulator VM: got %T", e.model.Service.Context.Map.Get(template.Reference()))
	}
	for i, device := range simVM.Config.Hardware.Device {
		if nic, ok := device.(types.BaseVirtualEthernetCard); ok {
			simVM.Config.Hardware.Device[i] = &types.VirtualVmxnet3{
				VirtualVmxnet: types.VirtualVmxnet{
					VirtualEthernetCard: *nic.GetVirtualEthernetCard(),
				},
			}
			break
		}
	}
	devices, err := template.Device(e.ctx)
	if err != nil {
		t.Fatalf("reading template devices: %v", err)
	}
	var original types.BaseVirtualDevice
	for _, device := range devices {
		if _, ok := device.(types.BaseVirtualEthernetCard); ok {
			original = device
			break
		}
	}
	if _, ok := original.(*types.VirtualVmxnet3); !ok {
		t.Fatalf("template adapter type = %T, want *types.VirtualVmxnet3", original)
	}
	network, err := e.session.Finder.Network(e.ctx, e.network)
	if err != nil {
		t.Fatalf("finding network: %v", err)
	}
	changes, err := singleNICDeviceChange(e.ctx, template, network)
	if err != nil {
		t.Fatalf("singleNICDeviceChange: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("singleNICDeviceChange returned %d changes, want 1", len(changes))
	}
	change, ok := changes[0].(*types.VirtualDeviceConfigSpec)
	if !ok {
		t.Fatalf("device change type = %T, want *types.VirtualDeviceConfigSpec", changes[0])
	}
	if got, want := fmt.Sprintf("%T", change.Device), fmt.Sprintf("%T", original); got != want {
		t.Errorf("edited adapter type = %s, want original subtype %s", got, want)
	}
}

func TestCreateProbeVM(t *testing.T) {
	e := newProbeTestEnv(t)

	spec := e.baseSpec("netcheck-test-fd-a")
	spec.NetworkKargs = "ip=10.0.1.5::10.0.1.1:255.255.255.0:probe:ens2:fib"
	vm, err := e.session.CreateProbeVM(e.ctx, spec)
	if err != nil {
		t.Fatalf("CreateProbeVM: %v", err)
	}
	t.Cleanup(func() { _ = DestroyProbeVM(e.ctx, vm) })

	extra := probeExtraConfig(t, e.ctx, vm)
	data := extra["guestinfo.ignition.config.data"]
	if data == "" {
		t.Fatal("CreateProbeVM: guestinfo.ignition.config.data missing")
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("CreateProbeVM: decoding ignition: %v", err)
	}
	if string(decoded) != defaultProbeIgnition {
		t.Errorf("CreateProbeVM: ignition = %s, want default", string(decoded))
	}
	if got := extra["guestinfo.ignition.config.data.encoding"]; got != "base64" {
		t.Errorf("CreateProbeVM: ignition encoding = %q, want base64", got)
	}
	if got := extra["guestinfo.hostname"]; got != spec.Name {
		t.Errorf("CreateProbeVM: hostname = %q, want %q", got, spec.Name)
	}
	if got := extra["stealclock.enable"]; got != "TRUE" {
		t.Errorf("CreateProbeVM: stealclock.enable = %q, want TRUE", got)
	}
	if got := extra[probeMarkerExtraConfig]; got != probeMarkerValue {
		t.Errorf("CreateProbeVM: probe marker = %q, want %q", got, probeMarkerValue)
	}
	if got := extra["guestinfo.afterburn.initrd.network-kargs"]; got != spec.NetworkKargs {
		t.Errorf("CreateProbeVM: network-kargs = %q, want %q", got, spec.NetworkKargs)
	}

	devices, err := vm.Device(e.ctx)
	if err != nil {
		t.Fatalf("reading probe devices: %v", err)
	}
	nicCount := 0
	for _, base := range devices {
		nic, ok := base.(types.BaseVirtualEthernetCard)
		if !ok {
			continue
		}
		nicCount++
		backing, ok := nic.GetVirtualEthernetCard().Backing.(*types.VirtualEthernetCardNetworkBackingInfo)
		if !ok {
			t.Fatalf("CreateProbeVM: NIC backing = %T, want network backing", nic.GetVirtualEthernetCard().Backing)
		}
		if backing.DeviceName != spec.Network {
			t.Errorf("CreateProbeVM: NIC network = %q, want %q", backing.DeviceName, spec.Network)
		}
	}
	if nicCount != 1 {
		t.Errorf("CreateProbeVM: NIC count = %d, want 1", nicCount)
	}

	var moVM mo.VirtualMachine
	if err := vm.Properties(e.ctx, vm.Reference(), []string{"storage"}, &moVM); err != nil {
		t.Fatalf("reading probe storage: %v", err)
	}
	if moVM.Storage == nil || len(moVM.Storage.PerDatastoreUsage) == 0 {
		t.Errorf("CreateProbeVM: probe storage info missing")
	} else if got := moVM.Storage.PerDatastoreUsage[0].Datastore.Value; got != e.dsRef.Value {
		t.Errorf("CreateProbeVM: probe datastore = %q, want %q", got, e.dsRef.Value)
	}

	state, err := vm.PowerState(e.ctx)
	if err != nil {
		t.Fatalf("reading power state: %v", err)
	}
	if state != types.VirtualMachinePowerStatePoweredOn {
		t.Errorf("CreateProbeVM: power state = %s, want poweredOn", state)
	}
}

func TestCreateProbeVMCustomIgnitionAndExtraConfig(t *testing.T) {
	e := newProbeTestEnv(t)

	ignition := `{"ignition":{"version":"3.5.0"}}`
	spec := e.baseSpec("netcheck-test-fd-b")
	spec.Ignition = ignition
	spec.ExtraConfig = map[string]string{"guestinfo.custom.key": "custom-value"}
	vm, err := e.session.CreateProbeVM(e.ctx, spec)
	if err != nil {
		t.Fatalf("CreateProbeVM: %v", err)
	}
	t.Cleanup(func() { _ = DestroyProbeVM(e.ctx, vm) })

	extra := probeExtraConfig(t, e.ctx, vm)
	data := extra["guestinfo.ignition.config.data"]
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("CreateProbeVM: decoding ignition: %v", err)
	}
	if string(decoded) != ignition {
		t.Errorf("CreateProbeVM: ignition = %s, want %s", string(decoded), ignition)
	}
	if got := extra["guestinfo.custom.key"]; got != "custom-value" {
		t.Errorf("CreateProbeVM: custom extra config = %q, want custom-value", got)
	}
}

func TestCleanupFailedClone(t *testing.T) {
	e := newProbeTestEnv(t)
	waitErr := fmt.Errorf("cloning template %s as %s: %w", e.template, "netcheck-cleanup-a", context.DeadlineExceeded)

	// A leftover probe VM is destroyed and the original wait error returned.
	spec := e.baseSpec("netcheck-cleanup-a")
	vm, err := e.session.CreateProbeVM(e.ctx, spec)
	if err != nil {
		t.Fatalf("CreateProbeVM: %v", err)
	}
	t.Cleanup(func() { _ = DestroyProbeVM(e.ctx, vm) })
	if got := e.session.cleanupFailedClone(e.ctx, spec, waitErr); got != waitErr {
		t.Errorf("cleanupFailedClone error = %v, want the original wait error", got)
	}
	if _, err := e.session.Finder.VirtualMachine(e.ctx, spec.Name); err == nil {
		t.Errorf("cleanupFailedClone: probe VM %s still present", spec.Name)
	}

	// No leftover VM: the wait error is returned unchanged.
	if got := e.session.cleanupFailedClone(e.ctx, e.baseSpec("netcheck-cleanup-missing"), waitErr); got != waitErr {
		t.Errorf("cleanupFailedClone (missing) error = %v, want the original wait error", got)
	}

	// A name-colliding VM without the probe marker is not destroyed.
	decoy := e.cloneUnmarkedVM(t, "netcheck-cleanup-decoy")
	t.Cleanup(func() { _ = DestroyProbeVM(e.ctx, decoy) })
	if got := e.session.cleanupFailedClone(e.ctx, e.baseSpec("netcheck-cleanup-decoy"), waitErr); got != waitErr {
		t.Errorf("cleanupFailedClone (decoy) error = %v, want the original wait error", got)
	}
	if _, err := e.session.Finder.VirtualMachine(e.ctx, "netcheck-cleanup-decoy"); err != nil {
		t.Errorf("cleanupFailedClone: decoy VM must survive: %v", err)
	}
}

func TestWaitForGuestNetworks(t *testing.T) {
	e := newProbeTestEnv(t)

	vm, err := e.session.Finder.VirtualMachine(e.ctx, e.template)
	if err != nil {
		t.Fatalf("finding template VM: %v", err)
	}
	e.seedGuest(t, vm.Reference(), readyGuest())

	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	networks, err := WaitForGuestNetworks(ctx, vm)
	if err != nil {
		t.Fatalf("WaitForGuestNetworks: %v", err)
	}
	want := []NetworkInfo{{IP: "10.0.1.5", Prefix: 24, Gateway: "10.0.1.1"}}
	if len(networks) != len(want) {
		t.Fatalf("WaitForGuestNetworks = %v, want %v", networks, want)
	}
	for i := range want {
		if networks[i] != want[i] {
			t.Errorf("WaitForGuestNetworks[%d] = %+v, want %+v", i, networks[i], want[i])
		}
	}
}

func TestWaitForGuestNetworksTimeout(t *testing.T) {
	e := newProbeTestEnv(t)

	vm, err := e.session.Finder.VirtualMachine(e.ctx, e.template)
	if err != nil {
		t.Fatalf("finding template VM: %v", err)
	}
	e.seedGuest(t, vm.Reference(), nil)

	ctx, cancel := context.WithTimeout(e.ctx, time.Second)
	defer cancel()
	_, err = WaitForGuestNetworks(ctx, vm)
	if err == nil {
		t.Fatal("WaitForGuestNetworks: expected timeout error")
	}
	if !strings.Contains(err.Error(), "open-vm-tools") {
		t.Errorf("WaitForGuestNetworks error = %q, want mention of open-vm-tools", err)
	}
}

func TestGetVMNetworks(t *testing.T) {
	e := newProbeTestEnv(t)

	vm, err := e.session.Finder.VirtualMachine(e.ctx, e.template)
	if err != nil {
		t.Fatalf("finding template VM: %v", err)
	}
	e.seedGuest(t, vm.Reference(), readyGuest())

	networks, err := e.session.GetVMNetworks(e.ctx, e.template)
	if err != nil {
		t.Fatalf("GetVMNetworks: %v", err)
	}
	want := []NetworkInfo{{IP: "10.0.1.5", Prefix: 24, Gateway: "10.0.1.1"}}
	if len(networks) != len(want) || networks[0] != want[0] {
		t.Errorf("GetVMNetworks = %v, want %v", networks, want)
	}

	if _, err := e.session.GetVMNetworks(e.ctx, "no-such-vm"); err == nil {
		t.Error("GetVMNetworks: expected error for missing VM")
	}

	// Stale or missing guest snapshots must be refused, not returned.
	if _, err := e.session.GetVMNetworks(e.ctx, ""); err == nil {
		t.Error("GetVMNetworks: expected error for empty name")
	}
	stopped := *readyGuest()
	stopped.ToolsRunningStatus = "guestToolsNotRunning"
	e.seedGuest(t, vm.Reference(), &stopped)
	if _, err := e.session.GetVMNetworks(e.ctx, e.template); err == nil {
		t.Error("GetVMNetworks: expected error when guest tools are not running")
	}
	e.seedGuest(t, vm.Reference(), nil)
	if _, err := e.session.GetVMNetworks(e.ctx, e.template); err == nil {
		t.Error("GetVMNetworks: expected error when guest info is absent")
	}
}

func TestReapProbeVMs(t *testing.T) {
	e := newProbeTestEnv(t)

	created := make([]*object.VirtualMachine, 0, 2)
	for _, name := range []string{"netcheck-reap-a", "netcheck-reap-b"} {
		vm, err := e.session.CreateProbeVM(e.ctx, e.baseSpec(name))
		if err != nil {
			t.Fatalf("CreateProbeVM(%s): %v", name, err)
		}
		created = append(created, vm)
	}
	t.Cleanup(func() {
		for _, vm := range created {
			_ = DestroyProbeVM(e.ctx, vm)
		}
	})

	// A VM that shares the name prefix but was not created as a probe must
	// survive the reap.
	decoy := e.cloneUnmarkedVM(t, "netcheck-reap-decoy")
	t.Cleanup(func() { _ = DestroyProbeVM(e.ctx, decoy) })

	destroyed, err := e.session.ReapProbeVMs(e.ctx, "netcheck-reap-")
	if err != nil {
		t.Fatalf("ReapProbeVMs: %v", err)
	}
	if len(destroyed) != 2 {
		t.Fatalf("ReapProbeVMs destroyed %v, want 2 names", destroyed)
	}
	for _, name := range []string{"netcheck-reap-a", "netcheck-reap-b"} {
		if !contains(destroyed, name) {
			t.Errorf("ReapProbeVMs destroyed %v, want %q included", destroyed, name)
		}
	}

	// Only the decoy may remain: the probes are gone and the decoy untouched.
	remaining, err := e.session.Finder.VirtualMachineList(e.ctx, "netcheck-reap-*")
	if err != nil || len(remaining) != 1 {
		t.Errorf("ReapProbeVMs: %d VMs remain after reap (want only the decoy), err = %v", len(remaining), err)
	}

	// Non-matching inventory VMs must be untouched.
	if _, err := e.session.Finder.VirtualMachine(e.ctx, e.template); err != nil {
		t.Errorf("ReapProbeVMs: template VM missing after reap: %v", err)
	}

	// Reaping again with nothing to reap succeeds with an empty list.
	again, err := e.session.ReapProbeVMs(e.ctx, "netcheck-reap-")
	if err != nil {
		t.Errorf("ReapProbeVMs (empty): %v", err)
	}
	if len(again) != 0 {
		t.Errorf("ReapProbeVMs (empty) destroyed %v, want none", again)
	}

	if _, err := e.session.Finder.VirtualMachine(e.ctx, "netcheck-reap-decoy"); err != nil {
		t.Errorf("ReapProbeVMs: decoy VM must survive the reap: %v", err)
	}
}

func TestDestroyProbeVM(t *testing.T) {
	e := newProbeTestEnv(t)

	vm, err := e.session.CreateProbeVM(e.ctx, e.baseSpec("netcheck-destroy-a"))
	if err != nil {
		t.Fatalf("CreateProbeVM: %v", err)
	}
	if err := DestroyProbeVM(e.ctx, vm); err != nil {
		t.Fatalf("DestroyProbeVM: %v", err)
	}
	if _, err := e.session.Finder.VirtualMachine(e.ctx, "netcheck-destroy-a"); err == nil {
		t.Error("DestroyProbeVM: VM still present after destroy")
	}
}

func TestNetworkInfoMatchesAny(t *testing.T) {
	tests := []struct {
		name string
		n    NetworkInfo
		srcs []NetworkInfo
		want bool
	}{
		{
			name: "same subnet and gateway matches",
			n:    NetworkInfo{IP: "192.168.5.42", Prefix: 24, Gateway: "192.168.5.1"},
			srcs: []NetworkInfo{{IP: "192.168.5.10", Prefix: 24, Gateway: "192.168.5.1"}},
			want: true,
		},
		{
			name: "same subnet different gateway does not match",
			n:    NetworkInfo{IP: "192.168.5.42", Prefix: 24, Gateway: "192.168.5.2"},
			srcs: []NetworkInfo{{IP: "192.168.5.10", Prefix: 24, Gateway: "192.168.5.1"}},
		},
		{
			name: "different subnet does not match",
			n:    NetworkInfo{IP: "10.0.1.5", Prefix: 24, Gateway: "192.168.5.1"},
			srcs: []NetworkInfo{{IP: "192.168.5.10", Prefix: 24, Gateway: "192.168.5.1"}},
		},
		{
			name: "different prefix length does not match",
			n:    NetworkInfo{IP: "192.168.5.42", Prefix: 26, Gateway: "192.168.5.1"},
			srcs: []NetworkInfo{{IP: "192.168.5.10", Prefix: 24, Gateway: "192.168.5.1"}},
		},
		{
			name: "empty sources does not match",
			n:    NetworkInfo{IP: "192.168.5.42", Prefix: 24, Gateway: "192.168.5.1"},
		},
		{
			name: "unknown probe prefix does not match",
			n:    NetworkInfo{IP: "192.168.5.42", Prefix: 0, Gateway: "192.168.5.1"},
			srcs: []NetworkInfo{{IP: "192.168.5.10", Prefix: 24, Gateway: "192.168.5.1"}},
		},
		{
			name: "unknown source prefix does not match",
			n:    NetworkInfo{IP: "192.168.5.42", Prefix: 24, Gateway: "192.168.5.1"},
			srcs: []NetworkInfo{{IP: "192.168.5.10", Prefix: 0, Gateway: "192.168.5.1"}},
		},
		{
			name: "identical malformed gateways do not match",
			n:    NetworkInfo{IP: "192.168.5.42", Prefix: 24, Gateway: "not-an-ip"},
			srcs: []NetworkInfo{{IP: "192.168.5.10", Prefix: 24, Gateway: "not-an-ip"}},
		},
		{
			name: "identical ipv6 gateways do not match",
			n:    NetworkInfo{IP: "192.168.5.42", Prefix: 24, Gateway: "fe80::1"},
			srcs: []NetworkInfo{{IP: "192.168.5.10", Prefix: 24, Gateway: "fe80::1"}},
		},
		{
			name: "empty gateways do not match",
			n:    NetworkInfo{IP: "192.168.5.42", Prefix: 24, Gateway: ""},
			srcs: []NetworkInfo{{IP: "192.168.5.10", Prefix: 24, Gateway: ""}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := tt.n.MatchesAny(tt.srcs)
			if got != tt.want {
				t.Errorf("MatchesAny(%+v, %v) = %v, want %v", tt.n, tt.srcs, got, tt.want)
			}
		})
	}
}

func TestNetworksFromGuest(t *testing.T) {
	gatewayStack := func(gw string) types.GuestStackInfo {
		return types.GuestStackInfo{
			IpRouteConfig: &types.NetIpRouteConfigInfo{
				IpRoute: []types.NetIpRouteConfigInfoIpRoute{{
					Network: "0.0.0.0",
					Gateway: types.NetIpRouteConfigInfoGateway{IpAddress: gw},
				}},
			},
		}
	}
	tests := []struct {
		name    string
		guest   *types.GuestInfo
		want    []NetworkInfo
		wantErr bool
	}{
		{
			name: "ipconfig addresses retained with prefix",
			guest: &types.GuestInfo{
				Net: []types.GuestNicInfo{{
					Connected: true,
					IpConfig: &types.NetIpConfigInfo{
						IpAddress: []types.NetIpConfigInfoIpAddress{
							{IpAddress: "10.0.1.5", PrefixLength: 24},
						},
					},
				}},
				IpStack: []types.GuestStackInfo{gatewayStack("10.0.1.1")},
			},
			want: []NetworkInfo{{IP: "10.0.1.5", Prefix: 24, Gateway: "10.0.1.1"}},
		},
		{
			name: "flat-list-only address retained with unknown prefix",
			guest: &types.GuestInfo{
				Net: []types.GuestNicInfo{{
					Connected: true,
					IpAddress: []string{"10.0.2.7"},
				}},
				IpStack: []types.GuestStackInfo{gatewayStack("10.0.2.1")},
			},
			want: []NetworkInfo{{IP: "10.0.2.7", Prefix: 0, Gateway: "10.0.2.1"}},
		},
		{
			name: "duplicate address between ipconfig and flat list reported once",
			guest: &types.GuestInfo{
				Net: []types.GuestNicInfo{{
					Connected: true,
					IpAddress: []string{"10.0.1.5"},
					IpConfig: &types.NetIpConfigInfo{
						IpAddress: []types.NetIpConfigInfoIpAddress{
							{IpAddress: "10.0.1.5", PrefixLength: 24},
						},
					},
				}},
				IpStack: []types.GuestStackInfo{gatewayStack("10.0.1.1")},
			},
			want: []NetworkInfo{{IP: "10.0.1.5", Prefix: 24, Gateway: "10.0.1.1"}},
		},
		{
			name: "ipv6 and malformed addresses excluded",
			guest: &types.GuestInfo{
				Net: []types.GuestNicInfo{{
					Connected: true,
					IpAddress: []string{"fd00::5", "not-an-ip", "10.0.1.5"},
				}},
				IpStack: []types.GuestStackInfo{gatewayStack("10.0.1.1")},
			},
			want: []NetworkInfo{{IP: "10.0.1.5", Prefix: 0, Gateway: "10.0.1.1"}},
		},
		{
			name: "disconnected nic excluded and connected nic retained",
			guest: &types.GuestInfo{
				Net: []types.GuestNicInfo{
					{
						Connected: false,
						IpAddress: []string{"10.0.9.9"},
						IpConfig: &types.NetIpConfigInfo{
							IpAddress: []types.NetIpConfigInfoIpAddress{
								{IpAddress: "10.0.9.9", PrefixLength: 24},
							},
						},
					},
					{
						Connected: true,
						IpAddress: []string{"10.0.1.5"},
					},
				},
				IpStack: []types.GuestStackInfo{gatewayStack("10.0.1.1")},
			},
			want: []NetworkInfo{{IP: "10.0.1.5", Prefix: 0, Gateway: "10.0.1.1"}},
		},
		{
			name: "no default route yields empty gateway",
			guest: &types.GuestInfo{
				Net: []types.GuestNicInfo{{
					Connected: true,
					IpAddress: []string{"10.0.1.5"},
				}},
			},
			want: []NetworkInfo{{IP: "10.0.1.5", Prefix: 0, Gateway: ""}},
		},
		{
			name: "ambiguous default gateways rejected",
			guest: &types.GuestInfo{
				Net: []types.GuestNicInfo{{
					Connected: true,
					IpAddress: []string{"10.0.1.5"},
				}},
				IpStack: []types.GuestStackInfo{gatewayStack("10.0.1.1"), gatewayStack("10.0.1.254")},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := networksFromGuest(tt.guest)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("networksFromGuest: want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("networksFromGuest: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("networksFromGuest = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultGateway(t *testing.T) {
	route := func(network, gw string) types.NetIpRouteConfigInfoIpRoute {
		return types.NetIpRouteConfigInfoIpRoute{
			Network: network,
			Gateway: types.NetIpRouteConfigInfoGateway{IpAddress: gw},
		}
	}
	stack := func(routes ...types.NetIpRouteConfigInfoIpRoute) types.GuestStackInfo {
		return types.GuestStackInfo{IpRouteConfig: &types.NetIpRouteConfigInfo{IpRoute: routes}}
	}
	tests := []struct {
		name    string
		stacks  []types.GuestStackInfo
		want    string
		wantErr bool
	}{
		{
			name:   "single default gateway",
			stacks: []types.GuestStackInfo{stack(route("0.0.0.0", "10.0.1.1"))},
			want:   "10.0.1.1",
		},
		{
			name:   "identical default gateways across stacks are unambiguous",
			stacks: []types.GuestStackInfo{stack(route("0.0.0.0", "10.0.2.1")), stack(route("0.0.0.0", "10.0.2.1"))},
			want:   "10.0.2.1",
		},
		{
			name:    "two distinct default gateways are rejected",
			stacks:  []types.GuestStackInfo{stack(route("0.0.0.0", "10.0.3.1"), route("0.0.0.0", "10.0.3.254"))},
			wantErr: true,
		},
		{
			name:    "two distinct default gateways are rejected in reverse order",
			stacks:  []types.GuestStackInfo{stack(route("0.0.0.0", "10.0.4.254"), route("0.0.0.0", "10.0.4.1"))},
			wantErr: true,
		},
		{
			name:   "ipv6 and non-default routes are ignored",
			stacks: []types.GuestStackInfo{stack(route("::", "fe80::1"), route("10.0.0.0", "10.0.5.1"))},
			want:   "",
		},
		{
			name:   "no routes",
			stacks: []types.GuestStackInfo{stack()},
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := defaultGateway(tt.stacks)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("defaultGateway: want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("defaultGateway: %v", err)
			}
			if got != tt.want {
				t.Errorf("defaultGateway = %q, want %q", got, tt.want)
			}
		})
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
