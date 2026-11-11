package controller

// Probe-VM networking preflight tests against a live vCenter simulator.
//
// netcheckTestEnv is the shared fixture for the probe-VM networking preflight
// (checkNetworkingViaProbeVMs) and collectSourceNetworks, and is reused by the
// later controller tasks that need a live vSphere session. It mirrors the
// vsphere probe_test.go simulator setup plus the controller reconciler wiring.

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

const (
	netcheckInfraID      = "netcheck-infra"
	netcheckNodeName     = "node-a"
	netcheckSourceIP     = "192.168.5.10"
	netcheckSourceGW     = "192.168.5.1"
	netcheckMatchIP      = "192.168.5.20" // same /24 + gateway as the source node
	netcheckMismatchIP   = "10.0.1.20"    // different /24
	netcheckGuestIP      = "10.0.1.5"
	netcheckGuestGateway = "10.0.1.1"
	netcheckGuestCIDR    = "10.0.1.0/24 (gateway 10.0.1.1)"
)

type netcheckTestEnv struct {
	t         *testing.T
	ctx       context.Context
	model     *simulator.Model
	server    *simulator.Server
	host      string
	user      string
	pass      string
	dcName    string
	inv       preflightTestInventory
	infraID   string
	probeStr  string
	finder    *find.Finder
	migration *migrationv1alpha1.VmwareCloudFoundationMigration
	infra     *configv1.Infrastructure
}

func newNetcheckTestEnv(t *testing.T) *netcheckTestEnv {
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
	user := simulator.DefaultLogin.Username()
	pass, ok := simulator.DefaultLogin.Password()
	if !ok {
		t.Fatal("simulator default login missing password")
	}
	host := server.URL.Host
	vsphere.ClearSessions(ctx)
	t.Cleanup(func() { vsphere.ClearSessions(ctx) })

	vc, err := govmomi.NewClient(ctx, server.URL, true)
	if err != nil {
		t.Fatalf("creating govmomi client: %v", err)
	}
	t.Cleanup(func() { _ = vc.Logout(ctx) })
	finder := find.NewFinder(vc.Client, true)
	dc, err := finder.DefaultDatacenter(ctx)
	if err != nil {
		t.Fatalf("finding default datacenter: %v", err)
	}
	finder.SetDatacenter(dc)

	inv := discoverPreflightTestInventory(ctx, t, server.URL, user, pass)

	e := &netcheckTestEnv{
		t:         t,
		ctx:       ctx,
		model:     model,
		server:    server,
		host:      host,
		user:      user,
		pass:      pass,
		dcName:    dc.Name(),
		inv:       inv,
		infraID:   netcheckInfraID,
		probeStr:  probeName(netcheckInfraID, "fd-a"),
		finder:    finder,
		migration: newNetcheckMigration(host, inv),
		infra:     newNetcheckInfra(host, dc.Name()),
	}
	// Rename a spare default VM to the source node name and give it a ready
	// guest on the source network, so collectSourceNetworks can read it.
	e.renameVM(t, "DC0_H0_VM0", netcheckNodeName)
	e.seedVMGuest(t, netcheckNodeName, readyGuestFor(netcheckSourceIP, netcheckSourceGW, 24))
	return e
}

// newNetcheckMigration builds a single-FD migration (fd-a) pointed at the
// simulator inventory.
func newNetcheckMigration(host string, inv preflightTestInventory) *migrationv1alpha1.VmwareCloudFoundationMigration {
	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "migration", Namespace: "default"},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{
				Name: "target-vcenter-creds", Namespace: "default",
			},
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{
					Name: "fd-a", Region: "region-a", Zone: "zone-a", Server: host,
					Topology: configv1.VSpherePlatformTopology{
						Template:       inv.templatePath,
						Datacenter:     inv.datacenterName,
						ComputeCluster: inv.clusterPath,
						Datastore:      inv.datastorePath,
						Networks:       []string{inv.networkPath},
						Folder:         inv.folderPath,
					},
				},
			},
		},
	}
}

