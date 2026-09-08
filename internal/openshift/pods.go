package openshift

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// PodSelector defines a namespace and label set used to locate pods.
type PodSelector struct {
	// Namespace is the Kubernetes namespace to search.
	Namespace string
	// Labels is the set of label key-value pairs to match.
	Labels map[string]string
}

// VSpherePodsSelectors contains the selectors for all vSphere-related pods that
// need to be restarted after configuration changes.
//
// Supported OpenShift versions: 4.18 through 4.21. Label and naming conventions
// differ across versions, so multiple selectors per component are intentional:
// each selector targets a specific label set; List with a non-matching selector
// returns 0 pods and is harmless. Restart and readiness checks skip selectors
// whose namespace is not found (e.g. a component introduced in a later 4.x).
//
// Components and version notes:
//   - CCM: k8s-app (older) and infrastructure.openshift.io/cloud-controller-manager (newer).
//   - Machine API: api=clusterapi (consistent across 4.18–4.21).
//   - CSI: app=vmware-vsphere-csi-driver-* (older) and app=vsphere-csi-driver-* (newer).
//   - Cluster storage operator: present in 4.x; namespace may not exist on minimal clusters.
//   - CCM operator and kube-controller-manager: standard labels.
const csiDriversNamespace = "openshift-cluster-csi-drivers"

var VSpherePodsSelectors = []PodSelector{
	// Cloud controller manager (vSphere CCM) — 4.18–4.21
	{
		Namespace: "openshift-cloud-controller-manager",
		Labels:    map[string]string{"k8s-app": "vsphere-cloud-controller-manager"},
	},
	{
		Namespace: "openshift-cloud-controller-manager",
		Labels:    map[string]string{"infrastructure.openshift.io/cloud-controller-manager": "CCM"},
	},
	// Cloud controller manager operator
	{
		Namespace: "openshift-cloud-controller-manager-operator",
		Labels:    map[string]string{"name": "cluster-cloud-controller-manager-operator"},
	},
	// Machine API controller — 4.18–4.21
	{
		Namespace: MachineAPINamespace,
		Labels:    map[string]string{"api": "clusterapi"},
	},
	// CSI driver controller — vmware-vsphere-* (4.18/4.19), vsphere-* (4.20+)
	{
		Namespace: csiDriversNamespace,
		Labels:    map[string]string{"app": "vmware-vsphere-csi-driver-controller"},
	},
	{
		Namespace: csiDriversNamespace,
		Labels:    map[string]string{"app": "vsphere-csi-driver-controller"},
	},
	// CSI driver node
	{
		Namespace: csiDriversNamespace,
		Labels:    map[string]string{"app": "vmware-vsphere-csi-driver-node"},
	},
	{
		Namespace: csiDriversNamespace,
		Labels:    map[string]string{"app": "vsphere-csi-driver-node"},
	},
	// Cluster storage operator — skip if namespace not present (e.g. 4.18 minimal)
	{
		Namespace: "openshift-cluster-storage-operator",
		Labels:    map[string]string{"name": "cluster-storage-operator"},
	},
	// Kube controller manager
	{
		Namespace: "openshift-kube-controller-manager",
		Labels:    map[string]string{"app": "kube-controller-manager"},
	},
}

// PodManager manages pod operations for vSphere-related workloads.
type PodManager struct {
	client kubernetes.Interface
}

// NewPodManager creates a new PodManager with the given Kubernetes client.
func NewPodManager(client kubernetes.Interface) *PodManager {
	return &PodManager{client: client}
}

// DeletePodsByLabel deletes all pods matching the given label selector in the
// specified namespace. It returns the number of pods deleted.
func (p *PodManager) DeletePodsByLabel(ctx context.Context, namespace string, labelSelector map[string]string) (int, error) {
	log := klog.FromContext(ctx)
	selector := labels.Set(labelSelector).String()
	log.V(2).Info("deleting pods by label", "namespace", namespace, "selector", selector)

	pods, err := p.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return 0, fmt.Errorf("listing pods in %s with selector %s: %w", namespace, selector, err)
	}

	deleted := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if err := p.client.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			log.V(2).Info("failed to delete pod", "namespace", namespace, "pod", pod.Name, "err", err)
			continue
		}
		deleted++
	}

	log.V(2).Info("deleted pods", "namespace", namespace, "count", deleted)
	return deleted, nil
}

