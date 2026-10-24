package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestEnsureWorkloadMigratedFailureDomainMapping covers mapping-driven worker
// MachineSet creation: one destination MachineSet per mapped source MachineSet,
// each copying its source provider spec (VM size) with the destination failure
// domain's template and topology, replicas summed exactly per destination
// failure domain, source assignments kept disjoint, and explicit zero replica
// totals preserved.
func TestEnsureWorkloadMigratedFailureDomainMapping(t *testing.T) {
	ctx := context.Background()

	t.Run("creates one destination MachineSet per mapped source MachineSet", func(t *testing.T) {
		reconciler := newRolloutReconciler(t, []runtime.Object{
			newInfrastructureForMapping(
				newSourceFailureDomainForMapping("source-fd-1", "dc-source-1", "rp-a"),
				newSourceFailureDomainForMapping("source-fd-2", "dc-source-2", "rp-b"),
				newTargetFailureDomain("target-fd-1"),
				newTargetFailureDomain("target-fd-2"),
			),
			newSourceMachineSetForMapping("source-worker-a1", "dc-source-1", "rp-a", 2, 4),
			newSourceMachineSetForMapping("source-worker-a2", "dc-source-1", "rp-a", 1, 8),
			newSourceMachineSetForMapping("source-worker-b1", "dc-source-2", "rp-b", 3, 2),
		})
		migration := newMappingTestMigration(
			[]configv1.VSpherePlatformFailureDomainSpec{newTargetFailureDomain("target-fd-1"), newTargetFailureDomain("target-fd-2")},
			[]migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-1", Destination: "target-fd-1"},
				{Source: "source-fd-2", Destination: "target-fd-2"},
			},
		)

		if _, err := reconciler.ensureWorkloadMigrated(ctx, migration); err != nil {
			t.Fatalf("ensureWorkloadMigrated: %v", err)
		}

		assertMappingMachineSet(t, ctx, reconciler, "test-infra-worker-target-fd-1", "target-fd-1", 2, 4)
		assertMappingMachineSet(t, ctx, reconciler, "test-infra-worker-target-fd-1-2", "target-fd-1", 1, 8)
		assertMappingMachineSet(t, ctx, reconciler, "test-infra-worker-target-fd-2", "target-fd-2", 3, 2)
		assertMachineSetAbsent(t, ctx, reconciler, "test-infra-worker-target-fd-2-2")
	})

	t.Run("keeps source failure domain assignments disjoint", func(t *testing.T) {
		// Two source failure domains share a datacenter but own distinct
		// resource pools; each destination failure domain must inherit exactly
		// its own machinesets' provider specs and replica counts.
		reconciler := newRolloutReconciler(t, []runtime.Object{
			newInfrastructureForMapping(
				newSourceFailureDomainForMapping("source-fd-1", "dc-source", "rp-a"),
				newSourceFailureDomainForMapping("source-fd-2", "dc-source", "rp-b"),
				newTargetFailureDomain("target-fd-1"),
				newTargetFailureDomain("target-fd-2"),
			),
			newSourceMachineSetForMapping("source-worker-a1", "dc-source", "rp-a", 2, 4),
			newSourceMachineSetForMapping("source-worker-a2", "dc-source", "rp-a", 3, 8),
			newSourceMachineSetForMapping("source-worker-b1", "dc-source", "rp-b", 1, 2),
		})
		migration := newMappingTestMigration(
			[]configv1.VSpherePlatformFailureDomainSpec{newTargetFailureDomain("target-fd-1"), newTargetFailureDomain("target-fd-2")},
			[]migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-1", Destination: "target-fd-1"},
				{Source: "source-fd-2", Destination: "target-fd-2"},
			},
		)

		if _, err := reconciler.ensureWorkloadMigrated(ctx, migration); err != nil {
			t.Fatalf("ensureWorkloadMigrated: %v", err)
		}

		assertMappingMachineSet(t, ctx, reconciler, "test-infra-worker-target-fd-1", "target-fd-1", 2, 4)
		assertMappingMachineSet(t, ctx, reconciler, "test-infra-worker-target-fd-1-2", "target-fd-1", 3, 8)
		assertMappingMachineSet(t, ctx, reconciler, "test-infra-worker-target-fd-2", "target-fd-2", 1, 2)
		// No double counting: the second failure domain gets no spillover
		// machineset and no replica floor is applied.
		assertMachineSetAbsent(t, ctx, reconciler, "test-infra-worker-target-fd-2-2")
	})

	t.Run("preserves an explicit zero replica total", func(t *testing.T) {
		reconciler := newRolloutReconciler(t, []runtime.Object{
			newInfrastructureForMapping(
				newSourceFailureDomainForMapping("source-fd-1", "dc-source", "rp-a"),
				newTargetFailureDomain("target-fd-1"),
			),
			newSourceMachineSetForMapping("source-worker-a1", "dc-source", "rp-a", 0, 4),
			newSourceMachineSetForMapping("source-worker-a2", "dc-source", "rp-a", 0, 8),
		})
		migration := newMappingTestMigration(
			[]configv1.VSpherePlatformFailureDomainSpec{newTargetFailureDomain("target-fd-1")},
			[]migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}},
		)

		if _, err := reconciler.ensureWorkloadMigrated(ctx, migration); err != nil {
			t.Fatalf("ensureWorkloadMigrated: %v", err)
		}

		assertMappingMachineSet(t, ctx, reconciler, "test-infra-worker-target-fd-1", "target-fd-1", 0, 4)
		assertMappingMachineSet(t, ctx, reconciler, "test-infra-worker-target-fd-1-2", "target-fd-1", 0, 8)
		// No MachineSet may be forced to 1 to satisfy a minimum replica floor.
		for _, name := range []string{"test-infra-worker-target-fd-1", "test-infra-worker-target-fd-1-2"} {
			ms := getMappingMachineSet(t, ctx, reconciler, name)
			if ms.Spec.Replicas != nil && *ms.Spec.Replicas != 0 {
				t.Fatalf("machineset %q replicas = %d, want 0 (zero replica totals are preserved)", name, *ms.Spec.Replicas)
			}
		}
	})

	t.Run("skips a mapped source FD with no source MachineSets", func(t *testing.T) {
		reconciler := newRolloutReconciler(t, []runtime.Object{
			newInfrastructureForMapping(
				newSourceFailureDomainForMapping("source-fd-empty", "dc-source", "rp-empty"),
				newSourceFailureDomainForMapping("source-fd-1", "dc-source", "rp-a"),
				newTargetFailureDomain("target-fd-1"),
				newTargetFailureDomain("target-fd-2"),
			),
			newSourceMachineSetForMapping("source-worker-a1", "dc-source", "rp-a", 2, 4),
		})
		migration := newMappingTestMigration(
			[]configv1.VSpherePlatformFailureDomainSpec{newTargetFailureDomain("target-fd-1"), newTargetFailureDomain("target-fd-2")},
			[]migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-empty", Destination: "target-fd-1"},
				{Source: "source-fd-1", Destination: "target-fd-2"},
			},
		)

		_, err := reconciler.ensureWorkloadMigrated(ctx, migration)
		if err != nil {
			t.Fatalf("ensureWorkloadMigrated: %v", err)
		}
		mgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
		if _, err := mgr.GetMachineSet(ctx, workerMachineSetName("test-infra", "target-fd-1")); err == nil {
			t.Fatal("unexpected target MachineSet for empty source")
		}
		// A second reconcile must reach worker readiness rather than attempt
		// creation again or demand a nonexistent target for the empty source.
		if _, err := reconciler.ensureWorkloadMigrated(ctx, migration); err != nil {
			t.Fatalf("second ensureWorkloadMigrated: %v", err)
		}
		for _, cond := range migration.Status.Conditions {
			if cond.Type == migrationv1alpha1.ConditionWorkloadMigrated && !strings.Contains(cond.Message, "Waiting for target workers") {
				t.Fatalf("second reconcile message = %q, want worker readiness gate", cond.Message)
			}
		}
	})

	t.Run("errors when an existing MachineSet with a generated name targets another vCenter", func(t *testing.T) {
		reconciler := newRolloutReconciler(t, []runtime.Object{
			newInfrastructureForMapping(
				newSourceFailureDomainForMapping("source-fd-1", "dc-source", "rp-a"),
				newSourceFailureDomainForMapping("source-fd-2", "dc-source", "rp-b"),
				newTargetFailureDomain("target-fd-1"),
				newTargetFailureDomain("target-fd-2"),
			),
			// Occupies the generated name for target-fd-1 but targets a
			// different vCenter, so idempotent skipping must not apply.
			newTargetMachineSetForRollout(workerMachineSetName("test-infra", "target-fd-1"), "stray.example.com", 1),
			newSourceMachineSetForMapping("source-worker-a1", "dc-source", "rp-a", 2, 4),
			newSourceMachineSetForMapping("source-worker-b1", "dc-source", "rp-b", 1, 2),
		})
		migration := newMappingTestMigration(
			[]configv1.VSpherePlatformFailureDomainSpec{newTargetFailureDomain("target-fd-1"), newTargetFailureDomain("target-fd-2")},
			[]migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-1", Destination: "target-fd-1"},
				{Source: "source-fd-2", Destination: "target-fd-2"},
			},
		)

		_, err := reconciler.ensureWorkloadMigrated(ctx, migration)
		if err == nil || !strings.Contains(err.Error(), "collides with existing MachineSet") {
			t.Fatalf("ensureWorkloadMigrated error = %v, want error containing %q", err, "collides with existing MachineSet")
		}
	})
}

