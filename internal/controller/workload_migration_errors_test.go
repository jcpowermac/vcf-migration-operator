package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	machineconfigurationv1 "github.com/openshift/api/machineconfiguration/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	machineconfigfake "github.com/openshift/client-go/machineconfiguration/clientset/versioned/fake"
	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newCloudProviderConfigCM builds a minimal valid cloud-provider-config ConfigMap.
func newCloudProviderConfigCM() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: openshift.CloudProviderConfigMapNamespace, Name: openshift.CloudProviderConfigMapName},
		Data:       map[string]string{"config": "global: {}"},
	}
}

// errWorkloadFault is the sentinel error injected by fault reactors in these tests.
var errWorkloadFault = errors.New("workload migration test fault")

// Documented coverage residuals for the workload-migration step (all defensive
// single-statement error returns that need a fault on top of an operation that
// already succeeded, or a fault that the preceding check makes unreachable):
//   - ensureWorkloadMigrated: the create/annotate manager-fault returns (813,
//     841, 846) and the source-failure-domain-not-recorded return (784, 795)
//     fire only when the recorded mapping names a failure domain the live
//     infrastructure no longer carries.
//   - ensureWorkloadMigratedRolloutAndScaleDown: the Step-8 destination refuse
//     (1039) is unreachable because scaleDownSourceMachineSets rejects the same
//     destination server first; oldWorkerStallDetail (1024) and the post-delete
//     list (1052) need a fault layered on an already-successful step.
//   - mergeCompletionRecord: completionSpecHash's json.Marshal error (1684) is
//     defensive for plain serialisable spec fields.

// wmErrEnv wires every client the workload-migration step touches and returns
// handles for reactor-based fault injection. It mirrors newRolloutReconciler but
// also sets the MachineConfig, Dynamic and embedded status clients so the
// ensureCPMSUpdated / ensureSourceCleaned / persistSourceVCenter paths can run.
type wmErrEnv struct {
	t          *testing.T
	reconciler *VmwareCloudFoundationMigrationReconciler
	kube       *fakekube.Clientset
	config     *configfake.Clientset
	machine    *fakemachineclient.Clientset
	machineCfg *machineconfigfake.Clientset
	dynamic    *dynamicfake.FakeDynamicClient
	recorder   *events.FakeRecorder
	migration  *migrationv1alpha1.VmwareCloudFoundationMigration
}

func newWMErrEnv(t *testing.T, migration *migrationv1alpha1.VmwareCloudFoundationMigration, objects ...runtime.Object) *wmErrEnv {
	t.Helper()
	var kubeObjs, configObjs, machineObjs, poolObjs []runtime.Object
	for _, o := range objects {
		switch o.(type) {
		case *configv1.Infrastructure:
			configObjs = append(configObjs, o)
		case *machinev1.ControlPlaneMachineSet, *machinev1beta1.MachineSet, *machinev1beta1.Machine:
			machineObjs = append(machineObjs, o)
		case *corev1.Node, *corev1.Secret, *corev1.ConfigMap:
			kubeObjs = append(kubeObjs, o)
		case *machineconfigurationv1.MachineConfigPool:
			poolObjs = append(poolObjs, o)
		default:
			t.Fatalf("newWMErrEnv: unsupported object type %T", o)
		}
	}
	env := &wmErrEnv{
		t:          t,
		kube:       fakekube.NewClientset(kubeObjs...),
		config:     configfake.NewClientset(configObjs...),
		machine:    fakemachineclient.NewClientset(machineObjs...),
		machineCfg: machineconfigfake.NewClientset(poolObjs...),
		dynamic:    dynamicfake.NewSimpleDynamicClient(newMigrationTestScheme(t)),
		recorder:   events.NewFakeRecorder(50),
		migration:  migration,
	}
	env.reconciler = &VmwareCloudFoundationMigrationReconciler{
		KubeClient:          env.kube,
		ConfigClient:        env.config,
		MachineClient:       env.machine,
		MachineConfigClient: env.machineCfg,
		DynamicClient:       env.dynamic,
		Recorder:            env.recorder,
		Client: clientfake.NewClientBuilder().
			WithScheme(newMigrationTestScheme(t)).
			WithObjects(migration).
			WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
			Build(),
	}
	return env
}

func (e *wmErrEnv) machineMgr() *openshift.MachineManager {
	return openshift.NewMachineManager(e.kube, e.machine, e.dynamic)
}

func (e *wmErrEnv) assertErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
		return
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want substring %q", err.Error(), want)
	}
}

func (e *wmErrEnv) sawEvent(substr string) bool {
	e.t.Helper()
	// Non-blocking drain: the FakeRecorder channel is never closed, so ranging
	// over it would hang when the expected event is absent (see drainRecorder).
	for {
		select {
		case ev := <-e.recorder.Events:
			if strings.Contains(ev, substr) {
				return true
			}
		default:
			return false
		}
	}
}