// RestartVSpherePods deletes all vSphere-related pods defined in VSpherePodsSelectors
// to trigger a restart. Supports 4.18–4.21: missing namespaces or labels (e.g. a
// component not present on a given version) are skipped with a log instead of failing.
func (p *PodManager) RestartVSpherePods(ctx context.Context) error {
	log := klog.FromContext(ctx)
	log.V(2).Info("restarting vSphere pods")

	for _, sel := range VSpherePodsSelectors {
		deleted, err := p.DeletePodsByLabel(ctx, sel.Namespace, sel.Labels)
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.V(2).Info("skipping pod restart, namespace or resource not found", "namespace", sel.Namespace, "selector", labels.Set(sel.Labels).String())
				continue
			}
			return fmt.Errorf("restarting pods in %s: %w", sel.Namespace, err)
		}
		if deleted > 0 {
			log.V(2).Info("restarted pods", "namespace", sel.Namespace, "selector", labels.Set(sel.Labels).String(), "count", deleted)
		}
	}

	log.V(2).Info("all vSphere pod restarts initiated")
	return nil
}

// VSpherePodsStatus holds the aggregate readiness status of all vSphere-related pods.
type VSpherePodsStatus struct {
	// AllReady is true when every vSphere-related pod is in a Ready state.
	AllReady bool
	// Components maps component names to their readiness counts.
	Components map[string]ComponentStatus
	// NotReadyReason provides a human-readable summary of why pods are not ready.
	NotReadyReason string
}

// ComponentStatus holds ready and total pod counts for a single component.
type ComponentStatus struct {
	// Ready is the number of pods in a Ready state.
	Ready int32
	// Total is the total number of pods.
	Total int32
}

// CheckVSpherePodsReady performs a non-blocking check of all vSphere-related pods
// and returns their aggregate readiness status. Supports 4.18–4.21: if a
// namespace or selector returns NotFound (e.g. component not present on that
// version), that component is skipped and does not block readiness.
func (p *PodManager) CheckVSpherePodsReady(ctx context.Context) (*VSpherePodsStatus, error) {
	log := klog.FromContext(ctx)
	log.V(2).Info("checking vSphere pods readiness")

	status := &VSpherePodsStatus{
		AllReady:   true,
		Components: make(map[string]ComponentStatus),
	}

	var notReadyParts []string

	for _, sel := range VSpherePodsSelectors {
		selector := labels.Set(sel.Labels).String()
		pods, err := p.client.CoreV1().Pods(sel.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.V(3).Info("skipping readiness check, namespace or resource not found", "namespace", sel.Namespace, "selector", selector)
				continue
			}
			return nil, fmt.Errorf("listing pods in %s with selector %s: %w", sel.Namespace, selector, err)
		}

		componentName := fmt.Sprintf("%s/%s", sel.Namespace, selector)
		var ready int32
		total := int32(len(pods.Items))

		for i := range pods.Items {
			if isPodReady(&pods.Items[i]) {
				ready++
			}
		}

		status.Components[componentName] = ComponentStatus{
			Ready: ready,
			Total: total,
		}

		if ready != total {
			status.AllReady = false
			notReadyParts = append(notReadyParts, fmt.Sprintf("%s: %d/%d ready", componentName, ready, total))
		}
	}

	if !status.AllReady {
		status.NotReadyReason = strings.Join(notReadyParts, "; ")
		log.V(2).Info("vSphere pods not all ready", "reason", status.NotReadyReason)
	} else {
		log.V(2).Info("all vSphere pods are ready")
	}

	return status, nil
}

// isPodReady returns true if the pod has the Ready condition set to True.
func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