// TestCreateMappedWorkerMachineSetsRecoveryGates covers the fail-closed gates on
// the creation path: a destination MachineSet recorded earlier must never be
// recreated as zero capacity, and must not be silently abandoned once the
// source MachineSets that carried its replica counts are gone.
func TestCreateMappedWorkerMachineSetsRecoveryGates(t *testing.T) {
	ctx := context.Background()
	const infraID = "test-infra"
	sourceFD := newSourceFailureDomainForMapping("source-fd-1", "dc-source", "rp-a")
	destFD := newTargetFailureDomain("target-fd-1")
	base := workerMachineSetName(infraID, destFD.Name)
	mappings := []migrationv1alpha1.FailureDomainMapping{{Source: sourceFD.Name, Destination: destFD.Name}}

	tests := []struct {
		name        string
		objects     []runtime.Object
		recorded    []string
		wantErr     string
		wantCreated []string
		wantAbsent  []string
	}{
		{
			name:       "recovering a recorded destination whose source is scaled to zero fails closed",
			objects:    []runtime.Object{newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 0, 4)},
			recorded:   []string{base},
			wantErr:    `recreating destination MachineSet "test-infra-worker-target-fd-1": source MachineSet "source-worker-a" is already scaled to zero`,
			wantAbsent: []string{base},
		},
		{
			name:        "first-time creation of a zero-replica source keeps zero replicas",
			objects:     []runtime.Object{newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 0, 4)},
			wantCreated: []string{base},
		},
		{
			name:       "recorded destinations with no source MachineSets fail closed",
			recorded:   []string{base, base + "-2"},
			wantErr:    fmt.Sprintf("recorded destination MachineSets for failure domain %q are missing but their source MachineSets are gone: restore %s, %s before retrying", destFD.Name, base, base+"-2"),
			wantAbsent: []string{base, base + "-2"},
		},
		{
			name: "originally empty mapping with nothing recorded stays empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler := newRolloutReconciler(t, tt.objects)
			machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
			migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{destFD}, mappings)
			migration.Status.WorkerMachineSetNames = tt.recorded

			_, err := reconciler.createMappedWorkerMachineSets(ctx, migration, machineMgr, infraID, mappings, []configv1.VSpherePlatformFailureDomainSpec{sourceFD})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("createMappedWorkerMachineSets: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("createMappedWorkerMachineSets error = %v, want containing %q", err, tt.wantErr)
			}
			for _, name := range tt.wantCreated {
				ms := getMappingMachineSet(t, ctx, reconciler, name)
				if ms.Spec.Replicas == nil || *ms.Spec.Replicas != 0 {
					t.Fatalf("machineset %q replicas = %v, want 0 (no minimum replica floor on first creation)", name, ms.Spec.Replicas)
				}
			}
			for _, name := range tt.wantAbsent {
				assertMachineSetAbsent(t, ctx, reconciler, name)
			}
		})
	}
}

// TestCreateMappedWorkerMachineSetsSourceIdentity covers the annotation that
// records which source MachineSet a destination copy was created from: the
// generated name comes from the copy's position in the sorted source list, so an
// added earlier source MachineSet would otherwise silently re-point an existing
// copy at different hardware.
func TestCreateMappedWorkerMachineSetsSourceIdentity(t *testing.T) {
	ctx := context.Background()
	const infraID = "test-infra"
	sourceFD := newSourceFailureDomainForMapping("source-fd-1", "dc-source", "rp-a")
	destFD := newTargetFailureDomain("target-fd-1")
	base := workerMachineSetName(infraID, destFD.Name)
	mappings := []migrationv1alpha1.FailureDomainMapping{{Source: sourceFD.Name, Destination: destFD.Name}}
	// Two replica-one sources so the base aggregate sum (2) never matches the
	// per-source replicas (1) and legacy-aggregate detection stays out of the way.
	withSources := func(extra ...runtime.Object) []runtime.Object {
		return append([]runtime.Object{
			newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 1, 4),
			newSourceMachineSetForMapping("source-worker-b", "dc-source", "rp-a", 1, 8),
		}, extra...)
	}

	tests := []struct {
		name    string
		objects []runtime.Object
		wantErr string
	}{
		{
			name: "existing copy annotated for another source MachineSet fails closed",
			objects: withSources(
				newDestinationMachineSetForSource(base, "source-worker-a", 1),
				newDestinationMachineSetForSource(base+"-2", "source-worker-gone", 1),
			),
			wantErr: fmt.Sprintf("destination MachineSet %q was created for source MachineSet %q but now maps to %q", base+"-2", "source-worker-gone", "source-worker-b"),
		},
		{
			name: "existing copies matching their annotated source are idempotent",
			objects: withSources(
				newDestinationMachineSetForSource(base, "source-worker-a", 1),
				newDestinationMachineSetForSource(base+"-2", "source-worker-b", 1),
			),
		},
		{
			// Destination MachineSets created before the annotation existed carry
			// no source identity, so the check cannot be applied retroactively.
			name: "existing copies without the annotation are accepted",
			objects: withSources(
				newTargetMachineSetForRollout(base, "target.example.com", 1),
				newTargetMachineSetForRollout(base+"-2", "target.example.com", 1),
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler := newRolloutReconciler(t, tt.objects)
			machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
			migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{destFD}, mappings)

			_, err := reconciler.createMappedWorkerMachineSets(ctx, migration, machineMgr, infraID, mappings, []configv1.VSpherePlatformFailureDomainSpec{sourceFD})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("createMappedWorkerMachineSets: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("createMappedWorkerMachineSets error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestCreateMappedWorkerMachineSetsRecordsNameAfterCreate covers the ordering of
// the destination inventory write: a name reaches status.workerMachineSetNames
// only once its destination MachineSet exists. A name recorded for a create that
// failed is treated as recovery on the retry, and the zero-replica recovery gate
// then turns that transient failure into a permanent fail-closed error after the
// source MachineSets are scaled to 0.
func TestCreateMappedWorkerMachineSetsRecordsNameAfterCreate(t *testing.T) {
	ctx := context.Background()
	const infraID = "test-infra"
	sourceFD := newSourceFailureDomainForMapping("source-fd-1", "dc-source", "rp-a")
	destFD := newTargetFailureDomain("target-fd-1")
	base := workerMachineSetName(infraID, destFD.Name)
	mappings := []migrationv1alpha1.FailureDomainMapping{{Source: sourceFD.Name, Destination: destFD.Name}}
	infraFDs := []configv1.VSpherePlatformFailureDomainSpec{sourceFD}
	// failFirstCreate makes the first create fail, leaving every later call to
	// the fake client's own create.
	failFirstCreate := func(t *testing.T, reconciler *VmwareCloudFoundationMigrationReconciler) {
		t.Helper()
		calls := 0
		reconciler.MachineClient.(*fakemachineclient.Clientset).PrependReactor("create", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			if calls == 1 {
				return true, nil, fmt.Errorf("injected create failure")
			}
			return false, nil, nil
		})
	}

	t.Run("a failed create records no name and the retry copies the source replicas", func(t *testing.T) {
		reconciler := newRolloutReconciler(t, []runtime.Object{
			newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 2, 4),
		})
		failFirstCreate(t, reconciler)
		machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
		migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{destFD}, mappings)

		if _, err := reconciler.createMappedWorkerMachineSets(ctx, migration, machineMgr, infraID, mappings, infraFDs); err == nil || !strings.Contains(err.Error(), "injected create failure") {
			t.Fatalf("createMappedWorkerMachineSets error = %v, want the injected create failure", err)
		}
		if len(migration.Status.WorkerMachineSetNames) != 0 {
			t.Fatalf("WorkerMachineSetNames = %v, want none recorded for a MachineSet that was never created", migration.Status.WorkerMachineSetNames)
		}
		assertMachineSetAbsent(t, ctx, reconciler, base)

		// The retry is a first creation, not a recovery, so the destination is
		// created from the source replicas and its name is recorded.
		if _, err := reconciler.createMappedWorkerMachineSets(ctx, migration, machineMgr, infraID, mappings, infraFDs); err != nil {
			t.Fatalf("second createMappedWorkerMachineSets: %v", err)
		}
		assertMappingMachineSet(t, ctx, reconciler, base, destFD.Name, 2, 4)
		if !slices.Equal(migration.Status.WorkerMachineSetNames, []string{base}) {
			t.Fatalf("WorkerMachineSetNames = %v, want [%s]", migration.Status.WorkerMachineSetNames, base)
		}
	})

	t.Run("a failed create does not fail the retry once the sources are scaled to zero", func(t *testing.T) {
		reconciler := newRolloutReconciler(t, []runtime.Object{
			newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 2, 4),
		})
		failFirstCreate(t, reconciler)
		machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
		migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{destFD}, mappings)

		if _, err := reconciler.createMappedWorkerMachineSets(ctx, migration, machineMgr, infraID, mappings, infraFDs); err == nil {
			t.Fatal("createMappedWorkerMachineSets succeeded, want the injected create failure")
		}
		// Cleanup scales the source down before the retry, which is what makes a
		// name recorded without a MachineSet uncreatable forever.
		source := getMappingMachineSet(t, ctx, reconciler, "source-worker-a")
		source.Spec.Replicas = new(int32)
		if _, err := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Update(ctx, source, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("scaling source MachineSet down: %v", err)
		}

		if _, err := reconciler.createMappedWorkerMachineSets(ctx, migration, machineMgr, infraID, mappings, infraFDs); err != nil {
			t.Fatalf("second createMappedWorkerMachineSets: %v (a failed create must not become a permanent recovery error)", err)
		}
		assertMachineSetReplicas(t, ctx, reconciler, base, 0)
		if !slices.Equal(migration.Status.WorkerMachineSetNames, []string{base}) {
			t.Fatalf("WorkerMachineSetNames = %v, want [%s]", migration.Status.WorkerMachineSetNames, base)
		}
	})

	t.Run("an existing verified destination is still recorded", func(t *testing.T) {
		reconciler := newRolloutReconciler(t, []runtime.Object{
			newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 2, 4),
			newDestinationMachineSetForSource(base, "source-worker-a", 2),
		})
		machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
		migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{destFD}, mappings)

		if _, err := reconciler.createMappedWorkerMachineSets(ctx, migration, machineMgr, infraID, mappings, infraFDs); err != nil {
			t.Fatalf("createMappedWorkerMachineSets: %v", err)
		}
		if !slices.Equal(migration.Status.WorkerMachineSetNames, []string{base}) {
			t.Fatalf("WorkerMachineSetNames = %v, want the existing destination [%s] recorded", migration.Status.WorkerMachineSetNames, base)
		}
	})
}

