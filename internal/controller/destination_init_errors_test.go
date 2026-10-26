package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	"github.com/vmware/govmomi/simulator"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/execution"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

// msServers are the source/target vCenter servers used by the multi-site tests.
const (
	msSourceServer = "192.168.10.1"
	msTargetServer = "192.168.10.2"
)

var errMultiSiteFault = errors.New("multi-site test fault")

// msEnv builds the full in-memory state for ensureMultiSiteConfigured: a
// source-configured Infrastructure (source vCenter + one source FD, with an
// InfrastructureName), the target-credentials secret, an empty vsphere-creds
// secret, and a minimal valid cloud-provider-config. It returns the reconciler
// and the concrete fakes so tests can inject reactors to fault a specific step.
func msEnv(t *testing.T) (*VmwareCloudFoundationMigrationReconciler, *fakekube.Clientset, *configfake.Clientset, *migrationv1alpha1.VmwareCloudFoundationMigration) {
	t.Helper()
	ns := migrationv1alpha1.SingletonNamespace

	targetCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: tgtCredsName},
		Data: map[string][]byte{
			msTargetServer + ".username": []byte("tu"),
			msTargetServer + ".password": []byte("tp"),
		},
	}
	vsphereCreds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: openshift.VSphereCredsSecretNamespace, Name: openshift.VSphereCredsSecretName},
	}
	cloudConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: openshift.CloudProviderConfigMapNamespace, Name: openshift.CloudProviderConfigMapName},
		Data:       map[string]string{"config": "global: {}"},
	}
	kube := fakekube.NewClientset(targetCreds, vsphereCreds, cloudConfig)

	infra := &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: testInfraName},
		Spec: configv1.InfrastructureSpec{
			PlatformSpec: configv1.PlatformSpec{VSphere: &configv1.VSpherePlatformSpec{
				VCenters: []configv1.VSpherePlatformVCenterSpec{{Server: msSourceServer, Port: 443, Datacenters: []string{testSourceDC}}},
				FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
					{Name: "fd-src", Server: msSourceServer, Region: "region-src", Zone: "zone-src",
						Topology: configv1.VSpherePlatformTopology{Datacenter: testSourceDC, ComputeCluster: "DC0_C0"}},
				},
			}},
		},
		Status: configv1.InfrastructureStatus{InfrastructureName: "ms-infra"},
	}
	cfg := configfake.NewClientset(infra)

	m := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: migrationv1alpha1.SingletonName, UID: types.UID("uid-ms"), Generation: 1},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			State: migrationv1alpha1.MigrationStateRunning,
			TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{
				Name: tgtCredsName,
			},
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "fd-tgt", Server: msTargetServer, Region: "region-tgt", Zone: "zone-tgt",
					Topology: configv1.VSpherePlatformTopology{Datacenter: testTargetDC, ComputeCluster: "DC1_C1"}},
			},
		},
	}

	r := &VmwareCloudFoundationMigrationReconciler{
		ConfigClient: cfg,
		KubeClient:   kube,
		Recorder:     events.NewFakeRecorder(50),
	}
	return r, kube, cfg, m
}

// seedTargetInInfra rewrites the Infrastructure to include the target vCenter and
// its failure domain so hasTargetVCenterConfiguration returns true.
func seedTargetInInfra(t *testing.T, r *VmwareCloudFoundationMigrationReconciler) {
	t.Helper()
	infra, err := r.ConfigClient.ConfigV1().Infrastructures().Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("seedTargetInInfra getting infra: %v", err)
	}
	infra.Spec.PlatformSpec.VSphere.VCenters = append(infra.Spec.PlatformSpec.VSphere.VCenters,
		configv1.VSpherePlatformVCenterSpec{Server: msTargetServer, Port: 443, Datacenters: []string{testTargetDC}})
	infra.Spec.PlatformSpec.VSphere.FailureDomains = append(infra.Spec.PlatformSpec.VSphere.FailureDomains,
		configv1.VSpherePlatformFailureDomainSpec{
			Name: "fd-tgt", Server: msTargetServer, Region: "region-tgt", Zone: "zone-tgt",
			Topology: configv1.VSpherePlatformTopology{Datacenter: testTargetDC, ComputeCluster: "DC1_C1"},
		})
	if _, err := r.ConfigClient.ConfigV1().Infrastructures().Update(context.Background(), infra, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("seedTargetInInfra updating infra: %v", err)
	}
}

