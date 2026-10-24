package openshift

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
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
		strategy      machinev1.ControlPlaneMachineSetStrategyType
		wantState     machinev1.ControlPlaneMachineSetState
		wantPlatform  configv1.PlatformType
		wantFDNames   []string
		wantErrSub    string
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
		{
			name:          "preserves explicit RollingUpdate strategy",
			initialState:  machinev1.ControlPlaneMachineSetStateInactive,
			initialFDs:    nil,
			targetFDNames: []string{"target-fd"},
			strategy:      machinev1.RollingUpdate,
			wantState:     machinev1.ControlPlaneMachineSetStateActive,
			wantPlatform:  configv1.VSpherePlatformType,
			wantFDNames:   []string{"target-fd"},
		},
		{
			name:          "rejects OnDelete strategy",
			initialState:  machinev1.ControlPlaneMachineSetStateInactive,
			initialFDs:    nil,
			targetFDNames: []string{"target-fd"},
			strategy:      machinev1.OnDelete,
			wantErrSub:    "is not RollingUpdate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpms := newTestCPMS(tt.initialState, tt.initialFDs)
			cpms.Spec.Strategy.Type = tt.strategy
			machineClient := fakemachineclient.NewClientset(cpms)
			mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

			ctx := context.Background()
			err := mgr.UpdateCPMSFailureDomain(ctx, tt.targetFDNames)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("UpdateCPMSFailureDomain error = %v, want containing %q", err, tt.wantErrSub)
				}
				// The spec must be untouched on rejection.
				got, gerr := machineClient.MachineV1().ControlPlaneMachineSets(MachineAPINamespace).Get(ctx, "cluster", metav1.GetOptions{})
				if gerr != nil {
					t.Fatalf("getting CPMS after rejected update: %v", gerr)
				}
				if got.Spec.State != tt.initialState || got.Spec.Template.OpenShiftMachineV1Beta1Machine.FailureDomains != nil {
					t.Errorf("spec mutated despite rejected update: state=%q failureDomains=%v", got.Spec.State, got.Spec.Template.OpenShiftMachineV1Beta1Machine.FailureDomains)
				}
				return
			}
			if err != nil {
				t.Fatalf("UpdateCPMSFailureDomain: %v", err)
			}

			// Re-fetch and verify.
			got, err := machineClient.MachineV1().ControlPlaneMachineSets(MachineAPINamespace).Get(ctx, "cluster", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("getting CPMS after update: %v", err)
			}

			if got.Spec.Strategy.Type != tt.strategy {
				t.Errorf("strategy = %q, want preserved %q", got.Spec.Strategy.Type, tt.strategy)
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
		name            string
		replicas        int32
		updated         int32
		ready           int32
		conditions      []metav1.Condition
		wantComplete    bool
		wantHoldReason  string
		wantHoldMessage string
		wantErrSub      string
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
		{
			name:     "error when CPMS is degraded",
			replicas: 3,
			updated:  1,
			ready:    3,
			conditions: []metav1.Condition{{
				Type:    "Degraded",
				Status:  metav1.ConditionTrue,
				Reason:  "FailedReplacement",
				Message: "machine cluster-cpms-1 has error",
			}},
			wantComplete: false,
			wantErrSub:   "FailedReplacement",
		},
		{
			name:     "wait when CPMS reports unmanaged control plane nodes",
			replicas: 4,
			updated:  2,
			ready:    3,
			conditions: []metav1.Condition{{
				Type:    "Degraded",
				Status:  metav1.ConditionTrue,
				Reason:  "UnmanagedNodes",
				Message: "Found 1 unmanaged node(s)",
			}},
			wantComplete:    false,
			wantHoldReason:  "UnmanagedNodes",
			wantHoldMessage: "Found 1 unmanaged node(s)",
		},
		{
			// Regression: UnmanagedNodes must hold the gate even when the counts
			// look complete, otherwise scale-down deletes the machines a control
			// plane node still needs. The hold reason and message are what tell the
			// caller why an apparently complete rollout is still held.
			name:     "not complete when unmanaged nodes are reported with equal counts",
			replicas: 3,
			updated:  3,
			ready:    3,
			conditions: []metav1.Condition{{
				Type:    "Degraded",
				Status:  metav1.ConditionTrue,
				Reason:  "UnmanagedNodes",
				Message: "Found 1 unmanaged node(s)",
			}},
			wantComplete:    false,
			wantHoldReason:  "UnmanagedNodes",
			wantHoldMessage: "Found 1 unmanaged node(s)",
		},
		{
			name:     "error when CPMS reports continuous errors",
			replicas: 3,
			updated:  1,
			ready:    3,
			conditions: []metav1.Condition{{
				Type:    "Error",
				Status:  metav1.ConditionTrue,
				Reason:  "ContinuousErrors",
				Message: "repeatedly failed to create machine",
			}},
			wantComplete: false,
			wantErrSub:   "ContinuousErrors",
		},
		{
			name:     "no error when conditions are healthy",
			replicas: 3,
			updated:  2,
			ready:    3,
			conditions: []metav1.Condition{
				{Type: "Available", Status: metav1.ConditionFalse, Reason: "UnavailableReplicas"},
				{Type: "Degraded", Status: metav1.ConditionFalse, Reason: "AsExpected"},
				{Type: "Error", Status: metav1.ConditionFalse, Reason: "AsExpected"},
				{Type: "Progressing", Status: metav1.ConditionTrue, Reason: "NeedsUpdateReplicas"},
			},
			wantComplete: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpms := newTestCPMS(machinev1.ControlPlaneMachineSetStateActive, nil)
			cpms.Status.Replicas = tt.replicas
			cpms.Status.UpdatedReplicas = tt.updated
			cpms.Status.ReadyReplicas = tt.ready
			cpms.Status.Conditions = tt.conditions
			machineClient := fakemachineclient.NewClientset(cpms)
			mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

			got, err := mgr.CheckControlPlaneRolloutStatus(context.Background())
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("CheckControlPlaneRolloutStatus error = %v, want containing %q", err, tt.wantErrSub)
				}
				if got.Complete {
					t.Errorf("complete = true, want false when rollout is stuck")
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckControlPlaneRolloutStatus: %v", err)
			}
			if got.Complete != tt.wantComplete {
				t.Errorf("complete = %v, want %v", got.Complete, tt.wantComplete)
			}
			if got.Replicas != tt.replicas {
				t.Errorf("replicas = %d, want %d", got.Replicas, tt.replicas)
			}
			if got.UpdatedReplicas != tt.updated {
				t.Errorf("updated = %d, want %d", got.UpdatedReplicas, tt.updated)
			}
			if got.ReadyReplicas != tt.ready {
				t.Errorf("ready = %d, want %d", got.ReadyReplicas, tt.ready)
			}
			if got.HoldReason != tt.wantHoldReason {
				t.Errorf("holdReason = %q, want %q", got.HoldReason, tt.wantHoldReason)
			}
			if got.HoldMessage != tt.wantHoldMessage {
				t.Errorf("holdMessage = %q, want %q", got.HoldMessage, tt.wantHoldMessage)
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

			got, gen, obsGen, err := mgr.IsCPMSGenerationObserved(context.Background())
			if err != nil {
				t.Fatalf("IsCPMSGenerationObserved: %v", err)
			}
			if got != tt.want {
				t.Errorf("observed = %v, want %v", got, tt.want)
			}
			if gen != tt.generation {
				t.Errorf("generation = %d, want %d", gen, tt.generation)
			}
			if obsGen != tt.observedGeneration {
				t.Errorf("observedGeneration = %d, want %d", obsGen, tt.observedGeneration)
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

// TestCreateWorkerMachineSetSourceAnnotation covers the annotation stamped on a
// generated destination MachineSet: it records which source MachineSet the copy
// was created from, so a later reconcile can tell that the generated name still
// stands for the same source.
func TestCreateWorkerMachineSetSourceAnnotation(t *testing.T) {
	ctx := context.Background()
	source := newTestMachineSetForFailureDomain("source-worker-a", "source.example.com", "dc-source", "rp-source", 2)
	source.Annotations = map[string]string{LegacyAggregateAnnotation: "true", "user-note": "keep"}
	fd := &configv1.VSpherePlatformFailureDomainSpec{
		Name:   "target-fd-1",
		Server: "target.example.com",
		Topology: configv1.VSpherePlatformTopology{
			Template:     "/dc-target/vm/target-template",
			Datacenter:   "dc-target",
			Datastore:    "ds-target",
			ResourcePool: "rp-target",
		},
	}
	mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(), nil)

	created, err := mgr.CreateWorkerMachineSet(ctx, "test-infra-worker-target-fd-1", 2, fd, source, "test-infra")
	if err != nil {
		t.Fatalf("CreateWorkerMachineSet: %v", err)
	}
	if got := created.Annotations[SourceMachineSetAnnotation]; got != source.Name {
		t.Fatalf("created machineset annotation %q = %q, want %q", SourceMachineSetAnnotation, got, source.Name)
	}
	if _, ok := created.Annotations[LegacyAggregateAnnotation]; ok {
		t.Fatal("new per-source copy inherited legacy aggregate provenance")
	}
	if created.Annotations["user-note"] != "keep" || source.Annotations[LegacyAggregateAnnotation] != "true" {
		t.Fatal("unrelated annotation or source mutated")
	}

	stored, err := mgr.GetMachineSet(ctx, created.Name)
	if err != nil {
		t.Fatalf("GetMachineSet(%q): %v", created.Name, err)
	}
	if got := stored.Annotations[SourceMachineSetAnnotation]; got != source.Name {
		t.Fatalf("stored machineset annotation %q = %q, want %q", SourceMachineSetAnnotation, got, source.Name)
	}
	if _, ok := source.Annotations[SourceMachineSetAnnotation]; ok {
		t.Fatal("template MachineSet was mutated, want a deep copy to be annotated")
	}
}

// TestAnnotateMachineSetLegacyAggregate covers the durable provenance marker: the
// released operator's aggregate is detected by replica arithmetic once, then
// annotated so classification survives the source scale-down that follows.
func TestAnnotateMachineSetLegacyAggregate(t *testing.T) {
	ctx := context.Background()
	aggregate := newTestMachineSetForFailureDomain("test-infra-worker-target-fd-1", "target.example.com", "dc-target", "rp-target", 5)
	mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(aggregate), nil)

	if err := mgr.AnnotateMachineSetLegacyAggregate(ctx, aggregate.Name); err != nil {
		t.Fatalf("AnnotateMachineSetLegacyAggregate: %v", err)
	}
	stored, err := mgr.GetMachineSet(ctx, aggregate.Name)
	if err != nil {
		t.Fatalf("GetMachineSet(%q): %v", aggregate.Name, err)
	}
	if got := stored.Annotations[LegacyAggregateAnnotation]; got != LegacyAggregateAnnotationValue {
		t.Fatalf("machineset annotation %q = %q, want %q", LegacyAggregateAnnotation, got, LegacyAggregateAnnotationValue)
	}
	if stored.Spec.Replicas == nil || *stored.Spec.Replicas != 5 {
		t.Fatalf("machineset replicas = %v, want 5 (annotating must not change capacity)", stored.Spec.Replicas)
	}

	// Idempotent, and the caller's object is left alone.
	if err := mgr.AnnotateMachineSetLegacyAggregate(ctx, aggregate.Name); err != nil {
		t.Fatalf("second AnnotateMachineSetLegacyAggregate: %v", err)
	}
	if _, ok := aggregate.Annotations[LegacyAggregateAnnotation]; ok {
		t.Fatal("input MachineSet was mutated, want the annotation written to the stored object only")
	}
}

func TestGetMachineSetsByVCenter_emptyServerCausesNoMachineSetUpdates(t *testing.T) {
	ctx := context.Background()
	source := newTestMachineSetForVCenter("source-worker", "source.example.com", 3)
	target := newTestMachineSetForVCenter("target-worker", "target.example.com", 3)
	machineClient := fakemachineclient.NewClientset(source, target)
	mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

	_, err := mgr.GetMachineSetsByVCenter(ctx, "")
	if err == nil {
		t.Fatal("expected error for empty vcenter server")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error %q should mention empty server", err)
	}

	// Simulate Step 6: only scale when listing succeeds. Empty server must not
	// lead to any MachineSet updates.
	for _, name := range []string{"source-worker", "target-worker"} {
		got, getErr := machineClient.MachineV1beta1().MachineSets(MachineAPINamespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			t.Fatalf("getting machineset %q: %v", name, getErr)
		}
		if got.Spec.Replicas == nil || *got.Spec.Replicas != 3 {
			t.Fatalf("machineset %q replicas = %v, want 3 (unchanged)", name, got.Spec.Replicas)
		}
	}
}

func TestListMachineSets(t *testing.T) {
	ctx := context.Background()
	msA := newTestMachineSetForVCenter("source-worker", "source.example.com", 3)
	msB := newTestMachineSetForVCenter("target-worker", "target.example.com", 1)
	mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(msA, msB), nil)

	got, err := mgr.ListMachineSets(ctx)
	if err != nil {
		t.Fatalf("ListMachineSets: %v", err)
	}
	gotNames := make([]string, 0, len(got))
	for _, ms := range got {
		gotNames = append(gotNames, ms.Name)
	}
	sort.Strings(gotNames)
	if !slices.Equal(gotNames, []string{"source-worker", "target-worker"}) {
		t.Fatalf("ListMachineSets = %v, want [source-worker target-worker]", gotNames)
	}
}