// legacyAggregateFixture is the cluster state left behind by the released
// implementation: two source MachineSets in one source failure domain and a single
// unannotated aggregate destination MachineSet in the mapped destination failure
// domain.
type legacyAggregateFixture struct {
	ctx          context.Context
	infraID      string
	sourceFD     configv1.VSpherePlatformFailureDomainSpec
	destFD       configv1.VSpherePlatformFailureDomainSpec
	base         string
	mappings     []migrationv1alpha1.FailureDomainMapping
	sourceMSList []*machinev1beta1.MachineSet
}

func newLegacyAggregateFixture() legacyAggregateFixture {
	f := legacyAggregateFixture{
		ctx:      context.Background(),
		infraID:  "test-infra",
		sourceFD: newSourceFailureDomainForMapping("source-fd-1", "dc-source", "rp-a"),
		destFD:   newTargetFailureDomain("target-fd-1"),
		sourceMSList: []*machinev1beta1.MachineSet{
			newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 2, 4),
			newSourceMachineSetForMapping("source-worker-b", "dc-source", "rp-a", 3, 8),
		},
	}
	f.base = workerMachineSetName(f.infraID, f.destFD.Name)
	f.mappings = []migrationv1alpha1.FailureDomainMapping{{Source: f.sourceFD.Name, Destination: f.destFD.Name}}
	return f
}

// TestEnsureWorkloadMigratedLegacyAggregate covers the in-flight upgrade from the
// released implementation, which created one aggregate destination MachineSet per
// failure domain holding the sum of the mapped source replicas. Step 1 must accept
// it unchanged and record it once, and the scale-down path must stop demanding the
// per-source copies the aggregate already replaces.
func TestEnsureWorkloadMigratedLegacyAggregate(t *testing.T) {
	fixture := newLegacyAggregateFixture()
	// The released operator created the aggregate holding the sum of the mapped
	// source replicas: 2 + 3. Any other count is ambiguous, not legacy.
	aggregate := newTargetMachineSetForRollout(fixture.base, "target.example.com", 5)

	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
		fixture.sourceMSList[0], fixture.sourceMSList[1], aggregate,
	})
	machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
	migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)

	if _, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration); err != nil {
		t.Fatalf("ensureWorkloadMigrated: %v", err)
	}
	assertMachineSetAbsent(t, fixture.ctx, reconciler, fixture.base+"-2")
	// The base is the replacement the migration scales down against, so detection
	// records it as an expected name.
	if !slices.Equal(migration.Status.WorkerMachineSetNames, []string{fixture.base}) {
		t.Fatalf("WorkerMachineSetNames = %v, want [%s]", migration.Status.WorkerMachineSetNames, fixture.base)
	}
	if got := drainLegacyAggregateEvents(reconciler); got != 1 {
		t.Fatalf("LegacyAggregateReplacement events = %d, want 1", got)
	}

	// A second reconcile with the base recorded must stay silent: recording the
	// replacement is what makes the announcement idempotent.
	if _, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration); err != nil {
		t.Fatalf("second ensureWorkloadMigrated: %v", err)
	}
	if got := drainLegacyAggregateEvents(reconciler); got != 0 {
		t.Fatalf("LegacyAggregateReplacement events after recording = %d, want 0", got)
	}
	if !slices.Equal(migration.Status.WorkerMachineSetNames, []string{fixture.base}) {
		t.Fatalf("WorkerMachineSetNames after second reconcile = %v, want [%s]", migration.Status.WorkerMachineSetNames, fixture.base)
	}

	// Scale-down must not require a phantom copy: name recovery has nothing left to
	// record and the coverage gate passes against the same state.
	recovered, err := recoverUnrecordedDestinationNames(fixture.ctx, machineMgr, migration, fixture.sourceMSList, fixture.infraID, fixture.mappings, []configv1.VSpherePlatformFailureDomainSpec{fixture.sourceFD})
	if err != nil {
		t.Fatalf("recoverUnrecordedDestinationNames after legacy aggregate detection: %v", err)
	}
	if len(recovered) != 0 {
		t.Fatalf("recovered = %v, want none: the aggregate is already recorded", recovered)
	}
	if err := validateSourceMachineSetCoverage(fixture.ctx, fixture.sourceMSList, fixture.infraID, fixture.mappings, []configv1.VSpherePlatformFailureDomainSpec{fixture.sourceFD}, migration.Status.WorkerMachineSetNames); err != nil {
		t.Fatalf("validateSourceMachineSetCoverage after legacy aggregate detection: %v", err)
	}
}

// drainLegacyAggregateEvents counts and discards the recorded
// LegacyAggregateReplacement events.
func drainLegacyAggregateEvents(reconciler *VmwareCloudFoundationMigrationReconciler) int {
	recorder := reconciler.Recorder.(*events.FakeRecorder)
	count := 0
	for len(recorder.Events) > 0 {
		if strings.Contains(<-recorder.Events, "LegacyAggregateReplacement") {
			count++
		}
	}
	return count
}