func msCondition(m *migrationv1alpha1.VmwareCloudFoundationMigration, condType string) *metav1.Condition {
	return apimeta.FindStatusCondition(m.Status.Conditions, condType)
}

func msSetCondition(m *migrationv1alpha1.VmwareCloudFoundationMigration, condType string, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type: condType, Status: status, Reason: reason, Message: msg,
		ObservedGeneration: m.Generation,
	})
}

func assertMultiSiteErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), want)
	}
}

// TestEnsureMultiSiteConfiguredHappyPath runs the full config path (target not
// yet in the Infrastructure) and asserts the condition flips to True once the
// (absent) vSphere pods are reported ready.
func TestEnsureMultiSiteConfiguredHappyPath(t *testing.T) {
	r, _, _, m := msEnv(t)
	res, err := r.ensureMultiSiteConfigured(context.Background(), m)
	if err != nil {
		t.Fatalf("ensureMultiSiteConfigured: %v", err)
	}
	if !res.IsZero() {
		t.Fatalf("result = %+v, want empty (pods are absent and thus ready)", res)
	}
	cond := msCondition(m, migrationv1alpha1.ConditionMultiSiteConfigured)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("MultiSiteConfigured = %+v, want True", cond)
	}
}

// TestEnsureMultiSiteConfiguredWaitingForPodsReady covers the fast path where the
// target configuration was already applied on a prior reconcile: the condition is
// seeded with WaitingForVSpherePods, so only the readiness check runs.
func TestEnsureMultiSiteConfiguredWaitingForPodsReady(t *testing.T) {
	r, _, _, m := msEnv(t)
	seedTargetInInfra(t, r)
	msSetCondition(m, migrationv1alpha1.ConditionMultiSiteConfigured, metav1.ConditionFalse, reasonWaitingForVSpherePods, "waiting")
	if _, err := r.ensureMultiSiteConfigured(context.Background(), m); err != nil {
		t.Fatalf("ensureMultiSiteConfigured: %v", err)
	}
	cond := msCondition(m, migrationv1alpha1.ConditionMultiSiteConfigured)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("MultiSiteConfigured = %+v, want True", cond)
	}
}

// TestEnsureMultiSiteConfiguredPodsNotReady covers the requeue-while-waiting path:
// a vSphere pod matching a selector exists but is not ready, so the function
// requeues instead of declaring completion.
func TestEnsureMultiSiteConfiguredPodsNotReady(t *testing.T) {
	r, kube, _, m := msEnv(t)
	seedTargetInInfra(t, r)
	msSetCondition(m, migrationv1alpha1.ConditionMultiSiteConfigured, metav1.ConditionFalse, reasonWaitingForVSpherePods, "waiting")
	notReady := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: ccoNamespace,
		Name:      "ccm-0",
		Labels:    map[string]string{"k8s-app": "vsphere-cloud-controller-manager"},
	}}
	if _, err := kube.CoreV1().Pods(ccoNamespace).Create(context.Background(), notReady, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating not-ready pod: %v", err)
	}
	res, err := r.ensureMultiSiteConfigured(context.Background(), m)
	if err != nil {
		t.Fatalf("ensureMultiSiteConfigured: %v", err)
	}
	if res.RequeueAfter != 15*time.Second {
		t.Fatalf("RequeueAfter = %v, want 15s", res.RequeueAfter)
	}
	cond := msCondition(m, migrationv1alpha1.ConditionMultiSiteConfigured)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonWaitingForVSpherePods {
		t.Fatalf("MultiSiteConfigured = %+v, want False/WaitingForVSpherePods", cond)
	}
}

func TestEnsureMultiSiteConfiguredErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("hasTargetVCenterConfiguration infra error", func(t *testing.T) {
		r, _, cfg, m := msEnv(t)
		cfg.PrependReactor("get", "infrastructures", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errMultiSiteFault
		})
		_, err := r.ensureMultiSiteConfigured(ctx, m)
		assertMultiSiteErr(t, err, "getting infrastructure for target vCenter check")
	})

	t.Run("get vsphere-creds secret error", func(t *testing.T) {
		r, kube, _, m := msEnv(t)
		kube.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.(k8stesting.GetAction).GetName() == openshift.VSphereCredsSecretName {
				return true, nil, errMultiSiteFault
			}
			return false, nil, nil
		})
		_, err := r.ensureMultiSiteConfigured(ctx, m)
		assertMultiSiteErr(t, err, "getting vsphere-creds secret")
	})

	t.Run("get target credentials error", func(t *testing.T) {
		r, kube, _, m := msEnv(t)
		kube.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.(k8stesting.GetAction).GetName() == tgtCredsName {
				return true, nil, errMultiSiteFault
			}
			return false, nil, nil
		})
		_, err := r.ensureMultiSiteConfigured(ctx, m)
		assertMultiSiteErr(t, err, "getting credentials for")
	})

	t.Run("add target creds secret update error", func(t *testing.T) {
		r, kube, _, m := msEnv(t)
		kube.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errMultiSiteFault
		})
		_, err := r.ensureMultiSiteConfigured(ctx, m)
		assertMultiSiteErr(t, err, "adding target credentials for")
	})

	t.Run("second infrastructure get error", func(t *testing.T) {
		r, _, cfg, m := msEnv(t)
		gets := 0
		cfg.PrependReactor("get", "infrastructures", func(k8stesting.Action) (bool, runtime.Object, error) {
			gets++
			if gets == 2 {
				return true, nil, errMultiSiteFault
			}
			return false, nil, nil
		})
		_, err := r.ensureMultiSiteConfigured(ctx, m)
		assertMultiSiteErr(t, err, "getting infrastructure:")
	})

	t.Run("add target vcenter infra update error", func(t *testing.T) {
		r, _, cfg, m := msEnv(t)
		cfg.PrependReactor("update", "infrastructures", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errMultiSiteFault
		})
		_, err := r.ensureMultiSiteConfigured(ctx, m)
		assertMultiSiteErr(t, err, "adding target vCenter to infrastructure")
	})

	t.Run("get cloud-provider-config error", func(t *testing.T) {
		r, kube, _, m := msEnv(t)
		kube.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errMultiSiteFault
		})
		_, err := r.ensureMultiSiteConfigured(ctx, m)
		assertMultiSiteErr(t, err, "getting cloud-provider-config")
	})

	t.Run("add target vcenter configmap update error", func(t *testing.T) {
		r, kube, _, m := msEnv(t)
		kube.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errMultiSiteFault
		})
		_, err := r.ensureMultiSiteConfigured(ctx, m)
		assertMultiSiteErr(t, err, "adding target vCenter to cloud-provider-config")
	})

	t.Run("sync controller config (MCO list) error", func(t *testing.T) {
		r, kube, _, m := msEnv(t)
		kube.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.(k8stesting.ListAction).GetNamespace() == "openshift-machine-config-operator" {
				return true, nil, errMultiSiteFault
			}
			return false, nil, nil
		})
		_, err := r.ensureMultiSiteConfigured(ctx, m)
		assertMultiSiteErr(t, err, "syncing controller config")
	})

	t.Run("restart vSphere pods error", func(t *testing.T) {
		r, kube, _, m := msEnv(t)
		kube.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.(k8stesting.ListAction).GetNamespace() == ccoNamespace {
				return true, nil, errMultiSiteFault
			}
			return false, nil, nil
		})
		_, err := r.ensureMultiSiteConfigured(ctx, m)
		assertMultiSiteErr(t, err, "restarting vSphere pods")
	})
}

// failAfterNChecks returns a context whose execution.Check succeeds for the first
// n calls and fails on the (n+1)th, letting a test fault a specific vSphere
// operation (identified by its position in the check sequence) while the earlier
// ones succeed.
func failAfterNChecks(ctx context.Context, n int) context.Context {
	count := 0
	return execution.WithCheck(ctx, func(context.Context) error {
		count++
		if count > n {
			return errors.New("dest-init: execution fault after N checks")
		}
		return nil
	})
}

// destSim is a live govmomi VPX simulator used by the destination-init and
// folder-ownership tests. It mirrors the setup in destination_initialized_test.go.
type destSim struct {
	ctx       context.Context
	host      string
	user      string
	pass      string
	inventory preflightTestInventory
}

