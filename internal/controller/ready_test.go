package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	machineconfigurationv1 "github.com/openshift/api/machineconfiguration/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	machineconfigclient "github.com/openshift/client-go/machineconfiguration/clientset/versioned"
	machineconfigfake "github.com/openshift/client-go/machineconfiguration/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

func newStableReadyTestOperator(name string) *configv1.ClusterOperator {
	return &configv1.ClusterOperator{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: configv1.ClusterOperatorStatus{
			Conditions: []configv1.ClusterOperatorStatusCondition{
				{Type: configv1.OperatorAvailable, Status: configv1.ConditionTrue},
				{Type: configv1.OperatorProgressing, Status: configv1.ConditionFalse},
				{Type: configv1.OperatorDegraded, Status: configv1.ConditionFalse},
			},
		},
	}
}

func newConvergedReadyTestPool(name string) *machineconfigurationv1.MachineConfigPool {
	return &machineconfigurationv1.MachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: machineconfigurationv1.MachineConfigPoolStatus{
			MachineCount:        3,
			UpdatedMachineCount: 3,
			Conditions: []machineconfigurationv1.MachineConfigPoolCondition{
				{Type: machineconfigurationv1.MachineConfigPoolUpdated, Status: corev1.ConditionTrue},
			},
		},
	}
}

func newUnconvergedReadyTestPool(name string) *machineconfigurationv1.MachineConfigPool {
	return &machineconfigurationv1.MachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: machineconfigurationv1.MachineConfigPoolStatus{
			MachineCount:        3,
			UpdatedMachineCount: 2,
			Conditions: []machineconfigurationv1.MachineConfigPoolCondition{
				{Type: machineconfigurationv1.MachineConfigPoolUpdated, Status: corev1.ConditionFalse},
			},
		},
	}
}

func newReadyTestReconciler(cfgClient *configfake.Clientset, mcClient *machineconfigfake.Clientset) *VmwareCloudFoundationMigrationReconciler {
	return &VmwareCloudFoundationMigrationReconciler{
		ConfigClient:        cfgClient,
		MachineConfigClient: mcClient,
		Recorder:            record.NewFakeRecorder(10),
	}
}

func TestEnsureReadyRequeuesWhenOperatorProgressing(t *testing.T) {
	operator := newStableReadyTestOperator("etcd")
	operator.Status.Conditions[1].Status = configv1.ConditionTrue

	reconciler := newReadyTestReconciler(
		configfake.NewClientset(operator, newInfrastructureForReadyTest([]string{"target.example.com"})),
		machineconfigfake.NewClientset(newConvergedReadyTestPool("master")),
	)
	migration := newMigrationForReadyTest([]string{"target.example.com"})

	result, err := reconciler.ensureReady(context.Background(), migration)
	if err != nil {
		t.Fatalf("ensureReady returned error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, 30*time.Second)
	}
	cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("ready condition = %+v, want False", cond)
	}
	if !strings.Contains(cond.Message, "progressing=etcd") {
		t.Fatalf("message = %q, want it to mention progressing=etcd", cond.Message)
	}
}

func TestEnsureReadyRequiresSustainedStabilityBeforeCompletion(t *testing.T) {
	reconciler := newReadyTestReconciler(
		configfake.NewClientset(newStableReadyTestOperator("etcd"), newInfrastructureForReadyTest([]string{"target.example.com"})),
		machineconfigfake.NewClientset(newConvergedReadyTestPool("master"), newConvergedReadyTestPool("worker")),
	)
	migration := newMigrationForReadyTest([]string{"target.example.com"})
	ctx := context.Background()

	for i := 1; i < readyStabilityThreshold; i++ {
		// Simulate the ~30s requeue cadence between counted observations.
		reconciler.lastCountedStabilityCheck = time.Now().Add(-2 * readyStabilityObservationInterval)
		result, err := reconciler.ensureReady(ctx, migration)
		if err != nil {
			t.Fatalf("ensureReady (%d) returned error: %v", i, err)
		}
		if result.RequeueAfter != 30*time.Second {
			t.Fatalf("ensureReady (%d) RequeueAfter = %s, want %s", i, result.RequeueAfter, 30*time.Second)
		}
		cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("ensureReady (%d) ready condition = %+v, want False", i, cond)
		}
		wantMsg := fmt.Sprintf("Waiting for sustained cluster stability (%d/%d)", i, readyStabilityThreshold)
		if cond.Message != wantMsg {
			t.Fatalf("ensureReady (%d) message = %q, want %q", i, cond.Message, wantMsg)
		}
	}

	// The final observation completes the window on the normal cadence.
	reconciler.lastCountedStabilityCheck = time.Now().Add(-2 * readyStabilityObservationInterval)
	result, err := reconciler.ensureReady(ctx, migration)
	if err != nil {
		t.Fatalf("ensureReady (final) returned error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %s, want 0", result.RequeueAfter)
	}
	cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ready condition = %+v, want True", cond)
	}
	if migration.Status.CompletionTime == nil {
		t.Fatal("expected completion time to be set")
	}
}