// TestEnsureWorkloadMigratedLegacyAggregateAfterScaleDown covers the state the
// migration itself creates between Step 6 and Step 8: the source MachineSets are
// scaled to 0 while the aggregate still holds the migrated capacity. Replica
// arithmetic cannot reach the sum here, so that state must stay legacy and Step 1
// must keep running. A recorded destination that is genuinely gone must still fail
// closed: the legacy layout only suppresses derived names, never recorded ones.
func TestEnsureWorkloadMigratedLegacyAggregateAfterScaleDown(t *testing.T) {
	fixture := newLegacyAggregateFixture()
	tests := []struct {
		name       string
		recorded   []string
		wantAbsent string
	}{
		{
			name:     "scale-down state keeps the aggregate",
			recorded: []string{fixture.base},
		},
		{
			name:       "missing recorded copy still fails closed",
			recorded:   []string{fixture.base, fixture.base + "-2"},
			wantAbsent: fixture.base + "-2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler := newRolloutReconciler(t, []runtime.Object{
				newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
				newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 0, 4),
				newSourceMachineSetForMapping("source-worker-b", "dc-source", "rp-a", 0, 8),
				// The aggregate still holds the sum the sources carried before
				// Step 6 scaled them to 0.
				newTargetMachineSetForRollout(fixture.base, "target.example.com", 5),
			})
			migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)
			migration.Status.WorkerMachineSetNames = slices.Clone(tt.recorded)

			result, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration)
			if tt.wantAbsent != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantAbsent) {
					t.Fatalf("ensureWorkloadMigrated = (%v, %v), want an error naming the missing recorded copy %q", result, err, tt.wantAbsent)
				}
				assertMachineSetAbsent(t, fixture.ctx, reconciler, tt.wantAbsent)
				return
			}
			if err != nil {
				t.Fatalf("ensureWorkloadMigrated: %v", err)
			}
			if result.RequeueAfter != 30*time.Second {
				t.Fatalf("result requeueAfter = %v, want the worker readiness wait", result.RequeueAfter)
			}
			assertMachineSetAbsent(t, fixture.ctx, reconciler, fixture.base+"-2")
			if !slices.Equal(migration.Status.WorkerMachineSetNames, tt.recorded) {
				t.Fatalf("WorkerMachineSetNames = %v, want %v", migration.Status.WorkerMachineSetNames, tt.recorded)
			}
			if got := drainLegacyAggregateEvents(reconciler); got != 0 {
				t.Fatalf("LegacyAggregateReplacement events = %d, want 0 for an already recorded aggregate", got)
			}
		})
	}
}

// TestEnsureWorkloadMigratedInterruptedAggregateScaleDown covers the state a crash
// or a failed reconcile leaves in the middle of Step 6: the released operator's
// aggregate base still holds the migrated capacity while the source MachineSets are
// only partly scaled down, so the source total matches neither the aggregate sum nor
// a per-source copy. The annotation written at detection is what keeps that state
// classified as legacy, because re-derived arithmetic cannot.
func TestEnsureWorkloadMigratedInterruptedAggregateScaleDown(t *testing.T) {
	fixture := newLegacyAggregateFixture()
	// The aggregate held the sum of the mapped sources before Step 6 scaled one
	// of them to 0.
	const aggregateReplicas = 5

	t.Run("detection annotates the legacy aggregate base", func(t *testing.T) {
		reconciler := newRolloutReconciler(t, []runtime.Object{
			newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
			fixture.sourceMSList[0], fixture.sourceMSList[1],
			newTargetMachineSetForRollout(fixture.base, "target.example.com", aggregateReplicas),
		})
		migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)

		if _, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration); err != nil {
			t.Fatalf("ensureWorkloadMigrated: %v", err)
		}
		base := getMappingMachineSet(t, fixture.ctx, reconciler, fixture.base)
		if got := base.Annotations[openshift.LegacyAggregateAnnotation]; got != openshift.LegacyAggregateAnnotationValue {
			t.Fatalf("machineset %q annotation %q = %q, want %q (provenance must outlive the source scale-down)", fixture.base, openshift.LegacyAggregateAnnotation, got, openshift.LegacyAggregateAnnotationValue)
		}
	})

	t.Run("unannotated interrupted scale-down stays ambiguous", func(t *testing.T) {
		reconciler := newRolloutReconciler(t, []runtime.Object{
			newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
			newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 2, 4),
			newSourceMachineSetForMapping("source-worker-b", "dc-source", "rp-a", 0, 8),
			newTargetMachineSetForRollout(fixture.base, "target.example.com", aggregateReplicas),
		})
		migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)
		want := fmt.Sprintf("destination MachineSet %q has no source MachineSet annotation and %d replicas do not match the %d replicas of its source MachineSets", fixture.base, aggregateReplicas, 2)

		result, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("ensureWorkloadMigrated = (%v, %v), want an error containing %q", result, err, want)
		}
		assertMachineSetAbsent(t, fixture.ctx, reconciler, fixture.base+"-2")
	})

	t.Run("annotated interrupted scale-down stays legacy and scales down", func(t *testing.T) {
		aggregate := newTargetMachineSetForRollout(fixture.base, "target.example.com", aggregateReplicas)
		aggregate.Annotations = map[string]string{openshift.LegacyAggregateAnnotation: openshift.LegacyAggregateAnnotationValue}
		reconciler := newRolloutReconciler(t, []runtime.Object{
			newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
			newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 2, 4),
			newSourceMachineSetForMapping("source-worker-b", "dc-source", "rp-a", 0, 8),
			aggregate,
		})
		migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)

		// Step 1 must accept the state instead of stopping on ambiguity.
		result, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration)
		if err != nil {
			t.Fatalf("ensureWorkloadMigrated: %v", err)
		}
		if result.RequeueAfter != 30*time.Second {
			t.Fatalf("result requeueAfter = %v, want the worker readiness wait", result.RequeueAfter)
		}
		assertMachineSetAbsent(t, fixture.ctx, reconciler, fixture.base+"-2")
		if !slices.Equal(migration.Status.WorkerMachineSetNames, []string{fixture.base}) {
			t.Fatalf("WorkerMachineSetNames = %v, want [%s]", migration.Status.WorkerMachineSetNames, fixture.base)
		}

		// Step 6 must now run: recovery has nothing to add, the coverage gate
		// accepts the recorded aggregate, and the running source is scaled down.
		reconciler.Client = newMigrationStatusClient(t, migration)
		machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
		infraMgr := openshift.NewInfrastructureManager(reconciler.ConfigClient)
		oldMachineSets, err := machineMgr.GetMachineSetsByVCenter(fixture.ctx, fixture.sourceFD.Server)
		if err != nil {
			t.Fatalf("GetMachineSetsByVCenter: %v", err)
		}
		requeued, err := reconciler.scaleDownSourceMachineSets(fixture.ctx, migration, machineMgr, infraMgr, oldMachineSets, fixture.sourceFD.Server, fixture.infraID)
		if err != nil {
			t.Fatalf("scaleDownSourceMachineSets: %v", err)
		}
		if !requeued {
			t.Fatal("scaleDownSourceMachineSets did not scale the remaining running source MachineSet")
		}
		for _, name := range []string{"source-worker-a", "source-worker-b"} {
			if ms := getMappingMachineSet(t, fixture.ctx, reconciler, name); ms.Spec.Replicas == nil || *ms.Spec.Replicas != 0 {
				t.Fatalf("machineset %q replicas = %v, want 0", name, ms.Spec.Replicas)
			}
		}
	})
}