func newDestSim(t *testing.T) *destSim {
	t.Helper()
	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("Create simulator model: %v", err)
	}
	t.Cleanup(model.Remove)
	model.Service.TLS = new(tls.Config)
	model.Service.RegisterEndpoints = true
	server := model.Service.NewServer()
	t.Cleanup(server.Close)
	ctx := model.Service.Context
	user := simulator.DefaultLogin.Username()
	pass, ok := simulator.DefaultLogin.Password()
	if !ok {
		t.Fatal("simulator default login missing password")
	}
	vsphere.ClearSessions(ctx)
	t.Cleanup(func() { vsphere.ClearSessions(ctx) })
	inventory := discoverPreflightTestInventory(ctx, t, server.URL, user, pass)
	return &destSim{ctx: ctx, host: server.URL.Host, user: user, pass: pass, inventory: inventory}
}

// destReconciler wires a reconciler to the simulator server with the target-creds
// secret (name secretName) and an Infrastructure whose InfrastructureName is infraID
// (may be empty to fault GetInfrastructureID).
func (s *destSim) reconciler(t *testing.T, secretName, infraID string) *VmwareCloudFoundationMigrationReconciler { //nolint:unparam
	t.Helper()
	kube := fakekube.NewClientset(newTargetCredentialsSecret("default", secretName, s.host, s.user, s.pass))
	cfg := configfake.NewClientset(&configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: testInfraName},
		Status:     configv1.InfrastructureStatus{InfrastructureName: infraID},
	})
	return &VmwareCloudFoundationMigrationReconciler{
		KubeClient:   kube,
		ConfigClient: cfg,
		Recorder:     events.NewFakeRecorder(10),
	}
}

// destMigration builds a single-FD migration pointing at the simulator server.
// overrideCluster, when non-empty, replaces the (valid) cluster path so the
// Finder.Cluster lookup faults.
func (s *destSim) migration(secretName, overrideCluster string) *migrationv1alpha1.VmwareCloudFoundationMigration {
	cluster := s.inventory.clusterPath
	if overrideCluster != "" {
		cluster = overrideCluster
	}
	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Namespace: migrationv1alpha1.SingletonNamespace},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{Name: secretName, Namespace: "default"},
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "fd-a", Server: s.host, Region: "region-a", Zone: "zone-a",
					Topology: configv1.VSpherePlatformTopology{
						Datacenter:     s.inventory.datacenterName,
						ComputeCluster: cluster,
					}},
			},
		},
	}
}

func TestEnsureDestinationInitializedErrors(t *testing.T) {
	s := newDestSim(t)

	t.Run("get infrastructure ID error", func(t *testing.T) {
		r := s.reconciler(t, "target-vcenter-creds", "")
		_, err := r.ensureDestinationInitialized(s.ctx, s.migration("target-vcenter-creds", ""))
		assertMultiSiteErr(t, err, "getting infrastructure ID")
	})

	t.Run("get target credentials error", func(t *testing.T) {
		r := s.reconciler(t, "target-vcenter-creds", "dest-infra")
		// The migration references a secret the KubeClient does not have.
		_, err := r.ensureDestinationInitialized(s.ctx, s.migration("missing-creds", ""))
		assertMultiSiteErr(t, err, "getting credentials for")
	})

	t.Run("vCenter connect error", func(t *testing.T) {
		// A cred-keyed secret for an unreachable server so the lookup succeeds and
		// the connection itself faults.
		kube := fakekube.NewClientset(newTargetCredentialsSecret("default", "target-vcenter-creds", unreachableVCAddr, s.user, s.pass))
		r := &VmwareCloudFoundationMigrationReconciler{
			KubeClient:   kube,
			ConfigClient: configfake.NewClientset(&configv1.Infrastructure{ObjectMeta: metav1.ObjectMeta{Name: testInfraName}, Status: configv1.InfrastructureStatus{InfrastructureName: "dest-infra"}}),
			Recorder:     events.NewFakeRecorder(10),
		}
		m := s.migration("target-vcenter-creds", "")
		m.Spec.FailureDomains[0].Server = unreachableVCAddr
		_, err := r.ensureDestinationInitialized(s.ctx, m)
		assertMultiSiteErr(t, err, "connecting to")
	})

	t.Run("finder cluster error", func(t *testing.T) {
		r := s.reconciler(t, "target-vcenter-creds", "dest-infra")
		_, err := r.ensureDestinationInitialized(s.ctx, s.migration("target-vcenter-creds", "bad-cluster"))
		assertMultiSiteErr(t, err, "finding cluster")
	})
}

