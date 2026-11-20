package controller

// Error/mismatch-branch coverage for preflight.go's validation helpers. These
// tests reuse the netcheckTestEnv simulator (Task 9) plus the existing
// preflight_test.go object builders, and drive the fake-client reactors and
// broken-topology paths that the happy-path TestRunPreflightChecks does not.

import (
	"context"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/vmware/govmomi/object"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	machinefake "github.com/openshift/client-go/machine/clientset/versioned/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

// preflightReconciler builds a fully-wired reconciler for preflight error
// tests: clientfake (migration), configfake (infra + cluster version + feature
// gate), fakekube (target + source credential secrets plus extras), machinefake,
// and dynamicfake (default Removed management-state resources plus extras).
func preflightReconciler(t *testing.T, e *netcheckTestEnv, migration *migrationv1alpha1.VmwareCloudFoundationMigration, infra *configv1.Infrastructure, kubeObjs, machineObjs, dynObjs []runtime.Object) *VmwareCloudFoundationMigrationReconciler { //nolint:unparam
	t.Helper()
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding migration to scheme: %v", err)
	}
	client := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(migration).
		WithStatusSubresource(migration).
		Build()

	kubeObjects := append([]runtime.Object{
		newTargetCredentialsSecret("default", "target-vcenter-creds", e.host, e.user, e.pass),
		newTargetCredentialsSecret(openshift.VSphereCredsSecretNamespace, openshift.VSphereCredsSecretName, e.host, e.user, e.pass),
	}, kubeObjs...)

	dynScheme := runtime.NewScheme()
	objs := make([]runtime.Object, 0, 2+len(dynObjs))
	objs = append(objs,
		newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Removed"),
		newManagementStateResource("Storage", "cluster", "Removed"),
	)
	objs = append(objs, dynObjs...)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynScheme, preflightListKinds(), objs...)

	return &VmwareCloudFoundationMigrationReconciler{
		Client:        client,
		ConfigClient:  configfake.NewClientset(infra, newClusterVersionForPreflight("4.18.0", false), newFeatureGateForPreflight("4.18.0", true)),
		KubeClient:    fakekube.NewClientset(kubeObjects...),
		MachineClient: machinefake.NewSimpleClientset(machineObjs...),
		DynamicClient: dyn,
		Recorder:      events.NewFakeRecorder(20),
	}
}

func configClientset(t *testing.T, r *VmwareCloudFoundationMigrationReconciler) *configfake.Clientset {
	t.Helper()
	cc, ok := r.ConfigClient.(*configfake.Clientset)
	if !ok {
		t.Fatalf("ConfigClient is %T, want *configfake.Clientset", r.ConfigClient)
	}
	return cc
}

func machineClientset(t *testing.T, r *VmwareCloudFoundationMigrationReconciler) *machinefake.Clientset {
	t.Helper()
	mc, ok := r.MachineClient.(*machinefake.Clientset)
	if !ok {
		t.Fatalf("MachineClient is %T, want *machinefake.Clientset", r.MachineClient)
	}
	return mc
}

func (e *netcheckTestEnv) preflightSession(t *testing.T) *vsphere.Session {
	t.Helper()
	session, err := getVSphereSession(e.ctx, e.host, e.dcName, e.user, e.pass)
	if err != nil {
		t.Fatalf("creating preflight session: %v", err)
	}
	return session
}

func (e *netcheckTestEnv) preflightDC(t *testing.T) *object.Datacenter {
	t.Helper()
	dc, err := e.finder.DefaultDatacenter(e.ctx)
	if err != nil {
		t.Fatalf("finding datacenter: %v", err)
	}
	return dc
}

func (e *netcheckTestEnv) preflightCluster(t *testing.T) *object.ClusterComputeResource {
	t.Helper()
	clusters, err := e.finder.ClusterComputeResourceList(e.ctx, "*")
	if err != nil || len(clusters) == 0 {
		t.Fatalf("listing clusters: %v", err)
	}
	return clusters[0]
}

