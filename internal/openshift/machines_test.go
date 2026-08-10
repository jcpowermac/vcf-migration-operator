package openshift

import (
	"context"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
)

// newTestCPMS creates a ControlPlaneMachineSet for testing with the given state
// and optional failure domains.
func newTestCPMS(state machinev1.ControlPlaneMachineSetState, failureDomains *machinev1.FailureDomains) *machinev1.ControlPlaneMachineSet {
	replicas := int32(3)
	cpms := &machinev1.ControlPlaneMachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: MachineAPINamespace,
		},
		Spec: machinev1.ControlPlaneMachineSetSpec{
			State:    state,
			Replicas: &replicas,
			Template: machinev1.ControlPlaneMachineSetTemplate{
				MachineType: machinev1.OpenShiftMachineV1Beta1MachineType,
				OpenShiftMachineV1Beta1Machine: &machinev1.OpenShiftMachineV1Beta1MachineTemplate{
					FailureDomains: failureDomains,
					ObjectMeta: machinev1.ControlPlaneMachineSetTemplateObjectMeta{
						Labels: map[string]string{
							"machine.openshift.io/cluster-api-cluster":      "test-infra",
							"machine.openshift.io/cluster-api-machine-role": "master",
							"machine.openshift.io/cluster-api-machine-type": "master",
						},
					},
					Spec: machinev1beta1.MachineSpec{
						ProviderSpec: machinev1beta1.ProviderSpec{
							Value: &runtime.RawExtension{Raw: []byte(`{"kind":"VSphereMachineProviderSpec","apiVersion":"machine.openshift.io/v1beta1"}`)},
						},
					},
				},
			},
		},
	}
	return cpms
}

func TestUpdateCPMSFailureDomain(t *testing.T) {
	tests := []struct {
		name          string
		initialState  machinev1.ControlPlaneMachineSetState
		initialFDs    *machinev1.FailureDomains
		targetFDNames []string
		wantState     machinev1.ControlPlaneMachineSetState
		wantPlatform  configv1.PlatformType
		wantFDNames   []string
	}{
		{
			name:          "updates Inactive CPMS without failure domains to Active with target failure domain",
			initialState:  machinev1.ControlPlaneMachineSetStateInactive,
			initialFDs:    nil,
			targetFDNames: []string{"target-fd"},
			wantState:     machinev1.ControlPlaneMachineSetStateActive,
			wantPlatform:  configv1.VSpherePlatformType,
			wantFDNames:   []string{"target-fd"},
		},
		{
			name:         "replaces old failure domain with target failure domain",
			initialState: machinev1.ControlPlaneMachineSetStateInactive,
			initialFDs: &machinev1.FailureDomains{
				Platform: configv1.VSpherePlatformType,
				VSphere: []machinev1.VSphereFailureDomain{
					{Name: "us-east-1"},
				},
			},
			targetFDNames: []string{"target-fd"},
			wantState:     machinev1.ControlPlaneMachineSetStateActive,
			wantPlatform:  configv1.VSpherePlatformType,
			wantFDNames:   []string{"target-fd"},
		},
		{
			name:          "sets multiple failure domains",
			initialState:  machinev1.ControlPlaneMachineSetStateInactive,
			initialFDs:    nil,
			targetFDNames: []string{"fd-1", "fd-2", "fd-3"},
			wantState:     machinev1.ControlPlaneMachineSetStateActive,
			wantPlatform:  configv1.VSpherePlatformType,
			wantFDNames:   []string{"fd-1", "fd-2", "fd-3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpms := newTestCPMS(tt.initialState, tt.initialFDs)
			machineClient := fakemachineclient.NewClientset(cpms)
			mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

			ctx := context.Background()
			if err := mgr.UpdateCPMSFailureDomain(ctx, tt.targetFDNames); err != nil {
				t.Fatalf("UpdateCPMSFailureDomain: %v", err)
			}

			// Re-fetch and verify.
			got, err := machineClient.MachineV1().ControlPlaneMachineSets(MachineAPINamespace).Get(ctx, "cluster", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("getting CPMS after update: %v", err)
			}

			if got.Spec.State != tt.wantState {
				t.Errorf("state = %q, want %q", got.Spec.State, tt.wantState)
			}

			tmpl := got.Spec.Template.OpenShiftMachineV1Beta1Machine
			if tmpl == nil {
				t.Fatal("template.machines_v1beta1_machine_openshift_io is nil")
			}
			if tmpl.FailureDomains == nil {
				t.Fatal("failureDomains is nil")
			}
			if tmpl.FailureDomains.Platform != tt.wantPlatform {
				t.Errorf("platform = %q, want %q", tmpl.FailureDomains.Platform, tt.wantPlatform)
			}
			if len(tmpl.FailureDomains.VSphere) != len(tt.wantFDNames) {
				t.Fatalf("vsphere failure domains len = %d, want %d", len(tmpl.FailureDomains.VSphere), len(tt.wantFDNames))
			}
			for i, fd := range tmpl.FailureDomains.VSphere {
				if fd.Name != tt.wantFDNames[i] {
					t.Errorf("vsphere[%d].name = %q, want %q", i, fd.Name, tt.wantFDNames[i])
				}
			}
		})
	}
}