// wmMigrationWithMappings builds a migration with the given spec and status
// failure-domain mappings, ready for the workload-migration step.
func wmMigrationWithMappings(specMappings, statusMappings []migrationv1alpha1.FailureDomainMapping) *migrationv1alpha1.VmwareCloudFoundationMigration {
	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       migrationv1alpha1.SingletonName,
			Namespace:  migrationv1alpha1.SingletonNamespace,
			Generation: 1,
			UID:        types.UID("uid-wm"),
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "target-fd-1", Server: "target.example.com"},
			},
			FailureDomainMappings: specMappings,
		},
		Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
			FailureDomainMappings: statusMappings,
			WorkerMachineSetNames: []string{workerMachineSetName("test-infra", "target-fd-1")},
		},
	}
}

// TestCheckMappingForReconcileDrift covers the drift branch of
// checkMappingForReconcile: when the spec mappings differ from the recorded
// status mappings, ensureMappingImmutable fails, the Accepted condition is
// rejected and a warning fires, and the workflow short-circuits.
func TestCheckMappingForReconcileDrift(t *testing.T) {
	ctx := context.Background()
	migration := wmMigrationWithMappings(
		[]migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-2"}},
		[]migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}},
	)
	// Mappings are only immutable once the migration has started.
	startTime := metav1.Now()
	migration.Status.StartTime = &startTime
	env := newWMErrEnv(t, migration)

	shortCircuit, err := env.reconciler.checkMappingForReconcile(ctx, migration, migration.Status)
	if err != nil {
		t.Fatalf("checkMappingForReconcile: %v", err)
	}
	if !shortCircuit {
		t.Fatalf("expected the workflow to short-circuit on mapping drift")
	}
	cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionAccepted)
	if cond == nil || cond.Reason != migrationv1alpha1.ReasonMappingRejected || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected a False MappingRejected Accepted condition, got %+v", cond)
	}
	if !env.sawEvent(string(migrationv1alpha1.ReasonMappingRejected)) {
		t.Fatalf("expected a MappingRejected warning event")
	}
}

// TestCheckMappingForReconcileClean covers the no-drift path that clears a stale
// MappingRejected condition once the mapping matches again.
func TestCheckMappingForReconcileClean(t *testing.T) {
	ctx := context.Background()
	mapping := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}}
	migration := wmMigrationWithMappings(mapping, mapping)
	migration.Status.Conditions = []metav1.Condition{{
		Type:   migrationv1alpha1.ConditionAccepted,
		Status: metav1.ConditionFalse,
		Reason: migrationv1alpha1.ReasonMappingRejected,
	}}
	env := newWMErrEnv(t, migration)

	shortCircuit, err := env.reconciler.checkMappingForReconcile(ctx, migration, migration.Status)
	if err != nil {
		t.Fatalf("checkMappingForReconcile: %v", err)
	}
	if shortCircuit {
		t.Fatalf("expected no short-circuit when the mapping is clean")
	}
	if cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionAccepted); cond != nil {
		t.Fatalf("expected the stale MappingRejected condition to be removed, got %+v", cond)
	}
}

// TestOldMachinesAndNodesDeletedErrors covers the error and still-present
// branches of oldMachinesAndNodesDeleted.
func TestOldMachinesAndNodesDeletedErrors(t *testing.T) {
	ctx := context.Background()
	log := klog.Background()
	ms := newSourceMachineSetForRollout(2)

	t.Run("machine list error", func(t *testing.T) {
		env := newWMErrEnv(t, newRolloutMigration(), ms)
		env.machine.PrependReactor("list", "machines", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		deleted, err := oldMachinesAndNodesDeleted(ctx, log, env.machineMgr(), []*machinev1beta1.MachineSet{ms})
		if deleted || !errors.Is(err, errWorkloadFault) {
			t.Fatalf("deleted=%v err=%v, want false and machine list fault", deleted, err)
		}
	})

	t.Run("node list error after machines gone", func(t *testing.T) {
		env := newWMErrEnv(t, newRolloutMigration(), ms)
		calls := 0
		env.machine.PrependReactor("list", "machines", func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			if calls == 1 {
				// CheckMachinesDeleted sees no machines: all deleted.
				return true, &machinev1beta1.MachineList{}, nil
			}
			// CheckNodesDeletedForMachines faults on its own list.
			return true, nil, errWorkloadFault
		})
		deleted, err := oldMachinesAndNodesDeleted(ctx, log, env.machineMgr(), []*machinev1beta1.MachineSet{ms})
		if deleted || !errors.Is(err, errWorkloadFault) {
			t.Fatalf("deleted=%v err=%v, want false and node list fault", deleted, err)
		}
	})

	t.Run("nodes still present", func(t *testing.T) {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "stale-node"}}
		env := newWMErrEnv(t, newRolloutMigration(), ms, node)
		calls := 0
		env.machine.PrependReactor("list", "machines", func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			if calls == 1 {
				// CheckMachinesDeleted: no machines, so all deleted.
				return true, &machinev1beta1.MachineList{}, nil
			}
			// CheckNodesDeletedForMachines: a machine whose node still exists.
			// It carries the MachineSet label so the label-scoped list keeps it.
			return true, &machinev1beta1.MachineList{Items: []machinev1beta1.Machine{{
				ObjectMeta: metav1.ObjectMeta{Name: "old-machine",
					Labels: map[string]string{"machine.openshift.io/cluster-api-machineset": "source-worker-a"}},
				Status: machinev1beta1.MachineStatus{NodeRef: &corev1.ObjectReference{Name: "stale-node"}},
			}}}, nil
		})
		deleted, err := oldMachinesAndNodesDeleted(ctx, log, env.machineMgr(), []*machinev1beta1.MachineSet{ms})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if deleted {
			t.Fatalf("deleted=true, want false while a node for the old machine remains")
		}
	})
}