// preflightFD returns a valid destination failure domain spec against the
// simulator inventory.
func (e *netcheckTestEnv) preflightFD(t *testing.T, name string) *configv1.VSpherePlatformFailureDomainSpec { //nolint:unparam
	t.Helper()
	return &configv1.VSpherePlatformFailureDomainSpec{
		Name:   name,
		Region: "region",
		Zone:   zoneName,
		Server: e.host,
		Topology: configv1.VSpherePlatformTopology{
			Template:       e.inv.templatePath,
			Datacenter:     e.dcName,
			ComputeCluster: e.inv.clusterPath,
			Datastore:      e.inv.datastorePath,
			Networks:       []string{e.inv.networkPath},
			Folder:         e.inv.folderPath,
		},
	}
}

func TestPreflightValidateMigrationFailureDomains(t *testing.T) {
	t.Run("infra get error", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		r := preflightReconciler(t, e, newMigrationForPreflight(e.host, e.inv), newInfrastructureForPreflight(e.host, e.dcName), nil, nil, nil)
		configClientset(t, r).PrependReactor("get", "infrastructures", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		_, err := r.validateMigrationFailureDomains(e.ctx, newMigrationForPreflight(e.host, e.inv))
		if err == nil || !strings.Contains(err.Error(), "getting infrastructure for failure domain validation") {
			t.Fatalf("err = %v, want infra get error", err)
		}
	})
	t.Run("no InfrastructureName", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		infra := newInfrastructureForPreflight(e.host, e.dcName)
		infra.Status.InfrastructureName = ""
		r := preflightReconciler(t, e, newMigrationForPreflight(e.host, e.inv), infra, nil, nil, nil)
		_, err := r.validateMigrationFailureDomains(e.ctx, newMigrationForPreflight(e.host, e.inv))
		if err == nil || !strings.Contains(err.Error(), "infrastructure status has no InfrastructureName") {
			t.Fatalf("err = %v, want no-InfrastructureName error", err)
		}
	})
	t.Run("unsupported region type", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		infra := newInfrastructureForPreflight(e.host, e.dcName)
		regionType := configv1.ComputeClusterFailureDomainRegion
		infra.Spec.PlatformSpec.VSphere.FailureDomains[0].RegionAffinity = &configv1.VSphereFailureDomainRegionAffinity{Type: regionType}
		r := preflightReconciler(t, e, newMigrationForPreflight(e.host, e.inv), infra, nil, nil, nil)
		_, err := r.validateMigrationFailureDomains(e.ctx, newMigrationForPreflight(e.host, e.inv))
		if err == nil || !strings.Contains(err.Error(), "uses unsupported region type") {
			t.Fatalf("err = %v, want unsupported region type error", err)
		}
	})
}

func TestPreflightValidateDestinationMachineSetCount(t *testing.T) {
	e := newNetcheckTestEnv(t)
	r := preflightReconciler(t, e, newMigrationForPreflight(e.host, e.inv), newInfrastructureForPreflight(e.host, e.dcName), nil, nil, nil)
	machineClientset(t, r).PrependReactor("list", "machinesets", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	mappings := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-a", Destination: "fd-a"}}
	sourceFDs := newInfrastructureForPreflight(e.host, e.dcName).Spec.PlatformSpec.VSphere.FailureDomains
	if err := r.validateDestinationMachineSetCount(e.ctx, mappings, sourceFDs); err == nil || !strings.Contains(err.Error(), "listing machinesets for source failure domain") {
		t.Fatalf("err = %v, want machineset list error", err)
	}
}

func TestPreflightRunPreflightChecksWiring(t *testing.T) {
	t.Run("empty target secret name", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		migration := newMigrationForPreflight(e.host, e.inv)
		migration.Spec.TargetVCenterCredentialsSecret.Name = ""
		r := preflightReconciler(t, e, migration, newInfrastructureForPreflight(e.host, e.dcName), nil, nil, nil)
		_, err := r.runPreflightChecks(e.ctx, migration)
		if err == nil || !strings.Contains(err.Error(), "spec.targetVCenterCredentialsSecret.name must not be empty") {
			t.Fatalf("err = %v, want empty-secret-name error", err)
		}
	})
	t.Run("target secret not found", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		migration := newMigrationForPreflight(e.host, e.inv)
		migration.Spec.TargetVCenterCredentialsSecret.Name = "missing-secret"
		r := preflightReconciler(t, e, migration, newInfrastructureForPreflight(e.host, e.dcName), nil, nil, nil)
		_, err := r.runPreflightChecks(e.ctx, migration)
		if err == nil || !strings.Contains(err.Error(), "target credentials secret") {
			t.Fatalf("err = %v, want target-secret-not-found error", err)
		}
	})
}