func newTestMachineSetForFailureDomain(name, server, datacenter, resourcePool string, replicas int32) *machinev1beta1.MachineSet {
	ms := newTestMachineSet(name, replicas)
	raw := []byte(fmt.Sprintf(
		`{"kind":"VSphereMachineProviderSpec","apiVersion":"machine.openshift.io/v1beta1","workspace":{"server":%q,"datacenter":%q,"resourcePool":%q}}`,
		server, datacenter, resourcePool,
	))
	ms.Spec.Template.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: raw}
	return ms
}

func TestEffectiveResourcePool(t *testing.T) {
	tests := []struct {
		name           string
		resourcePool   string
		computeCluster string
		want           string
	}{
		{
			name:           "explicit pool wins",
			resourcePool:   "/dc/host/cluster/Resources/rp",
			computeCluster: "/dc/host/cluster",
			want:           "/dc/host/cluster/Resources/rp",
		},
		{
			name:           "empty pool defaults to the cluster resources pool",
			computeCluster: "/dc/host/cluster",
			want:           "/dc/host/cluster/Resources",
		},
		{
			name:           "trailing slash on the compute cluster is normalized",
			computeCluster: "/dc/host/cluster/",
			want:           "/dc/host/cluster/Resources",
		},
		{
			name: "no pool and no compute cluster is undeterminable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fd := configv1.VSpherePlatformFailureDomainSpec{}
			fd.Topology.ResourcePool = tt.resourcePool
			fd.Topology.ComputeCluster = tt.computeCluster
			if got := EffectiveResourcePool(fd); got != tt.want {
				t.Errorf("EffectiveResourcePool() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetMachineSetsByFailureDomain(t *testing.T) {
	ctx := context.Background()
	msA := newTestMachineSetForFailureDomain("source-worker-a", "source.example.com", "dc1", "rp1", 3)
	msB := newTestMachineSetForFailureDomain("source-worker-b", "source.example.com", "dc1", "rp2", 2)
	msOtherDC := newTestMachineSetForFailureDomain("source-worker-c", "source.example.com", "dc2", "rp1", 1)
	msOtherServer := newTestMachineSetForFailureDomain("target-worker", "target.example.com", "dc1", "rp1", 1)
	// Two compute clusters in dc1, each running the cluster's own default pool,
	// plus a custom pool inside cluster-a: an empty topology.resourcePool must
	// resolve to "<computeCluster>/Resources", not match every pool in dc1.
	msClusterADefaultPool := newTestMachineSetForFailureDomain("source-worker-da", "source.example.com", "dc1", "/dc1/host/cluster-a/Resources", 1)
	msClusterACustomPool := newTestMachineSetForFailureDomain("source-worker-dap", "source.example.com", "dc1", "/dc1/host/cluster-a/Resources/pool-1", 1)
	msClusterBDefaultPool := newTestMachineSetForFailureDomain("source-worker-db", "source.example.com", "dc1", "/dc1/host/cluster-b/Resources", 1)

	tests := []struct {
		name      string
		fd        *configv1.VSpherePlatformFailureDomainSpec
		wantNames []string
	}{
		{
			name: "matches datacenter and resource pool",
			fd: &configv1.VSpherePlatformFailureDomainSpec{
				Name:     "source-fd-1",
				Server:   "source.example.com",
				Topology: configv1.VSpherePlatformTopology{Datacenter: "dc1", ResourcePool: "rp1"},
			},
			wantNames: []string{"source-worker-a"},
		},
		{
			name: "explicit resource pool still matches only that pool",
			fd: &configv1.VSpherePlatformFailureDomainSpec{
				Name:     "source-fd-1",
				Server:   "source.example.com",
				Topology: configv1.VSpherePlatformTopology{Datacenter: "dc1", ResourcePool: "/dc1/host/cluster-a/Resources/pool-1", ComputeCluster: "/dc1/host/cluster-a"},
			},
			wantNames: []string{"source-worker-dap"},
		},
		{
			// Control: with no compute cluster the failure domain resolves to no
			// pool at all, so the datacenter-wide match is unchanged.
			name: "empty resource pool without a compute cluster matches every pool in the datacenter",
			fd: &configv1.VSpherePlatformFailureDomainSpec{
				Name:     "source-fd-1",
				Server:   "source.example.com",
				Topology: configv1.VSpherePlatformTopology{Datacenter: "dc1"},
			},
			wantNames: []string{"source-worker-a", "source-worker-b", "source-worker-da", "source-worker-dap", "source-worker-db"},
		},
		{
			// An empty topology.resourcePool means the compute cluster's default
			// pool, so a MachineSet in another cluster's default pool belongs to
			// another failure domain.
			name: "empty resource pool with a compute cluster matches only that cluster's default pool",
			fd: &configv1.VSpherePlatformFailureDomainSpec{
				Name:     "source-fd-1",
				Server:   "source.example.com",
				Topology: configv1.VSpherePlatformTopology{Datacenter: "dc1", ComputeCluster: "/dc1/host/cluster-a"},
			},
			wantNames: []string{"source-worker-da"},
		},
		{
			name: "no match returns empty result without error",
			fd: &configv1.VSpherePlatformFailureDomainSpec{
				Name:     "source-fd-1",
				Server:   "source.example.com",
				Topology: configv1.VSpherePlatformTopology{Datacenter: "dc3", ResourcePool: "rp1"},
			},
			wantNames: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machineClient := fakemachineclient.NewClientset(msA, msB, msOtherDC, msOtherServer, msClusterADefaultPool, msClusterACustomPool, msClusterBDefaultPool)
			mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

			got, err := mgr.GetMachineSetsByFailureDomain(ctx, tt.fd.Server, tt.fd)
			if err != nil {
				t.Fatalf("GetMachineSetsByFailureDomain: %v", err)
			}
			var gotNames []string
			for _, ms := range got {
				gotNames = append(gotNames, ms.Name)
			}
			sort.Strings(gotNames)
			sort.Strings(tt.wantNames)
			if !slices.Equal(gotNames, tt.wantNames) {
				t.Fatalf("GetMachineSetsByFailureDomain(%q) = %v, want %v", tt.fd.Name, gotNames, tt.wantNames)
			}
		})
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

func TestIsCPMSUpdatedForFailureDomains(t *testing.T) {
	tests := []struct {
		name      string
		cpms      *machinev1.ControlPlaneMachineSet
		wantNames []string
		want      bool
		wantErr   string
	}{
		{
			name: "active with matching failure domains",
			cpms: newTestCPMS(machinev1.ControlPlaneMachineSetStateActive, &machinev1.FailureDomains{
				Platform: configv1.VSpherePlatformType,
				VSphere: []machinev1.VSphereFailureDomain{
					{Name: "a"},
					{Name: "b"},
				},
			}),
			wantNames: []string{"a", "b"},
			want:      true,
		},
		{
			name: "active with matching failure domains different order",
			cpms: newTestCPMS(machinev1.ControlPlaneMachineSetStateActive, &machinev1.FailureDomains{
				Platform: configv1.VSpherePlatformType,
				VSphere: []machinev1.VSphereFailureDomain{
					{Name: "a"},
					{Name: "b"},
				},
			}),
			wantNames: []string{"b", "a"},
			want:      true,
		},
		{
			name: "active with mismatched failure domains",
			cpms: newTestCPMS(machinev1.ControlPlaneMachineSetStateActive, &machinev1.FailureDomains{
				Platform: configv1.VSpherePlatformType,
				VSphere: []machinev1.VSphereFailureDomain{
					{Name: "a"},
				},
			}),
			wantNames: []string{"b"},
			want:      false,
		},
		{
			name: "inactive with matching failure domains",
			cpms: newTestCPMS(machinev1.ControlPlaneMachineSetStateInactive, &machinev1.FailureDomains{
				Platform: configv1.VSpherePlatformType,
				VSphere: []machinev1.VSphereFailureDomain{
					{Name: "a"},
				},
			}),
			wantNames: []string{"a"},
			want:      false,
		},
		{
			name:      "active with nil failure domains",
			cpms:      newTestCPMS(machinev1.ControlPlaneMachineSetStateActive, nil),
			wantNames: []string{"a"},
			want:      false,
		},
		{
			name:      "no CPMS in clientset",
			cpms:      nil,
			wantNames: []string{"a"},
			wantErr:   "getting ControlPlaneMachineSet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var machineClient *fakemachineclient.Clientset
			if tt.cpms != nil {
				machineClient = fakemachineclient.NewClientset(tt.cpms)
			} else {
				machineClient = fakemachineclient.NewClientset()
			}
			mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

			got, err := mgr.IsCPMSUpdatedForFailureDomains(context.Background(), tt.wantNames)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("IsCPMSUpdatedForFailureDomains: %v", err)
			}
			if got != tt.want {
				t.Errorf("updated = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListControlPlaneMachines(t *testing.T) {
	machines := []*machinev1beta1.Machine{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cp-1",
				Namespace: MachineAPINamespace,
				Labels:    map[string]string{"machine.openshift.io/cluster-api-machine-role": "master"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cp-2",
				Namespace: MachineAPINamespace,
				Labels:    map[string]string{"machine.openshift.io/cluster-api-machine-role": "control-plane"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "worker-1",
				Namespace: MachineAPINamespace,
				Labels:    map[string]string{"machine.openshift.io/cluster-api-machine-role": "worker"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "unlabeled",
				Namespace: MachineAPINamespace,
			},
		},
	}

	objs := make([]runtime.Object, len(machines))
	for i, m := range machines {
		objs[i] = m
	}
	machineClient := fakemachineclient.NewClientset(objs...)
	mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

	got, err := mgr.ListControlPlaneMachines(context.Background())
	if err != nil {
		t.Fatalf("ListControlPlaneMachines: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	names := make(map[string]struct{}, len(got))
	for _, m := range got {
		names[m.Name] = struct{}{}
	}
	for _, want := range []string{"cp-1", "cp-2"} {
		if _, ok := names[want]; !ok {
			t.Errorf("missing machine %q in result", want)
		}
	}
}

func TestListMachinesForMachineSet(t *testing.T) {
	tests := []struct {
		name        string
		machineSets []*machinev1beta1.MachineSet
		machines    []*machinev1beta1.Machine
		lookupName  string
		wantNames   []string
	}{
		{
			name:       "returns machines selected by the machineset selector",
			lookupName: "workers-a",
			machineSets: []*machinev1beta1.MachineSet{
				testSelectorMachineSet("workers-a"),
			},
			machines: []*machinev1beta1.Machine{
				testMachineWithMSLabel("worker-a-0", "workers-a"),
				testMachineWithMSLabel("worker-b-0", "workers-b"),
			},
			wantNames: []string{"worker-a-0"},
		},
		{
			name:       "falls back to machineset name when selector label is absent",
			lookupName: "legacy-workers",
			machineSets: []*machinev1beta1.MachineSet{
				newTestMachineSet("legacy-workers", 1),
			},
			machines: []*machinev1beta1.Machine{
				testMachineWithMSLabel("legacy-worker-0", "legacy-workers"),
			},
			wantNames: []string{"legacy-worker-0"},
		},
		{
			name:       "returns an empty list when no machines match",
			lookupName: "workers-empty",
			machineSets: []*machinev1beta1.MachineSet{
				newTestMachineSet("workers-empty", 0),
			},
			wantNames: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]runtime.Object, 0, len(tt.machineSets)+len(tt.machines))
			for _, ms := range tt.machineSets {
				objects = append(objects, ms)
			}
			for _, m := range tt.machines {
				objects = append(objects, m)
			}
			machineClient := fakemachineclient.NewClientset(objects...)
			mgr := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

			got, err := mgr.ListMachinesForMachineSet(context.Background(), tt.lookupName)
			if err != nil {
				t.Fatalf("ListMachinesForMachineSet: %v", err)
			}

			names := make([]string, 0, len(got))
			for _, m := range got {
				names = append(names, m.Name)
			}
			if len(names) != len(tt.wantNames) {
				t.Fatalf("machine names = %v, want %v", names, tt.wantNames)
			}
			for i := range names {
				if names[i] != tt.wantNames[i] {
					t.Fatalf("machine names = %v, want %v", names, tt.wantNames)
				}
			}
		})
	}
}

// testSelectorMachineSet creates a MachineSet whose selector carries the
// cluster-api-machineset label, matching how real worker MachineSets are labeled.
func testSelectorMachineSet(name string) *machinev1beta1.MachineSet {
	replicas := int32(1)
	return &machinev1beta1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: MachineAPINamespace},
		Spec: machinev1beta1.MachineSetSpec{
			Replicas: &replicas,
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"machine.openshift.io/cluster-api-machineset": name,
				},
			},
		},
	}
}

// testMachineWithMSLabel creates a Machine carrying the cluster-api-machineset
// label with the given value.
func testMachineWithMSLabel(name, labelValue string) *machinev1beta1.Machine {
	return &machinev1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: MachineAPINamespace,
			Labels: map[string]string{
				"machine.openshift.io/cluster-api-machineset": labelValue,
			},
		},
	}
}

func TestCheckMachinesReady(t *testing.T) {
	running := "Running"
	pending := "Pending"
	newMachine := func(name string, phase *string, nodeName string) *machinev1beta1.Machine {
		machine := testMachineWithMSLabel(name, "workers-a")
		machine.Status.Phase = phase
		if nodeName != "" {
			machine.Status.NodeRef = &corev1.ObjectReference{Name: nodeName}
		}
		return machine
	}

	failedPhase := machinev1beta1.PhaseFailed
	withFailure := func(machine *machinev1beta1.Machine, reason machinev1beta1.MachineStatusError, message string) *machinev1beta1.Machine {
		machine.Status.ErrorReason = &reason
		machine.Status.ErrorMessage = &message
		return machine
	}

	tests := []struct {
		name              string
		machines          []*machinev1beta1.Machine
		missingMachineSet bool
		failList          bool
		wantComplete      bool
		wantReady         int32
		wantTotal         int32
		wantError         bool
		wantErrSub        string
	}{
		{
			name: "all running machines with node references are ready",
			machines: []*machinev1beta1.Machine{
				newMachine("worker-a", &running, "node-a"),
				newMachine("worker-b", &running, "node-b"),
			},
			wantComplete: true,
			wantReady:    2,
			wantTotal:    2,
		},
		{
			name: "pending, missing phase, and missing node reference are not ready",
			machines: []*machinev1beta1.Machine{
				newMachine("worker-a", &running, "node-a"),
				newMachine("worker-b", &pending, "node-b"),
				newMachine("worker-c", nil, "node-c"),
				newMachine("worker-d", &running, ""),
			},
			wantReady: 1,
			wantTotal: 4,
		},
		{
			name: "failed machine returns an error",
			machines: []*machinev1beta1.Machine{
				newMachine("worker-a", &running, "node-a"),
				withFailure(newMachine("worker-f", &failedPhase, ""), machinev1beta1.CreateMachineError, "simulated create failure"),
			},
			wantErrSub: `"worker-f" is Failed`,
		},
		{
			name:      "empty machine set is incomplete",
			wantTotal: 0,
		},
		{
			name:              "missing machine set returns an error",
			missingMachineSet: true,
			wantError:         true,
		},
		{
			name:      "machine list failure returns an error",
			failList:  true,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]runtime.Object, 0, len(tt.machines)+1)
			if !tt.missingMachineSet {
				objects = append(objects, testSelectorMachineSet("workers-a"))
			}
			for _, machine := range tt.machines {
				objects = append(objects, machine)
			}
			machineClient := fakemachineclient.NewClientset(objects...)
			if tt.failList {
				machineClient.PrependReactor("list", "machines", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("simulated machine list failure")
				})
			}
			manager := NewMachineManager(fakekube.NewClientset(), machineClient, nil)

			complete, ready, total, err := manager.CheckMachinesReady(context.Background(), "workers-a")
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("CheckMachinesReady() error = %v, want containing %q", err, tt.wantErrSub)
				}
				return
			}
			if (err != nil) != tt.wantError {
				t.Fatalf("CheckMachinesReady() error = %v, wantError %t", err, tt.wantError)
			}
			if complete != tt.wantComplete || ready != tt.wantReady || total != tt.wantTotal {
				t.Errorf("CheckMachinesReady() = (%t, %d, %d), want (%t, %d, %d)", complete, ready, total, tt.wantComplete, tt.wantReady, tt.wantTotal)
			}
		})
	}
}