// wmConvergedPool builds a MachineConfigPool whose Updated condition is set so
// CheckPoolsConverged reports the pool as converged.
func wmConvergedPool(name string) *machineconfigurationv1.MachineConfigPool {
	return &machineconfigurationv1.MachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: machineconfigurationv1.MachineConfigPoolStatus{
			Conditions: []machineconfigurationv1.MachineConfigPoolCondition{{
				Type:   machineconfigurationv1.MachineConfigPoolUpdated,
				Status: corev1.ConditionTrue,
			}},
			MachineCount:        3,
			UpdatedMachineCount: 3,
		},
	}
}

// wmStalePool builds a pool that has not fully updated, so CheckPoolsConverged
// reports it not converged.
func wmStalePool(name string) *machineconfigurationv1.MachineConfigPool {
	p := wmConvergedPool(name)
	p.Status.UpdatedMachineCount = 1
	p.Status.Conditions[0].Status = corev1.ConditionFalse
	return p
}

// TestEnsureCPMSUpdatedErrors covers the error and not-converged branches of
// ensureCPMSUpdated.
func TestEnsureCPMSUpdatedErrors(t *testing.T) {
	ctx := context.Background()
	targetFDs := []string{"target-fd-1"}

	newMigration := func() *migrationv1alpha1.VmwareCloudFoundationMigration {
		return wmMigrationWithMappings(
			[]migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}},
			[]migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}},
		)
	}

	t.Run("pool list error", func(t *testing.T) {
		migration := newMigration()
		env := newWMErrEnv(t, migration, newCPMSForRollout(true, true))
		env.machineCfg.PrependReactor("list", "machineconfigpools", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureCPMSUpdated(ctx, migration, env.machineMgr(), targetFDs)
		env.assertErr(t, err, "pool")
	})

	t.Run("pool not converged", func(t *testing.T) {
		migration := newMigration()
		env := newWMErrEnv(t, migration, newCPMSForRollout(true, true), wmStalePool("worker"))
		result, err := env.reconciler.ensureCPMSUpdated(ctx, migration, env.machineMgr(), targetFDs)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.RequeueAfter == 0 {
			t.Fatalf("expected a requeue while the pool is not converged")
		}
	})

	t.Run("update cpms failure domain error", func(t *testing.T) {
		migration := newMigration()
		env := newWMErrEnv(t, migration, newCPMSForRollout(true, true), wmConvergedPool("master"), wmConvergedPool("worker"))
		env.machine.PrependReactor("update", "controlplanemachinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureCPMSUpdated(ctx, migration, env.machineMgr(), targetFDs)
		env.assertErr(t, err, errWorkloadFault.Error())
	})

	t.Run("cpms generation observed error", func(t *testing.T) {
		migration := newMigration()
		env := newWMErrEnv(t, migration, newCPMSForRollout(true, true), wmConvergedPool("master"), wmConvergedPool("worker"))
		env.machine.PrependReactor("get", "controlplanemachinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureCPMSUpdated(ctx, migration, env.machineMgr(), targetFDs)
		env.assertErr(t, err, errWorkloadFault.Error())
	})
}

// TestPersistSourceVCenterGaps covers the rebind refusal and supersede-skip
// branches of persistSourceVCenter.
func TestPersistSourceVCenterGaps(t *testing.T) {
	ctx := context.Background()

	t.Run("refuses to rebind a recorded source", func(t *testing.T) {
		migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
			ObjectMeta: metav1.ObjectMeta{
				Name: migrationv1alpha1.SingletonName, Namespace: migrationv1alpha1.SingletonNamespace,
				UID: types.UID("uid-wm"), Generation: 1,
			},
			Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{SourceVCenter: "original.example.com"},
		}
		env := newWMErrEnv(t, migration)
		err := env.reconciler.persistSourceVCenter(ctx, migration, "different.example.com")
		env.assertErr(t, err, "refusing to rebind")
	})

	t.Run("skips a superseded migration object", func(t *testing.T) {
		migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
			ObjectMeta: metav1.ObjectMeta{
				Name: migrationv1alpha1.SingletonName, Namespace: migrationv1alpha1.SingletonNamespace,
				UID: types.UID("uid-old"), Generation: 1,
			},
		}
		// The persisted object carries a different UID: the write is a no-op.
		env := newWMErrEnv(t, migration)
		latest := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := env.reconciler.Get(ctx, client.ObjectKeyFromObject(migration), latest); err != nil {
			t.Fatalf("getting migration: %v", err)
		}
		latest.UID = "uid-new"
		if err := env.reconciler.Update(ctx, latest); err != nil {
			t.Fatalf("updating migration: %v", err)
		}
		if err := env.reconciler.persistSourceVCenter(ctx, migration, "src.example.com"); err != nil {
			t.Fatalf("persistSourceVCenter: %v", err)
		}
		if migration.Status.SourceVCenter != "" {
			t.Fatalf("source was recorded on a superseded object: %q", migration.Status.SourceVCenter)
		}
	})
}