func TestPreflightHasTargetVCenterConfiguration(t *testing.T) {
	t.Run("infra get error", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		r := preflightReconciler(t, e, newMigrationForPreflight(e.host, e.inv), newInfrastructureForPreflight(e.host, e.dcName), nil, nil, nil)
		configClientset(t, r).PrependReactor("get", "infrastructures", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		_, err := r.hasTargetVCenterConfiguration(e.ctx, newMigrationForPreflight(e.host, e.inv))
		if err == nil || !strings.Contains(err.Error(), "getting infrastructure") {
			t.Fatalf("err = %v, want infra get error", err)
		}
	})
	t.Run("target vCenter not found returns false", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		migration := newMigrationForPreflight(e.host, e.inv)
		migration.Spec.FailureDomains[0].Server = "not-in-infra-host"
		r := preflightReconciler(t, e, migration, newInfrastructureForPreflight(e.host, e.dcName), nil, nil, nil)
		ok, err := r.hasTargetVCenterConfiguration(e.ctx, migration)
		if err != nil {
			t.Fatalf("err = %v, want nil (no error, just false)", err)
		}
		if ok {
			t.Fatal("hasTargetVCenterConfiguration = true, want false")
		}
	})
}

func TestPreflightCheckNoVSphereCSIPersistentVolumes(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		kube := fakekube.NewClientset()
		kube.PrependReactor("list", "persistentvolumes", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		if err := checkNoVSphereCSIPersistentVolumes(e.ctx, kube); err == nil || !strings.Contains(err.Error(), "listing persistentvolumes") {
			t.Fatalf("err = %v, want pv list error", err)
		}
	})
	t.Run("blocked vsphere pv", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-vsphere"},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: vsphereCSIDriverName, VolumeHandle: "vol-1"},
				},
			},
		}
		kube := fakekube.NewClientset(pv)
		if err := checkNoVSphereCSIPersistentVolumes(e.ctx, kube); err == nil || !strings.Contains(err.Error(), "vSphere CSI-backed persistent volumes block migration") {
			t.Fatalf("err = %v, want blocked-vsphere-pv error", err)
		}
	})
}

func TestPreflightGetManagementState(t *testing.T) {
	t.Run("get error", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		dynScheme := runtime.NewScheme()
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynScheme, preflightListKinds())
		dyn.PrependReactor("get", "clustercsidrivers", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		_, err := getManagementState(e.ctx, dyn, clusterCSIDriverGVR, vsphereCSIDriverName)
		if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("err = %v, want raw get error", err)
		}
	})
	t.Run("managementState not set", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		csiDriver := newUnstructuredResource("operator.openshift.io/v1", "ClusterCSIDriver", "", vsphereCSIDriverName)
		dynScheme := runtime.NewScheme()
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynScheme, preflightListKinds(), csiDriver)
		_, err := getManagementState(e.ctx, dyn, clusterCSIDriverGVR, vsphereCSIDriverName)
		if err == nil || !strings.Contains(err.Error(), "spec.managementState not set") {
			t.Fatalf("err = %v, want not-set error", err)
		}
	})
}

