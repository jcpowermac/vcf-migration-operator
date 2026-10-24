package controller

import (
	"context"
	"crypto/tls"
	"reflect"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	machinefake "github.com/openshift/client-go/machine/clientset/versioned/fake"
	"github.com/vmware/govmomi/simulator"
	_ "github.com/vmware/govmomi/vapi/simulator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	fakekube "k8s.io/client-go/kubernetes/fake"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

func fdSpec(name string) configv1.VSpherePlatformFailureDomainSpec {
	return configv1.VSpherePlatformFailureDomainSpec{Name: name}
}

func mappingList(pairs ...[2]string) []migrationv1alpha1.FailureDomainMapping {
	mappings := make([]migrationv1alpha1.FailureDomainMapping, len(pairs))
	for i, pair := range pairs {
		mappings[i] = migrationv1alpha1.FailureDomainMapping{Source: pair[0], Destination: pair[1]}
	}
	return mappings
}

func fdSpecs(names ...string) []configv1.VSpherePlatformFailureDomainSpec {
	fds := make([]configv1.VSpherePlatformFailureDomainSpec, len(names))
	for i, name := range names {
		fds[i] = fdSpec(name)
	}
	return fds
}

// newTestInfraWithVCenters builds an Infrastructure with the given vCenter entries
// for tests that exercise source-vCenter validation.
func newTestInfraWithVCenters(vcenters []configv1.VSpherePlatformVCenterSpec) *configv1.Infrastructure {
	return &configv1.Infrastructure{
		Spec: configv1.InfrastructureSpec{
			PlatformSpec: configv1.PlatformSpec{
				VSphere: &configv1.VSpherePlatformSpec{VCenters: vcenters},
			},
		},
	}
}

func TestResolveFailureDomainMappings(t *testing.T) {
	tests := []struct {
		name       string
		mappings   []migrationv1alpha1.FailureDomainMapping
		sourceFDs  []configv1.VSpherePlatformFailureDomainSpec
		destFDs    []configv1.VSpherePlatformFailureDomainSpec
		want       []migrationv1alpha1.FailureDomainMapping
		wantErr    bool
		wantSubstr string
	}{
		{
			name:      "explicit bijection",
			mappings:  mappingList([2]string{"s1", "d1"}, [2]string{"s2", "d2"}),
			sourceFDs: fdSpecs("s1", "s2"),
			destFDs:   fdSpecs("d1", "d2"),
			want:      mappingList([2]string{"s1", "d1"}, [2]string{"s2", "d2"}),
		},
		{
			name:      "omitted 1:1",
			sourceFDs: fdSpecs("s1"),
			destFDs:   fdSpecs("d1"),
			want:      mappingList([2]string{"s1", "d1"}),
		},
		{
			name:       "omitted with 2 dest FDs",
			sourceFDs:  fdSpecs("s1"),
			destFDs:    fdSpecs("d1", "d2"),
			wantErr:    true,
			wantSubstr: "provide mappings",
		},
		{
			name:       "omitted with 2 source FDs",
			sourceFDs:  fdSpecs("s1", "s2"),
			destFDs:    fdSpecs("d1"),
			wantErr:    true,
			wantSubstr: "provide mappings",
		},
		{
			name:       "unknown source name",
			mappings:   mappingList([2]string{"nope", "d1"}),
			sourceFDs:  fdSpecs("s1"),
			destFDs:    fdSpecs("d1"),
			wantErr:    true,
			wantSubstr: `"nope"`,
		},
		{
			name:       "unknown destination name",
			mappings:   mappingList([2]string{"s1", "nope"}),
			sourceFDs:  fdSpecs("s1"),
			destFDs:    fdSpecs("d1"),
			wantErr:    true,
			wantSubstr: `"nope"`,
		},
		{
			name:       "partial coverage",
			mappings:   mappingList([2]string{"s1", "d1"}),
			sourceFDs:  fdSpecs("s1", "s2"),
			destFDs:    fdSpecs("d1", "d2"),
			wantErr:    true,
			wantSubstr: "must cover",
		},
		{
			name:       "wrong cardinality",
			mappings:   mappingList([2]string{"s1", "d1"}, [2]string{"s2", "d2"}),
			sourceFDs:  fdSpecs("s1", "s2"),
			destFDs:    fdSpecs("d1", "d2", "d3"),
			wantErr:    true,
			wantSubstr: "must cover",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveFailureDomainMappings(tt.mappings, tt.sourceFDs, tt.destFDs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveFailureDomainMappings succeeded, want error containing %q", tt.wantSubstr)
				}
				if !strings.Contains(err.Error(), tt.wantSubstr) {
					t.Fatalf("resolveFailureDomainMappings error = %q, want substring %q", err.Error(), tt.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveFailureDomainMappings: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("resolveFailureDomainMappings = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestIsSuffixedMachineSetName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		base string
		want bool
	}{
		{"suffixed", "test-infra-fd-a-2", "test-infra-fd-a", true},
		{"double digit", "test-infra-fd-a-12", "test-infra-fd-a", true},
		{"leading zero", "test-infra-fd-a-02", "test-infra-fd-a", true},
		{"exact base", "test-infra-fd-a", "test-infra-fd-a", false},
		{"empty suffix", "test-infra-fd-a-", "test-infra-fd-a", false},
		{"non numeric", "test-infra-fd-a-2x", "test-infra-fd-a", false},
		{"different base", "other-fd-a-2", "test-infra-fd-a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSuffixedMachineSetName(tt.in, tt.base); got != tt.want {
				t.Errorf("isSuffixedMachineSetName(%q, %q) = %v, want %v", tt.in, tt.base, got, tt.want)
			}
		})
	}
}

func TestSuffixedWorkerMachineSetName(t *testing.T) {
	tests := []struct {
		name string
		base string
		si   int
		want string
	}{
		{"first copy keeps the base name", "test-infra-fd-a", 0, "test-infra-fd-a"},
		{"second copy is suffixed -2", "test-infra-fd-a", 1, "test-infra-fd-a-2"},
		{"third copy is suffixed -3", "test-infra-fd-a", 2, "test-infra-fd-a-3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := suffixedWorkerMachineSetName(tt.base, tt.si); got != tt.want {
				t.Errorf("suffixedWorkerMachineSetName(%q, %d) = %q, want %q", tt.base, tt.si, got, tt.want)
			}
		})
	}
}

func TestWorkerMachineSetNamesForBase(t *testing.T) {
	newMS := func(name string) *machinev1beta1.MachineSet {
		return &machinev1beta1.MachineSet{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	msList := []*machinev1beta1.MachineSet{
		newMS("other-fd"),
		newMS("test-infra-fd-a-3"),
		newMS("test-infra-fd-a"),
		newMS("test-infra-fd-a-2"),
		newMS("test-infra-fd-a-2x"),
		newMS("test-infra-fd-b-2"),
	}
	tests := []struct {
		name string
		base string
		list []*machinev1beta1.MachineSet
		want []string
	}{
		{
			name: "base plus existing suffixed copies",
			base: "test-infra-fd-a",
			list: msList,
			want: []string{"test-infra-fd-a", "test-infra-fd-a-3", "test-infra-fd-a-2"},
		},
		{
			name: "missing base still yields the base name",
			base: "test-infra-fd-c",
			list: msList,
			want: []string{"test-infra-fd-c"},
		},
		{
			name: "empty machineset list yields the base name",
			base: "test-infra-fd-a",
			list: nil,
			want: []string{"test-infra-fd-a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workerMachineSetNamesForBase(tt.list, tt.base)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("workerMachineSetNamesForBase(%q) = %v, want %v", tt.base, got, tt.want)
			}
		})
	}
}

func TestValidateFailureDomainServers(t *testing.T) {
	tests := []struct {
		name       string
		fds        []configv1.VSpherePlatformFailureDomainSpec
		wantErr    bool
		wantSubstr string
	}{
		{
			name: "single server",
			fds: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "fd-a", Server: "vc.example.com"},
				{Name: "fd-b", Server: "vc.example.com"},
			},
		},
		{
			name: "two servers",
			fds: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "fd-a", Server: "vc-one.example.com"},
				{Name: "fd-b", Server: "vc-two.example.com"},
			},
			wantErr:    true,
			wantSubstr: "vc-one.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFailureDomainServers(tt.fds)
			if tt.wantErr {
				if err == nil {
					t.Fatal("validateFailureDomainServers succeeded, want error")
				}
				if !strings.Contains(err.Error(), tt.wantSubstr) || !strings.Contains(err.Error(), "vc-two.example.com") {
					t.Fatalf("validateFailureDomainServers error = %q, want both server names", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("validateFailureDomainServers: %v", err)
			}
		})
	}
}