// wmCompletedMigration builds a migration and a latest copy whose generation and
// spec match and whose completion is recorded, so mergeCompletionRecord reaches
// the merge path.
func wmCompletedMigration() (*migrationv1alpha1.VmwareCloudFoundationMigration, *migrationv1alpha1.VmwareCloudFoundationMigration) {
	now := metav1.Now()
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Generation: 3},
		Spec:       migrationv1alpha1.VmwareCloudFoundationMigrationSpec{State: migrationv1alpha1.MigrationStateRunning},
		Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
			CompletionTime: &now,
		},
	}
	latest := migration.DeepCopy()
	latest.Status.CompletionTime = nil // latest has not yet recorded the completion
	return migration, latest
}

// TestMergeCompletionRecordMerge covers the merge path of mergeCompletionRecord:
// a ready, same-generation migration whose completion is not yet on latest.
func TestMergeCompletionRecordMerge(t *testing.T) {
	ready := &metav1.Condition{
		Type:               migrationv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 3,
	}

	t.Run("merges unrecorded completion", func(t *testing.T) {
		migration, latest := wmCompletedMigration()
		merged, err := mergeCompletionRecord(migration, latest, ready)
		if err != nil {
			t.Fatalf("mergeCompletionRecord: %v", err)
		}
		if !merged {
			t.Fatal("expected the completion record to merge")
		}
		if latest.Status.CompletionTime == nil || latest.Status.CompletionSpecHash == "" {
			t.Fatalf("expected latest to record the completion time and spec hash, got %+v", latest.Status)
		}
	})

	t.Run("no-op when already complete", func(t *testing.T) {
		migration, latest := wmCompletedMigration()
		hash, err := completionSpecHash(migration.Spec)
		if err != nil {
			t.Fatalf("completionSpecHash: %v", err)
		}
		now := metav1.Now()
		latest.Status.CompletionTime = &now
		latest.Status.CompletionSpecHash = hash
		merged, err := mergeCompletionRecord(migration, latest, ready)
		if err != nil {
			t.Fatalf("mergeCompletionRecord: %v", err)
		}
		if merged {
			t.Fatal("expected no merge when latest already records the completion")
		}
	})
}

// TestResolveWorkerMachineSetNameErrors covers the legacy GetMachineSet error
// branch of resolveWorkerMachineSetName.
func TestResolveWorkerMachineSetNameErrors(t *testing.T) {
	ctx := context.Background()
	env := newWMErrEnv(t, newRolloutMigration())
	env.machine.PrependReactor("get", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errWorkloadFault
	})
	// A long failure-domain name keeps the legacy aggregate name under the 63
	// char limit while the generated base is truncated, so the two differ and
	// the function consults the legacy name.
	fdName := strings.Repeat("a", 44)
	_, err := resolveWorkerMachineSetName(ctx, env.machineMgr(), "test-infra", fdName, nil)
	if !errors.Is(err, errWorkloadFault) {
		t.Fatalf("err=%v, want legacy machineset fault", err)
	}
}

// TestTargetMachineSetsExistErrors covers the destination failure-domain and
// name-resolution error branches of targetMachineSetsExist.
func TestTargetMachineSetsExistErrors(t *testing.T) {
	ctx := context.Background()
	mapping := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}}
	infraFDs := []configv1.VSpherePlatformFailureDomainSpec{newSourceFailureDomainForRollout()}

	t.Run("destination failure domain missing", func(t *testing.T) {
		// A mapping whose destination is not among the spec failure domains.
		migration := wmMigrationWithMappings(mapping, mapping)
		migration.Spec.FailureDomains = []configv1.VSpherePlatformFailureDomainSpec{{Name: "other-fd", Server: "target.example.com"}}
		env := newWMErrEnv(t, migration)
		exists, err := env.reconciler.targetMachineSetsExist(ctx, migration, env.machineMgr(), "test-infra", mapping, infraFDs)
		if exists {
			t.Fatalf("expected the target check to fail")
		}
		env.assertErr(t, err, "resolving destination failure domain")
	})

	t.Run("name resolution error", func(t *testing.T) {
		migration := wmMigrationWithMappings(mapping, mapping)
		env := newWMErrEnv(t, migration)
		env.machine.PrependReactor("get", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.targetMachineSetsExist(ctx, migration, env.machineMgr(), "test-infra", mapping, infraFDs)
		env.assertErr(t, err, errWorkloadFault.Error())
	})
}

