package controller

import (
	"context"
	"fmt"
	"testing"

	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

func TestUpdateWorkloadProgressPreservesTargetsOnListFailure(t *testing.T) {
	ctx := context.Background()
	migration := newRolloutMigration()
	base := migration.Status.WorkerMachineSetNames[0]
	suffix := base + "-2"
	migration.Status.WorkerMachineSetNames = append(migration.Status.WorkerMachineSetNames, suffix)
	migration.Status.Progress = &migrationv1alpha1.MigrationProgress{
		Workers: &migrationv1alpha1.WorkerMigrationProgress{TargetMachinesTotal: 5, TargetMachinesReady: 3, TargetNodesReady: 3},
	}
	r := newRolloutReconciler(t, []runtime.Object{
		newTargetMachineSetForRollout(base, "target.example.com", 1),
		newTargetMachineSetForRollout(suffix, "target.example.com", 1),
		newReadyWorkerMachineForRollout(base), newReadyWorkerMachineForRollout(suffix),
		newReadyNodeForRollout(rolloutReadyNodeName),
	})
	machineClient := r.MachineClient.(*fakemachineclient.Clientset)
	failList := true
	machineClient.PrependReactor("list", "machinesets", func(ktesting.Action) (bool, runtime.Object, error) {
		if failList {
			return true, nil, fmt.Errorf("injected machineset list failure")
		}
		return false, nil, nil
	})
	mgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, nil)
	r.updateWorkloadProgress(ctx, migration, mgr, "test-infra", "source.example.com")
	if got := migration.Status.Progress.Workers; got.TargetMachinesTotal != 5 || got.TargetMachinesReady != 3 || got.TargetNodesReady != 3 {
		t.Errorf("incomplete observation replaced previous counters: %+v", got)
	}
	readBase := false
	for _, action := range machineClient.Actions() {
		if action.Matches("get", "machinesets") && action.(ktesting.GetAction).GetName() == base {
			readBase = true
		}
	}
	if !readBase {
		t.Fatal("regression requires successful individual base read")
	}
	failList = false
	r.updateWorkloadProgress(ctx, migration, mgr, "test-infra", "source.example.com")
	if got := migration.Status.Progress.Workers; got.TargetMachinesTotal != 2 || got.TargetMachinesReady != 2 || got.TargetNodesReady != 2 {
		t.Errorf("successful retry did not update counters: %+v", got)
	}
}
