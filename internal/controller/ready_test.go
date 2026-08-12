package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

func TestEnsureReadyRequeuesWhenOperatorProgressing(t *testing.T) {
	operator := &configv1.ClusterOperator{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd"},
		Status: configv1.ClusterOperatorStatus{
			Conditions: []configv1.ClusterOperatorStatusCondition{
				{Type: configv1.OperatorAvailable, Status: configv1.ConditionTrue},
				{Type: configv1.OperatorProgressing, Status: configv1.ConditionTrue},
				{Type: configv1.OperatorDegraded, Status: configv1.ConditionFalse},
			},
		},
	}
	infra := newInfrastructureForReadyTest([]string{"target.example.com"})

	reconciler := &VmwareCloudFoundationMigrationReconciler{
		ConfigClient: configfake.NewClientset(operator, infra),
		Recorder:     record.NewFakeRecorder(10),
	}
	migration := newMigrationForReadyTest([]string{"target.example.com"})

	result, err := reconciler.ensureReady(context.Background(), migration)
	if err != nil {
		t.Fatalf("ensureReady returned error: %v", err)
	}

	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, 30*time.Second)
	}

	cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
	if cond == nil {
		t.Fatal("expected ready condition to be set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("ready condition status = %s, want %s", cond.Status, metav1.ConditionFalse)
	}
	if !strings.Contains(cond.Message, "progressing=etcd") {
		t.Fatalf("ready condition message = %q, want to contain progressing operator", cond.Message)
	}
}

func TestEnsureReadySetsConditionTrueWhenOperatorsStable(t *testing.T) {
	operator := &configv1.ClusterOperator{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd"},
		Status: configv1.ClusterOperatorStatus{
			Conditions: []configv1.ClusterOperatorStatusCondition{
				{Type: configv1.OperatorAvailable, Status: configv1.ConditionTrue},
				{Type: configv1.OperatorProgressing, Status: configv1.ConditionFalse},
				{Type: configv1.OperatorDegraded, Status: configv1.ConditionFalse},
			},
		},
	}
	infra := newInfrastructureForReadyTest([]string{"target.example.com"})

	reconciler := &VmwareCloudFoundationMigrationReconciler{
		ConfigClient: configfake.NewClientset(operator, infra),
		Recorder:     record.NewFakeRecorder(10),
	}
	migration := newMigrationForReadyTest([]string{"target.example.com"})

	result, err := reconciler.ensureReady(context.Background(), migration)
	if err != nil {
		t.Fatalf("ensureReady returned error: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %s, want 0", result.RequeueAfter)
	}

	cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
	if cond == nil {
		t.Fatal("expected ready condition to be set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("ready condition status = %s, want %s", cond.Status, metav1.ConditionTrue)
	}
	if migration.Status.CompletionTime == nil {
		t.Fatal("expected completion time to be set")
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
