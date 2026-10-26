package openshift

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakekube "k8s.io/client-go/kubernetes/fake"
)

func newTestPod(name, namespace string, labels map[string]string, ready bool) *corev1.Pod {
	conditions := []corev1.PodCondition{}
	if ready {
		conditions = append(conditions, corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		})
	} else {
		conditions = append(conditions, corev1.PodCondition{
			Type:   corev1.PodReady,
			Status: corev1.ConditionFalse,
		})
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: conditions,
		},
	}
}

func TestIsPodReady(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "ready pod",
			pod:  newTestPod(testPodName, "ns", nil, true),
			want: true,
		},
		{
			name: "not ready pod",
			pod:  newTestPod(testPodName, "ns", nil, false),
			want: false,
		},
		{
			name: "no conditions",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: testPodName, Namespace: "ns"},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPodReady(tt.pod)
			if got != tt.want {
				t.Fatalf("isPodReady = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeletePodsByLabel(t *testing.T) {
	tests := []struct {
		name        string
		pods        []*corev1.Pod
		namespace   string
		selector    map[string]string
		wantDeleted int
	}{
		{
			name: "deletes matching pods",
			pods: []*corev1.Pod{
				newTestPod(testPodName, "test-ns", map[string]string{"app": "test"}, true),
				newTestPod("pod-2", "test-ns", map[string]string{"app": "test"}, true),
				newTestPod("pod-3", "test-ns", map[string]string{"app": "other"}, true),
			},
			namespace:   "test-ns",
			selector:    map[string]string{"app": "test"},
			wantDeleted: 2,
		},
		{
			name: "returns zero when no pods match",
			pods: []*corev1.Pod{
				newTestPod(testPodName, "test-ns", map[string]string{"app": "other"}, true),
			},
			namespace:   "test-ns",
			selector:    map[string]string{"app": "test"},
			wantDeleted: 0,
		},
		{
			name:        "returns zero for empty namespace",
			pods:        nil,
			namespace:   "empty-ns",
			selector:    map[string]string{"app": "test"},
			wantDeleted: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var client *fakekube.Clientset
			if len(tt.pods) > 0 {
				client = fakekube.NewClientset(tt.pods[0])
				for _, p := range tt.pods[1:] {
					_, err := client.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{})
					if err != nil {
						t.Fatalf("failed to create test pod: %v", err)
					}
				}
			} else {
				client = fakekube.NewClientset()
			}

			mgr := NewPodManager(client)

			deleted, err := mgr.DeletePodsByLabel(context.Background(), tt.namespace, tt.selector)
			if err != nil {
				t.Fatalf("DeletePodsByLabel error = %v", err)
			}

			if deleted != tt.wantDeleted {
				t.Fatalf("deleted = %d, want %d", deleted, tt.wantDeleted)
			}
		})
	}
}

func TestRestartVSpherePods(t *testing.T) {
	tests := []struct {
		name string
		pods []*corev1.Pod
	}{
		{
			name: "restarts pods matching vsphere selectors",
			pods: []*corev1.Pod{
				newTestPod("ccm-1", "openshift-cloud-controller-manager",
					map[string]string{"k8s-app": "vsphere-cloud-controller-manager"}, true),
				newTestPod("mapi-1", "openshift-machine-api",
					map[string]string{"api": "clusterapi"}, true),
			},
		},
		{
			name: "succeeds with no pods present",
			pods: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var client *fakekube.Clientset
			if len(tt.pods) > 0 {
				client = fakekube.NewClientset(tt.pods[0])
				for _, p := range tt.pods[1:] {
					_, err := client.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{})
					if err != nil {
						t.Fatalf("failed to create test pod: %v", err)
					}
				}
			} else {
				client = fakekube.NewClientset()
			}

			mgr := NewPodManager(client)
			err := mgr.RestartVSpherePods(context.Background())
			if err != nil {
				t.Fatalf("RestartVSpherePods error = %v", err)
			}

			for _, p := range tt.pods {
				remaining, err := client.CoreV1().Pods(p.Namespace).List(context.Background(), metav1.ListOptions{})
				if err != nil {
					t.Fatalf("listing pods after restart: %v", err)
				}
				for _, rp := range remaining.Items {
					if rp.Name == p.Name {
						t.Fatalf("pod %s/%s should have been deleted", p.Namespace, p.Name)
					}
				}
			}
		})
	}
}

func TestCheckVSpherePodsReady(t *testing.T) {
	tests := []struct {
		name         string
		pods         []*corev1.Pod
		wantAllReady bool
	}{
		{
			name: "all pods ready",
			pods: []*corev1.Pod{
				newTestPod("ccm-1", "openshift-cloud-controller-manager",
					map[string]string{"k8s-app": "vsphere-cloud-controller-manager"}, true),
				newTestPod("mapi-1", "openshift-machine-api",
					map[string]string{"api": "clusterapi"}, true),
			},
			wantAllReady: true,
		},
		{
			name: "some pods not ready",
			pods: []*corev1.Pod{
				newTestPod("ccm-1", "openshift-cloud-controller-manager",
					map[string]string{"k8s-app": "vsphere-cloud-controller-manager"}, true),
				newTestPod("mapi-1", "openshift-machine-api",
					map[string]string{"api": "clusterapi"}, false),
			},
			wantAllReady: false,
		},
		{
			name:         "no pods in any namespace",
			pods:         nil,
			wantAllReady: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var client *fakekube.Clientset
			if len(tt.pods) > 0 {
				client = fakekube.NewClientset(tt.pods[0])
				for _, p := range tt.pods[1:] {
					_, err := client.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{})
					if err != nil {
						t.Fatalf("failed to create test pod: %v", err)
					}
				}
			} else {
				client = fakekube.NewClientset()
			}

			mgr := NewPodManager(client)

			status, err := mgr.CheckVSpherePodsReady(context.Background())
			if err != nil {
				t.Fatalf("CheckVSpherePodsReady error = %v", err)
			}

			if status.AllReady != tt.wantAllReady {
				t.Fatalf("AllReady = %v, want %v (reason: %s)", status.AllReady, tt.wantAllReady, status.NotReadyReason)
			}
		})
	}
}

func TestCheckVSpherePodsReadyComponentStatus(t *testing.T) {
	pods := []*corev1.Pod{
		newTestPod("ccm-1", "openshift-cloud-controller-manager",
			map[string]string{"k8s-app": "vsphere-cloud-controller-manager"}, true),
		newTestPod("ccm-2", "openshift-cloud-controller-manager",
			map[string]string{"k8s-app": "vsphere-cloud-controller-manager"}, false),
	}

	client := fakekube.NewClientset(pods[0])
	for _, p := range pods[1:] {
		_, err := client.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("failed to create test pod: %v", err)
		}
	}

	mgr := NewPodManager(client)
	status, err := mgr.CheckVSpherePodsReady(context.Background())
	if err != nil {
		t.Fatalf("CheckVSpherePodsReady error = %v", err)
	}

	if status.AllReady {
		t.Fatal("expected AllReady=false with a not-ready pod")
	}

	if len(status.Components) == 0 {
		t.Fatal("expected Components map to be populated")
	}

	if status.NotReadyReason == "" {
		t.Fatal("expected NotReadyReason to be non-empty")
	}

	found := false
	for _, cs := range status.Components {
		if cs.Total == 2 && cs.Ready == 1 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a component with 1/2 ready, got %+v", status.Components)
	}
}
