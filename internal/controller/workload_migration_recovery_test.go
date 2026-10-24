package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

func TestPerSourceDestinationCapacityAfterSourceGrowth(t *testing.T) {
	f := newLegacyAggregateFixture()
	source := f.sourceMSList[0]
	zero := int32(0)
	source.Spec.Replicas = &zero
	r := newRolloutReconciler(t, []runtime.Object{newInfrastructureForMapping(f.sourceFD, f.destFD), source})
	mgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, r.DynamicClient)
	migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{f.destFD}, f.mappings)
	if _, err := r.ensureWorkloadMigrated(f.ctx, migration); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ScaleMachineSet(f.ctx, source.Name, 1); err != nil {
		t.Fatal(err)
	}
	r.Client = newMigrationStatusClient(t, migration)
	source, err := mgr.GetMachineSet(f.ctx, source.Name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.scaleDownSourceMachineSets(f.ctx, migration, mgr, openshift.NewInfrastructureManager(r.ConfigClient), []*machinev1beta1.MachineSet{source}, f.sourceFD.Server, f.infraID)
	if err == nil {
		t.Fatal("scale-down accepted an undersized per-source destination")
	}
	stored, err := mgr.GetMachineSet(f.ctx, source.Name)
	if err != nil || machineSetDesiredReplicas(stored) != 1 {
		t.Fatalf("source scaled down: %v, %v", stored, err)
	}
	dest, err := mgr.GetMachineSet(f.ctx, f.base)
	if err != nil || machineSetDesiredReplicas(dest) != 0 {
		t.Fatalf("destination auto-resized: %v, %v", dest, err)
	}
	if _, err := r.ensureWorkloadMigrated(f.ctx, migration); err == nil {
		t.Fatal("workload accepted undersized replacement")
	}
	if err := mgr.ScaleMachineSet(f.ctx, f.base, 1); err != nil {
		t.Fatal(err)
	}
	result, err := r.ensureWorkloadMigrated(f.ctx, migration)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("unready replacement must wait: %v, %v", result, err)
	}
	assertMachineSetReplicas(t, f.ctx, r, source.Name, 1)

}

func TestLegacyLongDestinationIdentity(t *testing.T) {
	for length := 60; length <= 63; length++ {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			f := newLegacyAggregateFixture()
			f.destFD.Name = strings.Repeat("d", length-len(f.infraID+"-worker-"))
			f.base = f.infraID + "-worker-" + f.destFD.Name
			f.mappings[0].Destination = f.destFD.Name
			aggregate := newTargetMachineSetForRollout(f.base, f.destFD.Server, 5)
			r := newRolloutReconciler(t, []runtime.Object{newInfrastructureForMapping(f.sourceFD, f.destFD), f.sourceMSList[0], f.sourceMSList[1], aggregate})
			migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{f.destFD}, nil)
			if _, err := r.ensureWorkloadMigrated(f.ctx, migration); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(migration.Status.WorkerMachineSetNames, []string{f.base}) {
				t.Fatalf("inventory = %v, want %s", migration.Status.WorkerMachineSetNames, f.base)
			}
			assertMachineSetAbsent(t, f.ctx, r, workerMachineSetName(f.infraID, f.destFD.Name))
			mgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, r.DynamicClient)
			status, err := checkWorkerReadiness(f.ctx, mgr, []configv1.VSpherePlatformFailureDomainSpec{f.destFD}, f.infraID, migration.Status.WorkerMachineSetNames)
			if err != nil || status.Ready {
				t.Fatalf("readiness must check legacy destination: %+v, %v", status, err)
			}
			for _, source := range f.sourceMSList {
				if err := mgr.DeleteMachineSet(f.ctx, source.Name); err != nil {
					t.Fatal(err)
				}
			}
			exists, err := r.targetMachineSetsExist(f.ctx, migration, mgr, f.infraID, f.mappings, []configv1.VSpherePlatformFailureDomainSpec{f.sourceFD})
			if err != nil || !exists {
				t.Fatalf("post-source-deletion inventory: %v, %v", exists, err)
			}
			if err := mgr.DeleteMachineSet(f.ctx, f.base); err != nil {
				t.Fatal(err)
			}
			if _, err := checkWorkerReadiness(f.ctx, mgr, []configv1.VSpherePlatformFailureDomainSpec{f.destFD}, f.infraID, migration.Status.WorkerMachineSetNames); err == nil || !strings.Contains(err.Error(), f.base) {
				t.Fatalf("readiness lost missing legacy identity: %v", err)
			}

			exists, err = r.targetMachineSetsExist(f.ctx, migration, mgr, f.infraID, f.mappings, []configv1.VSpherePlatformFailureDomainSpec{f.sourceFD})
			if err == nil && exists {
				t.Fatal("missing recorded legacy destination accepted")
			}
		})
	}
}