func TestValidateSourceVCenterCount(t *testing.T) {
	tests := []struct {
		name       string
		vcenters   []configv1.VSpherePlatformVCenterSpec
		wantErr    bool
		wantSubstr string
	}{
		{
			name:     "one vCenter",
			vcenters: []configv1.VSpherePlatformVCenterSpec{{Server: "vc.example.com"}},
		},
		{
			name:       "two vCenters",
			vcenters:   []configv1.VSpherePlatformVCenterSpec{{Server: "vc.example.com"}, {Server: "vc2.example.com"}},
			wantErr:    true,
			wantSubstr: "found 2",
		},
		{
			name:     "zero vCenters",
			vcenters: nil,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infra := newTestInfraWithVCenters(tt.vcenters)
			err := validateSourceVCenterCount(infra)
			if tt.wantErr {
				if err == nil {
					t.Fatal("validateSourceVCenterCount succeeded, want error")
				}
				if tt.wantSubstr != "" && !strings.Contains(err.Error(), tt.wantSubstr) {
					t.Fatalf("validateSourceVCenterCount error = %q, want substring %q", err.Error(), tt.wantSubstr)
				}
				if !strings.Contains(err.Error(), "exactly one") {
					t.Fatalf("validateSourceVCenterCount error = %q, want the supported limit of exactly one vCenter", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("validateSourceVCenterCount: %v", err)
			}
		})
	}
}

func TestValidateSourceFDTopologyDisjoint(t *testing.T) {
	fd := func(name, datacenter, pool string) configv1.VSpherePlatformFailureDomainSpec {
		return configv1.VSpherePlatformFailureDomainSpec{
			Name: name,
			Topology: configv1.VSpherePlatformTopology{
				Datacenter:   datacenter,
				ResourcePool: pool,
			},
		}
	}
	// fdInCluster leaves topology.resourcePool empty, which resolves to the
	// compute cluster's default pool rather than matching every pool.
	fdInCluster := func(name, datacenter, cluster string) configv1.VSpherePlatformFailureDomainSpec {
		return configv1.VSpherePlatformFailureDomainSpec{
			Name: name,
			Topology: configv1.VSpherePlatformTopology{
				Datacenter:     datacenter,
				ComputeCluster: cluster,
			},
		}
	}

	tests := []struct {
		name       string
		fds        []configv1.VSpherePlatformFailureDomainSpec
		wantErr    bool
		wantSubstr string
	}{
		{
			name: "same datacenter, distinct pools",
			fds: []configv1.VSpherePlatformFailureDomainSpec{
				fd("fd-a", "DC0", "/DC0/host/c1/Resources/pool-a"),
				fd("fd-b", "DC0", "/DC0/host/c1/Resources/pool-b"),
			},
		},
		{
			name: "same datacenter, one pool empty",
			fds: []configv1.VSpherePlatformFailureDomainSpec{
				fd("fd-a", "DC0", "/DC0/host/c1/Resources/pool-a"),
				fd("fd-b", "DC0", ""),
			},
			wantErr:    true,
			wantSubstr: "overlapping",
		},
		{
			name: "same datacenter, same pool",
			fds: []configv1.VSpherePlatformFailureDomainSpec{
				fd("fd-a", "DC0", "/DC0/host/c1/Resources/pool-a"),
				fd("fd-b", "DC0", "/DC0/host/c1/Resources/pool-a"),
			},
			wantErr:    true,
			wantSubstr: "overlapping",
		},
		{
			name: "different datacenters",
			fds: []configv1.VSpherePlatformFailureDomainSpec{
				fd("fd-a", "DC0", ""),
				fd("fd-b", "DC1", ""),
			},
		},
		{
			// An empty resource pool is not a wildcard: each failure domain
			// resolves to its own compute cluster's default pool, so a source
			// MachineSet can match at most one of them.
			name: "same datacenter, empty pools in distinct compute clusters",
			fds: []configv1.VSpherePlatformFailureDomainSpec{
				fdInCluster("fd-a", "DC0", "/DC0/host/c1"),
				fdInCluster("fd-b", "DC0", "/DC0/host/c2"),
			},
		},
		{
			name: "same datacenter, empty pools in the same compute cluster",
			fds: []configv1.VSpherePlatformFailureDomainSpec{
				fdInCluster("fd-a", "DC0", "/DC0/host/c1"),
				fdInCluster("fd-b", "DC0", "/DC0/host/c1"),
			},
			wantErr:    true,
			wantSubstr: "overlapping",
		},
		{
			name: "same datacenter, explicit pool equal to another failure domain's default pool",
			fds: []configv1.VSpherePlatformFailureDomainSpec{
				fd("fd-a", "DC0", "/DC0/host/c1/Resources"),
				fdInCluster("fd-b", "DC0", "/DC0/host/c1"),
			},
			wantErr:    true,
			wantSubstr: "overlapping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSourceFDTopologyDisjoint(tt.fds)
			if tt.wantErr {
				if err == nil {
					t.Fatal("validateSourceFDTopologyDisjoint succeeded, want error")
				}
				if !strings.Contains(err.Error(), tt.wantSubstr) || !strings.Contains(err.Error(), "DC0") {
					t.Fatalf("validateSourceFDTopologyDisjoint error = %q, want %q naming the datacenter", err.Error(), tt.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateSourceFDTopologyDisjoint: %v", err)
			}
		})
	}
}

// TestMatchesSourceFailureDomain pins the failure domain predicate the coverage
// gates use. An empty topology.resourcePool resolves to the compute cluster's
// default pool instead of matching every pool in the datacenter, so a source
// MachineSet in another cluster's default pool is not attributed to this
// failure domain. Only a failure domain with no determinable pool is a wildcard.
func TestMatchesSourceFailureDomain(t *testing.T) {
	tests := []struct {
		name      string
		workspace machinev1beta1.Workspace
		fd        configv1.VSpherePlatformFailureDomainSpec
		want      bool
	}{
		{
			name:      "empty pool with a compute cluster matches that cluster's default pool",
			workspace: machinev1beta1.Workspace{Datacenter: "DC0", ResourcePool: "/DC0/host/c1/Resources"},
			fd:        configv1.VSpherePlatformFailureDomainSpec{Topology: configv1.VSpherePlatformTopology{Datacenter: "DC0", ComputeCluster: "/DC0/host/c1"}},
			want:      true,
		},
		{
			name:      "empty pool with a compute cluster rejects another cluster's default pool",
			workspace: machinev1beta1.Workspace{Datacenter: "DC0", ResourcePool: "/DC0/host/c2/Resources"},
			fd:        configv1.VSpherePlatformFailureDomainSpec{Topology: configv1.VSpherePlatformTopology{Datacenter: "DC0", ComputeCluster: "/DC0/host/c1"}},
			want:      false,
		},
		{
			name:      "empty pool with a compute cluster rejects a custom pool in that cluster",
			workspace: machinev1beta1.Workspace{Datacenter: "DC0", ResourcePool: "/DC0/host/c1/Resources/pool-a"},
			fd:        configv1.VSpherePlatformFailureDomainSpec{Topology: configv1.VSpherePlatformTopology{Datacenter: "DC0", ComputeCluster: "/DC0/host/c1"}},
			want:      false,
		},
		{
			// Unchanged behaviour: with neither field set no pool is
			// determinable, so every pool in the datacenter matches.
			name:      "empty pool without a compute cluster matches every pool in the datacenter",
			workspace: machinev1beta1.Workspace{Datacenter: "DC0", ResourcePool: "/DC0/host/c2/Resources"},
			fd:        configv1.VSpherePlatformFailureDomainSpec{Topology: configv1.VSpherePlatformTopology{Datacenter: "DC0"}},
			want:      true,
		},
		{
			name:      "explicit pool matches only that pool",
			workspace: machinev1beta1.Workspace{Datacenter: "DC0", ResourcePool: "/DC0/host/c1/Resources/pool-a"},
			fd:        configv1.VSpherePlatformFailureDomainSpec{Topology: configv1.VSpherePlatformTopology{Datacenter: "DC0", ResourcePool: "/DC0/host/c1/Resources/pool-a", ComputeCluster: "/DC0/host/c1"}},
			want:      true,
		},
		{
			name:      "explicit pool rejects another pool in the same datacenter",
			workspace: machinev1beta1.Workspace{Datacenter: "DC0", ResourcePool: "/DC0/host/c1/Resources/pool-b"},
			fd:        configv1.VSpherePlatformFailureDomainSpec{Topology: configv1.VSpherePlatformTopology{Datacenter: "DC0", ResourcePool: "/DC0/host/c1/Resources/pool-a", ComputeCluster: "/DC0/host/c1"}},
			want:      false,
		},
		{
			name:      "different datacenter never matches",
			workspace: machinev1beta1.Workspace{Datacenter: "DC1", ResourcePool: "/DC0/host/c1/Resources"},
			fd:        configv1.VSpherePlatformFailureDomainSpec{Topology: configv1.VSpherePlatformTopology{Datacenter: "DC0", ComputeCluster: "/DC0/host/c1"}},
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesSourceFailureDomain(&tt.workspace, tt.fd); got != tt.want {
				t.Fatalf("matchesSourceFailureDomain(%q, %+v) = %v, want %v", tt.workspace.ResourcePool, tt.fd.Topology, got, tt.want)
			}
		})
	}
}

func TestEnsureMappingImmutable(t *testing.T) {
	explicit := mappingList([2]string{"s1", "d1"}, [2]string{"s2", "d2"})

	// Infrastructure variants proving drift does not participate in the check.
	destFDAdded := newInfrastructureForPreflight("vc.example.com", "DC0")
	destFDAdded.Spec.PlatformSpec.VSphere.FailureDomains = append(destFDAdded.Spec.PlatformSpec.VSphere.FailureDomains, configv1.VSpherePlatformFailureDomainSpec{
		Name:   "fd-c",
		Server: "vc.example.com",
	})
	sourceFDsRemoved := newInfrastructureForPreflight("vc.example.com", "DC0")
	sourceFDsRemoved.Spec.PlatformSpec.VSphere.FailureDomains = sourceFDsRemoved.Spec.PlatformSpec.VSphere.FailureDomains[:1]

	tests := []struct {
		name       string
		status     []migrationv1alpha1.FailureDomainMapping
		spec       []migrationv1alpha1.FailureDomainMapping
		infra      *configv1.Infrastructure
		wantErr    bool
		wantSubstr string
	}{
		{
			name:  "status unset",
			spec:  explicit,
			infra: newInfrastructureForPreflight("vc.example.com", "DC0"),
		},
		{
			name:   "unchanged spec",
			status: explicit,
			spec:   explicit,
			infra:  newInfrastructureForPreflight("vc.example.com", "DC0"),
		},
		{
			name:   "reordered spec",
			status: explicit,
			spec:   mappingList([2]string{"s2", "d2"}, [2]string{"s1", "d1"}),
			infra:  newInfrastructureForPreflight("vc.example.com", "DC0"),
		},
		{
			name:       "edited spec",
			status:     explicit,
			spec:       mappingList([2]string{"s1", "d1"}, [2]string{"s2", "d3"}),
			infra:      newInfrastructureForPreflight("vc.example.com", "DC0"),
			wantErr:    true,
			wantSubstr: "failureDomainMappings are immutable",
		},
		{
			name:       "omitted then edited",
			status:     mappingList([2]string{"s1", "d1"}),
			spec:       mappingList([2]string{"s1", "d2"}),
			infra:      newInfrastructureForPreflight("vc.example.com", "DC0"),
			wantErr:    true,
			wantSubstr: "failureDomainMappings are immutable",
		},
		{
			name:       "renamed source failure domain",
			status:     explicit,
			spec:       mappingList([2]string{"s1", "d1"}, [2]string{"s3", "d2"}),
			infra:      newInfrastructureForPreflight("vc.example.com", "DC0"),
			wantErr:    true,
			wantSubstr: "is not part of the recorded mapping",
		},
		{
			name:   "explicit then cleared",
			status: explicit,
			spec:   nil,
			infra:  newInfrastructureForPreflight("vc.example.com", "DC0"),
		},
		{
			name:   "destination FD added",
			status: explicit,
			spec:   explicit,
			infra:  destFDAdded,
		},
		{
			name:   "source FD removed",
			status: explicit,
			spec:   explicit,
			infra:  sourceFDsRemoved,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
				Spec:   migrationv1alpha1.VmwareCloudFoundationMigrationSpec{FailureDomainMappings: tt.spec},
				Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{FailureDomainMappings: tt.status},
			}
			reconciler := &VmwareCloudFoundationMigrationReconciler{
				ConfigClient: configfake.NewClientset(tt.infra),
			}
			err := reconciler.ensureMappingImmutable(migration)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ensureMappingImmutable succeeded, want error containing %q", tt.wantSubstr)
				}
				if !strings.Contains(err.Error(), tt.wantSubstr) {
					t.Fatalf("ensureMappingImmutable error = %q, want substring %q", err.Error(), tt.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ensureMappingImmutable: %v", err)
			}
		})
	}
}