// TestEnsureWorkloadMigratedLegacyAggregateCapacity is the regression for treating
// the durable aggregate annotation as provenance rather than a bypass. The mapped
// source MachineSets can still grow or reappear after detection, and an aggregate
// smaller than their total would be accepted by Step 1, measured only by its own
// replicas, and then scaled away by Step 6. Only an aggregate at or above the
// current source total is accepted, which includes this code's own scale-down where
// the source total only decreases.
//
// The single-source and no-source cases guard the same proof: the single-source
// shortcut in classifyDestinationLayout must not skip it, because one source left
// behind by a partial cleanup can be larger than the aggregate, while zero sources
// is this code's own post-scale-down state and has to stay accepted.
func TestEnsureWorkloadMigratedLegacyAggregateCapacity(t *testing.T) {
	fixture := newLegacyAggregateFixture()
	tests := []struct {
		name              string
		sourceReplicas    []int32
		aggregateReplicas int32
		recorded          []string
		wantErr           string
	}{
		{
			name:              "source grown above the annotated aggregate",
			sourceReplicas:    []int32{2, 4},
			aggregateReplicas: 5,
			wantErr:           fmt.Sprintf("legacy aggregate destination MachineSet %q has 5 replicas but its source MachineSets run 6", fixture.base),
		},
		{
			name:              "annotated aggregate downscaled below the source total",
			sourceReplicas:    []int32{2, 3},
			aggregateReplicas: 4,
			wantErr:           fmt.Sprintf("legacy aggregate destination MachineSet %q has 4 replicas but its source MachineSets run 5", fixture.base),
		},
		{
			// The state Step 6 leaves behind, and the reason the annotation exists:
			// the sources are already scaled to 0, so the aggregate is the only
			// holder of the migrated capacity.
			name:              "own scale-down of the sources keeps the aggregate",
			sourceReplicas:    []int32{0, 0},
			aggregateReplicas: 5,
		},
		{
			// A single source surviving a partial cleanup is exactly the state the
			// single-source shortcut used to accept unseen: it can be larger than
			// the aggregate, which Step 6 would then scale away.
			name:              "single surviving source above the annotated aggregate",
			sourceReplicas:    []int32{6},
			aggregateReplicas: 5,
			wantErr:           fmt.Sprintf("legacy aggregate destination MachineSet %q has 5 replicas but its source MachineSets run 6", fixture.base),
		},
		{
			// Ordinary partial cleanup: the one remaining source still fits the
			// aggregate, so the migration must proceed instead of deadlocking.
			name:              "single remaining source fits the annotated aggregate",
			sourceReplicas:    []int32{2},
			aggregateReplicas: 5,
		},
		{
			// The post-scale-down state of a single-source mapping: no source
			// MachineSet is left at all, so the aggregate is the only holder of the
			// migrated capacity and the guard cannot apply.
			name:              "no source MachineSets keeps the annotated aggregate",
			aggregateReplicas: 5,
			recorded:          []string{fixture.base},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			aggregate := newTargetMachineSetForRollout(fixture.base, "target.example.com", tt.aggregateReplicas)
			aggregate.Annotations = map[string]string{openshift.LegacyAggregateAnnotation: openshift.LegacyAggregateAnnotationValue}
			objects := []runtime.Object{
				newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
				aggregate,
			}
			// Distinct VM sizes keep each source identifiable in assertions.
			sources := []string{"source-worker-a", "source-worker-b"}
			for si, replicas := range tt.sourceReplicas {
				objects = append(objects, newSourceMachineSetForMapping(sources[si], "dc-source", "rp-a", replicas, int32(4*(si+1))))
			}
			reconciler := newRolloutReconciler(t, objects)
			migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)
			migration.Status.WorkerMachineSetNames = slices.Clone(tt.recorded)

			// Every MachineSet write is destructive on this path: a scale-down of a
			// running source, or an added per-source copy. Neither may happen.
			var writes []string
			reconciler.MachineClient.(*fakemachineclient.Clientset).PrependReactor("update", "machinesets", func(action k8stesting.Action) (bool, runtime.Object, error) {
				writes = append(writes, action.(k8stesting.UpdateAction).GetObject().(*machinev1beta1.MachineSet).Name)
				return false, nil, nil
			})

			result, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ensureWorkloadMigrated: %v", err)
				}
				if result.RequeueAfter != 30*time.Second {
					t.Fatalf("result requeueAfter = %v, want the worker readiness wait", result.RequeueAfter)
				}
				if !slices.Equal(migration.Status.WorkerMachineSetNames, []string{fixture.base}) {
					t.Fatalf("WorkerMachineSetNames = %v, want [%s]", migration.Status.WorkerMachineSetNames, fixture.base)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ensureWorkloadMigrated = (%v, %v), want an error containing %q", result, err, tt.wantErr)
				}
				if len(migration.Status.WorkerMachineSetNames) != len(tt.recorded) {
					t.Fatalf("WorkerMachineSetNames = %v, want %v untouched by a refused run", migration.Status.WorkerMachineSetNames, tt.recorded)
				}
				// The creation path shares the resolver, so it must refuse the same
				// state instead of adding per-source capacity next to the aggregate.
				machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
				if _, err := reconciler.createMappedWorkerMachineSets(fixture.ctx, migration, machineMgr, fixture.infraID, fixture.mappings, []configv1.VSpherePlatformFailureDomainSpec{fixture.sourceFD}); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("createMappedWorkerMachineSets error = %v, want containing %q", err, tt.wantErr)
				}
			}
			if len(writes) != 0 {
				t.Fatalf("machine set writes = %v, want none: the aggregate must be accepted or refused without touching capacity", writes)
			}
			assertMachineSetAbsent(t, fixture.ctx, reconciler, fixture.base+"-2")
			assertMachineSetReplicas(t, fixture.ctx, reconciler, fixture.base, tt.aggregateReplicas)
			for si, replicas := range tt.sourceReplicas {
				assertMachineSetReplicas(t, fixture.ctx, reconciler, sources[si], replicas)
			}
		})
	}
}

// TestScaleDownSourceMachineSetsRefusesUndersizedAnnotatedAggregate covers Step 6
// re-running the layout resolver on an annotated aggregate that a single surviving
// source MachineSet outgrew. Refusing here is what stops the source from being
// scaled to 0 with nowhere for its capacity to go, which is the same proof Step 1
// applies on this state.
func TestScaleDownSourceMachineSetsRefusesUndersizedAnnotatedAggregate(t *testing.T) {
	fixture := newLegacyAggregateFixture()
	ctx := fixture.ctx
	aggregate := newTargetMachineSetForRollout(fixture.base, "target.example.com", 5)
	aggregate.Annotations = map[string]string{openshift.LegacyAggregateAnnotation: openshift.LegacyAggregateAnnotationValue}
	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
		newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 6, 4),
		aggregate,
	})
	migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)
	// The aggregate was already recorded before the surviving source grew.
	migration.Status.WorkerMachineSetNames = []string{fixture.base}
	reconciler.Client = newMigrationStatusClient(t, migration)

	var writes []string
	reconciler.MachineClient.(*fakemachineclient.Clientset).PrependReactor("update", "machinesets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		writes = append(writes, action.(k8stesting.UpdateAction).GetObject().(*machinev1beta1.MachineSet).Name)
		return false, nil, nil
	})
	machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
	infraMgr := openshift.NewInfrastructureManager(reconciler.ConfigClient)
	oldMachineSets, err := machineMgr.GetMachineSetsByVCenter(ctx, fixture.sourceFD.Server)
	if err != nil {
		t.Fatalf("GetMachineSetsByVCenter: %v", err)
	}

	want := fmt.Sprintf("legacy aggregate destination MachineSet %q has 5 replicas but its source MachineSets run 6", fixture.base)
	if _, err := reconciler.scaleDownSourceMachineSets(ctx, migration, machineMgr, infraMgr, oldMachineSets, fixture.sourceFD.Server, fixture.infraID); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("scaleDownSourceMachineSets error = %v, want containing %q", err, want)
	}
	if len(writes) != 0 {
		t.Fatalf("machine set writes = %v, want none: Step 6 must not scale the surviving source down", writes)
	}
	assertMachineSetReplicas(t, ctx, reconciler, "source-worker-a", 6)
	assertMachineSetReplicas(t, ctx, reconciler, fixture.base, 5)
}

// TestScaleDownPublishesRecordedNamesBeforeCleanup covers the durability barrier in
// front of the destructive steps: Step 1 records a detected aggregate in status in
// memory only, and name recovery then has nothing left to re-derive. Scale-down must
// still publish the full name set, and must not scale anything when that write fails.
func TestScaleDownPublishesRecordedNamesBeforeCleanup(t *testing.T) {
	fixture := newLegacyAggregateFixture()

	tests := []struct {
		name         string
		statusErr    error
		wantErr      string
		wantCalls    []string
		wantReplicas int32
	}{
		{
			name:         "publishes the in-memory name set before scaling down",
			wantCalls:    []string{"status-update", "scale-down"},
			wantReplicas: 0,
		},
		{
			name:         "a failing status write aborts before scaling down",
			statusErr:    fmt.Errorf("injected status write failure"),
			wantErr:      "persisting destination MachineSet names before scale-down",
			wantCalls:    []string{"status-update"},
			wantReplicas: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := fixture.ctx
			aggregate := newTargetMachineSetForRollout(fixture.base, "target.example.com", 5)
			aggregate.Annotations = map[string]string{openshift.LegacyAggregateAnnotation: openshift.LegacyAggregateAnnotationValue}
			reconciler := newRolloutReconciler(t, []runtime.Object{
				newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
				newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 2, 4),
				newSourceMachineSetForMapping("source-worker-b", "dc-source", "rp-a", 0, 8),
				aggregate,
			})
			migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)
			// Step 1 recorded the aggregate in memory; the server copy never received
			// that write, so this publication is the only chance to make it durable.
			migration.Status.WorkerMachineSetNames = []string{fixture.base}
			persisted := migration.DeepCopy()
			persisted.Status.WorkerMachineSetNames = nil

			calls := &orderedClientCalls{}
			machineClient := reconciler.MachineClient.(*fakemachineclient.Clientset)
			machineClient.PrependReactor("update", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
				calls.add("scale-down")
				return false, nil, nil
			})
			fakeClient := clientfake.NewClientBuilder().
				WithScheme(newMigrationTestScheme(t)).
				WithObjects(persisted).
				WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
						calls.add("status-update")
						if tt.statusErr != nil {
							return tt.statusErr
						}
						return c.SubResource(subResourceName).Update(ctx, obj, opts...)
					},
				}).
				Build()
			reconciler.Client = fakeClient

			machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
			infraMgr := openshift.NewInfrastructureManager(reconciler.ConfigClient)
			oldMachineSets, err := machineMgr.GetMachineSetsByVCenter(ctx, fixture.sourceFD.Server)
			if err != nil {
				t.Fatalf("GetMachineSetsByVCenter: %v", err)
			}
			_, err = reconciler.scaleDownSourceMachineSets(ctx, migration, machineMgr, infraMgr, oldMachineSets, fixture.sourceFD.Server, fixture.infraID)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("scaleDownSourceMachineSets: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("scaleDownSourceMachineSets error = %v, want containing %q", err, tt.wantErr)
			}

			if got := calls.snapshot(); !slices.Equal(got, tt.wantCalls) {
				t.Fatalf("client calls = %v, want %v", got, tt.wantCalls)
			}
			if ms := getMappingMachineSet(t, ctx, reconciler, "source-worker-a"); ms.Spec.Replicas == nil || *ms.Spec.Replicas != tt.wantReplicas {
				t.Fatalf("source machineset replicas = %v, want %d", ms.Spec.Replicas, tt.wantReplicas)
			}
			if tt.statusErr != nil {
				return
			}
			published := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), published); err != nil {
				t.Fatalf("getting persisted migration: %v", err)
			}
			if !slices.Contains(published.Status.WorkerMachineSetNames, fixture.base) {
				t.Fatalf("persisted WorkerMachineSetNames = %v, want the in-memory base %q published before cleanup", published.Status.WorkerMachineSetNames, fixture.base)
			}
		})
	}
}