// newNetcheckInfra builds an Infrastructure with a fixed infrastructureName
// (used as the infraID for probe names).
func newNetcheckInfra(server, datacenter string) *configv1.Infrastructure {
	return &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
		Spec: configv1.InfrastructureSpec{
			PlatformSpec: configv1.PlatformSpec{
				Type:    configv1.VSpherePlatformType,
				VSphere: &configv1.VSpherePlatformSpec{VCenters: []configv1.VSpherePlatformVCenterSpec{{Server: server, Port: 443, Datacenters: []string{datacenter}}}},
			},
		},
		Status: configv1.InfrastructureStatus{InfrastructureName: netcheckInfraID},
	}
}

// newReconciler builds a reconciler wired to the env's fake clients: the
// migration object, the infrastructure, the source + target credential secrets,
// and the given nodes.
func (e *netcheckTestEnv) newReconciler(t *testing.T, nodeNames []string) *VmwareCloudFoundationMigrationReconciler {
	t.Helper()
	return e.newReconcilerWithSecrets(t, nodeNames, true)
}

func (e *netcheckTestEnv) newReconcilerWithSecrets(t *testing.T, nodeNames []string, withSourceSecret bool) *VmwareCloudFoundationMigrationReconciler {
	t.Helper()
	return &VmwareCloudFoundationMigrationReconciler{
		Client:       e.newClient(t),
		ConfigClient: configfake.NewClientset(e.infra),
		KubeClient:   e.newKubeClient(t, nodeNames, withSourceSecret),
		Recorder:     events.NewFakeRecorder(20),
	}
}

// newClient builds a controller-runtime fake client with the migration object
// (status subresource) for the env's reconciler.
func (e *netcheckTestEnv) newClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding migration to scheme: %v", err)
	}
	return clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(e.migration).
		WithStatusSubresource(e.migration).
		Build()
}