func TestRecordFailureDomainMappings(t *testing.T) {
	ctx := context.Background()
	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("Create simulator model: %v", err)
	}
	defer model.Remove()

	model.Service.TLS = new(tls.Config)
	model.Service.RegisterEndpoints = true

	server := model.Service.NewServer()
	defer server.Close()

	ctx = model.Service.Context
	username := simulator.DefaultLogin.Username()
	password, ok := simulator.DefaultLogin.Password()
	if !ok {
		t.Fatal("simulator default login missing password")
	}

	vsphere.ClearSessions(ctx)
	defer vsphere.ClearSessions(ctx)

	inventory := discoverPreflightTestInventory(ctx, t, server.URL, username, password)

	tests := []struct {
		name                 string
		mutateMigration      func(*migrationv1alpha1.VmwareCloudFoundationMigration)
		mutateInfrastructure func(*configv1.Infrastructure)
		want                 []migrationv1alpha1.FailureDomainMapping
	}{
		{
			name: "explicit mappings recorded",
			want: mappingList([2]string{"source-fd-a", "fd-a"}, [2]string{"source-fd-b", "fd-b"}),
		},
		{
			name: "inferred one-to-one mapping recorded",
			mutateInfrastructure: func(infra *configv1.Infrastructure) {
				infra.Spec.PlatformSpec.VSphere.FailureDomains = infra.Spec.PlatformSpec.VSphere.FailureDomains[:1]
			},
			mutateMigration: func(m *migrationv1alpha1.VmwareCloudFoundationMigration) {
				m.Spec.FailureDomainMappings = nil
				m.Spec.FailureDomains = m.Spec.FailureDomains[:1]
			},
			want: mappingList([2]string{"source-fd-a", "fd-a"}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vsphere.ClearSessions(ctx)

			kubeClient := fakekube.NewClientset(
				newTargetCredentialsSecret("default", "target-vcenter-creds", server.URL.Host, username, password),
				newTargetCredentialsSecret(openshift.VSphereCredsSecretNamespace, openshift.VSphereCredsSecretName, server.URL.Host, username, password),
			)
			infra := newInfrastructureForPreflight(server.URL.Host, inventory.datacenterName)
			if tt.mutateInfrastructure != nil {
				tt.mutateInfrastructure(infra)
			}
			configClient := configfake.NewClientset(
				infra,
				newClusterVersionForPreflight("5.0.0", false),
				newFeatureGateForPreflight("5.0.0", true),
			)
			dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), preflightListKinds(),
				newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Removed"),
				newManagementStateResource("Storage", "cluster", "Removed"),
			)

			reconciler := &VmwareCloudFoundationMigrationReconciler{
				KubeClient:    kubeClient,
				ConfigClient:  configClient,
				DynamicClient: dynamicClient,
				MachineClient: machinefake.NewSimpleClientset(),
				checkNetworkingViaProbeVMsFunc: func(context.Context, *migrationv1alpha1.VmwareCloudFoundationMigration, *configv1.VSpherePlatformVCenterSpec) error {
					return nil
				},
			}

			migration := newMigrationForPreflight(server.URL.Host, inventory)
			if tt.mutateMigration != nil {
				tt.mutateMigration(migration)
			}

			if _, err := reconciler.runPreflightChecks(ctx, migration); err != nil {
				t.Fatalf("runPreflightChecks: %v", err)
			}
			if !reflect.DeepEqual(migration.Status.FailureDomainMappings, tt.want) {
				t.Fatalf("status failureDomainMappings = %+v, want %+v", migration.Status.FailureDomainMappings, tt.want)
			}
		})
	}
}