func TestEnsureReadyBlocksWhenPoolNotConverged(t *testing.T) {
	// A stable operator with a non-etcd name keeps the case focused on the
	// pool blocker rather than operator progress.
	reconciler := newReadyTestReconciler(
		configfake.NewClientset(newStableReadyTestOperator("kube-scheduler"), newInfrastructureForReadyTest([]string{"target.example.com"})),
		machineconfigfake.NewClientset(newConvergedReadyTestPool("worker"), newUnconvergedReadyTestPool("master")),
	)
	migration := newMigrationForReadyTest([]string{"target.example.com"})

	result, err := reconciler.ensureReady(context.Background(), migration)
	if err != nil {
		t.Fatalf("ensureReady returned error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, 30*time.Second)
	}
	cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("ready condition = %+v, want False", cond)
	}
	if !strings.Contains(cond.Message, "pools-not-updated=master") {
		t.Fatalf("message = %q, want it to mention pools-not-updated=master", cond.Message)
	}
}

func TestEnsureReadyResetsStabilityCounterOnUnstableObservation(t *testing.T) {
	operator := newStableReadyTestOperator("etcd")
	cfgClient := configfake.NewClientset(operator, newInfrastructureForReadyTest([]string{"target.example.com"}))
	reconciler := newReadyTestReconciler(
		cfgClient,
		machineconfigfake.NewClientset(newConvergedReadyTestPool("master")),
	)
	migration := newMigrationForReadyTest([]string{"target.example.com"})
	ctx := context.Background()

	setOperatorProgressing := func(progressing bool) {
		latest, err := cfgClient.ConfigV1().ClusterOperators().Get(ctx, "etcd", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting etcd operator: %v", err)
		}
		status := configv1.ConditionFalse
		if progressing {
			status = configv1.ConditionTrue
		}
		for i := range latest.Status.Conditions {
			if latest.Status.Conditions[i].Type == configv1.OperatorProgressing {
				latest.Status.Conditions[i].Status = status
			}
		}
		if _, err := cfgClient.ConfigV1().ClusterOperators().Update(ctx, latest, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("updating etcd operator: %v", err)
		}
	}

	assertNotReady := func(call string) {
		// Simulate the ~30s requeue cadence so each stable observation counts.
		reconciler.lastCountedStabilityCheck = time.Now().Add(-2 * readyStabilityObservationInterval)
		result, err := reconciler.ensureReady(ctx, migration)
		if err != nil {
			t.Fatalf("ensureReady (%s) returned error: %v", call, err)
		}
		if result.RequeueAfter != 30*time.Second {
			t.Fatalf("ensureReady (%s) RequeueAfter = %s, want %s", call, result.RequeueAfter, 30*time.Second)
		}
		cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("ensureReady (%s) ready condition = %+v, want False", call, cond)
		}
	}

	// Accumulate almost the full stability window.
	for i := 1; i < readyStabilityThreshold-1; i++ {
		assertNotReady(fmt.Sprintf("stable %d", i))
	}

	// An unstable observation must reset the counter.
	setOperatorProgressing(true)
	result, err := reconciler.ensureReady(ctx, migration)
	if err != nil {
		t.Fatalf("ensureReady (unstable) returned error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, 30*time.Second)
	}
	cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
	if !strings.Contains(cond.Message, "progressing=etcd") {
		t.Fatalf("unstable message = %q, want it to mention progressing=etcd", cond.Message)
	}

	// Back to stable: the full window must accumulate again.
	setOperatorProgressing(false)
	for i := 1; i < readyStabilityThreshold; i++ {
		assertNotReady(fmt.Sprintf("re-accumulating %d", i))
	}

	// The final observation completes the window on the normal cadence.
	reconciler.lastCountedStabilityCheck = time.Now().Add(-2 * readyStabilityObservationInterval)
	result, err = reconciler.ensureReady(ctx, migration)
	if err != nil {
		t.Fatalf("ensureReady (final) returned error: %v", err)
	}
	cond = apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ready condition = %+v, want True after full re-accumulation", cond)
	}
}

