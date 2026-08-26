package metadata

import (
	"context"
	"encoding/json"
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// MetadataKey is the key used to store the metadata JSON in the Secret.
const MetadataKey = "metadata.json"

const (
	metadataSecretLabelKey   = "migration.openshift.io/metadata"
	metadataSecretLabelValue = "true"
)

// ClusterMetadata describes the installer-compatible metadata.json stored in the
// migration metadata Secret.
//
// The Secret is named {migration-name}-metadata in the migration namespace and
// contains a single key, metadata.json. The JSON layout matches the installer's
// vSphere ClusterMetadata structure so openshift-install destroy cluster can use
// it directly. The Secret is created or updated during the SourceCleaned phase
// after source vCenter removal. It is labeled migration.openshift.io/metadata=true
// and intentionally has no owner reference so it remains available for manual
// teardown after the migration CR is deleted.
type ClusterMetadata struct {
	// ClusterName is the human-readable cluster name.
	ClusterName string `json:"clusterName"`
	// ClusterID is the unique cluster identifier.
	ClusterID string `json:"clusterID"`
	// InfraID is the infrastructure identifier used to name cloud resources.
	InfraID string `json:"infraID"`
	// VSphere contains the vSphere-specific metadata expected by the installer.
	VSphere *VSphereMetadata `json:"vsphere,omitempty"`
}

// VSphereMetadata contains the installer-compatible vSphere metadata payload.
type VSphereMetadata struct {
	// VCenter is the primary vCenter server address.
	VCenter string `json:"vCenter,omitempty"`
	// Username is the primary vCenter username.
	Username string `json:"username,omitempty"`
	// Password is the primary vCenter password.
	Password string `json:"password,omitempty"`
	// TerraformPlatform identifies the Terraform provider platform.
	TerraformPlatform string `json:"terraform_platform"`
	// VCenters lists every distinct vCenter the cluster uses.
	VCenters []VCenter `json:"VCenters"`
}

// VCenter contains installer-compatible credentials for an individual vCenter.
type VCenter struct {
	// VCenter is the vCenter FQDN or IP address.
	VCenter string `json:"vCenter"`
	// Username is the vCenter login username.
	Username string `json:"username"`
	// Password is the vCenter login password.
	Password string `json:"password"`
}

// MetadataManager generates and persists installer metadata.
type MetadataManager struct {
	kubeClient kubernetes.Interface
}

// NewMetadataManager creates a new MetadataManager with the given Kubernetes client.
func NewMetadataManager(kubeClient kubernetes.Interface) *MetadataManager {
	return &MetadataManager{kubeClient: kubeClient}
}

// GenerateMetadata builds a ClusterMetadata object from the given failure domains,
// Infrastructure resource, and credentials. The credentials map is keyed by
// vCenter server and the values are formatted as "username:password".
// Distinct vCenter servers produce distinct VCenters entries.
func (m *MetadataManager) GenerateMetadata(
	ctx context.Context,
	failureDomains []configv1.VSpherePlatformFailureDomainSpec,
	infra *configv1.Infrastructure,
	credentials map[string]string,
) (*ClusterMetadata, error) {
	log := klog.FromContext(ctx)
	log.V(2).Info("generating installer metadata")

	if infra == nil {
		return nil, fmt.Errorf("infrastructure must not be nil")
	}
	if len(failureDomains) == 0 {
		return nil, fmt.Errorf("failure domains must not be empty")
	}
	if infra.Status.InfrastructureName == "" {
		return nil, fmt.Errorf("infrastructure status has no InfrastructureName")
	}

	md := &ClusterMetadata{
		ClusterName: infra.Name,
		ClusterID:   string(infra.UID),
		InfraID:     infra.Status.InfrastructureName,
		VSphere: &VSphereMetadata{
			TerraformPlatform: "vsphere",
		},
	}

	seenServers := make(map[string]struct{}, len(failureDomains))

	// Build vCenter entries from failure domains.
	for i := range failureDomains {
		fd := &failureDomains[i]
		cred, ok := credentials[fd.Server]
		if !ok {
			return nil, fmt.Errorf("credentials for %s not found", fd.Server)
		}
		username, password, err := parseRequiredCredentials(fd.Server, cred)
		if err != nil {
			return nil, err
		}

		if i == 0 {
			md.VSphere.VCenter = fd.Server
			md.VSphere.Username = username
			md.VSphere.Password = password
		}

		if _, exists := seenServers[fd.Server]; exists {
			continue
		}
		seenServers[fd.Server] = struct{}{}

		vc := VCenter{
			VCenter:  fd.Server,
			Username: username,
			Password: password,
		}
		md.VSphere.VCenters = append(md.VSphere.VCenters, vc)
	}

	if md.VSphere.VCenter == "" || md.VSphere.Username == "" || md.VSphere.Password == "" {
		return nil, fmt.Errorf("primary vcenter credentials are incomplete")
	}
	if len(md.VSphere.VCenters) == 0 {
		return nil, fmt.Errorf("no vcenters available for metadata")
	}

	log.V(2).Info("generated metadata", "vcenterCount", len(md.VSphere.VCenters))
	return md, nil
}

// SaveToSecret persists the metadata as JSON in a Secret with the given
// namespace and name. If the Secret already exists, it is updated.
// A Secret is used instead of a ConfigMap because the metadata contains
// vCenter credentials.
func (m *MetadataManager) SaveToSecret(ctx context.Context, md *ClusterMetadata, namespace, name string) error {
	log := klog.FromContext(ctx)
	log.V(2).Info("saving metadata to Secret", "namespace", namespace, "name", name)

	data, err := json.Marshal(md)
	if err != nil {
		return fmt.Errorf("marshalling metadata: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				metadataSecretLabelKey: metadataSecretLabelValue,
			},
			OwnerReferences: nil,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			MetadataKey: data,
		},
	}

	existing, err := m.kubeClient.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		existing.OwnerReferences = nil
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[metadataSecretLabelKey] = metadataSecretLabelValue
		existing.Data = secret.Data
		existing.Type = corev1.SecretTypeOpaque
		if _, err := m.kubeClient.CoreV1().Secrets(namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating metadata Secret %s/%s: %w", namespace, name, err)
		}
	} else if apierrors.IsNotFound(err) {
		if _, err := m.kubeClient.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating metadata Secret %s/%s: %w", namespace, name, err)
		}
	} else {
		return fmt.Errorf("getting metadata Secret %s/%s: %w", namespace, name, err)
	}

	log.V(2).Info("saved metadata to Secret", "namespace", namespace, "name", name)
	return nil
}