func TestRecoverMappingsWithRecordedShortenedDestination(t *testing.T) {
	for _, length := range []int{60, 61, 62, 63, 64, 80} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			f := newLegacyAggregateFixture()
			f.destFD.Name = strings.Repeat("d", length-len(f.infraID+"-worker-"))
			legacyName := f.infraID + "-worker-" + f.destFD.Name
			recordedName := legacyName[:59]
			f.mappings[0].Destination = f.destFD.Name
			destination := newTargetMachineSetForRollout(recordedName, f.destFD.Server, 5)
			r := newRolloutReconciler(t, []runtime.Object{newInfrastructureForMapping(f.sourceFD, f.destFD), destination})
			migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{f.destFD}, nil)
			migration.Status.Conditions = []metav1.Condition{{Type: migrationv1alpha1.ConditionInfrastructurePrepared, Status: metav1.ConditionTrue}}
			migration.Status.WorkerMachineSetNames = []string{recordedName}

			for i := 0; i < 2; i++ {
				result, err := r.ensureWorkloadMigrated(f.ctx, migration)
				if err != nil || result.RequeueAfter == 0 {
					t.Fatalf("ensureWorkloadMigrated must wait for the recorded replacement: %v, %v", result, err)
				}
				if !slices.Equal(migration.Status.WorkerMachineSetNames, []string{recordedName}) {
					t.Fatalf("inventory = %v, want only %s", migration.Status.WorkerMachineSetNames, recordedName)
				}
				if !slices.Equal(migration.Status.FailureDomainMappings, f.mappings) {
					t.Fatalf("recovered mappings = %v, want %v", migration.Status.FailureDomainMappings, f.mappings)
				}
				assertMachineSetAbsent(t, f.ctx, r, legacyName)
				assertMachineSetReplicas(t, f.ctx, r, recordedName, 5)
			}
		})
	}
}

func TestRecoverMappingsHonorsRecordedInventoryPerMapping(t *testing.T) {
	f := newLegacyAggregateFixture()
	f.destFD.Name = strings.Repeat("d", 60-len(f.infraID+"-worker-"))
	recordedName := (f.infraID + "-worker-" + f.destFD.Name)[:59]
	otherSource := newSourceFailureDomainForMapping("source-fd-2", "dc-source", "rp-b")
	otherDestination := newTargetFailureDomain("target-fd-2")
	missingName := "test-infra-worker-target-fd-2"
	destination := newTargetMachineSetForRollout(recordedName, f.destFD.Server, 5)
	r := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForMapping(f.sourceFD, otherSource, f.destFD, otherDestination), destination,
	})
	migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{f.destFD, otherDestination}, nil)
	migration.Spec.FailureDomainMappings = []migrationv1alpha1.FailureDomainMapping{
		{Source: f.sourceFD.Name, Destination: f.destFD.Name},
		{Source: otherSource.Name, Destination: otherDestination.Name},
	}
	migration.Status.Conditions = []metav1.Condition{{Type: migrationv1alpha1.ConditionInfrastructurePrepared, Status: metav1.ConditionTrue}}
	migration.Status.WorkerMachineSetNames = []string{recordedName}

	for i := 0; i < 2; i++ {
		_, err := r.ensureWorkloadMigrated(f.ctx, migration)
		if err == nil || !strings.Contains(err.Error(), missingName) {
			t.Fatalf("ensureWorkloadMigrated must reject the unrecorded missing destination %s: %v", missingName, err)
		}
		if want := []string{recordedName, missingName}; !slices.Equal(migration.Status.WorkerMachineSetNames, want) {
			t.Fatalf("inventory = %v, want %v", migration.Status.WorkerMachineSetNames, want)
		}
		assertMachineSetAbsent(t, f.ctx, r, missingName)
		assertMachineSetReplicas(t, f.ctx, r, recordedName, 5)
	}
}

func TestLegacyMissingDestinationAfterSourceDeletion(t *testing.T) {
	for _, length := range []int{30, 63} {
		for _, recordedMapping := range []bool{false, true} {
			t.Run(fmt.Sprintf("length-%d/recorded-%t", length, recordedMapping), func(t *testing.T) {
				f := newLegacyAggregateFixture()
				f.destFD.Name = strings.Repeat("d", length-len(f.infraID+"-worker-"))
				f.base = f.infraID + "-worker-" + f.destFD.Name
				f.mappings[0].Destination = f.destFD.Name
				r := newRolloutReconciler(t, []runtime.Object{newInfrastructureForMapping(f.sourceFD, f.destFD)})
				migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{f.destFD}, nil)
				migration.Status.Conditions = []metav1.Condition{{Type: migrationv1alpha1.ConditionInfrastructurePrepared, Status: metav1.ConditionTrue}}
				if recordedMapping {
					migration.Status.FailureDomainMappings = f.mappings
				}
				mgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, r.DynamicClient)
				for i := 0; i < 2; i++ {
					_, err := r.ensureWorkloadMigrated(context.Background(), migration)
					if !recordedMapping {
						if err == nil {
							t.Fatal("legacy migration with missing destination did not fail closed")
						}
						if !slices.Contains(migration.Status.WorkerMachineSetNames, f.base) {
							t.Fatalf("legacy expectation lost: %v", migration.Status.WorkerMachineSetNames)
						}
					} else {
						if len(migration.Status.WorkerMachineSetNames) != 0 {
							t.Fatalf("originally empty mapping acquired expectations: %v", migration.Status.WorkerMachineSetNames)
						}
						exists, err := r.targetMachineSetsExist(f.ctx, migration, mgr, f.infraID, f.mappings, []configv1.VSpherePlatformFailureDomainSpec{f.sourceFD})
						if err != nil || !exists {
							t.Fatalf("originally empty mapping blocked: %t, %v", exists, err)
						}
						fds, err := workerReadinessFailureDomains(f.ctx, mgr, f.infraID, f.mappings, []configv1.VSpherePlatformFailureDomainSpec{f.sourceFD}, migration.Spec.FailureDomains, nil)
						if err != nil || len(fds) != 0 {
							t.Fatalf("originally empty mapping readiness: %v, %v", fds, err)
						}
					}
				}
			})
		}
	}
}