// TestPlanMappedWorkerMachineSetsErrors covers the resolution error branches of
// planMappedWorkerMachineSets.
func TestPlanMappedWorkerMachineSetsErrors(t *testing.T) {
	ctx := context.Background()
	mapping := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}}
	destFDs := []configv1.VSpherePlatformFailureDomainSpec{{Name: "target-fd-1", Server: "target.example.com"}}
	infraFDs := []configv1.VSpherePlatformFailureDomainSpec{newSourceFailureDomainForRollout()}

	t.Run("destination failure domain missing", func(t *testing.T) {
		env := newWMErrEnv(t, newRolloutMigration())
		_, err := planMappedWorkerMachineSets(ctx, env.machineMgr(), "test-infra", mapping, []configv1.VSpherePlatformFailureDomainSpec{}, infraFDs, []string{})
		env.assertErr(t, err, "resolving destination failure domain")
	})

	t.Run("source failure domain missing", func(t *testing.T) {
		env := newWMErrEnv(t, newRolloutMigration())
		_, err := planMappedWorkerMachineSets(ctx, env.machineMgr(), "test-infra", mapping, destFDs, []configv1.VSpherePlatformFailureDomainSpec{}, []string{})
		env.assertErr(t, err, "resolving source failure domain")
	})

	t.Run("source machineset list error", func(t *testing.T) {
		env := newWMErrEnv(t, newRolloutMigration())
		env.machine.PrependReactor("list", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := planMappedWorkerMachineSets(ctx, env.machineMgr(), "test-infra", mapping, destFDs, infraFDs, []string{})
		env.assertErr(t, err, errWorkloadFault.Error())
	})
}

// TestWorkerReadinessFailureDomainsErrors covers the error branches of
// workerReadinessFailureDomains.
func TestWorkerReadinessFailureDomainsErrors(t *testing.T) {
	ctx := context.Background()
	mapping := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}}
	destFDs := []configv1.VSpherePlatformFailureDomainSpec{{Name: "target-fd-1", Server: "target.example.com"}}
	sourceFDs := []configv1.VSpherePlatformFailureDomainSpec{newSourceFailureDomainForRollout()}

	t.Run("machineset list error", func(t *testing.T) {
		env := newWMErrEnv(t, newRolloutMigration())
		env.machine.PrependReactor("list", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := workerReadinessFailureDomains(ctx, env.machineMgr(), "test-infra", mapping, sourceFDs, destFDs, []string{})
		env.assertErr(t, err, "listing worker machinesets")
	})

	t.Run("destination failure domain missing", func(t *testing.T) {
		env := newWMErrEnv(t, newRolloutMigration())
		_, err := workerReadinessFailureDomains(ctx, env.machineMgr(), "test-infra", mapping, sourceFDs, []configv1.VSpherePlatformFailureDomainSpec{}, []string{})
		env.assertErr(t, err, "resolving destination failure domain")
	})

	t.Run("source machineset list error", func(t *testing.T) {
		env := newWMErrEnv(t, newRolloutMigration())
		// The readiness list (first) succeeds; the per-source failure-domain
		// list (second) faults.
		calls := 0
		env.machine.PrependReactor("list", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			if calls == 1 {
				return true, &machinev1beta1.MachineSetList{}, nil
			}
			return true, nil, errWorkloadFault
		})
		_, err := workerReadinessFailureDomains(ctx, env.machineMgr(), "test-infra", mapping, sourceFDs, destFDs, []string{})
		env.assertErr(t, err, "listing source machinesets")
	})
}