// orderedClientCalls records the order in which a test observes distinct client
// calls, so a test can prove a durable write precedes a destructive one.
type orderedClientCalls struct {
	mu    sync.Mutex
	calls []string
}

func (c *orderedClientCalls) add(call string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *orderedClientCalls) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// newMigrationTestScheme builds the scheme the status fake client needs.
func newMigrationTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	return scheme
}

// TestEnsureWorkloadMigratedAnnotatedBaseStaysPerSource is the regression for
// classifying by replica arithmetic: an annotated per-source base whose replicas
// happen to equal the source sum is not a legacy aggregate, so the suffixed copy
// stays required and a deleted copy fails closed.
func TestEnsureWorkloadMigratedAnnotatedBaseStaysPerSource(t *testing.T) {
	tests := []struct {
		name     string
		sourceB  int32
		baseRepl int32
		recorded []string
	}{
		{
			// The recorded suffixed copy was deleted: the run must fail closed.
			name:     "recorded suffixed copy stays required",
			sourceB:  0,
			baseRepl: 2,
		},
		{
			// The suffixed copy was never recorded, so the copy has to be
			// recreated from its source instead of being dropped as aggregate
			// capacity.
			name:     "unrecorded suffixed copy is still created",
			sourceB:  1,
			baseRepl: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newLegacyAggregateFixture()
			// The base copy's own replicas equal the sum 2+tt.sourceB: the trap
			// the arithmetic rule fell into.
			sourceA := newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 2, 4)
			sourceB := newSourceMachineSetForMapping("source-worker-b", "dc-source", "rp-a", tt.sourceB, 8)

			reconciler := newRolloutReconciler(t, []runtime.Object{
				newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
				sourceA, sourceB,
				newDestinationMachineSetForSource(fixture.base, "source-worker-a", tt.baseRepl),
			})
			machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
			migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)
			recorded := []string{fixture.base}
			if tt.sourceB == 0 {
				recorded = append(recorded, fixture.base+"-2")
			}
			migration.Status.WorkerMachineSetNames = slices.Clone(recorded)

			sourceMSList, err := machineMgr.GetMachineSetsByFailureDomain(fixture.ctx, fixture.sourceFD.Server, &fixture.sourceFD)
			if err != nil {
				t.Fatalf("GetMachineSetsByFailureDomain: %v", err)
			}
			names, legacyAggregate, err := expectedDestinationNames(fixture.ctx, machineMgr, fixture.base, sourceMSList, &fixture.destFD, recorded)
			if err != nil {
				t.Fatalf("expectedDestinationNames: %v", err)
			}
			if legacyAggregate {
				t.Fatalf("annotated per-source base with recorded %v classified as legacy aggregate", recorded)
			}
			wantNames := slices.Clone(recorded)
			if !slices.Contains(wantNames, fixture.base+"-2") {
				wantNames = append(wantNames, fixture.base+"-2")
			}
			if !slices.Equal(names, wantNames) {
				t.Fatalf("expectedDestinationNames = %v, want %v", names, wantNames)
			}

			_, err = reconciler.ensureWorkloadMigrated(fixture.ctx, migration)
			if tt.sourceB == 0 {
				if err == nil {
					t.Fatal("ensureWorkloadMigrated accepted a deleted recorded destination MachineSet")
				}
				assertMachineSetAbsent(t, fixture.ctx, reconciler, fixture.base+"-2")
				return
			}
			if err != nil {
				t.Fatalf("ensureWorkloadMigrated: %v", err)
			}
			assertMachineSetReplicas(t, fixture.ctx, reconciler, fixture.base+"-2", 1)
		})
	}
}

// TestEnsureWorkloadMigratedAmbiguousUnannotatedBase is the regression for
// guessing an unannotated base's provenance: under a multi-source mapping with no
// suffixed copy next to it, the base is either the released aggregate or a
// per-source copy from a commit that predates the source annotation. Only the
// aggregate's own replica count tells them apart, so a count that is not the source
// sum fails closed instead of either duplicating or dropping capacity.
func TestEnsureWorkloadMigratedAmbiguousUnannotatedBase(t *testing.T) {
	fixture := newLegacyAggregateFixture()
	tests := []struct {
		name         string
		replicasA    int32
		replicasB    int32
		baseReplicas int32
		recorded     []string
	}{
		{
			// The layout a commit without the annotation left behind: the base copied
			// sources[0] and a second source was added afterwards, so its capacity
			// was never copied and both sources can be scaled down.
			name:         "per-source base with an added source",
			replicasA:    2,
			replicasB:    2,
			baseReplicas: 2,
			recorded:     []string{fixture.base},
		},
		{
			// The released aggregate scaled away from the source replica sum.
			name:         "aggregate scaled away from the source sum",
			replicasA:    2,
			replicasB:    3,
			baseReplicas: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler := newRolloutReconciler(t, []runtime.Object{
				newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
				newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", tt.replicasA, 4),
				newSourceMachineSetForMapping("source-worker-b", "dc-source", "rp-a", tt.replicasB, 8),
				newTargetMachineSetForRollout(fixture.base, "target.example.com", tt.baseReplicas),
			})
			machineMgr := openshift.NewMachineManager(reconciler.KubeClient, reconciler.MachineClient, reconciler.DynamicClient)
			migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)
			migration.Status.WorkerMachineSetNames = slices.Clone(tt.recorded)
			want := fmt.Sprintf("destination MachineSet %q has no source MachineSet annotation and %d replicas do not match the %d replicas of its source MachineSets", fixture.base, tt.baseReplicas, tt.replicasA+tt.replicasB)

			// Step 1 must raise it: every expected name already exists, so the
			// creation path never runs for that state on its own.
			result, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("ensureWorkloadMigrated = (%v, %v), want an error containing %q", result, err, want)
			}
			// The remedy must be non-destructive and name the provenance
			// annotation: in the interrupted scale-down state, deleting or rescaling
			// the base destroys migrated capacity.
			if !strings.Contains(err.Error(), "migration.openshift.io/legacy-aggregate") {
				t.Fatalf("ambiguous error = %q, want it to name migration.openshift.io/legacy-aggregate", err)
			}
			if strings.Contains(strings.ToLower(err.Error()), "delete") {
				t.Fatalf("ambiguous error = %q, want no destructive delete instruction", err)
			}
			assertMachineSetAbsent(t, fixture.ctx, reconciler, fixture.base+"-2")

			// The creation path must refuse the same state instead of adding
			// suffixed capacity next to it.
			_, err = reconciler.createMappedWorkerMachineSets(fixture.ctx, migration, machineMgr, fixture.infraID, fixture.mappings, []configv1.VSpherePlatformFailureDomainSpec{fixture.sourceFD})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("createMappedWorkerMachineSets error = %v, want containing %q", err, want)
			}
			assertMachineSetAbsent(t, fixture.ctx, reconciler, fixture.base+"-2")
			assertMachineSetReplicas(t, fixture.ctx, reconciler, fixture.base, tt.baseReplicas)
		})
	}
}