func TestCheckNodesReady(t *testing.T) {
	newMachine := func(name, nodeName string) *machinev1beta1.Machine {
		machine := testMachineWithMSLabel(name, "workers-a")
		if nodeName != "" {
			machine.Status.NodeRef = &corev1.ObjectReference{Name: nodeName}
		}
		return machine
	}
	newNode := func(name string, conditions ...corev1.NodeCondition) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     corev1.NodeStatus{Conditions: conditions},
		}
	}

	tests := []struct {
		name              string
		machines          []*machinev1beta1.Machine
		nodes             []*corev1.Node
		missingMachineSet bool
		failList          bool
		wantComplete      bool
		wantReady         int32
		wantTotal         int32
		wantError         bool
	}{
		{
			name: "all referenced nodes reporting Ready are ready",
			machines: []*machinev1beta1.Machine{
				newMachine("worker-a", "node-a"),
				newMachine("worker-b", "node-b"),
			},
			nodes: []*corev1.Node{
				newNode("node-a", corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}),
				newNode("node-b", corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}),
			},
			wantComplete: true,
			wantReady:    2,
			wantTotal:    2,
		},
		{
			name: "not-ready, unknown, missing condition, missing node, and missing node reference do not count",
			machines: []*machinev1beta1.Machine{
				newMachine("worker-a", "node-ready"),
				newMachine("worker-b", "node-not-ready"),
				newMachine("worker-c", "node-unknown"),
				newMachine("worker-d", "node-no-condition"),
				newMachine("worker-e", "node-missing"),
				newMachine("worker-f", ""),
			},
			nodes: []*corev1.Node{
				newNode("node-ready", corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}),
				newNode("node-not-ready", corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse}),
				newNode("node-unknown", corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}),
				newNode("node-no-condition", corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse}),
			},
			wantReady: 1,
			wantTotal: 6,
		},
		{
			name:      "empty machine set is incomplete",
			wantTotal: 0,
		},
		{
			name:              "missing machine set returns an error",
			missingMachineSet: true,
			wantError:         true,
		},
		{
			name:      "machine list failure returns an error",
			failList:  true,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]runtime.Object, 0, len(tt.machines)+1)
			if !tt.missingMachineSet {
				objects = append(objects, testSelectorMachineSet("workers-a"))
			}
			for _, machine := range tt.machines {
				objects = append(objects, machine)
			}
			machineClient := fakemachineclient.NewClientset(objects...)
			if tt.failList {
				machineClient.PrependReactor("list", "machines", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("simulated machine list failure")
				})
			}
			nodeObjects := make([]runtime.Object, 0, len(tt.nodes))
			for _, node := range tt.nodes {
				nodeObjects = append(nodeObjects, node)
			}
			manager := NewMachineManager(fakekube.NewClientset(nodeObjects...), machineClient, nil)

			complete, ready, total, err := manager.CheckNodesReady(context.Background(), "workers-a")
			if (err != nil) != tt.wantError {
				t.Fatalf("CheckNodesReady() error = %v, wantError %t", err, tt.wantError)
			}
			if complete != tt.wantComplete || ready != tt.wantReady || total != tt.wantTotal {
				t.Errorf("CheckNodesReady() = (%t, %d, %d), want (%t, %d, %d)", complete, ready, total, tt.wantComplete, tt.wantReady, tt.wantTotal)
			}
		})
	}
}

