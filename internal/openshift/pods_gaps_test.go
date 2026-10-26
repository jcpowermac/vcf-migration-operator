package openshift

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/openshift/vcf-migration-operator/internal/execution"
)

func TestDeletePodsByLabelErrors(t *testing.T) {
	ctx := context.Background()
	ns := "openshift-cluster-csi-drivers"

	t.Run("list error", func(t *testing.T) {
		sentinel := errors.New("list sentinel")
		client := fakekube.NewSimpleClientset()
		client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		m := NewPodManager(client)
		if _, err := m.DeletePodsByLabel(ctx, ns, map[string]string{"app": "test"}); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped list sentinel", err)
		}
	})

	t.Run("per-pod delete error continues and remaining pods deleted", func(t *testing.T) {
		pod1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testPodName, Namespace: ns, Labels: map[string]string{"app": "test"}}}
		pod2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: ns, Labels: map[string]string{"app": "test"}}}
		client := fakekube.NewSimpleClientset(pod1, pod2)
		// pod-1 delete fails; pod-2 falls through to the default (success).
		client.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if a, ok := action.(k8stesting.DeleteAction); ok && a.GetName() == testPodName {
				return true, nil, errors.New("delete pod-1 failed")
			}
			return false, nil, nil
		})
		m := NewPodManager(client)
		deleted, err := m.DeletePodsByLabel(ctx, ns, map[string]string{"app": "test"})
		if err != nil {
			t.Fatalf("DeletePodsByLabel: %v", err)
		}
		if deleted != 1 {
			t.Errorf("deleted = %d, want 1 (pod-1 failed, pod-2 deleted)", deleted)
		}
	})

	t.Run("execution check error", func(t *testing.T) {
		sentinel := errors.New("exec sentinel")
		execCtx := execution.WithCheck(context.Background(), func(context.Context) error { return sentinel })
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testPodName, Namespace: ns, Labels: map[string]string{"app": "test"}}}
		client := fakekube.NewSimpleClientset(pod)
		m := NewPodManager(client)
		if _, err := m.DeletePodsByLabel(execCtx, ns, map[string]string{"app": "test"}); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped execution sentinel", err)
		}
	})
}

func TestRestartVSpherePodsNotFoundSkips(t *testing.T) {
	ctx := context.Background()

	t.Run("all namespaces not found skips all", func(t *testing.T) {
		client := fakekube.NewSimpleClientset()
		client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "pods")
		})
		m := NewPodManager(client)
		if err := m.RestartVSpherePods(ctx); err != nil {
			t.Fatalf("RestartVSpherePods: %v (all namespaces should be skipped)", err)
		}
	})

	t.Run("non not-found list error returns wrapped error", func(t *testing.T) {
		sentinel := errors.New("list sentinel")
		client := fakekube.NewSimpleClientset()
		client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		m := NewPodManager(client)
		err := m.RestartVSpherePods(ctx)
		if err == nil || !strings.Contains(err.Error(), "restarting pods in") {
			t.Fatalf("error = %v, want wrapped 'restarting pods in' message", err)
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped list sentinel", err)
		}
	})
}

func TestCheckVSpherePodsReadyNotFoundSkips(t *testing.T) {
	ctx := context.Background()

	t.Run("all namespaces not found skips all and reports ready", func(t *testing.T) {
		client := fakekube.NewSimpleClientset()
		client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "pods")
		})
		m := NewPodManager(client)
		status, err := m.CheckVSpherePodsReady(ctx)
		if err != nil {
			t.Fatalf("CheckVSpherePodsReady: %v", err)
		}
		if !status.AllReady {
			t.Error("expected AllReady=true when every namespace is skipped")
		}
	})

	t.Run("non not-found list error returns wrapped error", func(t *testing.T) {
		sentinel := errors.New("list sentinel")
		client := fakekube.NewSimpleClientset()
		client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, sentinel
		})
		m := NewPodManager(client)
		if _, err := m.CheckVSpherePodsReady(ctx); !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want wrapped list sentinel", err)
		}
	})
}