// TestEnsureWorkloadMigratedCleanupReindexingIdentity covers the identity check
// during ordinary cleanup: source MachineSets are deleted sequentially after being
// scaled to zero, so the surviving source legitimately reindexes onto the base name
// the deleted source owned. That reindexing must not be reported as an identity
// violation by a source that runs no replicas, and must still be caught by one that
// does: a running survivor was never copied.
func TestEnsureWorkloadMigratedCleanupReindexingIdentity(t *testing.T) {
	fixture := newLegacyAggregateFixture()
	tests := []struct {
		name            string
		survivingSource int32
		wantErr         bool
	}{
		{name: "zero-replica survivor reindexes onto the base name", survivingSource: 0},
		{name: "running survivor keeps the identity check", survivingSource: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Source A, the base copy's source, was deleted after being scaled to
			// zero; source B survives and now sorts first.
			reconciler := newRolloutReconciler(t, []runtime.Object{
				newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
				newSourceMachineSetForMapping("source-worker-b", "dc-source", "rp-a", tt.survivingSource, 8),
				newDestinationMachineSetForSource(fixture.base, "source-worker-a", 2),
				newDestinationMachineSetForSource(fixture.base+"-2", "source-worker-b", 2),
			})
			migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)
			migration.Status.WorkerMachineSetNames = []string{fixture.base, fixture.base + "-2"}

			result, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ensureWorkloadMigrated: %v", err)
				}
				// No identity error and no spin: the run waits on worker readiness.
				if result.RequeueAfter != 30*time.Second {
					t.Fatalf("result requeueAfter = %v, want the worker readiness wait", result.RequeueAfter)
				}
				return
			}
			want := fmt.Sprintf("destination MachineSet %q was created for source MachineSet %q but now maps to %q", fixture.base, "source-worker-a", "source-worker-b")
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("ensureWorkloadMigrated error = %v, want containing %q", err, want)
			}
		})
	}
}

// TestEnsureWorkloadMigratedCleanedUpMappingDoesNotBlockCreation covers two
// mappings in one reconcile: mapping A's source MachineSets are gone and its
// recorded destination still exists, so there is nothing to create for A, while
// mapping B still needs its destination. A must not turn into an error that blocks
// B's creation.
func TestEnsureWorkloadMigratedCleanedUpMappingDoesNotBlockCreation(t *testing.T) {
	sourceA := newSourceFailureDomainForMapping("source-fd-1", "dc-source-1", "rp-a")
	sourceB := newSourceFailureDomainForMapping("source-fd-2", "dc-source-2", "rp-b")
	destA := newTargetFailureDomain("target-fd-1")
	destB := newTargetFailureDomain("target-fd-2")
	baseA := workerMachineSetName("test-infra", destA.Name)
	baseB := workerMachineSetName("test-infra", destB.Name)

	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForMapping(sourceA, sourceB, destA, destB),
		newSourceMachineSetForMapping("source-worker-b1", "dc-source-2", "rp-b", 1, 8),
		newDestinationMachineSetForSource(baseA, "source-worker-a1", 2),
	})
	migration := newMappingTestMigration(
		[]configv1.VSpherePlatformFailureDomainSpec{destA, destB},
		[]migrationv1alpha1.FailureDomainMapping{
			{Source: sourceA.Name, Destination: destA.Name},
			{Source: sourceB.Name, Destination: destB.Name},
		},
	)
	migration.Status.WorkerMachineSetNames = []string{baseA}

	if _, err := reconciler.ensureWorkloadMigrated(context.Background(), migration); err != nil {
		t.Fatalf("ensureWorkloadMigrated: %v", err)
	}
	assertMachineSetReplicas(t, context.Background(), reconciler, baseA, 2)
	assertMappingMachineSet(t, context.Background(), reconciler, baseB, destB.Name, 1, 8)
}

// TestEnsureWorkloadMigratedVerifiesDestinationIdentity covers the existence path:
// when every expected destination already exists the creation path never runs, so
// the source identity of each copy must be verified here instead.
func TestEnsureWorkloadMigratedVerifiesDestinationIdentity(t *testing.T) {
	fixture := newLegacyAggregateFixture()
	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
		newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 1, 4),
		newSourceMachineSetForMapping("source-worker-b", "dc-source", "rp-a", 1, 8),
		newDestinationMachineSetForSource(fixture.base, "source-worker-a", 1),
		newDestinationMachineSetForSource(fixture.base+"-2", "source-worker-gone", 1),
	})
	migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)

	_, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration)
	want := fmt.Sprintf("destination MachineSet %q was created for source MachineSet %q but now maps to %q", fixture.base+"-2", "source-worker-gone", "source-worker-b")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ensureWorkloadMigrated error = %v, want containing %q", err, want)
	}
}

// TestEnsureWorkloadMigratedRecordedCopyWithoutSource is the regression for a
// partially removed source failure domain: a recorded copy whose source MachineSet
// is gone cannot be recreated, so the run must fail closed instead of requeueing
// forever behind a creation progress message.
func TestEnsureWorkloadMigratedRecordedCopyWithoutSource(t *testing.T) {
	fixture := newLegacyAggregateFixture()
	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForMapping(fixture.sourceFD, fixture.destFD),
		newSourceMachineSetForMapping("source-worker-a", "dc-source", "rp-a", 1, 4),
		newDestinationMachineSetForSource(fixture.base, "source-worker-a", 1),
	})
	migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)
	migration.Status.WorkerMachineSetNames = []string{fixture.base, fixture.base + "-2"}

	result, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration)
	want := fmt.Sprintf("destination MachineSets [%s] are missing and cannot be recreated", fixture.base+"-2")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ensureWorkloadMigrated = (%v, %v), want an error containing %q", result, err, want)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("ensureWorkloadMigrated requeueAfter = %v, want no requeue on a missing recorded copy", result.RequeueAfter)
	}
	// The remedy must restore the lost replacement, not delete its recorded
	// entry: a dropped recorded name makes a later reconcile stop requiring the
	// capacity that went missing.
	if !strings.Contains(err.Error(), "restore them before retrying") {
		t.Fatalf("ensureWorkloadMigrated error = %q, want the restore instruction", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "delete") {
		t.Fatalf("ensureWorkloadMigrated error = %q, want no destructive delete instruction", err)
	}
}

// TestEnsureWorkloadMigratedRefusesOversizedDestinationNameSet covers the runtime
// cardinality guard: preflight stops counting once InfrastructurePrepared is True,
// so creation must refuse a recorded union that status.workerMachineSetNames
// cannot hold, without creating anything.
func TestEnsureWorkloadMigratedRefusesOversizedDestinationNameSet(t *testing.T) {
	fixture := newLegacyAggregateFixture()
	objects := []runtime.Object{newInfrastructureForMapping(fixture.sourceFD, fixture.destFD)}
	for i := 0; i <= maxDestinationWorkerMachineSets; i++ {
		objects = append(objects, newSourceMachineSetForMapping(fmt.Sprintf("source-worker-%03d", i), "dc-source", "rp-a", 1, 4))
	}
	reconciler := newRolloutReconciler(t, objects)
	migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{fixture.destFD}, fixture.mappings)

	_, err := reconciler.ensureWorkloadMigrated(fixture.ctx, migration)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeding the %d supported by status.workerMachineSetNames", maxDestinationWorkerMachineSets)) {
		t.Fatalf("ensureWorkloadMigrated error = %v, want the cardinality guard error", err)
	}
	assertMachineSetAbsent(t, fixture.ctx, reconciler, fixture.base)
	if len(migration.Status.WorkerMachineSetNames) != 0 {
		t.Fatalf("WorkerMachineSetNames = %v, want none recorded by a refused run", migration.Status.WorkerMachineSetNames)
	}
}

// newSourceFailureDomainForMapping builds a source failure domain hosted on
// source.example.com with the given datacenter and resource pool.
func newSourceFailureDomainForMapping(name, datacenter, resourcePool string) configv1.VSpherePlatformFailureDomainSpec {
	return configv1.VSpherePlatformFailureDomainSpec{
		Name:   name,
		Server: "source.example.com",
		Topology: configv1.VSpherePlatformTopology{
			Datacenter:   datacenter,
			ResourcePool: resourcePool,
		},
	}
}

