package openshift

import (
	"context"
	"fmt"

	configv1 "github.com/openshift/api/config/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/vcf-migration-operator/internal/execution"
)

const (
	// InfrastructureName is the well-known name of the cluster Infrastructure resource.
	InfrastructureName = "cluster"
)

// InfrastructureManager manages the OpenShift Infrastructure custom resource.
type InfrastructureManager struct {
	client configclient.Interface
}

// NewInfrastructureManager creates a new InfrastructureManager with the given client.
func NewInfrastructureManager(client configclient.Interface) *InfrastructureManager {
	return &InfrastructureManager{
		client: client,
	}
}

// Get retrieves the cluster Infrastructure resource.
func (m *InfrastructureManager) Get(ctx context.Context) (*configv1.Infrastructure, error) {
	infra, err := m.client.ConfigV1().Infrastructures().Get(ctx, InfrastructureName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting infrastructure %q: %w", InfrastructureName, err)
	}
	return infra, nil
}

// GetSourceVCenter returns the first VCenter entry from the Infrastructure spec.
// This is typically the source vCenter in a single-vCenter cluster.
func (m *InfrastructureManager) GetSourceVCenter(ctx context.Context) (*configv1.VSpherePlatformVCenterSpec, error) {
	infra, err := m.Get(ctx)
	if err != nil {
		return nil, err
	}

	if infra.Spec.PlatformSpec.VSphere == nil || len(infra.Spec.PlatformSpec.VSphere.VCenters) == 0 {
		return nil, fmt.Errorf("no vCenters configured in infrastructure spec")
	}

	return &infra.Spec.PlatformSpec.VSphere.VCenters[0], nil
}

// GetSourceFailureDomain returns the first failure domain from the Infrastructure spec.
func (m *InfrastructureManager) GetSourceFailureDomain(ctx context.Context) (*configv1.VSpherePlatformFailureDomainSpec, error) {
	infra, err := m.Get(ctx)
	if err != nil {
		return nil, err
	}

	if infra.Spec.PlatformSpec.VSphere == nil || len(infra.Spec.PlatformSpec.VSphere.FailureDomains) == 0 {
		return nil, fmt.Errorf("no failure domains configured in infrastructure spec")
	}

	return &infra.Spec.PlatformSpec.VSphere.FailureDomains[0], nil
}

// AddTargetVCenter adds target vCenter entries and failure domains to the Infrastructure
// resource. VCenter entries are derived from the failure domains and are only added if
// not already present. The updated Infrastructure is persisted and returned.
func (m *InfrastructureManager) AddTargetVCenter(ctx context.Context, infra *configv1.Infrastructure, failureDomains []configv1.VSpherePlatformFailureDomainSpec) (*configv1.Infrastructure, error) {
	log := klog.FromContext(ctx)
	log.V(2).Info("adding target vCenter to infrastructure")

	if infra.Spec.PlatformSpec.VSphere == nil {
		return nil, fmt.Errorf("infrastructure spec has no vSphere platform configuration")
	}

	// Build set of existing vCenter servers for deduplication.
	existingServers := make(map[string]bool)
	for _, vc := range infra.Spec.PlatformSpec.VSphere.VCenters {
		existingServers[vc.Server] = true
	}

	// Build vCenter entries from failure domains.
	serverDatacenters := make(map[string]map[string]bool)
	for i := range failureDomains {
		fd := &failureDomains[i]
		if _, ok := serverDatacenters[fd.Server]; !ok {
			serverDatacenters[fd.Server] = make(map[string]bool)
		}
		serverDatacenters[fd.Server][fd.Topology.Datacenter] = true
	}

	for server, dcSet := range serverDatacenters {
		if existingServers[server] {
			log.V(2).Info("vCenter already exists in infrastructure, skipping", "server", server)
			continue
		}

		var datacenters []string
		for dc := range dcSet {
			datacenters = append(datacenters, dc)
		}

		infra.Spec.PlatformSpec.VSphere.VCenters = append(infra.Spec.PlatformSpec.VSphere.VCenters, configv1.VSpherePlatformVCenterSpec{
			Server:      server,
			Port:        443,
			Datacenters: datacenters,
		})
		log.V(2).Info("added vCenter entry", "server", server, "datacenters", datacenters)
	}

	// Add failure domains.
	existingFDs := make(map[string]bool)
	for _, fd := range infra.Spec.PlatformSpec.VSphere.FailureDomains {
		existingFDs[fd.Name] = true
	}
	for i := range failureDomains {
		if existingFDs[failureDomains[i].Name] {
			log.V(2).Info("failure domain already exists, skipping", "name", failureDomains[i].Name)
			continue
		}
		infra.Spec.PlatformSpec.VSphere.FailureDomains = append(infra.Spec.PlatformSpec.VSphere.FailureDomains, failureDomains[i])
		log.V(2).Info("added failure domain", "name", failureDomains[i].Name)
	}

	if err := execution.Check(ctx); err != nil {
		return nil, err
	}
	updated, err := m.client.ConfigV1().Infrastructures().Update(ctx, infra, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("updating infrastructure with target vCenter: %w", err)
	}

	return updated, nil
}

