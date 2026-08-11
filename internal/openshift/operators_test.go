package openshift

import (
	"context"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newTestClusterOperator(name string, available, degraded bool) *configv1.ClusterOperator {
	return newTestClusterOperatorWithProgressing(name, available, false, degraded)
}

func newTestClusterOperatorWithProgressing(name string, available, progressing, degraded bool) *configv1.ClusterOperator {
	conditions := []configv1.ClusterOperatorStatusCondition{}

	availableStatus := configv1.ConditionFalse
	if available {
		availableStatus = configv1.ConditionTrue
	}
	conditions = append(conditions, configv1.ClusterOperatorStatusCondition{
		Type:    configv1.OperatorAvailable,
		Status:  availableStatus,
		Message: "test",
	})

	degradedStatus := configv1.ConditionFalse
	if degraded {
		degradedStatus = configv1.ConditionTrue
	}
	conditions = append(conditions, configv1.ClusterOperatorStatusCondition{
		Type:    configv1.OperatorDegraded,
		Status:  degradedStatus,
		Message: "test",
	})

	progressingStatus := configv1.ConditionFalse
	if progressing {
		progressingStatus = configv1.ConditionTrue
	}
	conditions = append(conditions, configv1.ClusterOperatorStatusCondition{
		Type:    configv1.OperatorProgressing,
		Status:  progressingStatus,
		Message: "test",
	})

	return &configv1.ClusterOperator{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: configv1.ClusterOperatorStatus{
			Conditions: conditions,
		},
	}
}

func TestIsOperatorHealthyHelper(t *testing.T) {
	tests := []struct {
		name      string
		available bool
		degraded  bool
		want      bool
	}{
		{
			name:      "available and not degraded is healthy",
			available: true,
			degraded:  false,
			want:      true,
		},
		{
			name:      "not available is unhealthy",
			available: false,
			degraded:  false,
			want:      false,
		},
		{
			name:      "available but degraded is unhealthy",
			available: true,
			degraded:  true,
			want:      false,
		},
		{
			name:      "not available and degraded is unhealthy",
			available: false,
			degraded:  true,
			want:      false,
		},
		{
			name: "no conditions is unhealthy",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var co *configv1.ClusterOperator
			if tt.name == "no conditions is unhealthy" {
				co = &configv1.ClusterOperator{
					ObjectMeta: metav1.ObjectMeta{Name: "test-operator"},
				}
			} else {
				co = newTestClusterOperator("test-operator", tt.available, tt.degraded)
			}

			got := isOperatorHealthy(co)
			if got != tt.want {
				t.Fatalf("isOperatorHealthy = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckAllOperatorsHealthy(t *testing.T) {
	tests := []struct {
		name               string
		operators          []*configv1.ClusterOperator
		wantHealthy        bool
		wantUnhealthyCount int
	}{
		{
			name: "all healthy",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperator("etcd", true, false),
				newTestClusterOperator("kube-apiserver", true, false),
			},
			wantHealthy:        true,
			wantUnhealthyCount: 0,
		},
		{
			name: "one degraded",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperator("etcd", true, false),
				newTestClusterOperator("kube-apiserver", true, true),
			},
			wantHealthy:        false,
			wantUnhealthyCount: 1,
		},
		{
			name: "excluded operator is skipped even when unhealthy",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperator("etcd", true, false),
				newTestClusterOperator("machine-config", false, true),
			},
			wantHealthy:        true,
			wantUnhealthyCount: 0,
		},
		{
			name: "mix of healthy unhealthy and excluded",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperator("etcd", true, false),
				newTestClusterOperator("kube-apiserver", false, false),
				newTestClusterOperator("machine-config", false, true),
			},
			wantHealthy:        false,
			wantUnhealthyCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := configfake.NewClientset(tt.operators[0])
			for _, op := range tt.operators[1:] {
				_, err := client.ConfigV1().ClusterOperators().Create(context.Background(), op, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create test operator %s: %v", op.Name, err)
				}
			}

			mgr := NewOperatorManager(client)

			healthy, unhealthy, err := mgr.CheckAllOperatorsHealthy(context.Background())
			if err != nil {
				t.Fatalf("CheckAllOperatorsHealthy error = %v", err)
			}

			if healthy != tt.wantHealthy {
				t.Fatalf("healthy = %v, want %v", healthy, tt.wantHealthy)
			}

			if len(unhealthy) != tt.wantUnhealthyCount {
				t.Fatalf("unhealthy count = %d, want %d (operators: %v)", len(unhealthy), tt.wantUnhealthyCount, unhealthy)
			}
		})
	}
}

