package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

func TestScaleDownSourceMachineSetsVerifiesRecordedDestinations(t *testing.T) {
	for _, tt := range []struct {
		name    string
		server  string
		wantErr string
	}{
		{name: "missing suffix", wantErr: "does not exist"},
		{name: "suffix on wrong vCenter", server: "other.example.com", wantErr: "collides"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			migration := newRolloutMigration()
			migration.Spec.FailureDomains[0] = newTargetFailureDomain("target-fd-1")
			base := migration.Status.WorkerMachineSetNames[0]
			migration.Status.WorkerMachineSetNames = append(migration.Status.WorkerMachineSetNames, base+"-2")
			sources := []*machinev1beta1.MachineSet{newSourceMachineSetForRollout(1), newSourceMachineSetForRollout(1)}
			sources[1].Name = sourceMSB
			objects := []runtime.Object{
				newInfrastructureForRollout(), sources[0], sources[1],
				newDestinationMachineSetForSource(base, sources[0].Name, 1),
			}
			if tt.server != "" {
				objects = append(objects, newTargetMachineSetForRollout(base+"-2", tt.server, 1))
			}
			r := newRolloutReconciler(t, objects)
			r.Client, r.Scheme = newDestinationRecoveryClient(t, migration)
			machineMgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, nil)
			infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
			_, err := r.scaleDownSourceMachineSets(ctx, migration, machineMgr, infraMgr, sources, "source.example.com", "test-infra")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("scaleDownSourceMachineSets error = %v, want %q", err, tt.wantErr)
			}
			for _, source := range sources {
				assertMachineSetReplicas(t, ctx, r, source.Name, 1)
			}
		})
	}
}

func TestRecordedDestinationDeletedBeforeReadiness(t *testing.T) {
	for _, workflow := range []bool{false, true} {
		name := "readiness"
		if workflow {
			name = "normal workflow"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			migration := newRolloutMigration()
			migration.Spec.FailureDomains[0] = newTargetFailureDomain("target-fd-1")
			base := migration.Status.WorkerMachineSetNames[0]
			suffix := base + "-2"
			migration.Status.WorkerMachineSetNames = append(migration.Status.WorkerMachineSetNames, suffix)
			sources := []*machinev1beta1.MachineSet{newSourceMachineSetForRollout(1), newSourceMachineSetForRollout(1)}
			sources[1].Name = sourceMSB
			r := newRolloutReconciler(t, []runtime.Object{
				newInfrastructureForRollout(), newCPMSUpdatedForRollout([]string{"target-fd-1"}, true),
				sources[0], sources[1],
				newDestinationMachineSetForSource(base, sources[0].Name, 1),
				newDestinationMachineSetForSource(suffix, sources[1].Name, 1),
				newReadyWorkerMachineForRollout(base), newReadyWorkerMachineForRollout(suffix),
				newReadyNodeForRollout(rolloutReadyNodeName),
			})
			r.Client, r.Scheme = newDestinationRecoveryClient(t, migration)
			machineMgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, nil)
			if workflow {
				// Delete after Step 1's final existence lookup returns its live snapshot,
				// before readiness lists the remaining MachineSets.
				fakeClient := r.MachineClient.(*fakemachineclient.Clientset)
				gets := 0
				fakeClient.PrependReactor("get", "machinesets", func(action clienttesting.Action) (bool, runtime.Object, error) {
					if action.(clienttesting.GetAction).GetName() != suffix {
						return false, nil, nil
					}
					gets++
					if gets != 5 { // Three progress reads, one expected-name lookup, then the existence check.
						return false, nil, nil
					}
					obj, err := fakeClient.Tracker().Get(action.GetResource(), action.GetNamespace(), suffix)
					if err != nil {
						return true, nil, err
					}
					err = fakeClient.Tracker().Delete(action.GetResource(), action.GetNamespace(), suffix)
					return true, obj, err
				})
				result, err := r.ensureWorkloadMigrated(ctx, migration)
				if err != nil && !strings.Contains(err.Error(), suffix) {
					t.Fatalf("ensureWorkloadMigrated: %v", err)
				}
				assertMachineSetAbsent(t, ctx, r, suffix)
				if err == nil && result.RequeueAfter != 30*time.Second {
					t.Errorf("RequeueAfter = %v, want 30s readiness wait", result.RequeueAfter)
				}
				if err == nil {
					condition := findWorkloadCondition(t, migration)
					if condition.Status != metav1.ConditionFalse || !strings.Contains(condition.Message, "Waiting for target worker") {
						t.Errorf("WorkloadMigrated = %+v, want target worker readiness wait", condition)
					}
				}
			} else {
				exists, err := r.targetMachineSetsExist(ctx, migration, machineMgr, "test-infra", migration.Status.FailureDomainMappings,
					newInfrastructureForRollout().Spec.PlatformSpec.VSphere.FailureDomains)
				if err != nil || !exists {
					t.Fatalf("targetMachineSetsExist = %t, %v; want true", exists, err)
				}
				if err := r.MachineClient.MachineV1beta1().MachineSets(openshift.MachineAPINamespace).Delete(ctx, suffix, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				status, err := checkWorkerReadiness(ctx, machineMgr, migration.Spec.FailureDomains, "test-infra", migration.Status.WorkerMachineSetNames)
				if (err == nil && status.Ready) || (err != nil && !strings.Contains(err.Error(), suffix)) {
					t.Errorf("checkWorkerReadiness = %+v, %v; want not ready", status, err)
				}
			}
			for _, source := range sources {
				assertMachineSetReplicas(t, ctx, r, source.Name, 1)
			}
		})
	}
}