func TestPreflightCheckInterferingRolloutResources(t *testing.T) {
	e := newNetcheckTestEnv(t)
	t.Run("machinehealthcheck list error", func(t *testing.T) {
		dyn := preflightDynFor(t)
		dyn.PrependReactor("list", "machinehealthchecks", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		if err := checkInterferingRolloutResources(e.ctx, dyn); err == nil || !strings.Contains(err.Error(), "listing machinehealthchecks") {
			t.Fatalf("err = %v, want mhcs list error", err)
		}
	})
	t.Run("clusterautoscaler list error", func(t *testing.T) {
		dyn := preflightDynFor(t)
		dyn.PrependReactor("list", "clusterautoscalers", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		if err := checkInterferingRolloutResources(e.ctx, dyn); err == nil || !strings.Contains(err.Error(), "listing clusterautoscalers") {
			t.Fatalf("err = %v, want clusterautoscalers list error", err)
		}
	})
	t.Run("machineautoscaler list error", func(t *testing.T) {
		dyn := preflightDynFor(t)
		dyn.PrependReactor("list", "machineautoscalers", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		if err := checkInterferingRolloutResources(e.ctx, dyn); err == nil || !strings.Contains(err.Error(), "listing machineautoscalers") {
			t.Fatalf("err = %v, want machineautoscalers list error", err)
		}
	})
	t.Run("user MHC blocker", func(t *testing.T) {
		mhc := newUnstructuredResource("machine.openshift.io/v1beta1", "MachineHealthCheck", "", "user-mhc")
		dynScheme := runtime.NewScheme()
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynScheme, preflightListKinds(), mhc)
		if err := checkInterferingRolloutResources(e.ctx, dyn); err == nil || !strings.Contains(err.Error(), "remove interfering rollout resources") {
			t.Fatalf("err = %v, want interfering-resources error", err)
		}
	})
}

func TestPreflightListDynamicResourceNames(t *testing.T) {
	e := newNetcheckTestEnv(t)
	t.Run("not found returns nil", func(t *testing.T) {
		// A List that reports NotFound is treated as an absent resource type
		// (listDynamicResourceNames returns nil, nil rather than an error).
		dyn := preflightDynFor(t)
		dyn.PrependReactor("list", "machinehealthchecks", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(machineHealthCheckGVR.GroupResource(), "none")
		})
		names, err := listDynamicResourceNames(e.ctx, dyn, machineHealthCheckGVR)
		if err != nil {
			t.Fatalf("err = %v, want nil for not-found GVR", err)
		}
		if len(names) != 0 {
			t.Fatalf("names = %v, want empty", names)
		}
	})
	t.Run("list error propagates", func(t *testing.T) {
		dyn := preflightDynFor(t)
		dyn.PrependReactor("list", "machinehealthchecks", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		if _, err := listDynamicResourceNames(e.ctx, dyn, machineHealthCheckGVR); err == nil {
			t.Fatal("err = nil, want list error")
		}
	})
	t.Run("namespace-prefixed names", func(t *testing.T) {
		obj := newUnstructuredResource("machine.openshift.io/v1beta1", "MachineHealthCheck", "openshift-machine-api", "mh1")
		dynScheme := runtime.NewScheme()
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynScheme, preflightListKinds(), obj)
		names, err := listDynamicResourceNames(e.ctx, dyn, machineHealthCheckGVR)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(names) != 1 || names[0] != "openshift-machine-api/mh1" {
			t.Fatalf("names = %v, want [openshift-machine-api/mh1]", names)
		}
	})
}

func TestPreflightValidatePreflightSourceCoverage(t *testing.T) {
	e := newNetcheckTestEnv(t)
	r := preflightReconciler(t, e, newMigrationForPreflight(e.host, e.inv), newInfrastructureForPreflight(e.host, e.dcName), nil, nil, nil)
	configClientset(t, r).PrependReactor("get", "infrastructures", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	if err := r.validatePreflightSourceCoverage(e.ctx, nil); err == nil || !strings.Contains(err.Error(), "getting source vCenter") {
		t.Fatalf("err = %v, want source vCenter get error", err)
	}
}

func TestPreflightCheckVSphereStorageManagement(t *testing.T) {
	t.Run("csi state get error", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		dynScheme := runtime.NewScheme()
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynScheme, preflightListKinds())
		dyn.PrependReactor("get", "clustercsidrivers", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		if _, err := checkVSphereStorageManagement(e.ctx, dyn); err == nil || !strings.Contains(err.Error(), "unable to read ClusterCSIDriver") {
			t.Fatalf("err = %v, want csi state get error", err)
		}
	})
	t.Run("csi state not removed", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		dynScheme := runtime.NewScheme()
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynScheme, preflightListKinds(),
			newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Managed"),
			newManagementStateResource("Storage", "cluster", "Removed"),
		)
		if _, err := checkVSphereStorageManagement(e.ctx, dyn); err == nil || !strings.Contains(err.Error(), "vSphere CSI driver is not supported for migration; set ClusterCSIDriver") {
			t.Fatalf("err = %v, want csi-not-removed error", err)
		}
	})
	t.Run("storage state get error", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		dynScheme := runtime.NewScheme()
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynScheme, preflightListKinds(),
			newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Removed"),
		)
		dyn.PrependReactor("get", "storages", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		// The storage-operator read failure is non-fatal: it returns a warning
		// message with a nil error.
		msg, err := checkVSphereStorageManagement(e.ctx, dyn)
		if err != nil {
			t.Fatalf("err = %v, want nil (storage read failure is a warning)", err)
		}
		if !strings.Contains(msg, "unable to read Storage/cluster managementState") {
			t.Fatalf("msg = %q, want storage read warning", msg)
		}
	})
}

