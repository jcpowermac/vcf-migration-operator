package openshift

import (
	"context"
	"errors"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/openshift/vcf-migration-operator/internal/execution"
)

// Shared literals across openshift tests, hoisted for goconst.
const (
	testFailureDomain = "fd-1"
	testPodName       = "pod-1"
)

// fdWithTopology returns a failure domain with a full vSphere topology, including
// a network given as a full path (to exercise the path.Base fallback).
func fdWithTopology(server, datacenter, datastore, resourcePool string) *configv1.VSpherePlatformFailureDomainSpec {
	return &configv1.VSpherePlatformFailureDomainSpec{
		Name:   "fd-target",
		Server: server,
		Topology: configv1.VSpherePlatformTopology{
			Datacenter:   datacenter,
			Datastore:    datastore,
			ResourcePool: resourcePool,
			Template:     "/dc-target/vm/template",
			Networks:     []string{"/dc-target/network/VM Network"},
		},
	}
}

func TestScaleMachineSet(t *testing.T) {
	ctx := context.Background()

	t.Run("success sets replicas", func(t *testing.T) {
		client := fakemachineclient.NewClientset(newTestMachineSet("workers-a", 1))
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if err := mgr.ScaleMachineSet(ctx, "workers-a", 3); err != nil {
			t.Fatalf("ScaleMachineSet: %v", err)
		}
		got, err := client.MachineV1beta1().MachineSets(MachineAPINamespace).Get(ctx, "workers-a", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Spec.Replicas == nil || *got.Spec.Replicas != 3 {
			t.Errorf("replicas = %v, want 3", got.Spec.Replicas)
		}
	})

	t.Run("get error", func(t *testing.T) {
		client := fakemachineclient.NewClientset()
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if err := mgr.ScaleMachineSet(ctx, "missing", 3); err == nil || !strings.Contains(err.Error(), "getting machineset") {
			t.Fatalf("error = %v, want get error", err)
		}
	})

	t.Run("execution check error", func(t *testing.T) {
		client := fakemachineclient.NewClientset(newTestMachineSet("workers-a", 1))
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		sentinel := errors.New("exec")
		execCtx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		if err := mgr.ScaleMachineSet(execCtx, "workers-a", 3); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})

	t.Run("update error", func(t *testing.T) {
		client := fakemachineclient.NewClientset(newTestMachineSet("workers-a", 1))
		sentinel := errors.New("update failed")
		client.PrependReactor("update", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		err := mgr.ScaleMachineSet(ctx, "workers-a", 3)
		if err == nil || !strings.Contains(err.Error(), "scaling machineset") || !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped scaling error", err)
		}
	})
}

func TestCheckMachinesDeleted(t *testing.T) {
	ctx := context.Background()

	t.Run("all deleted", func(t *testing.T) {
		client := fakemachineclient.NewClientset(testSelectorMachineSet("workers-a"))
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		allDeleted, remaining, err := mgr.CheckMachinesDeleted(ctx, "workers-a")
		if err != nil {
			t.Fatalf("CheckMachinesDeleted: %v", err)
		}
		if !allDeleted || remaining != 0 {
			t.Errorf("allDeleted=%v remaining=%d, want true/0", allDeleted, remaining)
		}
	})

	t.Run("machines remain", func(t *testing.T) {
		client := fakemachineclient.NewClientset(
			testSelectorMachineSet("workers-a"),
			testMachineWithMSLabel("worker-a-0", "workers-a"),
		)
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		allDeleted, remaining, err := mgr.CheckMachinesDeleted(ctx, "workers-a")
		if err != nil {
			t.Fatalf("CheckMachinesDeleted: %v", err)
		}
		if allDeleted || remaining != 1 {
			t.Errorf("allDeleted=%v remaining=%d, want false/1", allDeleted, remaining)
		}
	})

	t.Run("get error", func(t *testing.T) {
		client := fakemachineclient.NewClientset()
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, _, err := mgr.CheckMachinesDeleted(ctx, "missing"); err == nil {
			t.Fatal("expected get error")
		}
	})

	t.Run("list error", func(t *testing.T) {
		client := fakemachineclient.NewClientset(testSelectorMachineSet("workers-a"))
		client.PrependReactor("list", "machines", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("list failed")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, _, err := mgr.CheckMachinesDeleted(ctx, "workers-a"); err == nil {
			t.Fatal("expected list error")
		}
	})
}

func TestCheckNodesDeletedForMachines(t *testing.T) {
	ctx := context.Background()
	nodeRefMachine := func(name string) *machinev1beta1.Machine {
		m := testMachineWithMSLabel(name, "workers-a")
		m.Status.NodeRef = &corev1.ObjectReference{Name: name + "-node"}
		return m
	}

	t.Run("no machines", func(t *testing.T) {
		client := fakemachineclient.NewClientset(testSelectorMachineSet("workers-a"))
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		allDeleted, remaining, err := mgr.CheckNodesDeletedForMachines(ctx, "workers-a")
		if err != nil {
			t.Fatalf("CheckNodesDeletedForMachines: %v", err)
		}
		if !allDeleted || remaining != 0 {
			t.Errorf("allDeleted=%v remaining=%d, want true/0", allDeleted, remaining)
		}
	})

	t.Run("node still present", func(t *testing.T) {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a-0-node"}}
		client := fakemachineclient.NewClientset(testSelectorMachineSet("workers-a"), nodeRefMachine("worker-a-0"))
		mgr := NewMachineManager(fakekube.NewClientset(node), client, nil)
		allDeleted, remaining, err := mgr.CheckNodesDeletedForMachines(ctx, "workers-a")
		if err != nil {
			t.Fatalf("CheckNodesDeletedForMachines: %v", err)
		}
		if allDeleted || remaining != 1 {
			t.Errorf("allDeleted=%v remaining=%d, want false/1", allDeleted, remaining)
		}
	})

	t.Run("node gone", func(t *testing.T) {
		// Machine has a NodeRef, but the node is absent → Get errors → not counted.
		client := fakemachineclient.NewClientset(testSelectorMachineSet("workers-a"), nodeRefMachine("worker-a-0"))
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		allDeleted, remaining, err := mgr.CheckNodesDeletedForMachines(ctx, "workers-a")
		if err != nil {
			t.Fatalf("CheckNodesDeletedForMachines: %v", err)
		}
		if !allDeleted || remaining != 0 {
			t.Errorf("allDeleted=%v remaining=%d, want true/0 (node gone)", allDeleted, remaining)
		}
	})

	t.Run("nil node ref skipped", func(t *testing.T) {
		client := fakemachineclient.NewClientset(testSelectorMachineSet("workers-a"), testMachineWithMSLabel("worker-a-0", "workers-a"))
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		allDeleted, remaining, err := mgr.CheckNodesDeletedForMachines(ctx, "workers-a")
		if err != nil {
			t.Fatalf("CheckNodesDeletedForMachines: %v", err)
		}
		if !allDeleted || remaining != 0 {
			t.Errorf("allDeleted=%v remaining=%d, want true/0 (nil node ref)", allDeleted, remaining)
		}
	})

	t.Run("get error", func(t *testing.T) {
		client := fakemachineclient.NewClientset()
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, _, err := mgr.CheckNodesDeletedForMachines(ctx, "missing"); err == nil {
			t.Fatal("expected get error")
		}
	})

	t.Run("list error", func(t *testing.T) {
		client := fakemachineclient.NewClientset(testSelectorMachineSet("workers-a"))
		client.PrependReactor("list", "machines", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("list failed")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, _, err := mgr.CheckNodesDeletedForMachines(ctx, "workers-a"); err == nil {
			t.Fatal("expected list error")
		}
	})
}

func TestCreateWorkerMachineSetValidation(t *testing.T) {
	ctx := context.Background()
	fd := fdWithTopology("target.example.com", "dc-target", "ds-target", "rp-target")

	t.Run("nil template", func(t *testing.T) {
		mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(), nil)
		if _, err := mgr.CreateWorkerMachineSet(ctx, "name", 1, fd, nil, "infra"); err == nil || !strings.Contains(err.Error(), "template MachineSet must not be nil") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("nil failureDomain", func(t *testing.T) {
		mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(), nil)
		if _, err := mgr.CreateWorkerMachineSet(ctx, "name", 1, nil, newTestMachineSet("src", 1), "infra"); err == nil || !strings.Contains(err.Error(), "failureDomain must not be nil") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("nil annotations initialized on success", func(t *testing.T) {
		template := newTestMachineSetForVCenter("source", "source.example.com", 1)
		if template.Annotations != nil {
			t.Fatal("precondition: template annotations should be nil")
		}
		mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(), nil)
		created, err := mgr.CreateWorkerMachineSet(ctx, "dest-worker", 2, fd, template, "infra")
		if err != nil {
			t.Fatalf("CreateWorkerMachineSet: %v", err)
		}
		if created.Annotations[SourceMachineSetAnnotation] != "source" {
			t.Errorf("source annotation = %q, want source", created.Annotations[SourceMachineSetAnnotation])
		}
	})

	t.Run("execution check error", func(t *testing.T) {
		template := newTestMachineSetForVCenter("source", "source.example.com", 1)
		client := fakemachineclient.NewClientset()
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		sentinel := errors.New("exec")
		execCtx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		if _, err := mgr.CreateWorkerMachineSet(execCtx, "dest", 1, fd, template, "infra"); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})

	t.Run("provider spec update error", func(t *testing.T) {
		// template without a provider spec → updateMachineSetProviderSpec fails.
		template := newTestMachineSet("src-noprovider", 1)
		mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(), nil)
		if _, err := mgr.CreateWorkerMachineSet(ctx, "dest", 1, fd, template, "infra"); err == nil || !strings.Contains(err.Error(), "updating provider spec") {
			t.Fatalf("error = %v, want provider spec update error", err)
		}
	})

	t.Run("create error", func(t *testing.T) {
		template := newTestMachineSetForVCenter("source", "source.example.com", 1)
		client := fakemachineclient.NewClientset()
		sentinel := errors.New("create failed")
		client.PrependReactor("create", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, err := mgr.CreateWorkerMachineSet(ctx, "dest", 1, fd, template, "infra"); err == nil || !strings.Contains(err.Error(), "creating machineset") || !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped create error", err)
		}
	})
}

func TestMachineSetLookupErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("get machineset error", func(t *testing.T) {
		mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(), nil)
		if _, err := mgr.GetMachineSet(ctx, "missing"); err == nil {
			t.Fatal("expected get error")
		}
	})

	t.Run("list machinesets error", func(t *testing.T) {
		client := fakemachineclient.NewClientset()
		client.PrependReactor("list", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("list failed")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, err := mgr.ListMachineSets(ctx); err == nil {
			t.Fatal("expected list error")
		}
	})

	t.Run("get machinesets by vcenter list error", func(t *testing.T) {
		client := fakemachineclient.NewClientset()
		client.PrependReactor("list", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("list failed")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, err := mgr.GetMachineSetsByVCenter(ctx, "server.example.com"); err == nil {
			t.Fatal("expected list error")
		}
	})

	t.Run("get machinesets by failure domain: nil fd", func(t *testing.T) {
		mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(), nil)
		if _, err := mgr.GetMachineSetsByFailureDomain(ctx, "server.example.com", nil); err == nil || !strings.Contains(err.Error(), "failure domain must not be nil") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("get machinesets by failure domain: list error", func(t *testing.T) {
		client := fakemachineclient.NewClientset()
		client.PrependReactor("list", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("list failed")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, err := mgr.GetMachineSetsByFailureDomain(ctx, "server.example.com", fdWithTopology("server.example.com", "dc", "ds", "rp")); err == nil {
			t.Fatal("expected list error")
		}
	})
}

func TestGetMachineSetsByVCenterSkipsBadProviderSpec(t *testing.T) {
	ctx := context.Background()
	// A machineset whose provider spec is invalid JSON must be skipped, not returned.
	bad := newTestMachineSet("bad", 1)
	bad.Spec.Template.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: []byte(`{bad json`)}
	good := newTestMachineSetForVCenter("good", "server.example.com", 1)
	client := fakemachineclient.NewClientset(bad, good)
	mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
	got, err := mgr.GetMachineSetsByVCenter(ctx, "server.example.com")
	if err != nil {
		t.Fatalf("GetMachineSetsByVCenter: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Fatalf("got %d machinesets, want only 'good'", len(got))
	}
}

func TestAnnotateMachineSetLegacyAggregateErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("get error", func(t *testing.T) {
		mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(), nil)
		if err := mgr.AnnotateMachineSetLegacyAggregate(ctx, "missing"); err == nil {
			t.Fatal("expected get error")
		}
	})

	t.Run("execution check error", func(t *testing.T) {
		client := fakemachineclient.NewClientset(newTestMachineSet("workers-a", 1))
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		sentinel := errors.New("exec")
		execCtx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		if err := mgr.AnnotateMachineSetLegacyAggregate(execCtx, "workers-a"); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})

	t.Run("update error", func(t *testing.T) {
		client := fakemachineclient.NewClientset(newTestMachineSet("workers-a", 1))
		client.PrependReactor("update", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("update failed")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if err := mgr.AnnotateMachineSetLegacyAggregate(ctx, "workers-a"); err == nil || !strings.Contains(err.Error(), "annotating machineset") {
			t.Fatalf("error = %v, want annotate error", err)
		}
	})
}

func TestDeleteMachineSetsErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("delete machineset non-notfound error", func(t *testing.T) {
		client := fakemachineclient.NewClientset(newTestMachineSet("workers-a", 0))
		client.PrependReactor("delete", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("delete failed")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if err := mgr.DeleteMachineSet(ctx, "workers-a"); err == nil || !strings.Contains(err.Error(), "deleting machineset") {
			t.Fatalf("error = %v, want delete error", err)
		}
	})

	t.Run("delete machineset execution check error", func(t *testing.T) {
		client := fakemachineclient.NewClientset(newTestMachineSet("workers-a", 0))
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		sentinel := errors.New("exec")
		execCtx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		if err := mgr.DeleteMachineSet(execCtx, "workers-a"); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})

	t.Run("delete machine sets by vcenter: list error", func(t *testing.T) {
		client := fakemachineclient.NewClientset()
		client.PrependReactor("list", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("list failed")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, err := mgr.DeleteMachineSetsByVCenter(ctx, "server.example.com"); err == nil {
			t.Fatal("expected list error")
		}
	})

	t.Run("delete machine sets by vcenter: delete error", func(t *testing.T) {
		// A zero-replica machineset on the vcenter; the delete itself fails.
		client := fakemachineclient.NewClientset(newTestMachineSetForVCenter("src-workers", "server.example.com", 0))
		client.PrependReactor("delete", "machinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("delete failed")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, err := mgr.DeleteMachineSetsByVCenter(ctx, "server.example.com"); err == nil {
			t.Fatal("expected delete error")
		}
	})
}

func TestUpdateCPMSFailureDomainErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("get CPMS error", func(t *testing.T) {
		mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(), nil)
		if err := mgr.UpdateCPMSFailureDomain(ctx, []string{testFailureDomain}); err == nil || !strings.Contains(err.Error(), "getting ControlPlaneMachineSet") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("CPMS without machine template", func(t *testing.T) {
		replicas := int32(3)
		cpms := &machinev1.ControlPlaneMachineSet{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: MachineAPINamespace},
			Spec: machinev1.ControlPlaneMachineSetSpec{
				State:    machinev1.ControlPlaneMachineSetStateInactive,
				Replicas: &replicas,
				Template: machinev1.ControlPlaneMachineSetTemplate{
					MachineType: machinev1.OpenShiftMachineV1Beta1MachineType,
				},
			},
		}
		mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(cpms), nil)
		err := mgr.UpdateCPMSFailureDomain(ctx, []string{testFailureDomain})
		if err == nil || !strings.Contains(err.Error(), "no machines_v1beta1_machine_openshift_io template") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("execution check error", func(t *testing.T) {
		client := fakemachineclient.NewClientset(newTestCPMS(machinev1.ControlPlaneMachineSetStateInactive, nil))
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		sentinel := errors.New("exec")
		execCtx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		if err := mgr.UpdateCPMSFailureDomain(execCtx, []string{testFailureDomain}); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want execution sentinel", err)
		}
	})

	t.Run("update error", func(t *testing.T) {
		client := fakemachineclient.NewClientset(newTestCPMS(machinev1.ControlPlaneMachineSetStateInactive, nil))
		client.PrependReactor("update", "controlplanemachinesets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("update failed")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if err := mgr.UpdateCPMSFailureDomain(ctx, []string{testFailureDomain}); err == nil || !strings.Contains(err.Error(), "updating ControlPlaneMachineSet") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestCheckControlPlaneRolloutStatusErrors(t *testing.T) {
	ctx := context.Background()
	mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(), nil)
	if _, err := mgr.CheckControlPlaneRolloutStatus(ctx); err == nil || !strings.Contains(err.Error(), "getting ControlPlaneMachineSet") {
		t.Fatalf("error = %v", err)
	}
}

func TestListControlPlaneMachinesErrors(t *testing.T) {
	ctx := context.Background()
	client := fakemachineclient.NewClientset()
	client.PrependReactor("list", "machines", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("list failed")
	})
	mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
	if _, err := mgr.ListControlPlaneMachines(ctx); err == nil {
		t.Fatal("expected list error")
	}
}

func TestIsCPMSGenerationObservedErrors(t *testing.T) {
	ctx := context.Background()
	mgr := NewMachineManager(fakekube.NewClientset(), fakemachineclient.NewClientset(), nil)
	if _, _, _, err := mgr.IsCPMSGenerationObserved(ctx); err == nil {
		t.Fatal("expected get error")
	}
}

func TestListMachinesForMachineSetErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("get error", func(t *testing.T) {
		client := fakemachineclient.NewClientset()
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, err := mgr.ListMachinesForMachineSet(ctx, "missing"); err == nil {
			t.Fatal("expected get error")
		}
	})

	t.Run("list error", func(t *testing.T) {
		client := fakemachineclient.NewClientset(testSelectorMachineSet("workers-a"))
		client.PrependReactor("list", "machines", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("list failed")
		})
		mgr := NewMachineManager(fakekube.NewClientset(), client, nil)
		if _, err := mgr.ListMachinesForMachineSet(ctx, "workers-a"); err == nil || !strings.Contains(err.Error(), "listing machines for machineset") {
			t.Fatalf("error = %v, want list error", err)
		}
	})
}

func TestUpdateMachineSetProviderSpecValidation(t *testing.T) {
	ctx := context.Background()
	fd := fdWithTopology("target.example.com", "dc-target", "ds-target", "rp-target")

	t.Run("nil machineset", func(t *testing.T) {
		if err := updateMachineSetProviderSpec(ctx, nil, fd, "infra"); err == nil || !strings.Contains(err.Error(), "machineset must not be nil") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("nil failure domain", func(t *testing.T) {
		ms := newTestMachineSetForVCenter("src", "server.example.com", 1)
		if err := updateMachineSetProviderSpec(ctx, ms, nil, "infra"); err == nil || !strings.Contains(err.Error(), "failure domain must not be nil") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("no provider spec", func(t *testing.T) {
		ms := newTestMachineSet("src", 1)
		if err := updateMachineSetProviderSpec(ctx, ms, fd, "infra"); err == nil || !strings.Contains(err.Error(), "has no provider spec") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("provider spec without raw value", func(t *testing.T) {
		ms := newTestMachineSet("src", 1)
		ms.Spec.Template.Spec.ProviderSpec.Value = &runtime.RawExtension{}
		if err := updateMachineSetProviderSpec(ctx, ms, fd, "infra"); err == nil || !strings.Contains(err.Error(), "has no raw value") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unmarshal error", func(t *testing.T) {
		ms := newTestMachineSet("src", 1)
		ms.Spec.Template.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: []byte(`{bad json`)}
		if err := updateMachineSetProviderSpec(ctx, ms, fd, "infra"); err == nil || !strings.Contains(err.Error(), "unmarshalling provider spec") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("success mutates all fields", func(t *testing.T) {
		ms := newTestMachineSetForVCenter("src", "source.example.com", 1)
		if err := updateMachineSetProviderSpec(ctx, ms, fd, "infra"); err != nil {
			t.Fatalf("updateMachineSetProviderSpec: %v", err)
		}
		spec, err := ExtractVSphereProviderSpec(ms)
		if err != nil {
			t.Fatalf("ExtractVSphereProviderSpec: %v", err)
		}
		if spec.Workspace.Server != "target.example.com" || spec.Workspace.Datacenter != "dc-target" || spec.Workspace.Datastore != "ds-target" {
			t.Errorf("workspace not updated: %+v", spec.Workspace)
		}
		if spec.Workspace.Folder != "/dc-target/vm/infra" {
			t.Errorf("folder = %q, want /dc-target/vm/infra", spec.Workspace.Folder)
		}
		if len(spec.Network.Devices) == 0 || spec.Network.Devices[0].NetworkName != "VM Network" {
			t.Errorf("network name = %+v, want path-based fallback to VM Network", spec.Network.Devices)
		}
	})

	t.Run("success with explicit folder", func(t *testing.T) {
		fdFolder := fdWithTopology("target.example.com", "dc-target", "ds-target", "rp-target")
		fdFolder.Topology.Folder = "/dc-target/folder"
		ms := newTestMachineSetForVCenter("src", "source.example.com", 1)
		if err := updateMachineSetProviderSpec(ctx, ms, fdFolder, "infra"); err != nil {
			t.Fatalf("updateMachineSetProviderSpec: %v", err)
		}
		spec, err := ExtractVSphereProviderSpec(ms)
		if err != nil {
			t.Fatalf("ExtractVSphereProviderSpec: %v", err)
		}
		if spec.Workspace.Folder != "/dc-target/folder" {
			t.Errorf("folder = %q, want /dc-target/folder", spec.Workspace.Folder)
		}
	})
}

func TestExtractVSphereProviderSpecValidation(t *testing.T) {
	t.Run("nil machineset", func(t *testing.T) {
		if _, err := ExtractVSphereProviderSpec(nil); err == nil || !strings.Contains(err.Error(), "machineset must not be nil") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("no provider spec", func(t *testing.T) {
		ms := newTestMachineSet("src", 1)
		if _, err := ExtractVSphereProviderSpec(ms); err == nil || !strings.Contains(err.Error(), "has no provider spec") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("no raw value", func(t *testing.T) {
		ms := newTestMachineSet("src", 1)
		ms.Spec.Template.Spec.ProviderSpec.Value = &runtime.RawExtension{}
		if _, err := ExtractVSphereProviderSpec(ms); err == nil || !strings.Contains(err.Error(), "has no raw value") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unmarshal error", func(t *testing.T) {
		ms := newTestMachineSet("src", 1)
		ms.Spec.Template.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: []byte(`{bad json`)}
		if _, err := ExtractVSphereProviderSpec(ms); err == nil || !strings.Contains(err.Error(), "unmarshalling provider spec") {
			t.Fatalf("error = %v", err)
		}
	})
}