// TestUpdateWorkloadProgressGaps covers the progress-init and name-resolution
// branches of updateWorkloadProgress.
func TestUpdateWorkloadProgressGaps(t *testing.T) {
	ctx := context.Background()

	t.Run("initialises a nil worker progress block", func(t *testing.T) {
		migration := newRolloutMigration()
		migration.Status.Progress = &migrationv1alpha1.MigrationProgress{
			ControlPlane: &migrationv1alpha1.ControlPlaneProgress{},
			// Workers left nil: updateWorkloadProgress must initialise it.
		}
		env := newWMErrEnv(t, migration, newInfrastructureForRollout(), newCPMSForRollout(true, true))
		env.reconciler.updateWorkloadProgress(ctx, migration, env.machineMgr(), "test-infra", "source.example.com")
		if migration.Status.Progress == nil || migration.Status.Progress.Workers == nil {
			t.Fatalf("expected updateWorkloadProgress to initialise the worker progress block")
		}
	})

	t.Run("tolerates a machineset resolution fault", func(t *testing.T) {
		migration := newRolloutMigration()
		env := newWMErrEnv(t, migration, newInfrastructureForRollout(), newCPMSForRollout(true, true))
		env.machine.PrependReactor("get", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		// No fatal error: progress reporting degrades to hasTargetErrors.
		env.reconciler.updateWorkloadProgress(ctx, migration, env.machineMgr(), "test-infra", "source.example.com")
		if migration.Status.Progress == nil {
			t.Fatalf("expected progress to be populated despite the fault")
		}
	})
}

// TestEnsureWorkloadMigratedErrors covers the early validation error branches of
// ensureWorkloadMigrated.
func TestEnsureWorkloadMigratedErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("empty failure domains", func(t *testing.T) {
		migration := wmMigrationWithMappings(nil, []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}})
		migration.Spec.FailureDomains = nil
		env := newWMErrEnv(t, migration, newInfrastructureForRollout())
		_, err := env.reconciler.ensureWorkloadMigrated(ctx, migration)
		env.assertErr(t, err, "spec.failureDomains must not be empty")
	})

	t.Run("missing template", func(t *testing.T) {
		migration := wmMigrationWithMappings(nil, []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}})
		migration.Spec.FailureDomains = []configv1.VSpherePlatformFailureDomainSpec{{Name: "target-fd-1", Server: "target.example.com"}}
		env := newWMErrEnv(t, migration, newInfrastructureForRollout())
		_, err := env.reconciler.ensureWorkloadMigrated(ctx, migration)
		env.assertErr(t, err, "topology.template is required")
	})

	t.Run("infrastructure get error", func(t *testing.T) {
		migration := wmMigrationWithMappings(nil, []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}})
		migration.Spec.FailureDomains = []configv1.VSpherePlatformFailureDomainSpec{
			{Name: "target-fd-1", Server: "target.example.com", Topology: configv1.VSpherePlatformTopology{Template: folderTemplate}},
		}
		env := newWMErrEnv(t, migration)
		env.config.PrependReactor("get", "infrastructures", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureWorkloadMigrated(ctx, migration)
		env.assertErr(t, err, "getting infrastructure")
	})

	t.Run("infrastructure has no name", func(t *testing.T) {
		migration := wmMigrationWithMappings(nil, []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}})
		migration.Spec.FailureDomains = []configv1.VSpherePlatformFailureDomainSpec{
			{Name: "target-fd-1", Server: "target.example.com", Topology: configv1.VSpherePlatformTopology{Template: folderTemplate}},
		}
		infra := newInfrastructureForRollout()
		infra.Status.InfrastructureName = ""
		env := newWMErrEnv(t, migration, infra)
		_, err := env.reconciler.ensureWorkloadMigrated(ctx, migration)
		env.assertErr(t, err, "no InfrastructureName")
	})
}

// TestEnsureWorkloadMigratedSeedErrors covers the mapping-recovery error
// branches of ensureWorkloadMigrated that run when no mapping is recorded in
// status.
func TestEnsureWorkloadMigratedSeedErrors(t *testing.T) {
	ctx := context.Background()

	// specMappings drives the seed path; status mappings are empty.
	migration := wmMigrationWithMappings(
		[]migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}},
		nil,
	)
	migration.Spec.FailureDomains = []configv1.VSpherePlatformFailureDomainSpec{
		{Name: "target-fd-1", Server: "target.example.com", Topology: configv1.VSpherePlatformTopology{Template: folderTemplate}},
	}

	t.Run("seeded mapping references an unknown source", func(t *testing.T) {
		// A mapping whose source is not an infrastructure failure domain fails
		// the mapping recovery before any side effect.
		migration.Spec.FailureDomainMappings = []migrationv1alpha1.FailureDomainMapping{{Source: "ghost-fd", Destination: "target-fd-1"}}
		env := newWMErrEnv(t, migration, newInfrastructureForRollout())
		_, err := env.reconciler.ensureWorkloadMigrated(ctx, migration)
		env.assertErr(t, err, "recovering failure domain mappings")
	})

	t.Run("recovering source machineset list error", func(t *testing.T) {
		migration.Spec.FailureDomainMappings = []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}}
		env := newWMErrEnv(t, migration, newInfrastructureForRollout())
		env.machine.PrependReactor("list", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureWorkloadMigrated(ctx, migration)
		env.assertErr(t, err, errWorkloadFault.Error())
	})
}

// rolloutWorkerMachineSet builds the destination worker MachineSet named by
// newRolloutMigration's recorded status, so the pre-scale-down name recovery
// finds an existing replacement.
func rolloutWorkerMachineSet() *machinev1beta1.MachineSet {
	return newTargetMachineSetForRollout(workerMachineSetName("test-infra", "target-fd-1"), "target.example.com", 2)
}