// GetMetadataFromSecret reads the metadata JSON from a Secret.
func (m *MetadataManager) GetMetadataFromSecret(ctx context.Context, namespace, name string) ([]byte, error) {
	secret, err := m.kubeClient.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting metadata Secret %s/%s: %w", namespace, name, err)
	}
	data, ok := secret.Data[MetadataKey]
	if !ok {
		return nil, fmt.Errorf("key %q not found in Secret %s/%s", MetadataKey, namespace, name)
	}
	return data, nil
}

// GetMetadataSecretName returns the conventional Secret name for storing
// migration metadata based on the migration resource name.
func GetMetadataSecretName(migrationName string) string {
	return fmt.Sprintf("%s-metadata", migrationName)
}

// parseCredentials splits a "username:password" string into its components.
// If the format is unexpected, both values are returned empty.
func parseCredentials(cred string) (username, password string) {
	if cred == "" {
		return "", ""
	}
	parts := splitFirst(cred, ':')
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func parseRequiredCredentials(server, cred string) (string, string, error) {
	username, password := parseCredentials(cred)
	if username == "" || password == "" {
		return "", "", fmt.Errorf("credentials for %s must be formatted as username:password", server)
	}
	return username, password, nil
}

// splitFirst splits s on the first occurrence of sep.
func splitFirst(s string, sep byte) []string {
	idx := -1
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			idx = i
			break
		}
	}
	if idx < 0 {
		return []string{s}
	}
	return []string{s[:idx], s[idx+1:]}
}