func TestEnsureReadyResetsCounterAfterLongEnsureReadyGap(t *testing.T) {
	reconciler := newReadyTestReconciler(
		configfake.NewClientset(newStableReadyTestOperator("etcd"), newInfrastructureForReadyTest([]string{"target.example.com"})),
		machineconfigfake.NewClientset(newConvergedReadyTestPool("master")),
	)
	migration := newMigrationForReadyTest([]string{"target.example.com"})
	ctx := context.Background()

	assertWaiting := func(call string, count int) {
		result, err := reconciler.ensureReady(ctx, migration)
		if err != nil {
			t.Fatalf("ensureReady (%s) returned error: %v", call, err)
		}
		if result.RequeueAfter != 30*time.Second {
			t.Fatalf("ensureReady (%s) RequeueAfter = %s, want %s", call, result.RequeueAfter, 30*time.Second)
		}
		cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("ensureReady (%s) ready condition = %+v, want False", call, cond)
		}
		wantMsg := fmt.Sprintf("Waiting for sustained cluster stability (%d/%d)", count, readyStabilityThreshold)
		if cond.Message != wantMsg {
			t.Fatalf("ensureReady (%s) message = %q, want %q", call, cond.Message, wantMsg)
		}
	}

	// Accumulate two stable observations on the normal ~30s cadence.
	assertWaiting("stable 1", 1)
	reconciler.lastCountedStabilityCheck = time.Now().Add(-2 * readyStabilityObservationInterval)
	assertWaiting("stable 2", 2)

	// Simulate reconciles that bypassed ensureReady for longer than
	// readyStabilityCheckGap; the counter must restart from zero.
	reconciler.lastReadyStabilityCheck = time.Now().Add(-2 * readyStabilityCheckGap)
	assertWaiting("after long gap", 1)
}

// TestEnsureReadyDoesNotCountRapidStableObservations verifies that event-driven
// reconciles running faster than the requeue cadence cannot satisfy the
// sustained-stability threshold within seconds.
func TestEnsureReadyDoesNotCountRapidStableObservations(t *testing.T) {
	reconciler := newReadyTestReconciler(
		configfake.NewClientset(newStableReadyTestOperator("etcd"), newInfrastructureForReadyTest([]string{"target.example.com"})),
		machineconfigfake.NewClientset(newConvergedReadyTestPool("master")),
	)
	migration := newMigrationForReadyTest([]string{"target.example.com"})
	ctx := context.Background()

	for i := 1; i <= readyStabilityThreshold; i++ {
		result, err := reconciler.ensureReady(ctx, migration)
		if err != nil {
			t.Fatalf("ensureReady (%d) returned error: %v", i, err)
		}
		if result.RequeueAfter != 30*time.Second {
			t.Fatalf("ensureReady (%d) RequeueAfter = %s, want %s", i, result.RequeueAfter, 30*time.Second)
		}
		cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
		wantMsg := fmt.Sprintf("Waiting for sustained cluster stability (1/%d)", readyStabilityThreshold)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Message != wantMsg {
			t.Fatalf("ensureReady (%d) ready condition = %+v, want message %q", i, cond, wantMsg)
		}
	}
	if reconciler.readyStabilityCount != 1 {
		t.Fatalf("readyStabilityCount = %d, want 1 after %d rapid stable observations", reconciler.readyStabilityCount, readyStabilityThreshold)
	}
}

