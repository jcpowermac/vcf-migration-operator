package vsphere

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gopkg.in/yaml.v2"
	vsphereconfig "k8s.io/cloud-provider-vsphere/pkg/common/config"
)

const (
	kubeCloudConfigName      = "kube-cloud-config"
	kubeCloudConfigNamespace = "openshift-config-managed"

	cloudConfNamespace = "openshift-cloud-controller-manager"

	cccmoOperatorNamespace = "openshift-cloud-controller-manager-operator"
	ccmNamespace           = "openshift-cloud-controller-manager"
)

type cloudProviderConfigYAML struct {
	Global  vsphereconfig.GlobalYAML                          `yaml:"global"`
	Vcenter map[string]*vsphereconfig.VirtualCenterConfigYAML `yaml:"vcenter"`
	Labels  vsphereconfig.LabelsYAML                          `yaml:"labels"`
}

func getKubeCloudConfig(ctx context.Context, client kubernetes.Interface) (*corev1.ConfigMap, error) {
	cm, err := client.CoreV1().ConfigMaps(kubeCloudConfigNamespace).Get(ctx, kubeCloudConfigName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting %s/%s: %w", kubeCloudConfigNamespace, kubeCloudConfigName, err)
	}
	return cm, nil
}

func getCloudConfConfigMaps(ctx context.Context, client kubernetes.Interface) (*corev1.ConfigMapList, error) {
	cms, err := client.CoreV1().ConfigMaps(cloudConfNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing configmaps in %s: %w", cloudConfNamespace, err)
	}
	return cms, nil
}

func parseVCenterServersFromConfig(cm *corev1.ConfigMap, key string) ([]string, error) {
	raw, ok := cm.Data[key]
	if !ok {
		return nil, fmt.Errorf("key %q not found in ConfigMap %s/%s", key, cm.Namespace, cm.Name)
	}

	cfg := &cloudProviderConfigYAML{
		Vcenter: make(map[string]*vsphereconfig.VirtualCenterConfigYAML),
	}
	if err := yaml.Unmarshal([]byte(raw), cfg); err != nil {
		return nil, fmt.Errorf("unmarshalling cloud config from %s/%s key %q: %w", cm.Namespace, cm.Name, key, err)
	}

	servers := make([]string, 0, len(cfg.Vcenter))
	for server := range cfg.Vcenter {
		servers = append(servers, server)
	}
	return servers, nil
}

func checkPodsHealthy(ctx context.Context, client kubernetes.Interface, namespace string) error {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing pods in %s: %w", namespace, err)
	}

	if len(pods.Items) == 0 {
		return fmt.Errorf("no pods found in %s", namespace)
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("pod %s/%s is %s, not Running", namespace, pod.Name, pod.Status.Phase)
		}
		ready := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			return fmt.Errorf("pod %s/%s is not Ready", namespace, pod.Name)
		}
	}
	return nil
}