func TestCheckControlPlaneRolloutStatus(t *testing.T) {
	tests := []struct {
		name         string
		replicas     int32
		updated      int32
		ready        int32
		wantComplete bool
	}{
		{
			name:         "complete when all counts match",
			replicas:     3,
			updated:      3,
			ready:        3,
			wantComplete: true,
		},
		{
			name:         "not complete when updated less than replicas",
			replicas:     3,
			updated:      1,
			ready:        3,
			wantComplete: false,
		},
		{
			name:         "not complete when ready less than replicas",
			replicas:     3,
			updated:      3,
			ready:        2,
			wantComplete: false,
		},
		{
			name:         "not complete when zero replicas",
			replicas:     0,
			updated:      0,
			ready:        0,
			wantComplete: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpms := newTestCPMS(machinev1.ControlPlaneMachineSetStateActive, nil)
			cpms.Status.Replicas = tt.replicas
			cpms.Status.UpdatedReplicas = tt.updated
			cpms.Status.ReadyReplicas = tt.ready
			machineClient := fakemachineclient.NewClientset(cpms)
			mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

			complete, replicas, updated, ready, err := mgr.CheckControlPlaneRolloutStatus(context.Background())
			if err != nil {
				t.Fatalf("CheckControlPlaneRolloutStatus: %v", err)
			}
			if complete != tt.wantComplete {
				t.Errorf("complete = %v, want %v", complete, tt.wantComplete)
			}
			if replicas != tt.replicas {
				t.Errorf("replicas = %d, want %d", replicas, tt.replicas)
			}
			if updated != tt.updated {
				t.Errorf("updated = %d, want %d", updated, tt.updated)
			}
			if ready != tt.ready {
				t.Errorf("ready = %d, want %d", ready, tt.ready)
			}
		})
	}
}