func TestPreflightValidateFailureDomain(t *testing.T) {
	e := newNetcheckTestEnv(t)
	creds := credentials{username: e.user, password: e.pass}
	t.Run("happy", func(t *testing.T) {
		if err := validateFailureDomain(e.ctx, e.preflightFD(t, "fd-a"), creds); err != nil {
			t.Fatalf("validateFailureDomain = %v, want nil", err)
		}
	})
	t.Run("session error", func(t *testing.T) {
		fd := e.preflightFD(t, "fd-a")
		fd.Server = unreachableVCAddr
		if err := validateFailureDomain(e.ctx, fd, creds); err == nil || !strings.Contains(err.Error(), "connecting to target vCenter") {
			t.Fatalf("err = %v, want session error", err)
		}
	})
	t.Run("datacenter not found", func(t *testing.T) {
		fd := e.preflightFD(t, "fd-a")
		fd.Topology.Datacenter = "no-such-dc"
		if err := validateFailureDomain(e.ctx, fd, creds); err == nil || !strings.Contains(err.Error(), "finding datacenter") {
			t.Fatalf("err = %v, want datacenter error", err)
		}
	})
	t.Run("cluster not found", func(t *testing.T) {
		fd := e.preflightFD(t, "fd-a")
		fd.Topology.ComputeCluster = "no-such-cluster"
		if err := validateFailureDomain(e.ctx, fd, creds); err == nil || !strings.Contains(err.Error(), "target cluster") {
			t.Fatalf("err = %v, want cluster error", err)
		}
	})
	t.Run("datastore not found", func(t *testing.T) {
		fd := e.preflightFD(t, "fd-a")
		fd.Topology.Datastore = "no-such-ds"
		if err := validateFailureDomain(e.ctx, fd, creds); err == nil || !strings.Contains(err.Error(), "target datastore") {
			t.Fatalf("err = %v, want datastore error", err)
		}
	})
	t.Run("network not found", func(t *testing.T) {
		fd := e.preflightFD(t, "fd-a")
		fd.Topology.Networks = []string{"no-such-net"}
		if err := validateFailureDomain(e.ctx, fd, creds); err == nil || !strings.Contains(err.Error(), "target network") {
			t.Fatalf("err = %v, want network error", err)
		}
	})
	t.Run("folder not found", func(t *testing.T) {
		fd := e.preflightFD(t, "fd-a")
		fd.Topology.Folder = "no-such-folder"
		if err := validateFailureDomain(e.ctx, fd, creds); err == nil || !strings.Contains(err.Error(), "target folder") {
			t.Fatalf("err = %v, want folder error", err)
		}
	})
	t.Run("template not found", func(t *testing.T) {
		fd := e.preflightFD(t, "fd-a")
		fd.Topology.Template = "no-such-template"
		if err := validateFailureDomain(e.ctx, fd, creds); err == nil || !strings.Contains(err.Error(), "target template") {
			t.Fatalf("err = %v, want template error", err)
		}
	})
}

func TestPreflightValidateTargetPrivileges(t *testing.T) {
	e := newNetcheckTestEnv(t)
	t.Run("nil client", func(t *testing.T) {
		if err := validateTargetPrivileges(e.ctx, nil, e.preflightDC(t), e.preflightCluster(t)); err == nil || !strings.Contains(err.Error(), "session client must not be nil") {
			t.Fatalf("err = %v, want nil-client error", err)
		}
	})
	t.Run("happy", func(t *testing.T) {
		session := e.preflightSession(t)
		if err := validateTargetPrivileges(e.ctx, session, e.preflightDC(t), e.preflightCluster(t)); err != nil {
			t.Fatalf("validateTargetPrivileges = %v, want nil (default admin has all privileges)", err)
		}
	})
}