// RemoveSourceVCenter removes the vCenter and associated failure domains for the
// given source server from the Infrastructure resource. The updated Infrastructure
// is persisted and returned.
func (m *InfrastructureManager) RemoveSourceVCenter(ctx context.Context, infra *configv1.Infrastructure, sourceServer string) (*configv1.Infrastructure, error) {
	log := klog.FromContext(ctx)
	log.V(2).Info("removing source vCenter from infrastructure", "server", sourceServer)

	if infra.Spec.PlatformSpec.VSphere == nil {
		return nil, fmt.Errorf("infrastructure spec has no vSphere platform configuration")
	}

	// Refuse to remove the last configured vCenter; cleanup must leave the
	// destination vCenters in place, and an empty list would break the cluster.
	if len(infra.Spec.PlatformSpec.VSphere.VCenters) == 1 && infra.Spec.PlatformSpec.VSphere.VCenters[0].Server == sourceServer {
		return nil, fmt.Errorf("refusing to remove the last configured vCenter %q", sourceServer)
	}

	// Remove the vCenter entry.
	var vcenters []configv1.VSpherePlatformVCenterSpec
	for _, vc := range infra.Spec.PlatformSpec.VSphere.VCenters {
		if vc.Server != sourceServer {
			vcenters = append(vcenters, vc)
		}
	}
	infra.Spec.PlatformSpec.VSphere.VCenters = vcenters

	// Remove failure domains that reference the source server.
	var fds []configv1.VSpherePlatformFailureDomainSpec
	for _, fd := range infra.Spec.PlatformSpec.VSphere.FailureDomains {
		if fd.Server != sourceServer {
			fds = append(fds, fd)
		}
	}
	infra.Spec.PlatformSpec.VSphere.FailureDomains = fds

	if err := execution.Check(ctx); err != nil {
		return nil, err
	}
	updated, err := m.client.ConfigV1().Infrastructures().Update(ctx, infra, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("updating infrastructure after removing source vCenter: %w", err)
	}

	log.V(2).Info("removed source vCenter from infrastructure", "server", sourceServer)
	return updated, nil
}

// GetInfrastructureID returns the infrastructure ID (InfrastructureName) from the
// Infrastructure status.
func (m *InfrastructureManager) GetInfrastructureID(ctx context.Context) (string, error) {
	infra, err := m.Get(ctx)
	if err != nil {
		return "", err
	}

	if infra.Status.InfrastructureName == "" {
		return "", fmt.Errorf("infrastructure status has no InfrastructureName")
	}

	return infra.Status.InfrastructureName, nil
}
