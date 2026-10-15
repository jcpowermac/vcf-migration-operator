package openshift

import (
	"context"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newTestInfrastructure(vcenters []configv1.VSpherePlatformVCenterSpec, failureDomains []configv1.VSpherePlatformFailureDomainSpec) *configv1.Infrastructure {
	return &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: InfrastructureName},
		Spec: configv1.InfrastructureSpec{
			PlatformSpec: configv1.PlatformSpec{
				Type: configv1.VSpherePlatformType,
				VSphere: &configv1.VSpherePlatformSpec{
					VCenters:       vcenters,
					FailureDomains: failureDomains,
				},
			},
		},
	}
}

func TestGetSourceVCenter(t *testing.T) {
	tests := []struct {
		name       string
		infra      *configv1.Infrastructure
		wantServer string
		wantErr    bool
	}{
		{
			name: "returns first vcenter",
			infra: newTestInfrastructure(
				[]configv1.VSpherePlatformVCenterSpec{
					{Server: "source.example.com", Port: 443, Datacenters: []string{"DC0"}},
					{Server: "target.example.com", Port: 443, Datacenters: []string{"DC1"}},
				},
				nil,
			),
			wantServer: "source.example.com",
		},
		{
			name:    "errors when no vcenters configured",
			infra:   newTestInfrastructure(nil, nil),
			wantErr: true,
		},
		{
			name: "errors when vsphere is nil",
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{Type: configv1.VSpherePlatformType},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := configfake.NewClientset(tt.infra)
			mgr := NewInfrastructureManager(client)
			vc, err := mgr.GetSourceVCenter(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("GetSourceVCenter error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && vc.Server != tt.wantServer {
				t.Fatalf("GetSourceVCenter server = %q, want %q", vc.Server, tt.wantServer)
			}
		})
	}
}

func TestGetSourceFailureDomain(t *testing.T) {
	tests := []struct {
		name     string
		infra    *configv1.Infrastructure
		wantName string
		wantErr  bool
	}{
		{
			name: "returns first failure domain",
			infra: newTestInfrastructure(
				nil,
				[]configv1.VSpherePlatformFailureDomainSpec{
					{Name: "fd-source", Server: "source.example.com"},
					{Name: "fd-target", Server: "target.example.com"},
				},
			),
			wantName: "fd-source",
		},
		{
			name:    "errors when no failure domains configured",
			infra:   newTestInfrastructure(nil, nil),
			wantErr: true,
		},
		{
			name: "errors when vsphere is nil",
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{Type: configv1.VSpherePlatformType},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := configfake.NewClientset(tt.infra)
			mgr := NewInfrastructureManager(client)
			fd, err := mgr.GetSourceFailureDomain(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("GetSourceFailureDomain error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && fd.Name != tt.wantName {
				t.Fatalf("GetSourceFailureDomain name = %q, want %q", fd.Name, tt.wantName)
			}
		})
	}
}

func TestAddTargetVCenter(t *testing.T) {
	tests := []struct {
		name             string
		infra            *configv1.Infrastructure
		failureDomains   []configv1.VSpherePlatformFailureDomainSpec
		wantErr          bool
		wantVCenterCount int
		wantFDCount      int
		wantVCenterNames []string
	}{
		{
			name: "adds new vcenter and failure domains",
			infra: newTestInfrastructure(
				[]configv1.VSpherePlatformVCenterSpec{
					{Server: "source.example.com", Port: 443, Datacenters: []string{"DC0"}},
				},
				[]configv1.VSpherePlatformFailureDomainSpec{
					{Name: "fd-source", Server: "source.example.com"},
				},
			),
			failureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{
					Name:   "fd-target",
					Server: "target.example.com",
					Topology: configv1.VSpherePlatformTopology{
						Datacenter: "DC1",
					},
				},
			},
			wantVCenterCount: 2,
			wantFDCount:      2,
			wantVCenterNames: []string{"source.example.com", "target.example.com"},
		},
		{
			name: "skips vcenter that already exists",
			infra: newTestInfrastructure(
				[]configv1.VSpherePlatformVCenterSpec{
					{Server: "existing.example.com", Port: 443, Datacenters: []string{"DC0"}},
				},
				nil,
			),
			failureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{
					Name:   "fd-a",
					Server: "existing.example.com",
					Topology: configv1.VSpherePlatformTopology{
						Datacenter: "DC0",
					},
				},
			},
			wantVCenterCount: 1,
			wantFDCount:      1,
			wantVCenterNames: []string{"existing.example.com"},
		},
		{
			name: "skips failure domain that already exists",
			infra: newTestInfrastructure(
				[]configv1.VSpherePlatformVCenterSpec{
					{Server: "source.example.com", Port: 443, Datacenters: []string{"DC0"}},
				},
				[]configv1.VSpherePlatformFailureDomainSpec{
					{Name: "fd-existing", Server: "source.example.com"},
				},
			),
			failureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{
					Name:   "fd-existing",
					Server: "target.example.com",
					Topology: configv1.VSpherePlatformTopology{
						Datacenter: "DC1",
					},
				},
			},
			wantVCenterCount: 2,
			wantFDCount:      1,
		},
		{
			name: "errors when vsphere platform config is nil",
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{Type: configv1.VSpherePlatformType},
				},
			},
			failureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "fd-a", Server: "target.example.com"},
			},
			wantErr: true,
		},
		{
			name: "handles multiple failure domains on different servers",
			infra: newTestInfrastructure(
				[]configv1.VSpherePlatformVCenterSpec{
					{Server: "source.example.com", Port: 443, Datacenters: []string{"DC0"}},
				},
				nil,
			),
			failureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{
					Name:   "fd-a",
					Server: "target-a.example.com",
					Topology: configv1.VSpherePlatformTopology{
						Datacenter: "DC1",
					},
				},
				{
					Name:   "fd-b",
					Server: "target-b.example.com",
					Topology: configv1.VSpherePlatformTopology{
						Datacenter: "DC2",
					},
				},
			},
			wantVCenterCount: 3,
			wantFDCount:      2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := configfake.NewClientset(tt.infra)
			mgr := NewInfrastructureManager(client)

			updated, err := mgr.AddTargetVCenter(context.Background(), tt.infra, tt.failureDomains)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AddTargetVCenter error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}

			if len(updated.Spec.PlatformSpec.VSphere.VCenters) != tt.wantVCenterCount {
				t.Fatalf("vCenter count = %d, want %d", len(updated.Spec.PlatformSpec.VSphere.VCenters), tt.wantVCenterCount)
			}
			if len(updated.Spec.PlatformSpec.VSphere.FailureDomains) != tt.wantFDCount {
				t.Fatalf("failure domain count = %d, want %d", len(updated.Spec.PlatformSpec.VSphere.FailureDomains), tt.wantFDCount)
			}

			if len(tt.wantVCenterNames) > 0 {
				gotServers := make(map[string]bool)
				for _, vc := range updated.Spec.PlatformSpec.VSphere.VCenters {
					gotServers[vc.Server] = true
				}
				for _, want := range tt.wantVCenterNames {
					if !gotServers[want] {
						t.Fatalf("expected vCenter %q not found in result", want)
					}
				}
			}

			persisted, err := client.ConfigV1().Infrastructures().Get(context.Background(), InfrastructureName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("getting persisted infrastructure: %v", err)
			}
			if len(persisted.Spec.PlatformSpec.VSphere.VCenters) != tt.wantVCenterCount {
				t.Fatalf("persisted vCenter count = %d, want %d", len(persisted.Spec.PlatformSpec.VSphere.VCenters), tt.wantVCenterCount)
			}
		})
	}
}

