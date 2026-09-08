package openshift

import (
	"context"
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"gopkg.in/yaml.v2"
	vsphereconfig "k8s.io/cloud-provider-vsphere/pkg/common/config"
)

const (
	// CloudProviderConfigMapName is the name of the cloud-provider-config ConfigMap.
	CloudProviderConfigMapName = "cloud-provider-config"
	// CloudProviderConfigMapNamespace is the namespace of the cloud-provider-config ConfigMap.
	CloudProviderConfigMapNamespace = "openshift-config"

	// cloudProviderConfigKey is the key inside the ConfigMap that holds the cloud config YAML.
	cloudProviderConfigKey = "config"
)

// cloudProviderConfigYAML is the on-disk shape of cloud-provider-config with correct
// yaml keys so marshal output matches what the vSphere CCM expects. Uses types from
// k8s.io/cloud-provider-vsphere/pkg/common/config.
type cloudProviderConfigYAML struct {
	Global  vsphereconfig.GlobalYAML                          `yaml:"global"`
	Vcenter map[string]*vsphereconfig.VirtualCenterConfigYAML `yaml:"vcenter"`
	Labels  vsphereconfig.LabelsYAML                          `yaml:"labels"`
}

// ConfigMapManager manages operations on the cloud-provider-config ConfigMap.
type ConfigMapManager struct {
	client kubernetes.Interface
}

// NewConfigMapManager creates a new ConfigMapManager with the given Kubernetes client.
func NewConfigMapManager(client kubernetes.Interface) *ConfigMapManager {
	return &ConfigMapManager{client: client}
}

// GetCloudProviderConfig retrieves the cloud-provider-config ConfigMap from the
// openshift-config namespace.
func (c *ConfigMapManager) GetCloudProviderConfig(ctx context.Context) (*corev1.ConfigMap, error) {
	log := klog.FromContext(ctx)
	log.V(2).Info("getting cloud-provider-config", "namespace", CloudProviderConfigMapNamespace, "name", CloudProviderConfigMapName)

	cm, err := c.client.CoreV1().ConfigMaps(CloudProviderConfigMapNamespace).Get(ctx, CloudProviderConfigMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting cloud-provider-config ConfigMap: %w", err)
	}

	return cm, nil
}

// AddTargetVCenterToConfig adds target vCenter entries to the cloud-provider-config
// ConfigMap based on the provided failure domains. Uses the same config types as
// k8s.io/cloud-provider-vsphere so the CCM reads the config correctly.
func (c *ConfigMapManager) AddTargetVCenterToConfig(ctx context.Context, cm *corev1.ConfigMap, failureDomains []configv1.VSpherePlatformFailureDomainSpec) (*corev1.ConfigMap, error) {
	log := klog.FromContext(ctx)
	log.V(2).Info("adding target vCenter entries to cloud-provider-config")

	cfg, err := parseCloudProviderConfig(cm)
	if err != nil {
		return nil, err
	}

	// Build a map of server -> datacenters from failure domains.
	serverDatacenters := make(map[string]map[string]bool)
	for i := range failureDomains {
		fd := &failureDomains[i]
		if _, ok := serverDatacenters[fd.Server]; !ok {
			serverDatacenters[fd.Server] = make(map[string]bool)
		}
		serverDatacenters[fd.Server][fd.Topology.Datacenter] = true
	}

	for server, dcSet := range serverDatacenters {
		if _, exists := cfg.Vcenter[server]; exists {
			log.V(2).Info("vcenter entry already exists, skipping", "server", server)
			continue
		}

		var datacenters []string
		for dc := range dcSet {
			datacenters = append(datacenters, dc)
		}

		// Use the same structure as k8s.io/cloud-provider-vsphere; credentials
		// come from global secretName/secretNamespace.
		cfg.Vcenter[server] = &vsphereconfig.VirtualCenterConfigYAML{
			VCenterIP:         server,
			VCenterPort:       443,
			InsecureFlag:      true,
			Datacenters:       datacenters,
			RoundTripperCount: 0,
			IPFamilyPriority:  []string{},
		}
		log.V(2).Info("added vcenter entry", "server", server, "datacenters", datacenters)
	}

	updated, err := marshalAndUpdateConfig(ctx, c.client, cm, cfg)
	if err != nil {
		return nil, fmt.Errorf("updating cloud-provider-config after adding target vCenter: %w", err)
	}

	return updated, nil
}

// RemoveSourceVCenterFromConfig removes the source vCenter entry from the
// cloud-provider-config ConfigMap.
func (c *ConfigMapManager) RemoveSourceVCenterFromConfig(ctx context.Context, cm *corev1.ConfigMap, sourceServer string) (*corev1.ConfigMap, error) {
	log := klog.FromContext(ctx)
	log.V(2).Info("removing source vCenter from cloud-provider-config", "server", sourceServer)

	cfg, err := parseCloudProviderConfig(cm)
	if err != nil {
		return nil, err
	}

	delete(cfg.Vcenter, sourceServer)

	updated, err := marshalAndUpdateConfig(ctx, c.client, cm, cfg)
	if err != nil {
		return nil, fmt.Errorf("updating cloud-provider-config after removing source vCenter: %w", err)
	}

	log.V(2).Info("removed source vCenter from cloud-provider-config", "server", sourceServer)
	return updated, nil
}

// parseCloudProviderConfig unmarshals the cloud provider YAML using the same types
// as k8s.io/cloud-provider-vsphere/pkg/common/config. Does not run full validation
// so we can parse configs that temporarily have zero vcenters (e.g. during remove).
func parseCloudProviderConfig(cm *corev1.ConfigMap) (*cloudProviderConfigYAML, error) {
	if cm == nil {
		return nil, fmt.Errorf("configmap must not be nil")
	}
	raw, ok := cm.Data[cloudProviderConfigKey]
	if !ok {
		return nil, fmt.Errorf("key %q not found in cloud-provider-config ConfigMap", cloudProviderConfigKey)
	}

	cfg := &cloudProviderConfigYAML{
		Vcenter: make(map[string]*vsphereconfig.VirtualCenterConfigYAML),
	}
	if err := yaml.Unmarshal([]byte(raw), cfg); err != nil {
		return nil, fmt.Errorf("unmarshalling cloud-provider-config: %w", err)
	}

	if cfg.Vcenter == nil {
		cfg.Vcenter = make(map[string]*vsphereconfig.VirtualCenterConfigYAML)
	}

	// Ensure global has secret ref so the CCM can resolve credentials. Do not
	// modify InsecureFlag or other fields; OpenShift defaults to insecure true
	// and existing config must be preserved.
	if cfg.Global.SecretName == "" && cfg.Global.SecretNamespace == "" {
		cfg.Global.SecretName = VSphereCredsSecretName
		cfg.Global.SecretNamespace = VSphereCredsSecretNamespace
	}

	return cfg, nil
}

// marshalAndUpdateConfig marshals the config with gopkg.in/yaml.v2 (same as
// cloud-provider-vsphere) and updates the ConfigMap in the cluster.
func marshalAndUpdateConfig(ctx context.Context, client kubernetes.Interface, cm *corev1.ConfigMap, cfg *cloudProviderConfigYAML) (*corev1.ConfigMap, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshalling cloud-provider-config: %w", err)
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[cloudProviderConfigKey] = string(data)

	updated, err := client.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("updating cloud-provider-config ConfigMap: %w", err)
	}

	return updated, nil
}
