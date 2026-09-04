package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

var (
	// MigrationPhaseGauge tracks the current phase of the migration workflow (1 for active, 0 for inactive).
	// The phase is derived from status conditions, the single source of truth for stage state.
	MigrationPhaseGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vcf_migration_phase",
			Help: "Current phase of the migration workflow, derived from status conditions (1 for active, 0 for inactive)",
		},
		[]string{"phase"},
	)

	// ConditionStatusGauge tracks the status of each migration condition.
	ConditionStatusGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vcf_migration_condition_status",
			Help: "Status of migration conditions (1 for current status, 0 for other statuses)",
		},
		[]string{"condition", "status"},
	)

	// DurationSecondsGauge tracks the elapsed time of the migration in seconds.
	DurationSecondsGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "vcf_migration_duration_seconds",
			Help: "Elapsed time of the migration in seconds",
		},
	)

	// WorkersTotalGauge tracks the desired total target worker machines.
	WorkersTotalGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "vcf_migration_workers_total",
			Help: "Total desired worker machines in target failure domains",
		},
	)

	// WorkersReadyGauge tracks the number of ready target worker machines.
	WorkersReadyGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "vcf_migration_workers_ready",
			Help: "Number of target worker machines in Running phase with a NodeRef",
		},
	)

	// NodesReadyGauge tracks the number of ready target worker nodes.
	NodesReadyGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "vcf_migration_nodes_ready",
			Help: "Number of target worker nodes reporting NodeReady condition True",
		},
	)

	// SourceWorkersRemainingGauge tracks the number of remaining source worker machines.
	SourceWorkersRemainingGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "vcf_migration_source_workers_remaining",
			Help: "Number of source worker machines still existing",
		},
	)

	// ControlPlaneReplicasGauge tracks the desired control plane replicas.
	ControlPlaneReplicasGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "vcf_migration_control_plane_replicas",
			Help: "Total desired control plane replicas",
		},
	)

	// ControlPlaneUpdatedReplicasGauge tracks the updated control plane replicas.
	ControlPlaneUpdatedReplicasGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "vcf_migration_control_plane_updated_replicas",
			Help: "Number of control plane replicas matching target specification",
		},
	)

	// ControlPlaneReadyReplicasGauge tracks the ready control plane replicas.
	ControlPlaneReadyReplicasGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "vcf_migration_control_plane_ready_replicas",
			Help: "Number of control plane replicas running and ready",
		},
	)

	registerOnce sync.Once

	// phaseOrder lists the workflow conditions in execution order.
	phaseOrder = []string{
		migrationv1alpha1.ConditionInfrastructurePrepared,
		migrationv1alpha1.ConditionDestinationInitialized,
		migrationv1alpha1.ConditionDestinationImageImported,
		migrationv1alpha1.ConditionMultiSiteConfigured,
		migrationv1alpha1.ConditionWorkloadMigrated,
		migrationv1alpha1.ConditionSourceCleaned,
	}

	allPhases = []string{
		"Pending", "InfrastructurePrepared", "DestinationInitialized",
		"DestinationImageImported", "MultiSiteConfigured", "WorkloadMigrated",
		"SourceCleaned", "Completed", "Failed", "Paused",
	}

	allConditionStatuses = []metav1.ConditionStatus{
		metav1.ConditionTrue,
		metav1.ConditionFalse,
		metav1.ConditionUnknown,
	}
)

func init() {
	InitMetrics()
}

// InitMetrics registers all custom Prometheus metrics with controller-runtime's metrics registry.
// It is safe to call multiple times.
func InitMetrics() {
	registerOnce.Do(func() {
		ctrlmetrics.Registry.MustRegister(
			MigrationPhaseGauge,
			ConditionStatusGauge,
			DurationSecondsGauge,
			WorkersTotalGauge,
			WorkersReadyGauge,
			NodesReadyGauge,
			SourceWorkersRemainingGauge,
			ControlPlaneReplicasGauge,
			ControlPlaneUpdatedReplicasGauge,
			ControlPlaneReadyReplicasGauge,
		)
	})
}