func TestIsCPMSGenerationObserved(t *testing.T) {
	tests := []struct {
		name               string
		generation         int64
		observedGeneration int64
		want               bool
	}{
		{
			name:               "observed when generations match",
			generation:         2,
			observedGeneration: 2,
			want:               true,
		},
		{
			name:               "not observed when generations differ",
			generation:         3,
			observedGeneration: 2,
			want:               false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpms := newTestCPMS(machinev1.ControlPlaneMachineSetStateActive, nil)
			cpms.Generation = tt.generation
			cpms.Status.ObservedGeneration = tt.observedGeneration
			machineClient := fakemachineclient.NewClientset(cpms)
			mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

			got, err := mgr.IsCPMSGenerationObserved(context.Background())
			if err != nil {
				t.Fatalf("IsCPMSGenerationObserved: %v", err)
			}
			if got != tt.want {
				t.Errorf("observed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdateMachineSetProviderSpecTagIDs(t *testing.T) {
	baseProviderSpec := `{
		"kind":"VSphereMachineProviderSpec",
		"apiVersion":"machine.openshift.io/v1beta1",
		"template":"/dc/vm/template",
		"workspace":{"server":"vcenter.example.com","datacenter":"dc1"},
		"network":{"devices":[{"networkName":"net1"}]}
	}`

	fd := &configv1.VSpherePlatformFailureDomainSpec{
		Name:   "fd-1",
		Server: "vcenter.example.com",
		Topology: configv1.VSpherePlatformTopology{
			Datacenter: "dc1",
			Datastore:  "/dc1/datastore/ds1",
			Folder:     "/dc1/vm/cluster-abc",
			Networks:   []string{"net1"},
		},
	}

	t.Run("appends ownership tag ID", func(t *testing.T) {
		ms := &machinev1beta1.MachineSet{
			Spec: machinev1beta1.MachineSetSpec{
				Template: machinev1beta1.MachineTemplateSpec{
					Spec: machinev1beta1.MachineSpec{
						ProviderSpec: machinev1beta1.ProviderSpec{
							Value: &runtime.RawExtension{Raw: []byte(baseProviderSpec)},
						},
					},
				},
			},
		}
		if err := updateMachineSetProviderSpec(ms, fd, "cluster-abc", "urn:vmomi:InventoryServiceTag:tag-1:GLOBAL"); err != nil {
			t.Fatalf("updateMachineSetProviderSpec: %v", err)
		}
		ps, err := extractVSphereProviderSpec(ms)
		if err != nil {
			t.Fatalf("extractVSphereProviderSpec: %v", err)
		}
		if len(ps.TagIDs) != 1 || ps.TagIDs[0] != "urn:vmomi:InventoryServiceTag:tag-1:GLOBAL" {
			t.Fatalf("TagIDs = %v, want [urn:vmomi:InventoryServiceTag:tag-1:GLOBAL]", ps.TagIDs)
		}
	})

	t.Run("preserves existing tag IDs and does not duplicate", func(t *testing.T) {
		withExisting := `{
			"kind":"VSphereMachineProviderSpec",
			"apiVersion":"machine.openshift.io/v1beta1",
			"template":"/dc/vm/template",
			"workspace":{"server":"vcenter.example.com","datacenter":"dc1"},
			"network":{"devices":[{"networkName":"net1"}]},
			"tagIDs":["urn:vmomi:InventoryServiceTag:existing:GLOBAL"]
		}`
		ms := &machinev1beta1.MachineSet{
			Spec: machinev1beta1.MachineSetSpec{
				Template: machinev1beta1.MachineTemplateSpec{
					Spec: machinev1beta1.MachineSpec{
						ProviderSpec: machinev1beta1.ProviderSpec{
							Value: &runtime.RawExtension{Raw: []byte(withExisting)},
						},
					},
				},
			},
		}
		ownershipID := "urn:vmomi:InventoryServiceTag:ownership:GLOBAL"
		if err := updateMachineSetProviderSpec(ms, fd, "cluster-abc", ownershipID); err != nil {
			t.Fatalf("updateMachineSetProviderSpec: %v", err)
		}
		ps, err := extractVSphereProviderSpec(ms)
		if err != nil {
			t.Fatalf("extractVSphereProviderSpec: %v", err)
		}
		if len(ps.TagIDs) != 2 {
			t.Fatalf("TagIDs len = %d, want 2: %v", len(ps.TagIDs), ps.TagIDs)
		}
		if ps.TagIDs[0] != "urn:vmomi:InventoryServiceTag:existing:GLOBAL" {
			t.Fatalf("TagIDs[0] = %q, want existing", ps.TagIDs[0])
		}
		if ps.TagIDs[1] != ownershipID {
			t.Fatalf("TagIDs[1] = %q, want %q", ps.TagIDs[1], ownershipID)
		}
		if err := updateMachineSetProviderSpec(ms, fd, "cluster-abc", ownershipID); err != nil {
			t.Fatalf("updateMachineSetProviderSpec second: %v", err)
		}
		ps, err = extractVSphereProviderSpec(ms)
		if err != nil {
			t.Fatalf("extractVSphereProviderSpec second: %v", err)
		}
		if len(ps.TagIDs) != 2 {
			t.Fatalf("TagIDs after second call = %v, want length 2", ps.TagIDs)
		}
	})

	t.Run("skips empty ownership tag ID", func(t *testing.T) {
		ms := &machinev1beta1.MachineSet{
			Spec: machinev1beta1.MachineSetSpec{
				Template: machinev1beta1.MachineTemplateSpec{
					Spec: machinev1beta1.MachineSpec{
						ProviderSpec: machinev1beta1.ProviderSpec{
							Value: &runtime.RawExtension{Raw: []byte(baseProviderSpec)},
						},
					},
				},
			},
		}
		if err := updateMachineSetProviderSpec(ms, fd, "cluster-abc", ""); err != nil {
			t.Fatalf("updateMachineSetProviderSpec: %v", err)
		}
		ps, err := extractVSphereProviderSpec(ms)
		if err != nil {
			t.Fatalf("extractVSphereProviderSpec: %v", err)
		}
		if len(ps.TagIDs) != 0 {
			t.Fatalf("TagIDs = %v, want empty", ps.TagIDs)
		}
	})
}