// TestWorkerMachineSetNameReservesSuffixSpace covers the Kubernetes label
// limit: generated MachineSet names are reused as selector label values, so
// the base name must leave room for copy suffixes and every generated name —
// including a suffixed copy at the reserve boundary — must fit.
func TestWorkerMachineSetNameReservesSuffixSpace(t *testing.T) {
	base := workerMachineSetName("test-infra", strings.Repeat("target-fd-", 20))
	if len(base) > maxGeneratedMachineSetNameLen-generatedMachineSetNameSuffixReserve {
		t.Fatalf("base name %q is %d bytes, want at most %d", base, len(base), maxGeneratedMachineSetNameLen-generatedMachineSetNameSuffixReserve)
	}
	for si := 0; si < 999; si++ {
		name := suffixedWorkerMachineSetName(base, si)
		if len(name) > maxGeneratedMachineSetNameLen {
			t.Fatalf("suffixed name %q (si=%d) is %d bytes, want at most %d", name, si, len(name), maxGeneratedMachineSetNameLen)
		}
	}
	// The largest in-reserve copy suffix must fit exactly.
	boundary := suffixedWorkerMachineSetName(base, 998)
	if len(boundary) != maxGeneratedMachineSetNameLen {
		t.Fatalf("boundary name %q is %d bytes, want exactly %d", boundary, len(boundary), maxGeneratedMachineSetNameLen)
	}
	// Distinct failure domain names truncated to the same base must be caught
	// by the preflight collision check instead of silently sharing names.
	r := newRolloutReconciler(t, nil)
	source := []configv1.VSpherePlatformFailureDomainSpec{newSourceFailureDomainForRollout()}
	dest := []configv1.VSpherePlatformFailureDomainSpec{
		{Name: strings.Repeat("a", 80) + "x"},
		{Name: strings.Repeat("a", 80) + "y"},
	}
	mappings := []migrationv1alpha1.FailureDomainMapping{
		{Source: "source-fd-1", Destination: dest[0].Name},
		{Source: "source-fd-1", Destination: dest[1].Name},
	}
	err := r.validateMachineSetNameCollisions(context.Background(), "test-infra", mappings, source, dest)
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("validateMachineSetNameCollisions error = %v, want containing %q", err, "overlap")
	}
}