func TestPreflightValidatePreflightVSphere(t *testing.T) {
	t.Run("source vCenter no datacenters", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		infra := newInfrastructureForPreflight(e.host, e.dcName)
		infra.Spec.PlatformSpec.VSphere.VCenters[0].Datacenters = nil
		migration := newMigrationForPreflight(e.host, e.inv)
		r := preflightReconciler(t, e, migration, infra, nil, nil, nil)
		if err := r.validatePreflightVSphere(e.ctx, e.ctx, migration); err == nil || !strings.Contains(err.Error(), "source vCenter has no datacenters configured") {
			t.Fatalf("err = %v, want no-datacenters error", err)
		}
	})
	t.Run("source vCenter multiple datacenters", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		infra := newInfrastructureForPreflight(e.host, e.dcName)
		infra.Spec.PlatformSpec.VSphere.VCenters[0].Datacenters = append(infra.Spec.PlatformSpec.VSphere.VCenters[0].Datacenters, "second-dc")
		migration := newMigrationForPreflight(e.host, e.inv)
		r := preflightReconciler(t, e, migration, infra, nil, nil, nil)
		if err := r.validatePreflightVSphere(e.ctx, e.ctx, migration); err == nil || !strings.Contains(err.Error(), "must have exactly one datacenter configured") {
			t.Fatalf("err = %v, want multiple-datacenters error", err)
		}
	})
	t.Run("source vCenter get error", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		migration := newMigrationForPreflight(e.host, e.inv)
		r := preflightReconciler(t, e, migration, newInfrastructureForPreflight(e.host, e.dcName), nil, nil, nil)
		configClientset(t, r).PrependReactor("get", "infrastructures", func(action ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})
		if err := r.validatePreflightVSphere(e.ctx, e.ctx, migration); err == nil || !strings.Contains(err.Error(), "getting source vCenter") {
			t.Fatalf("err = %v, want source vCenter get error", err)
		}
	})
	t.Run("target creds missing", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		migration := newMigrationForPreflight(e.host, e.inv)
		migration.Spec.FailureDomains[0].Server = "not-in-target-secret-host"
		r := preflightReconciler(t, e, migration, newInfrastructureForPreflight(e.host, e.dcName), nil, nil, nil)
		if err := r.validatePreflightVSphere(e.ctx, e.ctx, migration); err == nil || !strings.Contains(err.Error(), "getting credentials for target") {
			t.Fatalf("err = %v, want target creds error", err)
		}
	})
	t.Run("datacenter not accessible", func(t *testing.T) {
		e := newNetcheckTestEnv(t)
		infra := newInfrastructureForPreflight(e.host, e.dcName)
		infra.Spec.PlatformSpec.VSphere.VCenters[0].Datacenters = []string{"no-such-dc"}
		migration := newMigrationForPreflight(e.host, e.inv)
		r := preflightReconciler(t, e, migration, infra, nil, nil, nil)
		// The source session's datacenter lookup fails because "no-such-dc" is
		// not in the simulator.
		if err := r.validatePreflightVSphere(e.ctx, e.ctx, migration); err == nil || !strings.Contains(err.Error(), "finding datacenter") {
			t.Fatalf("err = %v, want source datacenter lookup error", err)
		}
	})
}

func TestPreflightDCVMFolder(t *testing.T) {
	e := newNetcheckTestEnv(t)
	folder, err := dcVMFolder(e.ctx, e.preflightDC(t))
	if err != nil {
		t.Fatalf("dcVMFolder = %v, want nil", err)
	}
	if folder == nil || folder.Reference().Value == "" {
		t.Fatal("dcVMFolder returned a nil/empty folder")
	}
}

// preflightDynFor builds a dynamic client pre-populated with the default
// Removed management-state resources (so non-targeted lookups succeed).
func preflightDynFor(t *testing.T) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dynScheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynScheme, preflightListKinds(),
		newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Removed"),
		newManagementStateResource("Storage", "cluster", "Removed"),
	)
}
