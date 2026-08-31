package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

func TestUpdateMigrationMetrics(t *testing.T) {
	InitMetrics()
	defer ResetMetrics()

	now := metav1.Now()
	startTime := metav1.NewTime(now.Add(-5 * time.Minute))

	status := &migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
		Phase:     migrationv1alpha1.PhaseWorkloadMigrated,
		StartTime: &startTime,
		Conditions: []metav1.Condition{
			{
				Type:   migrationv1alpha1.ConditionWorkloadMigrated,
				Status: metav1.ConditionTrue,
			},
			{
				Type:   migrationv1alpha1.ConditionReady,
				Status: metav1.ConditionFalse,
			},
		},
		Progress: &migrationv1alpha1.MigrationProgress{
			Workers: &migrationv1alpha1.WorkerMigrationProgress{
				TargetMachinesTotal:     3,
				TargetMachinesReady:     2,
				TargetNodesReady:        2,
				SourceMachinesRemaining: 1,
			},
			ControlPlane: &migrationv1alpha1.ControlPlaneProgress{
				Replicas:        3,
				UpdatedReplicas: 2,
				ReadyReplicas:   2,
			},
		},
	}

	UpdateMigrationMetrics(status)

	if val := testutil.ToFloat64(MigrationPhaseGauge.WithLabelValues(string(migrationv1alpha1.PhaseWorkloadMigrated))); val != 1 {
		t.Errorf("expected phase gauge for WorkloadMigrated to be 1, got %f", val)
	}
	if val := testutil.ToFloat64(MigrationPhaseGauge.WithLabelValues(string(migrationv1alpha1.PhasePending))); val != 0 {
		t.Errorf("expected phase gauge for Pending to be 0, got %f", val)
	}

	if val := testutil.ToFloat64(ConditionStatusGauge.WithLabelValues(migrationv1alpha1.ConditionWorkloadMigrated, string(metav1.ConditionTrue))); val != 1 {
		t.Errorf("expected ConditionWorkloadMigrated True gauge to be 1, got %f", val)
	}
	if val := testutil.ToFloat64(ConditionStatusGauge.WithLabelValues(migrationv1alpha1.ConditionWorkloadMigrated, string(metav1.ConditionFalse))); val != 0 {
		t.Errorf("expected ConditionWorkloadMigrated False gauge to be 0, got %f", val)
	}

	if val := testutil.ToFloat64(WorkersTotalGauge); val != 3 {
		t.Errorf("expected workers total to be 3, got %f", val)
	}
	if val := testutil.ToFloat64(WorkersReadyGauge); val != 2 {
		t.Errorf("expected workers ready to be 2, got %f", val)
	}
	if val := testutil.ToFloat64(NodesReadyGauge); val != 2 {
		t.Errorf("expected nodes ready to be 2, got %f", val)
	}
	if val := testutil.ToFloat64(SourceWorkersRemainingGauge); val != 1 {
		t.Errorf("expected source workers remaining to be 1, got %f", val)
	}
	if val := testutil.ToFloat64(ControlPlaneReplicasGauge); val != 3 {
		t.Errorf("expected cp replicas to be 3, got %f", val)
	}
	if val := testutil.ToFloat64(ControlPlaneUpdatedReplicasGauge); val != 2 {
		t.Errorf("expected cp updated replicas to be 2, got %f", val)
	}
	if val := testutil.ToFloat64(ControlPlaneReadyReplicasGauge); val != 2 {
		t.Errorf("expected cp ready replicas to be 2, got %f", val)
	}

	if val := testutil.ToFloat64(DurationSecondsGauge); val < 290 || val > 310 {
		t.Errorf("expected duration around 300s, got %f", val)
	}
}

func TestUpdateMigrationMetricsCompletion(t *testing.T) {
	InitMetrics()
	defer ResetMetrics()

	startTime := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	completionTime := metav1.NewTime(time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC))

	status := &migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
		Phase:          migrationv1alpha1.PhaseCompleted,
		StartTime:      &startTime,
		CompletionTime: &completionTime,
	}

	UpdateMigrationMetrics(status)

	if val := testutil.ToFloat64(DurationSecondsGauge); val != 600 {
		t.Errorf("expected duration to be 600s, got %f", val)
	}
}

func TestResetMetrics(t *testing.T) {
	InitMetrics()

	status := &migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
		Phase: migrationv1alpha1.PhaseCompleted,
		Progress: &migrationv1alpha1.MigrationProgress{
			Workers: &migrationv1alpha1.WorkerMigrationProgress{
				TargetMachinesTotal: 5,
			},
		},
	}

	UpdateMigrationMetrics(status)
	ResetMetrics()

	if val := testutil.ToFloat64(MigrationPhaseGauge.WithLabelValues(string(migrationv1alpha1.PhaseCompleted))); val != 0 {
		t.Errorf("expected phase gauge to be 0 after reset, got %f", val)
	}
	if val := testutil.ToFloat64(WorkersTotalGauge); val != 0 {
		t.Errorf("expected workers total gauge to be 0 after reset, got %f", val)
	}
}