func TestIsOperatorHealthyExported(t *testing.T) {
	tests := []struct {
		name        string
		operator    *configv1.ClusterOperator
		wantHealthy bool
	}{
		{
			name:        "healthy operator",
			operator:    newTestClusterOperator("etcd", true, false),
			wantHealthy: true,
		},
		{
			name:        "unhealthy operator not available",
			operator:    newTestClusterOperator("etcd", false, false),
			wantHealthy: false,
		},
		{
			name:        "unhealthy operator degraded",
			operator:    newTestClusterOperator("etcd", true, true),
			wantHealthy: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := configfake.NewClientset(tt.operator)
			mgr := NewOperatorManager(client)

			healthy, msg, err := mgr.IsOperatorHealthy(context.Background(), tt.operator.Name)
			if err != nil {
				t.Fatalf("IsOperatorHealthy error = %v", err)
			}

			if healthy != tt.wantHealthy {
				t.Fatalf("healthy = %v, want %v (message: %s)", healthy, tt.wantHealthy, msg)
			}

			if msg == "" {
				t.Fatal("expected non-empty message")
			}
		})
	}
}

func TestCheckAllOperatorsStable(t *testing.T) {
	tests := []struct {
		name                   string
		operators              []*configv1.ClusterOperator
		wantStable             bool
		wantUnavailableCount   int
		wantProgressingCount   int
		wantDegradedCount      int
	}{
		{
			name: "all stable",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperatorWithProgressing("etcd", true, false, false),
				newTestClusterOperatorWithProgressing("kube-apiserver", true, false, false),
			},
			wantStable:           true,
			wantUnavailableCount: 0,
			wantProgressingCount: 0,
			wantDegradedCount:    0,
		},
		{
			name: "progressing blocks stability",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperatorWithProgressing("etcd", true, true, false),
			},
			wantStable:           false,
			wantUnavailableCount: 0,
			wantProgressingCount: 1,
			wantDegradedCount:    0,
		},
		{
			name: "unavailable degraded and progressing are categorized",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperatorWithProgressing("etcd", false, false, false),
				newTestClusterOperatorWithProgressing("kube-apiserver", true, true, false),
				newTestClusterOperatorWithProgressing("ingress", true, false, true),
			},
			wantStable:           false,
			wantUnavailableCount: 1,
			wantProgressingCount: 1,
			wantDegradedCount:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := configfake.NewClientset(tt.operators[0])
			for _, op := range tt.operators[1:] {
				_, err := client.ConfigV1().ClusterOperators().Create(context.Background(), op, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create test operator %s: %v", op.Name, err)
				}
			}

			mgr := NewOperatorManager(client)
			stable, summary, err := mgr.CheckAllOperatorsStable(context.Background())
			if err != nil {
				t.Fatalf("CheckAllOperatorsStable error = %v", err)
			}

			if stable != tt.wantStable {
				t.Fatalf("stable = %v, want %v", stable, tt.wantStable)
			}
			if len(summary.UnavailableOperators) != tt.wantUnavailableCount {
				t.Fatalf("UnavailableOperators count = %d, want %d", len(summary.UnavailableOperators), tt.wantUnavailableCount)
			}
			if len(summary.ProgressingOperators) != tt.wantProgressingCount {
				t.Fatalf("ProgressingOperators count = %d, want %d", len(summary.ProgressingOperators), tt.wantProgressingCount)
			}
			if len(summary.DegradedOperators) != tt.wantDegradedCount {
				t.Fatalf("DegradedOperators count = %d, want %d", len(summary.DegradedOperators), tt.wantDegradedCount)
			}
		})
	}
}
