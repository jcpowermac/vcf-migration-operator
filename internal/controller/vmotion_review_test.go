package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

func TestVMotionInventoryRetriesNodeIdentityDrift(t *testing.T) {
	for _, change := range []string{"addition", "removal", "uid", "providerID", "role"} {
		for _, frozen := range []bool{false, true} {
			t.Run(change+map[bool]string{false: "/initial", true: "/frozen"}[frozen], func(t *testing.T) {
				h := newVMotionPlanHarness(t)
				if frozen {
					h.reconcileUntilFrozen(t)
				} else {
					h.reconcile(t)
				}
				original := h.migration.Status.NodeMigration.DeepCopy()
				list := h.reconciler.vmotionInventoryFunc
				h.reconciler.vmotionInventoryFunc = func(ctx context.Context, vc *configv1.VSpherePlatformVCenterSpec, liveServer string, fds []configv1.VSpherePlatformFailureDomainSpec, ids []string) (vsphere.VMotionInventory, error) {
					inventory, err := list(ctx, vc, liveServer, fds, ids)
					node, getErr := h.kubeClient.CoreV1().Nodes().Get(ctx, "worker-a", metav1.GetOptions{})
					if getErr != nil {
						t.Fatal(getErr)
					}
					switch change {
					case "addition":
						added := newVMotionPlanNode("worker-c", "worker", h.providerIDs[5], "new-node", false)
						_, getErr = h.kubeClient.CoreV1().Nodes().Create(ctx, &added, metav1.CreateOptions{})
						h.expectedInventoryCount++
					case "removal":
						getErr = h.kubeClient.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{})
						h.expectedInventoryCount--
					case "uid":
						node.UID = "replacement-node"
					case "providerID":
						node.Spec.ProviderID = h.providerIDs[5]
					case "role":
						other, e := h.kubeClient.CoreV1().Nodes().Get(ctx, "control-plane-a", metav1.GetOptions{})
						if e != nil {
							t.Fatal(e)
						}
						node.Labels, other.Labels = other.Labels, node.Labels
						_, getErr = h.kubeClient.CoreV1().Nodes().Update(ctx, other, metav1.UpdateOptions{})
					}
					if getErr != nil {
						t.Fatal(getErr)
					}
					if change != "addition" && change != "removal" {
						if _, e := h.kubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); e != nil {
							t.Fatal(e)
						}
					}
					return inventory, err
				}
				_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
				if err == nil || !strings.Contains(err.Error(), "identities changed during inventory") {
					t.Fatalf("Reconcile = %v, want identity drift retry", err)
				}
				if err := h.apiClient.Get(context.Background(), h.key, h.migration); err != nil {
					t.Fatal(err)
				}
				condition := apimeta.FindStatusCondition(h.migration.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
				if condition == nil || condition.Reason != migrationv1alpha1.ReasonRetrying {
					t.Fatalf("condition = %+v, want Retrying", condition)
				}
				if !reflect.DeepEqual(original, h.migration.Status.NodeMigration) {
					t.Fatal("identity drift changed frozen plan")
				}
				h.reconciler.vmotionInventoryFunc = list
				if !frozen {
					h.reconcileUntilFrozen(t)
				}
			})
		}
	}
}

func TestVMotionPlanningInitializesLifecycleStatus(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "eligible", true: "blocked"}[blocked], func(t *testing.T) {
			h := newVMotionPlanHarness(t)
			if blocked {
				if err := h.kubeClient.CoreV1().Nodes().Delete(context.Background(), "control-plane-a", metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			h.migration.Spec.State = migrationv1alpha1.MigrationStatePaused
			if err := h.apiClient.Update(context.Background(), h.migration); err != nil {
				t.Fatal(err)
			}
			h.reconcile(t)
			if h.migration.Status.StartTime != nil {
				t.Fatal("paused migration started clock")
			}
			h.migration.Spec.State = migrationv1alpha1.MigrationStateRunning
			if err := h.apiClient.Update(context.Background(), h.migration); err != nil {
				t.Fatal(err)
			}
			h.reconcile(t)
			if h.migration.Status.StartTime == nil {
				t.Fatal("planning did not set StartTime")
			}
			start := h.migration.Status.StartTime.DeepCopy()
			ready := apimeta.FindStatusCondition(h.migration.Status.Conditions, migrationv1alpha1.ConditionReady)
			if ready == nil || ready.Status != metav1.ConditionFalse {
				t.Fatalf("Ready = %+v, want False", ready)
			}
			h.reconcile(t)
			for _, state := range []migrationv1alpha1.MigrationState{migrationv1alpha1.MigrationStatePaused, migrationv1alpha1.MigrationStateRunning} {
				h.migration.Spec.State = state
				if err := h.apiClient.Update(context.Background(), h.migration); err != nil {
					t.Fatal(err)
				}
				h.reconcile(t)
				if !start.Equal(h.migration.Status.StartTime) {
					t.Fatal("reconcile changed StartTime")
				}
			}
			startedEvents := 0
			recorded := h.reconciler.Recorder.(*events.FakeRecorder).Events
			for len(recorded) > 0 {
				if strings.Contains(<-recorded, "MigrationStarted") {
					startedEvents++
				}
			}
			if startedEvents != 1 {
				t.Fatalf("MigrationStarted events = %d, want 1", startedEvents)
			}
		})
	}
	// A fresh Running CR must seed Ready without relying on pause/resume.
	h := newVMotionPlanHarness(t)
	h.reconcile(t)
	ready := apimeta.FindStatusCondition(h.migration.Status.Conditions, migrationv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || h.migration.Status.StartTime == nil {
		t.Fatalf("initial lifecycle = %+v", h.migration.Status)
	}
}

func TestVMotionPlanningStateFreezesPlanWithoutStartTime(t *testing.T) {
	h := newVMotionPlanHarness(t)
	h.migration.Spec.State = migrationv1alpha1.MigrationStatePlanning
	if err := h.apiClient.Update(context.Background(), h.migration); err != nil {
		t.Fatal(err)
	}
	h.reconcileUntilFrozen(t)
	if h.migration.Status.StartTime != nil {
		t.Fatal("Planning state started the migration clock")
	}
	h.migration.Spec.State = migrationv1alpha1.MigrationStateRunning
	if err := h.apiClient.Update(context.Background(), h.migration); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	if h.migration.Status.StartTime == nil {
		t.Fatal("Running state did not set StartTime after VMotion planning")
	}
}

func TestVMotionInventoryIgnoresNodeListOrder(t *testing.T) {
	h := newVMotionPlanHarness(t)
	h.reconcile(t)
	list := h.reconciler.vmotionInventoryFunc
	h.reconciler.vmotionInventoryFunc = func(ctx context.Context, vc *configv1.VSpherePlatformVCenterSpec, liveServer string, fds []configv1.VSpherePlatformFailureDomainSpec, ids []string) (vsphere.VMotionInventory, error) {
		inventory, err := list(ctx, vc, liveServer, fds, ids)
		nodes, e := h.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if e != nil {
			t.Fatal(e)
		}
		for i := range nodes.Items {
			node := &nodes.Items[len(nodes.Items)-1-i]
			if e := h.kubeClient.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{}); e != nil {
				t.Fatal(e)
			}
			if _, e := h.kubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); e != nil {
				t.Fatal(e)
			}
		}
		return inventory, err
	}
	h.reconcileUntilFrozen(t)
	if h.migration.Status.NodeMigration.Workers.Total != 2 {
		t.Fatal("list reordering changed plan")
	}
}
