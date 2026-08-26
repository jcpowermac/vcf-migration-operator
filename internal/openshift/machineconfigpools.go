package openshift

import (
	"context"
	"fmt"

	machineconfigurationv1 "github.com/openshift/api/machineconfiguration/v1"
	machineconfigclient "github.com/openshift/client-go/machineconfiguration/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// PoolConvergenceSummary names the MachineConfigPools that are not converged on
// their current configuration.
type PoolConvergenceSummary struct {
	// NotUpdatedPools are pools with at least one node still on an older
	// configuration (Updated condition not True or counts not equal).
	NotUpdatedPools []string
	// DegradedPools are pools with nodes that failed to apply their configuration.
	DegradedPools []string
}

// MachineConfigPoolManager checks MachineConfigPool convergence.
type MachineConfigPoolManager struct {
	client machineconfigclient.Interface
}

// NewMachineConfigPoolManager creates a new MachineConfigPoolManager.
func NewMachineConfigPoolManager(client machineconfigclient.Interface) *MachineConfigPoolManager {
	return &MachineConfigPoolManager{client: client}
}

// CheckPoolsConverged reports whether every MachineConfigPool has fully
// converged on its current configuration: the Updated condition is True, the
// pool is not degraded (aggregate Degraded or NodeDegraded condition), and
// updatedMachineCount equals machineCount. Convergence is the signal that
// node configuration rollouts (including control plane revision rotations)
// have finished.
func (m *MachineConfigPoolManager) CheckPoolsConverged(ctx context.Context) (bool, PoolConvergenceSummary, error) {
	log := klog.FromContext(ctx)

	pools, err := m.client.MachineconfigurationV1().MachineConfigPools().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, PoolConvergenceSummary{}, fmt.Errorf("listing machine config pools: %w", err)
	}

	var summary PoolConvergenceSummary
	for i := range pools.Items {
		pool := &pools.Items[i]

		// The aggregate Degraded condition is True when any node fails to
		// apply its configuration (NodeDegraded) or when render or image
		// build degradation is reported. NodeDegraded is checked separately
		// so an inconsistent pool that lacks the aggregate condition is
		// still rejected.
		if isPoolConditionTrue(pool, machineconfigurationv1.MachineConfigPoolDegraded) ||
			isPoolConditionTrue(pool, machineconfigurationv1.MachineConfigPoolNodeDegraded) {
			summary.DegradedPools = append(summary.DegradedPools, pool.Name)
			continue
		}

		updated, hasUpdated := poolConditionStatus(pool, machineconfigurationv1.MachineConfigPoolUpdated)
		if !hasUpdated || updated != corev1.ConditionTrue || pool.Status.UpdatedMachineCount != pool.Status.MachineCount {
			summary.NotUpdatedPools = append(summary.NotUpdatedPools, pool.Name)
		}
	}

	converged := len(summary.NotUpdatedPools) == 0 && len(summary.DegradedPools) == 0
	log.V(2).Info("machine config pool convergence",
		"converged", converged,
		"notUpdated", summary.NotUpdatedPools,
		"degraded", summary.DegradedPools,
	)
	return converged, summary, nil
}

func poolConditionStatus(pool *machineconfigurationv1.MachineConfigPool, condType machineconfigurationv1.MachineConfigPoolConditionType) (corev1.ConditionStatus, bool) {
	for i := range pool.Status.Conditions {
		if pool.Status.Conditions[i].Type == condType {
			return pool.Status.Conditions[i].Status, true
		}
	}
	return "", false
}

func isPoolConditionTrue(pool *machineconfigurationv1.MachineConfigPool, condType machineconfigurationv1.MachineConfigPoolConditionType) bool {
	status, ok := poolConditionStatus(pool, condType)
	return ok && status == corev1.ConditionTrue
}