// testProviderSpecMachineSet creates a MachineSet with an unmarshalled raw
// VSphereMachineProviderSpec, as required by updateMachineSetProviderSpec.
func testProviderSpecMachineSet(t *testing.T) *machinev1beta1.MachineSet {
	t.Helper()
	ms := newTestMachineSet("worker", 3)
	raw, err := json.Marshal(&machinev1beta1.VSphereMachineProviderSpec{})
	if err != nil {
		t.Fatalf("marshalling provider spec: %v", err)
	}
	ms.Spec.Template.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: raw}
	return ms
}

func TestUpdateMachineSetProviderSpecNetworks(t *testing.T) {
	tests := []struct {
		name        string
		network     string
		wantNetwork string
	}{
		{
			name:        "plain network name is used as-is",
			network:     "VM Network",
			wantNetwork: "VM Network",
		},
		{
			name:        "inventory path is split to last element",
			network:     "/DC1/network/VM Network",
			wantNetwork: "VM Network",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := testProviderSpecMachineSet(t)
			fd := &configv1.VSpherePlatformFailureDomainSpec{
				Name:   "fd1",
				Server: "target.example.com",
				Topology: configv1.VSpherePlatformTopology{
					Datacenter: "DC1",
					Datastore:  "/DC1/datastore/DS1",
					Networks:   []string{tt.network},
				},
			}

			ctx := context.Background()
			if err := updateMachineSetProviderSpec(ctx, ms, fd, "test-infra"); err != nil {
				t.Fatalf("updateMachineSetProviderSpec: %v", err)
			}

			providerSpec, err := ExtractVSphereProviderSpec(ms)
			if err != nil {
				t.Fatalf("ExtractVSphereProviderSpec: %v", err)
			}
			if len(providerSpec.Network.Devices) != 1 {
				t.Fatalf("network devices = %d, want 1", len(providerSpec.Network.Devices))
			}
			if got := providerSpec.Network.Devices[0].NetworkName; got != tt.wantNetwork {
				t.Fatalf("network name = %q, want %q", got, tt.wantNetwork)
			}
		})
	}
}
