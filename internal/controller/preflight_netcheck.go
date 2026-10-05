package controller

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/vmware/govmomi/object"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

const (
	// preflightNetcheckTimeout bounds the probe-VM networking check, which
	// includes guest boot and network configuration.
	preflightNetcheckTimeout = 10 * time.Minute

	// probeNamePrefix is the deterministic name prefix for netcheck probe VMs.
	probeNamePrefix = "netcheck-"

	// probeNameMaxLen is the maximum probe VM name length.
	probeNameMaxLen = 80
)

var probeNameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// probeName builds the deterministic probe VM name for a failure domain:
// netcheck-<infraID>-<sanitized fd name>, capped at probeNameMaxLen.
func probeName(infraID, fdName string) string {
	name := strings.Trim(fmt.Sprintf("%s%s-%s", probeNamePrefix, infraID, probeNameUnsafe.ReplaceAllString(strings.ToLower(fdName), "-")), "-")
	if len(name) > probeNameMaxLen {
		name = name[:probeNameMaxLen]
	}
	return name
}

// describeFDMismatch formats the preflight failure detail for one failure domain.
func describeFDMismatch(fdName string, probe, sources []vsphere.NetworkInfo) string {
	return fmt.Sprintf("failure domain %q: probe network %s does not match any source node network (source networks: %s); IP connectivity would not be preserved when recreating source machines on the destination vCenter",
		fdName, formatNetworks(probe), formatNetworks(sources))
}

// formatNetworks renders networks as "a.b.c.d/p (gateway x.y.z.w)" entries,
// de-duplicated and joined with commas.
func formatNetworks(nets []vsphere.NetworkInfo) string {
	seen := make(map[string]bool, len(nets))
	parts := make([]string, 0, len(nets))
	for _, n := range nets {
		ip, err := netip.ParseAddr(n.IP)
		if err != nil {
			continue
		}
		pfx, err := ip.Prefix(n.Prefix)
		if err != nil {
			continue
		}
		desc := fmt.Sprintf("%s (gateway %s)", pfx.Masked().String(), n.Gateway)
		if !seen[desc] {
			seen[desc] = true
			parts = append(parts, desc)
		}
	}
	return strings.Join(parts, ", ")
}

