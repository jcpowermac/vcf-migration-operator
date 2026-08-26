package openshift

import (
	"context"
	"fmt"
	"testing"

	machineconfigurationv1 "github.com/openshift/api/machineconfiguration/v1"
	machineconfigfake "github.com/openshift/client-go/machineconfiguration/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func newTestPool(name string, updated, degraded bool, machineCount, updatedCount int32) *machineconfigurationv1.MachineConfigPool {
	updatedStatus := corev1.ConditionFalse
	if updated {
		updatedStatus = corev1.ConditionTrue
	}
	conditions := []machineconfigurationv1.MachineConfigPoolCondition{
		{Type: machineconfigurationv1.MachineConfigPoolUpdated, Status: updatedStatus},
	}
	if degraded {
		conditions = append(conditions, machineconfigurationv1.MachineConfigPoolCondition{
			Type:   machineconfigurationv1.MachineConfigPoolNodeDegraded,
			Status: corev1.ConditionTrue,
		})
	}
	return &machineconfigurationv1.MachineConfigPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: machineconfigurationv1.MachineConfigPoolStatus{
			MachineCount:        machineCount,
			UpdatedMachineCount: updatedCount,
			Conditions:          conditions,
		},
	}
}

func TestCheckPoolsConverged(t *testing.T) {
	tests := []struct {
		name           string
		pools          []runtime.Object
		wantConverged  bool
		wantNotUpdated []string
		wantDegraded   []string
	}{
		{
			name:          "all pools converged",
			pools:         []runtime.Object{newTestPool("master", true, false, 3, 3), newTestPool("worker", true, false, 2, 2)},
			wantConverged: true,
		},
		{
			name:           "pool with Updated condition False",
			pools:          []runtime.Object{newTestPool("master", false, false, 3, 2), newTestPool("worker", true, false, 2, 2)},
			wantConverged:  false,
			wantNotUpdated: []string{"master"},
		},
		{
			name:           "pool with updated count below machine count",
			pools:          []runtime.Object{newTestPool("worker", true, false, 3, 2)},
			wantConverged:  false,
			wantNotUpdated: []string{"worker"},
		},
		{
			name:           "pool missing Updated condition",
			pools:          []runtime.Object{&machineconfigurationv1.MachineConfigPool{ObjectMeta: metav1.ObjectMeta{Name: "master"}}},
			wantConverged:  false,
			wantNotUpdated: []string{"master"},
		},
		{
			name:          "degraded pool",
			pools:         []runtime.Object{newTestPool("master", true, true, 3, 3)},
			wantConverged: false,
			wantDegraded:  []string{"master"},
		},
		{
			name:          "no pools reported converged",
			pools:         nil,
			wantConverged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := machineconfigfake.NewClientset(tt.pools...)
			mgr := NewMachineConfigPoolManager(client)

			got, summary, err := mgr.CheckPoolsConverged(context.Background())
			if err != nil {
				t.Fatalf("CheckPoolsConverged: %v", err)
			}
			if got != tt.wantConverged {
				t.Fatalf("CheckPoolsConverged = %v, want %v (summary %+v)", got, tt.wantConverged, summary)
			}
			if !stringSliceEqual(summary.NotUpdatedPools, tt.wantNotUpdated) {
				t.Fatalf("NotUpdatedPools = %v, want %v", summary.NotUpdatedPools, tt.wantNotUpdated)
			}
			if !stringSliceEqual(summary.DegradedPools, tt.wantDegraded) {
				t.Fatalf("DegradedPools = %v, want %v", summary.DegradedPools, tt.wantDegraded)
			}
		})
	}
}

func TestCheckPoolsConvergedReturnsListError(t *testing.T) {
	client := machineconfigfake.NewClientset()
	client.PrependReactor("list", "machineconfigpools", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("boom")
	})
	mgr := NewMachineConfigPoolManager(client)

	got, _, err := mgr.CheckPoolsConverged(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got {
		t.Fatal("expected converged=false on error")
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