// TestRolloutAndScaleDownErrors covers the error branches of
// ensureWorkloadMigratedRolloutAndScaleDown.
func TestRolloutAndScaleDownErrors(t *testing.T) {
	ctx := context.Background()

	readyMigration := newRolloutMigration()

	t.Run("infrastructure get error", func(t *testing.T) {
		env := newWMErrEnv(t, readyMigration)
		env.config.PrependReactor("get", "infrastructures", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		env.assertErr(t, err, "getting infrastructure")
	})

	t.Run("source vCenter cannot be resolved", func(t *testing.T) {
		migration := newRolloutMigration()
		migration.Status.FailureDomainMappings = nil // no recorded mapping
		env := newWMErrEnv(t, migration, newInfrastructureForRollout())
		_, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, migration)
		env.assertErr(t, err, "no failure domain mappings recorded")
	})

	t.Run("infrastructure id error", func(t *testing.T) {
		infra := newInfrastructureForRollout()
		infra.Status.InfrastructureName = ""
		env := newWMErrEnv(t, readyMigration, infra, newCPMSForRollout(true, true))
		_, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		env.assertErr(t, err, "InfrastructureName")
	})

	t.Run("cpms generation observed error", func(t *testing.T) {
		env := newWMErrEnv(t, readyMigration, newInfrastructureForRollout(), newCPMSForRollout(true, true))
		env.machine.PrependReactor("get", "controlplanemachinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		env.assertErr(t, err, "checking CPMS generation")
	})

	t.Run("old machinesets list error", func(t *testing.T) {
		env := newWMErrEnv(t, readyMigration, newInfrastructureForRollout(), newCPMSForRollout(true, true))
		env.machine.PrependReactor("list", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		env.assertErr(t, err, "getting old MachineSets")
	})

	t.Run("old machines list error", func(t *testing.T) {
		// The rollout is complete and the source MachineSets are already scaled to
		// zero, so Step 7 runs and faults on the machine list.
		env := newWMErrEnv(t, readyMigration, newInfrastructureForRollout(), newCPMSForRollout(true, true),
			newSourceMachineSetForRolloutNamed("source-worker-a", 0), rolloutWorkerMachineSet())
		env.machine.PrependReactor("list", "machines", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		env.assertErr(t, err, "machine deletion")
	})

	t.Run("source machineset deletion error", func(t *testing.T) {
		// Everything is deleted and the source is not a destination, so Step 8
		// deletes the (empty) source MachineSets; fault that delete.
		env := newWMErrEnv(t, readyMigration, newInfrastructureForRollout(), newCPMSForRollout(true, true),
			newSourceMachineSetForRolloutNamed("source-worker-a", 0), rolloutWorkerMachineSet())
		env.machine.PrependReactor("delete", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		env.assertErr(t, err, "deleting source MachineSets")
	})

	t.Run("source machineset deleted successfully", func(t *testing.T) {
		// The (empty) source MachineSets are deleted, so the success branch
		// records the deletion event and confirms none remain.
		env := newWMErrEnv(t, readyMigration, newInfrastructureForRollout(), newCPMSForRollout(true, true),
			newSourceMachineSetForRolloutNamed("source-worker-a", 0), rolloutWorkerMachineSet())
		res, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Fatalf("requeue=%v, want complete", res.RequeueAfter)
		}
		if !env.sawEvent("SourceWorkersDeleted") {
			t.Fatal("want a SourceWorkersDeleted event")
		}
	})

	t.Run("control plane rollout in progress", func(t *testing.T) {
		// The CPMS is observed but not yet complete, so the rollout step requeues
		// and reports the in-progress counts.
		env := newWMErrEnv(t, readyMigration, newInfrastructureForRollout(), newCPMSForRollout(true, false))
		res, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatal("expected a requeue while the rollout is in progress")
		}
	})

	t.Run("control plane rollout held on an unmanaged node", func(t *testing.T) {
		// A Degraded condition (UnmanagedNodes) with no message holds the rollout
		// with an empty hold detail, exercising the empty-detail fallback.
		cpms := newCPMSForRollout(true, true)
		cpms.Status.Conditions = []metav1.Condition{{
			Type:   "Degraded",
			Status: metav1.ConditionTrue,
			Reason: reasonUnmanaged,
		}}
		env := newWMErrEnv(t, readyMigration, newInfrastructureForRollout(), cpms)
		res, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatal("expected a requeue while the rollout is held")
		}
	})

	t.Run("control plane rollout in progress, machine list fails", func(t *testing.T) {
		// The rollout is not complete and the control-plane machine list fails;
		// the failure is logged, not returned, so the step still requeues.
		env := newWMErrEnv(t, readyMigration, newInfrastructureForRollout(), newCPMSForRollout(true, false))
		env.machine.PrependReactor("list", "machines", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		res, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatal("expected a requeue while the rollout is in progress")
		}
	})

	t.Run("source MachineSets still present after delete", func(t *testing.T) {
		// Step 8 deletes the source MachineSets but a second list still reports
		// one, so the step requeues to let the deletion settle.
		env := newWMErrEnv(t, readyMigration, newInfrastructureForRollout(), newCPMSForRollout(true, true),
			newSourceMachineSetForRolloutNamed("source-worker-a", 0), rolloutWorkerMachineSet())
		lists := 0
		env.machine.PrependReactor("list", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			lists++
			// Only the final post-delete list reports a remaining MachineSet.
			if lists >= 3 {
				return true, &machinev1beta1.MachineSetList{Items: []machinev1beta1.MachineSet{*newSourceMachineSetForRolloutNamed("source-worker-a", 0)}}, nil
			}
			return false, nil, nil
		})
		res, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Fatal("expected a requeue while source MachineSets still remain")
		}
	})

	t.Run("control plane rollout status error", func(t *testing.T) {
		// IsCPMSGenerationObserved succeeds on the first CPMS read; the second
		// read (CheckControlPlaneRolloutStatus) faults.
		env := newWMErrEnv(t, readyMigration, newInfrastructureForRollout(), newCPMSForRollout(true, true))
		// updateWorkloadProgress reads the CPMS once, then
		// IsCPMSGenerationObserved reads it a second time; only the third read
		// (CheckControlPlaneRolloutStatus) should fault.
		gets := 0
		env.machine.PrependReactor("get", "controlplanemachinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			gets++
			if gets < 3 {
				return false, nil, nil // fall through to the real (observed) CPMS
			}
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureWorkloadMigratedRolloutAndScaleDown(ctx, readyMigration)
		env.assertErr(t, err, "checking control plane rollout")
	})
}