// PhaseFromConditions derives the migration phase from the status conditions:
// Completed when Ready is True, Paused when Ready is False/Paused, Failed when
// the active workflow condition is False/Failed, the first non-True workflow
// condition name while progressing (absent stages count as not yet reached),
// and Pending before any workflow condition has been set.
func PhaseFromConditions(status *migrationv1alpha1.VmwareCloudFoundationMigrationStatus) string {
	ready := apimeta.FindStatusCondition(status.Conditions, migrationv1alpha1.ConditionReady)
	if ready != nil && ready.Status == metav1.ConditionTrue {
		return "Completed"
	}
	if ready != nil && ready.Reason == migrationv1alpha1.ReasonPaused {
		return "Paused"
	}
	started := false
	for _, condType := range phaseOrder {
		cond := apimeta.FindStatusCondition(status.Conditions, condType)
		if cond == nil {
			if started {
				// Prior stages exist, so this is the next stage.
				return condType
			}
			continue
		}
		started = true
		if cond.Status != metav1.ConditionTrue {
			if cond.Reason == migrationv1alpha1.ReasonFailed {
				return "Failed"
			}
			return condType
		}
	}
	if started {
		return "Completed"
	}
	return "Pending"
}

// UpdateMigrationMetrics updates all Prometheus metrics based on the current migration status.
func UpdateMigrationMetrics(status *migrationv1alpha1.VmwareCloudFoundationMigrationStatus) {
	if status == nil {
		return
	}

	// Update Phase gauge: 1 for active phase, 0 for all others.
	phase := PhaseFromConditions(status)
	for _, p := range allPhases {
		if phase == p {
			MigrationPhaseGauge.WithLabelValues(p).Set(1)
		} else {
			MigrationPhaseGauge.WithLabelValues(p).Set(0)
		}
	}

	// Update ConditionStatus gauges: 1 for observed status, 0 for others.
	for _, cond := range status.Conditions {
		for _, s := range allConditionStatuses {
			if cond.Status == s {
				ConditionStatusGauge.WithLabelValues(cond.Type, string(s)).Set(1)
			} else {
				ConditionStatusGauge.WithLabelValues(cond.Type, string(s)).Set(0)
			}
		}
	}

	// Update duration.
	if status.StartTime != nil {
		if status.CompletionTime != nil {
			DurationSecondsGauge.Set(status.CompletionTime.Sub(status.StartTime.Time).Seconds())
		} else {
			DurationSecondsGauge.Set(time.Since(status.StartTime.Time).Seconds())
		}
	} else {
		DurationSecondsGauge.Set(0)
	}

	// Update Progress counts.
	if status.Progress != nil {
		if status.Progress.Workers != nil {
			WorkersTotalGauge.Set(float64(status.Progress.Workers.TargetMachinesTotal))
			WorkersReadyGauge.Set(float64(status.Progress.Workers.TargetMachinesReady))
			NodesReadyGauge.Set(float64(status.Progress.Workers.TargetNodesReady))
			SourceWorkersRemainingGauge.Set(float64(status.Progress.Workers.SourceMachinesRemaining))
		}
		if status.Progress.ControlPlane != nil {
			ControlPlaneReplicasGauge.Set(float64(status.Progress.ControlPlane.Replicas))
			ControlPlaneUpdatedReplicasGauge.Set(float64(status.Progress.ControlPlane.UpdatedReplicas))
			ControlPlaneReadyReplicasGauge.Set(float64(status.Progress.ControlPlane.ReadyReplicas))
		}
	}
}

// ResetMetrics resets all Prometheus metrics gauges to zero or clears them.
func ResetMetrics() {
	for _, p := range allPhases {
		MigrationPhaseGauge.WithLabelValues(p).Set(0)
	}
	ConditionStatusGauge.Reset()
	DurationSecondsGauge.Set(0)
	WorkersTotalGauge.Set(0)
	WorkersReadyGauge.Set(0)
	NodesReadyGauge.Set(0)
	SourceWorkersRemainingGauge.Set(0)
	ControlPlaneReplicasGauge.Set(0)
	ControlPlaneUpdatedReplicasGauge.Set(0)
	ControlPlaneReadyReplicasGauge.Set(0)
}