func TestSeedFailureDomainMappings(t *testing.T) {
	infraFDs := []configv1.VSpherePlatformFailureDomainSpec{
		{Name: "source-fd-1", Server: "source.example.com"},
		{Name: "target-fd-1", Server: "target.example.com"},
	}
	newMigration := func(mappings []migrationv1alpha1.FailureDomainMapping) *migrationv1alpha1.VmwareCloudFoundationMigration {
		return &migrationv1alpha1.VmwareCloudFoundationMigration{
			Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
				FailureDomains:        []configv1.VSpherePlatformFailureDomainSpec{{Name: "target-fd-1", Server: "target.example.com"}},
				FailureDomainMappings: mappings,
			},
		}
	}

	tests := []struct {
		name     string
		mappings []migrationv1alpha1.FailureDomainMapping
		infraFDs []configv1.VSpherePlatformFailureDomainSpec
		want     []migrationv1alpha1.FailureDomainMapping
		wantErr  string
	}{
		{
			name:     "omitted mappings infer the single pair",
			infraFDs: infraFDs,
			want:     mappingList([2]string{"source-fd-1", "target-fd-1"}),
		},
		{
			name:     "explicit mappings are honoured",
			mappings: mappingList([2]string{"source-fd-1", "target-fd-1"}),
			infraFDs: infraFDs,
			want:     mappingList([2]string{"source-fd-1", "target-fd-1"}),
		},
		{
			name: "ambiguous without mappings fails closed",
			infraFDs: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "source-fd-1", Server: "source.example.com"},
				{Name: "source-fd-2", Server: "source.example.com"},
				{Name: "target-fd-1", Server: "target.example.com"},
			},
			wantErr: "provide mappings",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := seedFailureDomainMappings(newMigration(tt.mappings), tt.infraFDs)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("seedFailureDomainMappings error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("seedFailureDomainMappings: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("seedFailureDomainMappings = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestSourceVCenterServerReorderedInfrastructure is the regression test for
// resolving the source server from the recorded mapping: with the destination
// listed first, VCenters[0] is the destination and using it would make Steps
// 6/8 destroy the destination MachineSets.
func TestSourceVCenterServerReorderedInfrastructure(t *testing.T) {
	infraFDs := []configv1.VSpherePlatformFailureDomainSpec{
		{Name: "target-fd-1", Server: "target.example.com"},
		{Name: "source-fd-1", Server: "source.example.com"},
	}
	got, err := sourceVCenterServer(infraFDs, mappingList([2]string{"source-fd-1", "target-fd-1"}))
	if err != nil {
		t.Fatalf("sourceVCenterServer: %v", err)
	}
	if got != "source.example.com" {
		t.Fatalf("sourceVCenterServer = %q, want %q", got, "source.example.com")
	}

	if _, err := sourceVCenterServer(infraFDs, nil); err == nil {
		t.Fatal("sourceVCenterServer succeeded with no recorded mappings")
	}
	if _, err := sourceVCenterServer(infraFDs, mappingList([2]string{"missing-fd", "target-fd-1"})); err == nil {
		t.Fatal("sourceVCenterServer succeeded with an unknown source failure domain")
	}
}