// TestEnsureSourceCleanedErrors covers the error branches of
// ensureSourceCleaned.
func TestEnsureSourceCleanedErrors(t *testing.T) {
	ctx := context.Background()

	// A migration with a single source failure domain whose server is the
	// recorded source vCenter.
	newMigration := func(sourceVC string) *migrationv1alpha1.VmwareCloudFoundationMigration {
		return &migrationv1alpha1.VmwareCloudFoundationMigration{
			ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Namespace: migrationv1alpha1.SingletonNamespace, UID: types.UID("uid-wm"), Generation: 1},
			Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
				FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{Name: "target-fd-1", Server: "target.example.com"}},
			},
			Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{SourceVCenter: sourceVC},
		}
	}

	newInfra := func() *configv1.Infrastructure {
		infra := newInfrastructureForRollout()
		// Two vCenters so removing the source is not the "last vCenter" refusal.
		infra.Spec.PlatformSpec.VSphere.VCenters = []configv1.VSpherePlatformVCenterSpec{
			{Server: "source.example.com"}, {Server: "target.example.com"},
		}
		return infra
	}

	t.Run("infrastructure get error", func(t *testing.T) {
		migration := newMigration("source.example.com")
		env := newWMErrEnv(t, migration)
		env.config.PrependReactor("get", "infrastructures", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureSourceCleaned(ctx, migration)
		env.assertErr(t, err, "getting infrastructure")
	})

	t.Run("no vCenters configured and none recorded", func(t *testing.T) {
		migration := newMigration("")
		infra := newInfra()
		infra.Spec.PlatformSpec.VSphere.VCenters = nil
		env := newWMErrEnv(t, migration, infra)
		_, err := env.reconciler.ensureSourceCleaned(ctx, migration)
		env.assertErr(t, err, "no vCenters configured")
	})

	t.Run("source is a configured destination", func(t *testing.T) {
		// The recorded source server matches a destination failure domain.
		migration := newMigration("target.example.com")
		env := newWMErrEnv(t, migration, newInfra())
		_, err := env.reconciler.ensureSourceCleaned(ctx, migration)
		env.assertErr(t, err, "refusing to clean up")
	})

	t.Run("remove source vcenter error", func(t *testing.T) {
		migration := newMigration("source.example.com")
		env := newWMErrEnv(t, migration, newInfra())
		env.config.PrependReactor("update", "infrastructures", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureSourceCleaned(ctx, migration)
		env.assertErr(t, err, "removing source vCenter from infrastructure")
	})

	t.Run("cloud-provider-config get error", func(t *testing.T) {
		migration := newMigration("source.example.com")
		env := newWMErrEnv(t, migration, newInfra(), newCloudProviderConfigCM())
		env.kube.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureSourceCleaned(ctx, migration)
		env.assertErr(t, err, "getting cloud-provider-config")
	})

	t.Run("vsphere-creds secret get error", func(t *testing.T) {
		migration := newMigration("source.example.com")
		infra := newInfra()
		cm := newCloudProviderConfigCM()
		env := newWMErrEnv(t, migration, infra, cm)
		env.kube.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errWorkloadFault
		})
		_, err := env.reconciler.ensureSourceCleaned(ctx, migration)
		env.assertErr(t, err, "getting vsphere-creds secret")
	})

	t.Run("metadata credentials missing", func(t *testing.T) {
		migration := newMigration("source.example.com")
		infra := newInfra()
		cm := newCloudProviderConfigCM()
		// An empty vsphere-creds secret and no per-server credential secret.
		creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: openshift.VSphereCredsSecretName, Namespace: openshift.VSphereCredsSecretNamespace}}
		env := newWMErrEnv(t, migration, infra, cm, creds)
		_, err := env.reconciler.ensureSourceCleaned(ctx, migration)
		env.assertErr(t, err, "getting credentials for metadata")
	})
}