// The tag-error branches (region/zone EnsureTagCategory, EnsureTag, and
// AttachFailureDomainTags) are faulted by letting the first N execution checks
// pass and failing the (N+1)th. For a single-FD migration the check sequence is:
// CreateVMFolder, ownership EnsureTagCategory, ownership EnsureTag, ownership
// AttachTag (4), then region EnsureTagCategory (5), region EnsureTag (6), zone
// EnsureTagCategory (7), zone EnsureTag (8), region/zone AttachTag (9, 10).
func TestEnsureDestinationInitializedTagErrors(t *testing.T) {
	cases := []struct {
		name string
		n    int
		want string
	}{
		{"region tag category", 4, "region tag category for failure domain"},
		{"region tag", 5, "region tag for failure domain"},
		{"zone tag category", 6, "zone tag category for failure domain"},
		{"zone tag", 7, "zone tag for failure domain"},
		{"attach failure domain tags", 8, "attaching tags for failure domain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newDestSim(t)
			r := s.reconciler(t, "target-vcenter-creds", "dest-infra")
			_, err := r.ensureDestinationInitialized(failAfterNChecks(s.ctx, tc.n), s.migration("target-vcenter-creds", ""))
			assertMultiSiteErr(t, err, tc.want)
		})
	}
}

// ensureFolderAndOwnership's remaining branches are the CreateVMFolder + fallback
// GetVMFolder failure (618-619) and the EnsureClusterOwnershipTag failure
// (621-622), faulted by failing the first and second execution checks
// respectively. The GetVMFolder non-NotFound (585-586) and ownership-tag-check
// error (590-591) branches have no execution.Check and need a raw API fault to
// reach; they are documented residuals.
func TestEnsureFolderAndOwnershipErrors(t *testing.T) {
	t.Run("create VM folder failure", func(t *testing.T) {
		s := newDestSim(t)
		r := s.reconciler(t, "target-vcenter-creds", "dest-infra")
		session, err := getVSphereSession(s.ctx, s.host, s.inventory.datacenterName, s.user, s.pass)
		if err != nil {
			t.Fatalf("getVSphereSession: %v", err)
		}
		fd := &configv1.VSpherePlatformFailureDomainSpec{Name: "fd-a", Server: s.host, Region: "region-a", Zone: "zone-a",
			Topology: configv1.VSpherePlatformTopology{Datacenter: s.inventory.datacenterName, ComputeCluster: s.inventory.clusterPath}}
		key := serverDC{server: s.host, datacenter: s.inventory.datacenterName}
		m := s.migration("target-vcenter-creds", "")
		err = r.ensureFolderAndOwnership(failAfterNChecks(s.ctx, 0), m, session, fd, "dest-infra", key, make(map[serverDC]bool))
		assertMultiSiteErr(t, err, "creating VM folder")
	})

	t.Run("ensure cluster ownership tag failure", func(t *testing.T) {
		s := newDestSim(t)
		r := s.reconciler(t, "target-vcenter-creds", "dest-infra")
		session, err := getVSphereSession(s.ctx, s.host, s.inventory.datacenterName, s.user, s.pass)
		if err != nil {
			t.Fatalf("getVSphereSession: %v", err)
		}
		fd := &configv1.VSpherePlatformFailureDomainSpec{Name: "fd-a", Server: s.host, Region: "region-a", Zone: "zone-a",
			Topology: configv1.VSpherePlatformTopology{Datacenter: s.inventory.datacenterName, ComputeCluster: s.inventory.clusterPath}}
		key := serverDC{server: s.host, datacenter: s.inventory.datacenterName}
		m := s.migration("target-vcenter-creds", "")
		err = r.ensureFolderAndOwnership(failAfterNChecks(s.ctx, 1), m, session, fd, "dest-infra", key, make(map[serverDC]bool))
		assertMultiSiteErr(t, err, "ensuring cluster ownership tag")
	})
}
