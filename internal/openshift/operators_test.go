package openshift

import (
	"context"
	"sort"
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

func newTestClusterOperatorWithStatuses(name string, available, progressing, degraded configv1.ConditionStatus) *configv1.ClusterOperator {
	return &configv1.ClusterOperator{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: configv1.ClusterOperatorStatus{
			Conditions: []configv1.ClusterOperatorStatusCondition{
				{Type: configv1.OperatorAvailable, Status: available, Message: "test"},
				{Type: configv1.OperatorProgressing, Status: progressing, Message: "test"},
				{Type: configv1.OperatorDegraded, Status: degraded, Message: "test"},
			},
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
		name                     string
		operators                []*configv1.ClusterOperator
		wantStable               bool
		wantUnavailableOperators []string
		wantProgressingOperators []string
		wantDegradedOperators    []string
	}{
		{
			name: "all stable",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperatorWithProgressing("etcd", true, false, false),
				newTestClusterOperatorWithProgressing("kube-apiserver", true, false, false),
			},
			wantStable:               true,
			wantUnavailableOperators: nil,
			wantProgressingOperators: nil,
			wantDegradedOperators:    nil,
		},
		{
			name: "progressing blocks stability",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperatorWithProgressing("etcd", true, true, false),
			},
			wantStable:               false,
			wantUnavailableOperators: nil,
			wantProgressingOperators: []string{"etcd"},
			wantDegradedOperators:    nil,
		},
		{
			name: "unavailable degraded and progressing are categorized",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperatorWithProgressing("etcd", false, false, false),
				newTestClusterOperatorWithProgressing("kube-apiserver", true, true, false),
				newTestClusterOperatorWithProgressing("ingress", true, false, true),
			},
			wantStable:               false,
			wantUnavailableOperators: []string{"etcd"},
			wantProgressingOperators: []string{"kube-apiserver"},
			wantDegradedOperators:    []string{"ingress"},
		},
		{
			name: "missing progressing condition blocks stability",
			operators: []*configv1.ClusterOperator{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "etcd"},
					Status: configv1.ClusterOperatorStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{Type: configv1.OperatorAvailable, Status: configv1.ConditionTrue, Message: "test"},
							{Type: configv1.OperatorDegraded, Status: configv1.ConditionFalse, Message: "test"},
						},
					},
				},
			},
			wantStable:               false,
			wantUnavailableOperators: nil,
			wantProgressingOperators: []string{"etcd"},
			wantDegradedOperators:    nil,
		},
		{
			name: "missing available condition marks unavailable",
			operators: []*configv1.ClusterOperator{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "etcd"},
					Status: configv1.ClusterOperatorStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{Type: configv1.OperatorProgressing, Status: configv1.ConditionFalse, Message: "test"},
							{Type: configv1.OperatorDegraded, Status: configv1.ConditionFalse, Message: "test"},
						},
					},
				},
			},
			wantStable:               false,
			wantUnavailableOperators: []string{"etcd"},
			wantProgressingOperators: nil,
			wantDegradedOperators:    nil,
		},
		{
			name: "unknown condition statuses block stability",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperatorWithStatuses("etcd", configv1.ConditionTrue, configv1.ConditionUnknown, configv1.ConditionUnknown),
			},
			wantStable:               false,
			wantUnavailableOperators: nil,
			wantProgressingOperators: []string{"etcd"},
			wantDegradedOperators:    []string{"etcd"},
		},
		{
			name: "unknown available status marks unavailable",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperatorWithStatuses("etcd", configv1.ConditionUnknown, configv1.ConditionFalse, configv1.ConditionFalse),
			},
			wantStable:               false,
			wantUnavailableOperators: []string{"etcd"},
			wantProgressingOperators: nil,
			wantDegradedOperators:    nil,
		},
		{
			name: "missing degraded condition marks degraded",
			operators: []*configv1.ClusterOperator{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "etcd"},
					Status: configv1.ClusterOperatorStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{Type: configv1.OperatorAvailable, Status: configv1.ConditionTrue, Message: "test"},
							{Type: configv1.OperatorProgressing, Status: configv1.ConditionFalse, Message: "test"},
						},
					},
				},
			},
			wantStable:               false,
			wantUnavailableOperators: nil,
			wantProgressingOperators: nil,
			wantDegradedOperators:    []string{"etcd"},
		},
		{
			name: "unknown degraded status marks degraded",
			operators: []*configv1.ClusterOperator{
				newTestClusterOperatorWithStatuses("etcd", configv1.ConditionTrue, configv1.ConditionFalse, configv1.ConditionUnknown),
			},
			wantStable:               false,
			wantUnavailableOperators: nil,
			wantProgressingOperators: nil,
			wantDegradedOperators:    []string{"etcd"},
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
			if !equalStringSlices(summary.UnavailableOperators, tt.wantUnavailableOperators) {
				t.Fatalf("UnavailableOperators = %v, want %v", summary.UnavailableOperators, tt.wantUnavailableOperators)
			}
			if !equalStringSlices(summary.ProgressingOperators, tt.wantProgressingOperators) {
				t.Fatalf("ProgressingOperators = %v, want %v", summary.ProgressingOperators, tt.wantProgressingOperators)
			}
			if !equalStringSlices(summary.DegradedOperators, tt.wantDegradedOperators) {
				t.Fatalf("DegradedOperators = %v, want %v", summary.DegradedOperators, tt.wantDegradedOperators)
			}
		})
	}
}

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}

	gotCopy := append([]string(nil), got...)
	wantCopy := append([]string(nil), want...)
	sort.Strings(gotCopy)
	sort.Strings(wantCopy)

	for i := range gotCopy {
		if gotCopy[i] != wantCopy[i] {
			return false
		}
	}

	return true
}