// newTargetFailureDomain builds a destination failure domain from the migration
// spec, hosted on target.example.com with a per-name template so copied provider
// specs can be asserted against the destination template.
func newTargetFailureDomain(name string) configv1.VSpherePlatformFailureDomainSpec {
	return configv1.VSpherePlatformFailureDomainSpec{
		Name:   name,
		Server: "target.example.com",
		Topology: configv1.VSpherePlatformTopology{
			Template:       fmt.Sprintf("/dc-target/vm/template-%s", name),
			Datacenter:     "dc-target",
			Datastore:      "ds-target",
			ResourcePool:   "rp-target",
			ComputeCluster: "cl-target",
		},
	}
}

// newInfrastructureForMapping mirrors the post-destination-init Infrastructure:
// both vCenters and both source and destination failure domains are present.
func newInfrastructureForMapping(fds ...configv1.VSpherePlatformFailureDomainSpec) *configv1.Infrastructure {
	return &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
		Spec: configv1.InfrastructureSpec{
			PlatformSpec: configv1.PlatformSpec{
				Type: configv1.VSpherePlatformType,
				VSphere: &configv1.VSpherePlatformSpec{
					VCenters: []configv1.VSpherePlatformVCenterSpec{
						{Server: "source.example.com"},
						{Server: "target.example.com"},
					},
					FailureDomains: fds,
				},
			},
		},
		Status: configv1.InfrastructureStatus{
			InfrastructureName: "test-infra",
		},
	}
}

// newMappingTestMigration builds a migration whose destination failure domains
// and preflight-recorded effective mapping are set, as Reconcile would see
// them once the workload phase starts.
// TestEnsureWorkloadMigratedRecoversMappingsAfterUpgrade covers the in-flight
// operator upgrade case: preflight is already True so it never re-runs, and the
// CR predates status.failureDomainMappings. The workload phase must recover the
// mapping it would have recorded instead of dead-ending.
func TestEnsureWorkloadMigratedRecoversMappingsAfterUpgrade(t *testing.T) {
	ctx := context.Background()
	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForMapping(
			newSourceFailureDomainForMapping("source-fd-1", "dc-source-1", "rp-a"),
			newTargetFailureDomain("target-fd-1"),
		),
		newSourceMachineSetForMapping("source-worker-a1", "dc-source-1", "rp-a", 2, 4),
	})
	migration := newMappingTestMigration(
		[]configv1.VSpherePlatformFailureDomainSpec{newTargetFailureDomain("target-fd-1")},
		nil,
	)

	if _, err := reconciler.ensureWorkloadMigrated(ctx, migration); err != nil {
		t.Fatalf("ensureWorkloadMigrated: %v", err)
	}

	assertMappingMachineSet(t, ctx, reconciler, "test-infra-worker-target-fd-1", "target-fd-1", 2, 4)
	want := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}}
	if !reflect.DeepEqual(migration.Status.FailureDomainMappings, want) {
		t.Fatalf("recovered status mappings = %+v, want %+v", migration.Status.FailureDomainMappings, want)
	}
}

// TestEnsureWorkloadMigratedRefusesDestinationSourceServer covers the
// degenerate configuration where a destination failure domain reuses the
// source vCenter server: worker creation must not start, since Steps 6 and 8
// are server-keyed.
func TestEnsureWorkloadMigratedRefusesDestinationSourceServer(t *testing.T) {
	ctx := context.Background()
	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForMapping(newSourceFailureDomainForMapping("source-fd-1", "dc-source-1", "rp-a")),
		newSourceMachineSetForMapping("source-worker-a1", "dc-source-1", "rp-a", 2, 4),
	})
	dest := newTargetFailureDomain("target-fd-1")
	dest.Server = "source.example.com"
	migration := newMappingTestMigration(
		[]configv1.VSpherePlatformFailureDomainSpec{dest},
		[]migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}},
	)

	_, err := reconciler.ensureWorkloadMigrated(ctx, migration)
	if err == nil || !strings.Contains(err.Error(), "configured destination failure domain") {
		t.Fatalf("ensureWorkloadMigrated error = %v, want containing %q", err, "configured destination failure domain")
	}
	assertMachineSetAbsent(t, ctx, reconciler, "test-infra-worker-target-fd-1")
}

func newMappingTestMigration(destFDs []configv1.VSpherePlatformFailureDomainSpec, mappings []migrationv1alpha1.FailureDomainMapping) *migrationv1alpha1.VmwareCloudFoundationMigration {
	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Generation: 1},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			FailureDomains: destFDs,
		},
		Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
			FailureDomainMappings: mappings,
		},
	}
}

// newSourceMachineSetForMapping builds a source MachineSet in the given
// datacenter and resource pool with a distinct VM size (numCPUs) so tests can
// assert that each destination MachineSet copies its own source provider spec.
func newSourceMachineSetForMapping(name, datacenter, resourcePool string, replicas, numCPUs int32) *machinev1beta1.MachineSet {
	return &machinev1beta1.MachineSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: openshift.MachineAPINamespace,
		},
		Spec: machinev1beta1.MachineSetSpec{
			Replicas: &replicas,
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"machine.openshift.io/cluster-api-machineset": name,
				},
			},
			Template: machinev1beta1.MachineTemplateSpec{
				ObjectMeta: machinev1beta1.ObjectMeta{
					Labels: map[string]string{
						"machine.openshift.io/cluster-api-machineset": name,
					},
				},
				Spec: machinev1beta1.MachineSpec{
					ProviderSpec: machinev1beta1.ProviderSpec{
						Value: &runtime.RawExtension{Raw: []byte(fmt.Sprintf(
							`{"kind":"VSphereMachineProviderSpec","apiVersion":"machine.openshift.io/v1beta1","numCPUs":%d,"workspace":{"server":"source.example.com","datacenter":%q,"resourcePool":%q}}`,
							numCPUs, datacenter, resourcePool,
						))},
					},
				},
			},
		},
	}
}

func getMappingMachineSet(t *testing.T, ctx context.Context, reconciler *VmwareCloudFoundationMigrationReconciler, name string) *machinev1beta1.MachineSet {
	t.Helper()
	ms, err := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting machineset %q: %v", name, err)
	}
	return ms
}

// assertMappingMachineSet verifies a copied destination MachineSet: exact
// replicas from its source MachineSet, the source MachineSet's VM size, and
// the destination failure domain's template and vCenter.
func assertMappingMachineSet(t *testing.T, ctx context.Context, reconciler *VmwareCloudFoundationMigrationReconciler, name, fdName string, replicas, numCPUs int32) {
	t.Helper()
	ms := getMappingMachineSet(t, ctx, reconciler, name)
	if ms.Spec.Replicas == nil || *ms.Spec.Replicas != replicas {
		t.Fatalf("machineset %q replicas = %v, want %d", name, ms.Spec.Replicas, replicas)
	}
	providerSpec := mappingTestProviderSpec(t, ms)
	if providerSpec.NumCPUs != numCPUs {
		t.Fatalf("machineset %q numCPUs = %d, want %d (source provider spec must be copied)", name, providerSpec.NumCPUs, numCPUs)
	}
	wantTemplate := fmt.Sprintf("/dc-target/vm/template-%s", fdName)
	if providerSpec.Template != wantTemplate {
		t.Fatalf("machineset %q template = %q, want %q (destination failure domain template)", name, providerSpec.Template, wantTemplate)
	}
	if providerSpec.Workspace == nil || providerSpec.Workspace.Server != "target.example.com" {
		t.Fatalf("machineset %q workspace = %+v, want server target.example.com", name, providerSpec.Workspace)
	}
}

// newDestinationMachineSetForSource builds a destination MachineSet stamped with
// the source MachineSet annotation, as CreateWorkerMachineSet does.
func newDestinationMachineSetForSource(name, sourceMachineSet string, replicas int32) *machinev1beta1.MachineSet {
	ms := newTargetMachineSetForRollout(name, "target.example.com", replicas)
	ms.Annotations = map[string]string{openshift.SourceMachineSetAnnotation: sourceMachineSet}
	return ms
}

func assertMachineSetAbsent(t *testing.T, ctx context.Context, reconciler *VmwareCloudFoundationMigrationReconciler, name string) {
	t.Helper()
	if _, err := reconciler.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Fatalf("machineset %q exists, want absent", name)
	}
}

func mappingTestProviderSpec(t *testing.T, ms *machinev1beta1.MachineSet) *machinev1beta1.VSphereMachineProviderSpec {
	t.Helper()
	providerSpec := &machinev1beta1.VSphereMachineProviderSpec{}
	if err := json.Unmarshal(ms.Spec.Template.Spec.ProviderSpec.Value.Raw, providerSpec); err != nil {
		t.Fatalf("unmarshalling provider spec from machineset %q: %v", ms.Name, err)
	}
	return providerSpec
}