func TestRemoveSourceVCenter(t *testing.T) {
	tests := []struct {
		name             string
		infra            *configv1.Infrastructure
		sourceServer     string
		wantErr          bool
		wantVCenterCount int
		wantFDCount      int
	}{
		{
			name: "removes source vcenter and its failure domains",
			infra: newTestInfrastructure(
				[]configv1.VSpherePlatformVCenterSpec{
					{Server: "source.example.com", Port: 443, Datacenters: []string{"DC0"}},
					{Server: "target.example.com", Port: 443, Datacenters: []string{"DC1"}},
				},
				[]configv1.VSpherePlatformFailureDomainSpec{
					{Name: "fd-source", Server: "source.example.com"},
					{Name: "fd-target", Server: "target.example.com"},
				},
			),
			sourceServer:     "source.example.com",
			wantVCenterCount: 1,
			wantFDCount:      1,
		},
		{
			name: "errors when vsphere platform config is nil",
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{Type: configv1.VSpherePlatformType},
				},
			},
			sourceServer: "source.example.com",
			wantErr:      true,
		},
		{
			name: "errors when removing the last configured vCenter",
			infra: newTestInfrastructure(
				[]configv1.VSpherePlatformVCenterSpec{
					{Server: "source.example.com", Port: 443, Datacenters: []string{"DC0"}},
				},
				[]configv1.VSpherePlatformFailureDomainSpec{
					{Name: "fd-source", Server: "source.example.com"},
				},
			),
			sourceServer: "source.example.com",
			wantErr:      true,
		},
		{
			name: "no-ops when source server not present",
			infra: newTestInfrastructure(
				[]configv1.VSpherePlatformVCenterSpec{
					{Server: "target.example.com", Port: 443, Datacenters: []string{"DC1"}},
				},
				[]configv1.VSpherePlatformFailureDomainSpec{
					{Name: "fd-target", Server: "target.example.com"},
				},
			),
			sourceServer:     "nonexistent.example.com",
			wantVCenterCount: 1,
			wantFDCount:      1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := configfake.NewClientset(tt.infra)
			mgr := NewInfrastructureManager(client)

			updated, err := mgr.RemoveSourceVCenter(context.Background(), tt.infra, tt.sourceServer)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RemoveSourceVCenter error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}

			if len(updated.Spec.PlatformSpec.VSphere.VCenters) != tt.wantVCenterCount {
				t.Fatalf("vCenter count = %d, want %d", len(updated.Spec.PlatformSpec.VSphere.VCenters), tt.wantVCenterCount)
			}
			if len(updated.Spec.PlatformSpec.VSphere.FailureDomains) != tt.wantFDCount {
				t.Fatalf("failure domain count = %d, want %d", len(updated.Spec.PlatformSpec.VSphere.FailureDomains), tt.wantFDCount)
			}

			persisted, err := client.ConfigV1().Infrastructures().Get(context.Background(), InfrastructureName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("getting persisted infrastructure: %v", err)
			}
			if len(persisted.Spec.PlatformSpec.VSphere.VCenters) != tt.wantVCenterCount {
				t.Fatalf("persisted vCenter count = %d, want %d", len(persisted.Spec.PlatformSpec.VSphere.VCenters), tt.wantVCenterCount)
			}
		})
	}
}