// newKubeClient builds a kube clientset with the target-credentials secret,
// optionally the source-credentials secret, and the given nodes.
func (e *netcheckTestEnv) newKubeClient(t *testing.T, nodeNames []string, withSourceSecret bool) *fakekube.Clientset {
	t.Helper()
	kubeObjects := []runtime.Object{
		newTargetCredentialsSecret("default", "target-vcenter-creds", e.host, e.user, e.pass),
	}
	if withSourceSecret {
		kubeObjects = append(kubeObjects, newTargetCredentialsSecret(openshift.VSphereCredsSecretNamespace, openshift.VSphereCredsSecretName, e.host, e.user, e.pass))
	}
	for _, name := range nodeNames {
		kubeObjects = append(kubeObjects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	return fakekube.NewClientset(kubeObjects...)
}

// reconcilerWithSecrets builds a reconciler with the exact given secrets (for
// cases needing non-default credential keys) plus the given nodes.
func (e *netcheckTestEnv) reconcilerWithSecrets(t *testing.T, nodeNames []string, secrets ...*corev1.Secret) *VmwareCloudFoundationMigrationReconciler {
	t.Helper()
	kubeObjects := make([]runtime.Object, 0, len(secrets)+len(nodeNames))
	for _, s := range secrets {
		kubeObjects = append(kubeObjects, s)
	}
	for _, name := range nodeNames {
		kubeObjects = append(kubeObjects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	return &VmwareCloudFoundationMigrationReconciler{
		Client:       e.newClient(t),
		ConfigClient: configfake.NewClientset(e.infra),
		KubeClient:   fakekube.NewClientset(kubeObjects...),
		Recorder:     events.NewFakeRecorder(20),
	}
}

// recorderOf returns the reconciler's FakeRecorder for event inspection.
func recorderOf(r *VmwareCloudFoundationMigrationReconciler) *events.FakeRecorder {
	fr, _ := r.Recorder.(*events.FakeRecorder)
	return fr
}

// eventsRecorded drains the recorder's event channel.
func eventsRecorded(recorder *events.FakeRecorder) []string {
	var evts []string
	for {
		select {
		case ev := <-recorder.Events:
			evts = append(evts, ev)
		default:
			return evts
		}
	}
}

func containsNetworkMismatch(evts []string) bool {
	for _, ev := range evts {
		if strings.Contains(ev, "Warning") && strings.Contains(ev, "NetworkMismatch") {
			return true
		}
	}
	return false
}

// renameVM renames a simulator VM in the model's registry.
func (e *netcheckTestEnv) renameVM(t *testing.T, fromName, toName string) {
	t.Helper()
	vm, err := e.finder.VirtualMachine(e.ctx, fromName)
	if err != nil {
		t.Fatalf("finding VM %s: %v", fromName, err)
	}
	simVM, ok := e.model.Service.Context.Map.Get(vm.Reference()).(*simulator.VirtualMachine)
	if !ok {
		t.Fatalf("finding simulator VM %s: got %T", fromName, e.model.Service.Context.Map.Get(vm.Reference()))
	}
	e.model.Service.Context.WithLock(simVM, func() {
		simVM.Name = toName
	})
}

// seedGuest sets the guest info snapshot served for the simulator VM with ref.
func (e *netcheckTestEnv) seedGuest(t *testing.T, ref types.ManagedObjectReference, guest *types.GuestInfo) {
	t.Helper()
	simVM, ok := e.model.Service.Context.Map.Get(ref).(*simulator.VirtualMachine)
	if !ok {
		t.Fatalf("finding simulator VM %s: got %T", ref.Value, e.model.Service.Context.Map.Get(ref))
	}
	// Hold the simulator context's per-object lock: guest state is read
	// concurrently by requests served while the test polls. The lock context
	// is the shared Service.Context, so re-entrancy is keyed on that one
	// identity — seed only from the main test goroutine, never in parallel.
	e.model.Service.Context.WithLock(simVM, func() {
		simVM.Guest = guest
	})
}

// createStaleProbe clones a probe VM whose name carries the probe prefix but is
// distinct from the FD probe name, so the check's ReapProbeVMs destroys it and
// seedProbeGuestWhenUp only targets the fresh FD probe.
func (e *netcheckTestEnv) createStaleProbe(t *testing.T) {
	t.Helper()
	session, err := getVSphereSession(e.ctx, e.host, e.dcName, e.user, e.pass)
	if err != nil {
		t.Fatalf("creating session for stale probe: %v", err)
	}
	spec := vsphere.ProbeSpec{
		Name:       probeNamePrefix + e.infraID + "-stale",
		Datacenter: e.dcName,
		Cluster:    e.inv.clusterPath,
		Datastore:  e.inv.datastorePath,
		Folder:     e.inv.folderPath,
		Template:   e.inv.templatePath,
		Network:    e.inv.networkPath,
	}
	if _, err := session.CreateProbeVM(e.ctx, spec); err != nil {
		t.Fatalf("creating stale probe VM: %v", err)
	}
}

// seedVMGuest finds a simulator VM by name and seeds its guest.
func (e *netcheckTestEnv) seedVMGuest(t *testing.T, name string, guest *types.GuestInfo) {
	t.Helper()
	vm, err := e.finder.VirtualMachine(e.ctx, name)
	if err != nil {
		t.Fatalf("finding VM %s: %v", name, err)
	}
	e.seedGuest(t, vm.Reference(), guest)
}

// seedProbeGuestWhenUp polls for the cloned probe VM to appear and seeds its
// guest. It runs in the main test goroutine (so t.Fatal is safe) and returns
// once the guest is seeded or the check is done (success or error), so a check
// that faults before cloning the probe cannot hang the poll.
func (e *netcheckTestEnv) seedProbeGuestWhenUp(t *testing.T, want *types.GuestInfo, done <-chan struct{}) {
	t.Helper()
	for {
		if vm, err := e.finder.VirtualMachine(e.ctx, e.probeStr); err == nil {
			e.seedGuest(t, vm.Reference(), want)
			return
		}
		select {
		case <-done:
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// assertNoProbeVMs fails if any probe VM (exact name or prefix) remains in the
// simulator inventory, proving the deferred/inline DestroyProbeVM ran.
func (e *netcheckTestEnv) assertNoProbeVMs(t *testing.T) {
	t.Helper()
	vms, err := e.finder.VirtualMachineList(e.ctx, "*")
	if err != nil {
		t.Fatalf("listing VMs: %v", err)
	}
	prefix := probeNamePrefix + e.infraID + "-"
	for _, vm := range vms {
		if vm.Name() == e.probeStr || strings.HasPrefix(vm.Name(), prefix) {
			t.Fatalf("probe VM %q still exists after the check", vm.Name())
		}
	}
}

// readyGuestFor returns a guest snapshot with one IPv4 address, a /prefix mask,
// and an IPv4 default route.
func readyGuestFor(ip string, gw string, prefix int32) *types.GuestInfo { //nolint:unparam
	return &types.GuestInfo{
		ToolsRunningStatus: "guestToolsRunning",
		Net: []types.GuestNicInfo{{
			Network:   "netcheck-network",
			Connected: true,
			IpAddress: []string{ip},
			IpConfig: &types.NetIpConfigInfo{
				IpAddress: []types.NetIpConfigInfoIpAddress{{IpAddress: ip, PrefixLength: prefix}},
			},
		}},
		IpStack: []types.GuestStackInfo{{
			IpRouteConfig: &types.NetIpRouteConfigInfo{
				IpRoute: []types.NetIpRouteConfigInfoIpRoute{{
					Network: "0.0.0.0",
					Gateway: types.NetIpRouteConfigInfoGateway{IpAddress: gw},
				}},
			},
		}},
	}
}

// noNetworkGuest returns a tools-running guest with no NICs, so GetVMNetworks
// returns an empty list.
func noNetworkGuest() *types.GuestInfo {
	return &types.GuestInfo{ToolsRunningStatus: "guestToolsRunning"}
}

func newNetcheckSourceVC(host, dc string) *configv1.VSpherePlatformVCenterSpec {
	return &configv1.VSpherePlatformVCenterSpec{Server: host, Datacenters: []string{dc}}
}

// runProbeCheck runs checkNetworkingViaProbeVMs in a goroutine, seeds the probe
// guest (optionally) as soon as the probe VM appears, and returns the result.
func (e *netcheckTestEnv) runProbeCheck(t *testing.T, reconciler *VmwareCloudFoundationMigrationReconciler, ctx context.Context, probeGuest *types.GuestInfo) error {
	t.Helper()
	checkDone := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		err := reconciler.checkNetworkingViaProbeVMs(ctx, e.migration, newNetcheckSourceVC(e.host, e.dcName))
		checkDone <- err
		close(done)
	}()
	if probeGuest != nil {
		e.seedProbeGuestWhenUp(t, probeGuest, done)
	}
	return <-checkDone
}

func TestFormatNetworksEdges(t *testing.T) {
	tests := []struct {
		name  string
		infos []vsphere.NetworkInfo
		want  string
	}{
		{"empty", nil, ""},
		{"single", []vsphere.NetworkInfo{{IP: netcheckGuestIP, Prefix: 24, Gateway: netcheckGuestGateway}}, netcheckGuestCIDR},
		{"duplicates de-duplicated", []vsphere.NetworkInfo{
			{IP: netcheckGuestIP, Prefix: 24, Gateway: netcheckGuestGateway},
			{IP: "10.0.1.6", Prefix: 24, Gateway: netcheckGuestGateway},
		}, netcheckGuestCIDR},
		{"invalid ip skipped", []vsphere.NetworkInfo{
			{IP: "not-an-ip", Prefix: 24, Gateway: netcheckGuestGateway},
			{IP: netcheckGuestIP, Prefix: 24, Gateway: netcheckGuestGateway},
		}, netcheckGuestCIDR},
		{"prefix out of range skipped", []vsphere.NetworkInfo{
			{IP: netcheckGuestIP, Prefix: 33, Gateway: netcheckGuestGateway},
			{IP: netcheckGuestIP, Prefix: 24, Gateway: netcheckGuestGateway},
		}, netcheckGuestCIDR},
		{"all invalid", []vsphere.NetworkInfo{
			{IP: "x", Prefix: 24, Gateway: "g"},
			{IP: "y", Prefix: 99, Gateway: "g"},
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatNetworks(tt.infos); got != tt.want {
				t.Fatalf("formatNetworks = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckNetworkingViaProbeVMsMatch(t *testing.T) {
	e := newNetcheckTestEnv(t)
	reconciler := e.newReconciler(t, []string{netcheckNodeName})
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	defer cancel()
	// The probe guest is seeded on the SAME /24 + gateway as the source node, so
	// the probe network matches and preflight passes.
	err := e.runProbeCheck(t, reconciler, ctx, readyGuestFor(netcheckMatchIP, netcheckSourceGW, 24))
	if err != nil {
		t.Fatalf("checkNetworkingViaProbeVMs = %v, want nil", err)
	}
	e.assertNoProbeVMs(t)
}

// TestCheckNetworkingViaProbeVMsReapsStaleProbe plants a stale probe VM before
// the check and asserts the check reaps it and the fresh probe still matches.
func TestCheckNetworkingViaProbeVMsReapsStaleProbe(t *testing.T) {
	e := newNetcheckTestEnv(t)
	e.createStaleProbe(t)
	reconciler := e.newReconciler(t, []string{netcheckNodeName})
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	defer cancel()
	err := e.runProbeCheck(t, reconciler, ctx, readyGuestFor(netcheckMatchIP, netcheckSourceGW, 24))
	if err != nil {
		t.Fatalf("checkNetworkingViaProbeVMs = %v, want nil (stale probe reaped, fresh probe matches)", err)
	}
	e.assertNoProbeVMs(t)
}

func TestCheckNetworkingViaProbeVMsMismatch(t *testing.T) {
	e := newNetcheckTestEnv(t)
	reconciler := e.newReconciler(t, []string{netcheckNodeName})
	recorder := recorderOf(reconciler)
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	defer cancel()
	// The probe guest is on a different /24 than the source node, so the network
	// does not match and preflight fails with a NetworkMismatch event.
	err := e.runProbeCheck(t, reconciler, ctx, readyGuestFor(netcheckMismatchIP, netcheckGuestGateway, 24))
	if err == nil {
		t.Fatal("checkNetworkingViaProbeVMs = nil, want a mismatch error")
	}
	for _, want := range []string{
		"destination networking preflight failed:",
		`failure domain "fd-a"`,
		"probe network 10.0.1.0/24 (gateway 10.0.1.1) does not match any source node network",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
	if got := eventsRecorded(recorder); !containsNetworkMismatch(got) {
		t.Fatalf("recorded events = %v, want a Warning NetworkMismatch event", got)
	}
	e.assertNoProbeVMs(t)
}

// TestCheckNetworkingViaProbeVMsErrors exercises each hard-error branch of the
// probe-VM networking preflight and asserts no probe VM leaks after each.
func TestCheckNetworkingViaProbeVMsErrors(t *testing.T) {
	t.Run("two datacenters", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		r := e.newReconciler(t, []string{netcheckNodeName})
		vc := newNetcheckSourceVC(e.host, e.dcName)
		vc.Datacenters = append(vc.Datacenters, e.dcName)
		err := r.checkNetworkingViaProbeVMs(e.ctx, e.migration, vc)
		if err == nil || !strings.Contains(err.Error(), "has 2 datacenters configured; exactly one is required") {
			t.Fatalf("err = %v, want two-datacenters error", err)
		}
		e.assertNoProbeVMs(t)
	})
	t.Run("no datacenters", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		r := e.newReconciler(t, []string{netcheckNodeName})
		vc := newNetcheckSourceVC(e.host, e.dcName)
		vc.Datacenters = nil
		err := r.checkNetworkingViaProbeVMs(e.ctx, e.migration, vc)
		if err == nil || !strings.Contains(err.Error(), "has 0 datacenters configured; exactly one is required") {
			t.Fatalf("err = %v, want zero-datacenters error", err)
		}
		e.assertNoProbeVMs(t)
	})
	t.Run("infra get error", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		configClient := configfake.NewClientset(e.infra)
		configClient.PrependReactor("get", "infrastructures", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		r := &VmwareCloudFoundationMigrationReconciler{
			Client:       e.newClient(t),
			ConfigClient: configClient,
			KubeClient:   e.newKubeClient(t, []string{netcheckNodeName}, true),
			Recorder:     events.NewFakeRecorder(20),
		}
		err := r.checkNetworkingViaProbeVMs(e.ctx, e.migration, newNetcheckSourceVC(e.host, e.dcName))
		if err == nil || !strings.Contains(err.Error(), "getting infrastructure ID") {
			t.Fatalf("err = %v, want infrastructure ID error", err)
		}
		e.assertNoProbeVMs(t)
	})
	t.Run("missing source creds secret", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		r := e.newReconcilerWithSecrets(t, []string{netcheckNodeName}, false)
		err := r.checkNetworkingViaProbeVMs(e.ctx, e.migration, newNetcheckSourceVC(e.host, e.dcName))
		if err == nil || !strings.Contains(err.Error(), "getting source vCenter credentials for "+e.host) {
			t.Fatalf("err = %v, want missing source creds error", err)
		}
		e.assertNoProbeVMs(t)
	})
	t.Run("no source nodes", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		r := e.newReconciler(t, nil)
		err := r.checkNetworkingViaProbeVMs(e.ctx, e.migration, newNetcheckSourceVC(e.host, e.dcName))
		if err == nil || !strings.Contains(err.Error(), "no source node networks could be read") {
			t.Fatalf("err = %v, want no-source-nodes error", err)
		}
		e.assertNoProbeVMs(t)
	})
	t.Run("target creds missing", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		r := e.newReconciler(t, []string{netcheckNodeName})
		m := e.migration.DeepCopy()
		m.Spec.FailureDomains[0].Server = "other-host"
		err := r.checkNetworkingViaProbeVMs(e.ctx, m, newNetcheckSourceVC(e.host, e.dcName))
		if err == nil || !strings.Contains(err.Error(), "getting credentials for target other-host") {
			t.Fatalf("err = %v, want target creds error", err)
		}
		e.assertNoProbeVMs(t)
	})
	t.Run("unreachable source vCenter", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		unreachable := unreachableVCAddr
		// Source-creds secret keyed on the unreachable server so the creds
		// resolve and the connection itself is what fails.
		r := e.reconcilerWithSecrets(t, []string{netcheckNodeName},
			newTargetCredentialsSecret(openshift.VSphereCredsSecretNamespace, openshift.VSphereCredsSecretName, unreachable, e.user, e.pass),
			newTargetCredentialsSecret("default", "target-vcenter-creds", e.host, e.user, e.pass),
		)
		err := r.checkNetworkingViaProbeVMs(e.ctx, e.migration, newNetcheckSourceVC(unreachable, e.dcName))
		if err == nil || !strings.Contains(err.Error(), "connecting to source vCenter "+unreachable) {
			t.Fatalf("err = %v, want unreachable source error", err)
		}
		e.assertNoProbeVMs(t)
	})
	t.Run("no source node networks", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		// A node whose VM does not exist makes GetVMNetworks fail per node.
		r := e.newReconciler(t, []string{"node-missing"})
		err := r.checkNetworkingViaProbeVMs(e.ctx, e.migration, newNetcheckSourceVC(e.host, e.dcName))
		if err == nil || !strings.Contains(err.Error(), "reading networks for source node node-missing") {
			t.Fatalf("err = %v, want per-node network read error", err)
		}
		e.assertNoProbeVMs(t)
	})
	t.Run("target session unreachable", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		unreachable := unreachableVCAddr
		// Target-creds secret keyed on the unreachable server (source creds on
		// the real host) so both cred lookups resolve and only the target
		// session fails.
		r := e.reconcilerWithSecrets(t, []string{netcheckNodeName},
			newTargetCredentialsSecret(openshift.VSphereCredsSecretNamespace, openshift.VSphereCredsSecretName, e.host, e.user, e.pass),
			newTargetCredentialsSecret("default", "target-vcenter-creds", unreachable, e.user, e.pass),
		)
		m := e.migration.DeepCopy()
		m.Spec.FailureDomains[0].Server = unreachable
		err := r.checkNetworkingViaProbeVMs(e.ctx, m, newNetcheckSourceVC(e.host, e.dcName))
		if err == nil || !strings.Contains(err.Error(), "connecting to target vCenter "+unreachable) {
			t.Fatalf("err = %v, want unreachable target error", err)
		}
		e.assertNoProbeVMs(t)
	})
	t.Run("template not found", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		r := e.newReconciler(t, []string{netcheckNodeName})
		m := e.migration.DeepCopy()
		m.Spec.FailureDomains[0].Topology.Template = "/DC0/vm/no-such-template"
		err := r.checkNetworkingViaProbeVMs(e.ctx, m, newNetcheckSourceVC(e.host, e.dcName))
		if err == nil || !strings.Contains(err.Error(), `creating probe VM `) || !strings.Contains(err.Error(), `on failure domain "fd-a"`) {
			t.Fatalf("err = %v, want probe VM creation (template) error", err)
		}
		e.assertNoProbeVMs(t)
	})
	t.Run("guest network timeout", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		r := e.newReconciler(t, []string{netcheckNodeName})
		// Short parent deadline: the probe is created but its guest is never
		// seeded, so WaitForGuestNetworks hits the deadline before the 5s poll.
		ctx, cancel := context.WithTimeout(e.ctx, 4*time.Second)
		defer cancel()
		err := r.checkNetworkingViaProbeVMs(ctx, e.migration, newNetcheckSourceVC(e.host, e.dcName))
		if err == nil || !strings.Contains(err.Error(), "waiting for guest networks on probe VM") {
			t.Fatalf("err = %v, want guest network timeout error", err)
		}
		e.assertNoProbeVMs(t)
	})
}

// TestCollectSourceNetworks covers collectSourceNetworks' success and error
// branches against the simulator.
func TestCollectSourceNetworks(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		r := e.newReconciler(t, []string{netcheckNodeName})
		sourceSession, err := getVSphereSession(e.ctx, e.host, e.dcName, e.user, e.pass)
		if err != nil {
			t.Fatalf("creating source session: %v", err)
		}
		nets, err := r.collectSourceNetworks(e.ctx, sourceSession)
		if err != nil {
			t.Fatalf("collectSourceNetworks: %v", err)
		}
		if got := formatNetworks(nets); got != "192.168.5.0/24 (gateway 192.168.5.1)" {
			t.Fatalf("formatNetworks = %q, want the source network", got)
		}
	})
	t.Run("node list error", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		kubeClient := e.newKubeClient(t, []string{netcheckNodeName}, true)
		kubeClient.PrependReactor("list", "nodes", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		r := &VmwareCloudFoundationMigrationReconciler{
			Client:       e.newClient(t),
			ConfigClient: configfake.NewClientset(e.infra),
			KubeClient:   kubeClient,
			Recorder:     events.NewFakeRecorder(20),
		}
		sourceSession, err := getVSphereSession(e.ctx, e.host, e.dcName, e.user, e.pass)
		if err != nil {
			t.Fatalf("creating source session: %v", err)
		}
		if _, err := r.collectSourceNetworks(e.ctx, sourceSession); err == nil || !strings.Contains(err.Error(), "listing nodes") {
			t.Fatalf("err = %v, want listing-nodes error", err)
		}
	})
	t.Run("node VM missing", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		r := e.newReconciler(t, []string{"node-missing"})
		sourceSession, err := getVSphereSession(e.ctx, e.host, e.dcName, e.user, e.pass)
		if err != nil {
			t.Fatalf("creating source session: %v", err)
		}
		if _, err := r.collectSourceNetworks(e.ctx, sourceSession); err == nil || !strings.Contains(err.Error(), "reading networks for source node node-missing") {
			t.Fatalf("err = %v, want per-node read error", err)
		}
	})
	t.Run("node VM has no networks", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		e.renameVM(t, "DC0_H0_VM1", "node-nonetwork")
		e.seedVMGuest(t, "node-nonetwork", noNetworkGuest())
		r := e.newReconciler(t, []string{"node-nonetwork"})
		sourceSession, err := getVSphereSession(e.ctx, e.host, e.dcName, e.user, e.pass)
		if err != nil {
			t.Fatalf("creating source session: %v", err)
		}
		if _, err := r.collectSourceNetworks(e.ctx, sourceSession); err == nil || !strings.Contains(err.Error(), "no networks reported") {
			t.Fatalf("err = %v, want no-networks error", err)
		}
	})
}

// Residual branches in checkNetworkingViaProbeVMs (preflight_netcheck.go) that
// are reachable but not exercised here: the ReapProbeVMs finder-list error
// (needs a vCenter fault on the VM list) and its outer return; the deferred
// probe destroy, a double-safety that only fires if a step after the defer is
// registered returns without setting probeDestroyed (the wait-step's inline
// cleanup already does); and the final inline destroy's error path (needs a
// faulting execution check or vCenter destroy fault, not set up here).
