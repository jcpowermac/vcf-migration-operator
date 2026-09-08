package openshift

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// VSphereCredsSecretName is the name of the secret containing vSphere credentials.
	VSphereCredsSecretName = "vsphere-creds"
	// VSphereCredsSecretNamespace is the namespace containing the vSphere credentials secret.
	VSphereCredsSecretNamespace = "kube-system"
	// credsUsernameSuffix and credsPasswordSuffix are appended to a vCenter server
	// name to form the per-server credential keys in the secret.
	credsUsernameSuffix = ".username"
	credsPasswordSuffix = ".password"
)

// SecretManager manages OpenShift secrets for vSphere credential operations.
type SecretManager struct {
	client kubernetes.Interface
}

// NewSecretManager creates a new SecretManager with the given Kubernetes client.
func NewSecretManager(client kubernetes.Interface) *SecretManager {
	return &SecretManager{client: client}
}

// GetVSphereCredsSecret retrieves the vsphere-creds secret from kube-system.
func (s *SecretManager) GetVSphereCredsSecret(ctx context.Context) (*corev1.Secret, error) {
	log := klog.FromContext(ctx)
	log.V(2).Info("getting vsphere-creds secret", "namespace", VSphereCredsSecretNamespace, "name", VSphereCredsSecretName)

	secret, err := s.client.CoreV1().Secrets(VSphereCredsSecretNamespace).Get(ctx, VSphereCredsSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting vsphere-creds secret: %w", err)
	}

	return secret, nil
}

// AddTargetVCenterCreds adds credentials for a target vCenter to the vsphere-creds secret.
// If credentials for the server already exist, they are not overwritten. The updated
// secret is persisted and returned.
func (s *SecretManager) AddTargetVCenterCreds(ctx context.Context, secret *corev1.Secret, server, username, password string) (*corev1.Secret, error) {
	log := klog.FromContext(ctx)

	usernameKey := server + credsUsernameSuffix
	passwordKey := server + credsPasswordSuffix

	if _, exists := secret.Data[usernameKey]; exists {
		log.V(2).Info("credentials already exist for server, skipping", "server", server)
		return secret, nil
	}

	log.V(2).Info("adding target vCenter credentials", "server", server)

	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data[usernameKey] = []byte(username)
	secret.Data[passwordKey] = []byte(password)

	updated, err := s.client.CoreV1().Secrets(secret.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("updating vsphere-creds secret with target credentials: %w", err)
	}

	log.V(2).Info("added target vCenter credentials", "server", server)
	return updated, nil
}

// RemoveSourceVCenterCreds removes credentials for a source vCenter from the vsphere-creds
// secret. The updated secret is persisted and returned.
func (s *SecretManager) RemoveSourceVCenterCreds(ctx context.Context, secret *corev1.Secret, server string) (*corev1.Secret, error) {
	log := klog.FromContext(ctx)

	if secret == nil {
		return nil, fmt.Errorf("secret must not be nil")
	}

	usernameKey := server + credsUsernameSuffix
	passwordKey := server + credsPasswordSuffix

	log.V(2).Info("removing source vCenter credentials", "server", server)

	if secret.Data != nil {
		delete(secret.Data, usernameKey)
		delete(secret.Data, passwordKey)
	}

	updated, err := s.client.CoreV1().Secrets(secret.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("updating vsphere-creds secret to remove source credentials: %w", err)
	}

	log.V(2).Info("removed source vCenter credentials", "server", server)
	return updated, nil
}

// GetCredentials retrieves the username and password for a given vCenter server
// from the vsphere-creds secret.
func (s *SecretManager) GetCredentials(ctx context.Context, server string) (username, password string, err error) {
	return s.GetVCenterCredsFromSecret(ctx, VSphereCredsSecretNamespace, VSphereCredsSecretName, server)
}

// GetVCenterCredsFromSecret retrieves vCenter credentials from an arbitrary secret
// identified by namespace and name. The credentials are looked up using the key format
// {server}.username and {server}.password.
func (s *SecretManager) GetVCenterCredsFromSecret(ctx context.Context, namespace, name, server string) (username, password string, err error) {
	log := klog.FromContext(ctx)
	log.V(2).Info("getting vCenter credentials from secret", "namespace", namespace, "name", name, "server", server)

	secret, err := s.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
	}

	usernameKey := server + credsUsernameSuffix
	passwordKey := server + credsPasswordSuffix

	usernameBytes, ok := secret.Data[usernameKey]
	if !ok {
		return "", "", fmt.Errorf("key %q not found in secret %s/%s", usernameKey, namespace, name)
	}

	passwordBytes, ok := secret.Data[passwordKey]
	if !ok {
		return "", "", fmt.Errorf("key %q not found in secret %s/%s", passwordKey, namespace, name)
	}

	return string(usernameBytes), string(passwordBytes), nil
}
