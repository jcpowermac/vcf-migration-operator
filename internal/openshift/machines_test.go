package openshift

import (
	"context"
	"fmt"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
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

func newTestMachineSet(name string, replicas int32) *machinev1beta1.MachineSet {
	return &machinev1beta1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: MachineAPINamespace,
		},
		Spec: machinev1beta1.MachineSetSpec{
			Replicas: &replicas,
		},
	}
}

func TestDeleteMachineSet(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes existing machineset", func(t *testing.T) {
		ms := newTestMachineSet("source-worker", 0)
		machineClient := fakemachineclient.NewClientset(ms)
		mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

		if err := mgr.DeleteMachineSet(ctx, ms.Name); err != nil {
			t.Fatalf("DeleteMachineSet: %v", err)
		}

		_, err := machineClient.MachineV1beta1().MachineSets(MachineAPINamespace).Get(ctx, ms.Name, metav1.GetOptions{})
		if err == nil {
			t.Fatal("expected machineset to be deleted")
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected NotFound after delete, got: %v", err)
		}
	})

	t.Run("not found is success", func(t *testing.T) {
		machineClient := fakemachineclient.NewClientset()
		mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

		if err := mgr.DeleteMachineSet(ctx, "missing-ms"); err != nil {
			t.Fatalf("DeleteMachineSet: %v", err)
		}
	})

	t.Run("propagates non-notfound errors", func(t *testing.T) {
		machineClient := fakemachineclient.NewClientset()
		machineClient.PrependReactor("delete", "machinesets", func(action clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("apiserver unavailable")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

		err := mgr.DeleteMachineSet(ctx, "source-worker")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "deleting machineset") {
			t.Fatalf("error %q should wrap deleting machineset context", err)
		}
		if !strings.Contains(err.Error(), "apiserver unavailable") {
			t.Fatalf("error %q should include underlying cause", err)
		}
	})
}

func newTestMachineSetForVCenter(name, vcenter string, replicas int32) *machinev1beta1.MachineSet {
	ms := newTestMachineSet(name, replicas)
	raw := []byte(fmt.Sprintf(
		`{"kind":"VSphereMachineProviderSpec","apiVersion":"machine.openshift.io/v1beta1","workspace":{"server":%q}}`,
		vcenter,
	))
	ms.Spec.Template.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: raw}
	return ms
}

func TestDeleteMachineSetsByVCenter(t *testing.T) {
	ctx := context.Background()
	sourceVC := "source.example.com"
	targetVC := "target.example.com"

	t.Run("deletes zero-replica source machinesets only", func(t *testing.T) {
		source := newTestMachineSetForVCenter("source-worker", sourceVC, 0)
		target := newTestMachineSetForVCenter("target-worker", targetVC, 3)
		machineClient := fakemachineclient.NewClientset(source, target)
		mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

		deleted, err := mgr.DeleteMachineSetsByVCenter(ctx, sourceVC)
		if err != nil {
			t.Fatalf("DeleteMachineSetsByVCenter: %v", err)
		}
		if len(deleted) != 1 || deleted[0] != "source-worker" {
			t.Fatalf("deleted = %v, want [source-worker]", deleted)
		}

		if _, err := machineClient.MachineV1beta1().MachineSets(MachineAPINamespace).Get(ctx, "source-worker", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("expected source machineset deleted, got: %v", err)
		}
		if _, err := machineClient.MachineV1beta1().MachineSets(MachineAPINamespace).Get(ctx, "target-worker", metav1.GetOptions{}); err != nil {
			t.Fatalf("target machineset should remain: %v", err)
		}
	})

	t.Run("refuses when replicas greater than zero", func(t *testing.T) {
		source := newTestMachineSetForVCenter("source-worker", sourceVC, 2)
		machineClient := fakemachineclient.NewClientset(source)
		mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

		_, err := mgr.DeleteMachineSetsByVCenter(ctx, sourceVC)
		if err == nil {
			t.Fatal("expected error when replicas > 0")
		}
		if !strings.Contains(err.Error(), "source-worker") {
			t.Fatalf("error %q should name the machineset", err)
		}

		if _, err := machineClient.MachineV1beta1().MachineSets(MachineAPINamespace).Get(ctx, "source-worker", metav1.GetOptions{}); err != nil {
			t.Fatalf("machineset should not be deleted on refusal: %v", err)
		}
	})

	t.Run("refuses when replicas is nil", func(t *testing.T) {
		source := newTestMachineSetForVCenter("source-worker", sourceVC, 0)
		source.Spec.Replicas = nil
		machineClient := fakemachineclient.NewClientset(source)
		mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

		_, err := mgr.DeleteMachineSetsByVCenter(ctx, sourceVC)
		if err == nil {
			t.Fatal("expected error when replicas is nil")
		}
		if !strings.Contains(err.Error(), "source-worker") {
			t.Fatalf("error %q should name the machineset", err)
		}

		if _, err := machineClient.MachineV1beta1().MachineSets(MachineAPINamespace).Get(ctx, "source-worker", metav1.GetOptions{}); err != nil {
			t.Fatalf("machineset should not be deleted on refusal: %v", err)
		}
	})

	t.Run("rejects empty vcenter server without deleting", func(t *testing.T) {
		source := newTestMachineSetForVCenter("source-worker", sourceVC, 0)
		machineClient := fakemachineclient.NewClientset(source)
		mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

		_, err := mgr.DeleteMachineSetsByVCenter(ctx, "")
		if err == nil {
			t.Fatal("expected error for empty vcenter server")
		}
		if !strings.Contains(err.Error(), "empty") {
			t.Fatalf("error %q should mention empty server", err)
		}

		if _, err := machineClient.MachineV1beta1().MachineSets(MachineAPINamespace).Get(ctx, "source-worker", metav1.GetOptions{}); err != nil {
			t.Fatalf("machineset should not be deleted: %v", err)
		}
	})

	t.Run("idempotent when none remain", func(t *testing.T) {
		machineClient := fakemachineclient.NewClientset()
		mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

		deleted, err := mgr.DeleteMachineSetsByVCenter(ctx, sourceVC)
		if err != nil {
			t.Fatalf("DeleteMachineSetsByVCenter: %v", err)
		}
		if len(deleted) != 0 {
			t.Fatalf("deleted = %v, want empty", deleted)
		}
	})
}