// checkNetworkingViaProbeVMs validates that each destination failure domain's
// networking is compatible with the source node networking: it clones a probe
// VM onto each FD, waits for its guest networks, and compares them against the
// networks observed on the source node VMs. Any mismatch is a hard,
// non-transient preflight failure.
func (r *VmwareCloudFoundationMigrationReconciler) checkNetworkingViaProbeVMs(ctx context.Context, migration *migrationv1alpha1.VmwareCloudFoundationMigration, sourceVC *configv1.VSpherePlatformVCenterSpec) error {
	log := klog.FromContext(ctx).V(1)
	condType := migrationv1alpha1.ConditionInfrastructurePrepared

	infraMgr := openshift.NewInfrastructureManager(r.ConfigClient)
	var infraID string
	if err := runPreflightStep(log, "get infrastructure ID", func() error {
		var err error
		infraID, err = infraMgr.GetInfrastructureID(ctx)
		if err != nil {
			return fmt.Errorf("getting infrastructure ID: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	var sourceSession *vsphere.Session
	if err := runPreflightStep(log, "source vCenter network session", func() error {
		sm := openshift.NewSecretManager(r.KubeClient)
		sourceUser, sourcePass, err := sm.GetCredentials(ctx, sourceVC.Server)
		if err != nil {
			return fmt.Errorf("getting source vCenter credentials for %s: %w", sourceVC.Server, err)
		}
		sourceSession, err = getVSphereSession(ctx, sourceVC.Server, sourceVC.Datacenters[0], sourceUser, sourcePass)
		if err != nil {
			return fmt.Errorf("connecting to source vCenter %s: %w", sourceVC.Server, err)
		}
		return nil
	}, "server", sourceVC.Server, "datacenter", sourceVC.Datacenters[0]); err != nil {
		return err
	}

	var sources []vsphere.NetworkInfo
	if err := runPreflightStep(log, "collect source node networks", func() error {
		var err error
		sources, err = r.collectSourceNetworks(ctx, sourceSession)
		if err != nil {
			return err
		}
		if len(sources) == 0 {
			return fmt.Errorf("no source node networks could be read from the source vCenter %s; open-vm-tools must be running on source nodes and node names must match VM names", sourceVC.Server)
		}
		log.Info("source node networks collected", "server", sourceVC.Server, "networkCount", len(sources), "networks", formatNetworks(sources))
		return nil
	}, "server", sourceVC.Server); err != nil {
		return err
	}

	probePrefix := probeNamePrefix + infraID + "-"
	targetCreds := make(map[string]credentials)
	var mismatches []string
	for i := range migration.Spec.FailureDomains {
		fd := &migration.Spec.FailureDomains[i]
		r.setCondition(migration, condType, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, fmt.Sprintf("Checking networking for failure domain %q", fd.Name))

		var session *vsphere.Session
		if err := runPreflightStep(log, "target vCenter probe session", func() error {
			creds, ok := targetCreds[fd.Server]
			if !ok {
				username, password, err := getTargetCredentials(ctx, r.KubeClient, migration, fd.Server)
				if err != nil {
					return fmt.Errorf("getting credentials for target %s: %w", fd.Server, err)
				}
				creds = credentials{username: username, password: password}
				targetCreds[fd.Server] = creds
			}
			var err error
			session, err = getVSphereSession(ctx, fd.Server, fd.Topology.Datacenter, creds.username, creds.password)
			if err != nil {
				return fmt.Errorf("connecting to target vCenter %s: %w", fd.Server, err)
			}
			return nil
		}, "failureDomain", fd.Name, "server", fd.Server); err != nil {
			return err
		}

		var destroyedNames []string
		if err := runPreflightStep(log, "reap stale probe VMs", func() error {
			var err error
			destroyedNames, err = session.ReapProbeVMs(ctx, probePrefix)
			if err != nil {
				return fmt.Errorf("reaping stale probe VMs on %s: %w", fd.Server, err)
			}
			return nil
		}, "failureDomain", fd.Name, "server", fd.Server, "namePrefix", probePrefix); err != nil {
			return err
		}
		if len(destroyedNames) > 0 {
			log.Info("reaped stale probe VMs", "server", fd.Server, "names", destroyedNames)
		}

		spec := vsphere.ProbeSpec{
			Name:         probeName(infraID, fd.Name),
			Datacenter:   fd.Topology.Datacenter,
			Cluster:      fd.Topology.ComputeCluster,
			Datastore:    fd.Topology.Datastore,
			ResourcePool: fd.Topology.ResourcePool,
			Folder:       fd.Topology.Folder,
			Template:     fd.Topology.Template,
			Network:      fd.Topology.Networks[0],
		}
		var vm *object.VirtualMachine
		if err := runPreflightStep(log, "create probe VM", func() error {
			var err error
			vm, err = session.CreateProbeVM(ctx, spec)
			if err != nil {
				return fmt.Errorf("creating probe VM %s on failure domain %q: %w", spec.Name, fd.Name, err)
			}
			return nil
		}, "failureDomain", fd.Name, "server", fd.Server, "name", spec.Name, "template", spec.Template, "network", spec.Network); err != nil {
			return err
		}
		destroyProbe := func() error {
			// Each attempt gets its own deadline, independent of both the probe
			// phase and prior cleanup attempts.
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancelCleanup()
			return vsphere.DestroyProbeVM(cleanupCtx, vm)
		}
		probeDestroyed := false
		defer func() {
			if !probeDestroyed {
				_ = runPreflightStep(log, "destroy probe VM", destroyProbe, "failureDomain", fd.Name, "server", fd.Server, "name", spec.Name, "deferred", true)
			}
		}()

		var probeNetworks []vsphere.NetworkInfo
		if err := runPreflightStep(log, "wait for probe guest networks", func() error {
			var err error
			probeNetworks, err = vsphere.WaitForGuestNetworks(ctx, vm)
			if err != nil {
				return fmt.Errorf("waiting for guest networks on probe VM %s: %w", spec.Name, err)
			}
			return nil
		}, "failureDomain", fd.Name, "server", fd.Server, "name", spec.Name); err != nil {
			return err
		}
		log.Info("probe VM guest networks", "name", spec.Name, "networks", formatNetworks(probeNetworks))

		var bad []vsphere.NetworkInfo
		for _, n := range probeNetworks {
			if matched, _ := n.MatchesAny(sources); !matched {
				bad = append(bad, n)
			}
		}
		if len(bad) > 0 {
			mismatch := describeFDMismatch(fd.Name, bad, sources)
			mismatches = append(mismatches, mismatch)
			log.Error(nil, "probe network does not match source node networks", "failureDomain", fd.Name, "detail", mismatch)
		}

		if err := runPreflightStep(log, "destroy probe VM", destroyProbe, "failureDomain", fd.Name, "server", fd.Server, "name", spec.Name); err != nil {
			return fmt.Errorf("destroying probe VM %s: %w", spec.Name, err)
		}
		probeDestroyed = true
	}

	if len(mismatches) > 0 {
		msg := strings.Join(mismatches, "; ")
		r.Recorder.Eventf(migration, nil, "Warning", "NetworkMismatch", "NetworkMismatch", "%s", msg)
		return fmt.Errorf("destination networking preflight failed: %s", msg)
	}
	return nil
}

// collectSourceNetworks reads the observed networks of every node VM on the
// source vCenter. Per-node read failures are logged and skipped.
func (r *VmwareCloudFoundationMigrationReconciler) collectSourceNetworks(ctx context.Context, sourceSession *vsphere.Session) ([]vsphere.NetworkInfo, error) {
	log := klog.FromContext(ctx)
	nodes, err := r.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	var networks []vsphere.NetworkInfo
	for i := range nodes.Items {
		nodeName := nodes.Items[i].Name
		nets, err := sourceSession.GetVMNetworks(ctx, nodeName)
		if err != nil {
			log.V(1).Info("skipping node for source network check", "node", nodeName, "err", err)
			continue
		}
		networks = append(networks, nets...)
	}
	return networks, nil
}