// TestEnsureReadyResetsStabilityCounterOnCheckErrors verifies that a failure in
// any readiness check restarts the stability window, so the next successful
// check cannot immediately commit Ready=True.
func TestEnsureReadyResetsStabilityCounterOnCheckErrors(t *testing.T) {
	stableCfg := configfake.NewClientset(newStableReadyTestOperator("etcd"), newInfrastructureForReadyTest([]string{"target.example.com"}))
	stableMc := machineconfigfake.NewClientset(newConvergedReadyTestPool("master"))
	reconciler := newReadyTestReconciler(stableCfg, stableMc)
	migration := newMigrationForReadyTest([]string{"target.example.com"})
	ctx := context.Background()

	// Accumulate almost the full stability window.
	for i := 1; i < readyStabilityThreshold; i++ {
		reconciler.lastCountedStabilityCheck = time.Now().Add(-2 * readyStabilityObservationInterval)
		if _, err := reconciler.ensureReady(ctx, migration); err != nil {
			t.Fatalf("ensureReady (stable %d) returned error: %v", i, err)
		}
	}
	if reconciler.readyStabilityCount != readyStabilityThreshold-1 {
		t.Fatalf("readyStabilityCount = %d, want %d", reconciler.readyStabilityCount, readyStabilityThreshold-1)
	}

	operatorErrCfg := configfake.NewClientset()
	operatorErrCfg.PrependReactor("list", "clusteroperators", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("clusteroperators unavailable")
	})

	poolErrMc := machineconfigfake.NewClientset()
	poolErrMc.PrependReactor("list", "machineconfigpools", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("machineconfigpools unavailable")
	})

	infraErrCfg := configfake.NewClientset()
	infraErrCfg.PrependReactor("get", "infrastructures", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("infrastructures unavailable")
	})

	failures := []struct {
		name string
		cfg  configclient.Interface
		mc   machineconfigclient.Interface
	}{
		{"operator check", operatorErrCfg, stableMc},
		{"pool check", stableCfg, poolErrMc},
		{"infrastructure check", infraErrCfg, stableMc},
	}
	for _, failure := range failures {
		reconciler.ConfigClient = failure.cfg
		reconciler.MachineConfigClient = failure.mc
		if _, err := reconciler.ensureReady(ctx, migration); err == nil {
			t.Fatalf("ensureReady (%s) expected error", failure.name)
		}
		if reconciler.readyStabilityCount != 0 {
			t.Fatalf("after %s failure readyStabilityCount = %d, want 0", failure.name, reconciler.readyStabilityCount)
		}
		reconciler.ConfigClient = stableCfg
		reconciler.MachineConfigClient = stableMc
	}

	// The next stable observation restarts the window at 1/N instead of
	// completing it.
	reconciler.lastCountedStabilityCheck = time.Now().Add(-2 * readyStabilityObservationInterval)
	result, err := reconciler.ensureReady(ctx, migration)
	if err != nil {
		t.Fatalf("ensureReady (after errors) returned error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, 30*time.Second)
	}
	cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
	wantMsg := fmt.Sprintf("Waiting for sustained cluster stability (1/%d)", readyStabilityThreshold)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Message != wantMsg {
		t.Fatalf("ready condition = %+v, want message %q", cond, wantMsg)
	}
}

func newMigrationForReadyTest(targetServers []string) *migrationv1alpha1.VmwareCloudFoundationMigration {
	fds := make([]configv1.VSpherePlatformFailureDomainSpec, 0, len(targetServers))
	for i, server := range targetServers {
		fds = append(fds, configv1.VSpherePlatformFailureDomainSpec{
			Name:   "fd-" + string(rune('a'+i)),
			Server: server,
		})
	}

	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migrationv1alpha1.SingletonName,
			Namespace: "default",
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			FailureDomains: fds,
		},
	}
}

func newInfrastructureForReadyTest(servers []string) *configv1.Infrastructure {
	vcenters := make([]configv1.VSpherePlatformVCenterSpec, 0, len(servers))
	for _, server := range servers {
		vcenters = append(vcenters, configv1.VSpherePlatformVCenterSpec{
			Server:      server,
			Datacenters: []string{"dc1"},
		})
	}

	return &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
		Spec: configv1.InfrastructureSpec{
			PlatformSpec: configv1.PlatformSpec{
				Type: configv1.VSpherePlatformType,
				VSphere: &configv1.VSpherePlatformSpec{
					VCenters: vcenters,
				},
			},
		},
	}
}
