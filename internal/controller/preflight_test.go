package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	machinefake "github.com/openshift/client-go/machine/clientset/versioned/fake"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/simulator"
	_ "github.com/vmware/govmomi/vapi/simulator"
	"github.com/vmware/govmomi/vim25/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	fakekube "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

func TestCheckNoVSphereCSIPersistentVolumes(t *testing.T) {
	tests := []struct {
		name            string
		pvs             []runtime.Object
		wantErr         bool
		wantContains    []string
		wantNotContains []string
	}{
		{
			name: "passes without vsphere csi pvs",
			pvs: []runtime.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "nfs-pv"},
				},
			},
			wantErr: false,
		},
		{
			name: "fails when vsphere csi pv exists",
			pvs: []runtime.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "vsphere-csi-pv"},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{Driver: vsphereCSIDriverName},
						},
					},
				},
			},
			wantErr:      true,
			wantContains: []string{"vsphere-csi-pv", vsphereCSIDriverName},
		},
		{
			name: "fails with mixed pvs and reports only vsphere csi pv",
			pvs: []runtime.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "nfs-pv"},
				},
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "vsphere-csi-pv"},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{Driver: vsphereCSIDriverName},
						},
					},
				},
			},
			wantErr:         true,
			wantContains:    []string{"vsphere-csi-pv"},
			wantNotContains: []string{"nfs-pv"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fakekube.NewClientset(tt.pvs...)
			err := checkNoVSphereCSIPersistentVolumes(context.Background(), client)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkNoVSphereCSIPersistentVolumes error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil {
				return
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("checkNoVSphereCSIPersistentVolumes error %q does not contain %q", err.Error(), want)
				}
			}
			for _, unwanted := range tt.wantNotContains {
				if strings.Contains(err.Error(), unwanted) {
					t.Fatalf("checkNoVSphereCSIPersistentVolumes error %q unexpectedly contains %q", err.Error(), unwanted)
				}
			}
		})
	}
}

func TestCheckVSphereStorageManagement(t *testing.T) {
	tests := []struct {
		name           string
		objects        []runtime.Object
		getErrors      map[schema.GroupVersionResource]error
		wantErr        bool
		wantErrContain []string
		wantWarning    string
	}{
		{
			name: "passes when csi removed and storage removed",
			objects: []runtime.Object{
				newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Removed"),
				newManagementStateResource("Storage", "cluster", "Removed"),
			},
			wantErr:     false,
			wantWarning: "",
		},
		{
			name: "passes when csi removed and storage unmanaged",
			objects: []runtime.Object{
				newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Removed"),
				newManagementStateResource("Storage", "cluster", "Unmanaged"),
			},
			wantErr:     false,
			wantWarning: "",
		},
		{
			name: "warns when csi removed and storage managed",
			objects: []runtime.Object{
				newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Removed"),
				newManagementStateResource("Storage", "cluster", "Managed"),
			},
			wantErr:     false,
			wantWarning: "Storage/cluster managementState is Managed",
		},
		{
			name: "warns when csi removed and storage missing",
			objects: []runtime.Object{
				newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Removed"),
			},
			wantErr:     false,
			wantWarning: "Storage/cluster",
		},
		{
			name: "fails when csi managed",
			objects: []runtime.Object{
				newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Managed"),
				newManagementStateResource("Storage", "cluster", "Removed"),
			},
			wantErr:        true,
			wantErrContain: []string{"ClusterCSIDriver", vsphereCSIDriverName, "Removed", "Managed"},
		},
		{
			name: "fails when csi unmanaged",
			objects: []runtime.Object{
				newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Unmanaged"),
				newManagementStateResource("Storage", "cluster", "Removed"),
			},
			wantErr:        true,
			wantErrContain: []string{"ClusterCSIDriver", vsphereCSIDriverName, "Removed", "Unmanaged"},
		},
		{
			name: "fails when csi missing",
			objects: []runtime.Object{
				newManagementStateResource("Storage", "cluster", "Removed"),
			},
			wantErr:        true,
			wantErrContain: []string{"ClusterCSIDriver", vsphereCSIDriverName},
		},
		{
			name: "fails when csi api unavailable",
			getErrors: map[schema.GroupVersionResource]error{
				clusterCSIDriverGVR: &apimeta.NoResourceMatchError{PartialResource: clusterCSIDriverGVR},
			},
			wantErr:        true,
			wantErrContain: []string{"ClusterCSIDriver", vsphereCSIDriverName},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, preflightListKinds(), tt.objects...)
			if len(tt.getErrors) > 0 {
				client.PrependReactor("get", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
					if err, ok := tt.getErrors[action.GetResource()]; ok {
						return true, nil, err
					}
					return false, nil, nil
				})
			}

			warning, err := checkVSphereStorageManagement(context.Background(), client)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkVSphereStorageManagement error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				for _, want := range tt.wantErrContain {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("checkVSphereStorageManagement error %q does not contain %q", err.Error(), want)
					}
				}
				return
			}
			if tt.wantWarning == "" {
				if warning != "" {
					t.Fatalf("checkVSphereStorageManagement warning = %q, want empty", warning)
				}
				return
			}
			if !strings.Contains(warning, tt.wantWarning) {
				t.Fatalf("checkVSphereStorageManagement warning = %q, want substring %q", warning, tt.wantWarning)
			}
		})
	}
}

func TestCheckInterferingRolloutResources(t *testing.T) {
	tests := []struct {
		name         string
		objects      []runtime.Object
		listErrors   map[schema.GroupVersionResource]error
		wantErr      bool
		wantContains []string
	}{
		{
			name:    "passes when no interfering resources exist",
			objects: nil,
			wantErr: false,
		},
		{
			name: "fails when machine health check exists",
			objects: []runtime.Object{
				newUnstructuredResource("machine.openshift.io/v1beta1", "MachineHealthCheck", "openshift-machine-api", "worker-mhc"),
			},
			wantErr:      true,
			wantContains: []string{"MachineHealthCheck resources: openshift-machine-api/worker-mhc"},
		},
		{
			name: "fails when cluster autoscaler exists",
			objects: []runtime.Object{
				newUnstructuredResource("autoscaling.openshift.io/v1", "ClusterAutoscaler", "", "default"),
			},
			wantErr:      true,
			wantContains: []string{"ClusterAutoscaler resources: default"},
		},
		{
			name: "fails when machine autoscaler exists",
			objects: []runtime.Object{
				newUnstructuredResource("autoscaling.openshift.io/v1beta1", "MachineAutoscaler", "openshift-machine-api", "worker-a"),
			},
			wantErr:      true,
			wantContains: []string{"MachineAutoscaler resources: openshift-machine-api/worker-a"},
		},
		{
			name: "fails when all interfering resources exist",
			objects: []runtime.Object{
				newUnstructuredResource("machine.openshift.io/v1beta1", "MachineHealthCheck", "openshift-machine-api", "worker-mhc"),
				newUnstructuredResource("autoscaling.openshift.io/v1", "ClusterAutoscaler", "", "default"),
				newUnstructuredResource("autoscaling.openshift.io/v1beta1", "MachineAutoscaler", "openshift-machine-api", "worker-a"),
			},
			wantErr: true,
			wantContains: []string{
				"MachineHealthCheck resources: openshift-machine-api/worker-mhc",
				"ClusterAutoscaler resources: default",
				"MachineAutoscaler resources: openshift-machine-api/worker-a",
			},
		},
		{
			name: "skips platform-default machine-api-termination-handler",
			objects: []runtime.Object{
				newUnstructuredResource("machine.openshift.io/v1beta1", "MachineHealthCheck", "openshift-machine-api", "machine-api-termination-handler"),
			},
			wantErr: false,
		},
		{
			name: "blocks on user MHC while skipping termination handler",
			objects: []runtime.Object{
				newUnstructuredResource("machine.openshift.io/v1beta1", "MachineHealthCheck", "openshift-machine-api", "machine-api-termination-handler"),
				newUnstructuredResource("machine.openshift.io/v1beta1", "MachineHealthCheck", "openshift-machine-api", "worker-mhc"),
			},
			wantErr:      true,
			wantContains: []string{"MachineHealthCheck resources: openshift-machine-api/worker-mhc"},
		},
		{
			name: "ignores resource not found and no match errors",
			listErrors: map[schema.GroupVersionResource]error{
				clusterAutoscalerGVR: apierrors.NewNotFound(schema.GroupResource{Group: clusterAutoscalerGVR.Group, Resource: clusterAutoscalerGVR.Resource}, clusterAutoscalerGVR.Resource),
				machineAutoscalerGVR: &apimeta.NoResourceMatchError{PartialResource: machineAutoscalerGVR},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, preflightListKinds(), tt.objects...)
			if len(tt.listErrors) > 0 {
				client.PrependReactor("list", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
					if err, ok := tt.listErrors[action.GetResource()]; ok {
						return true, nil, err
					}
					return false, nil, nil
				})
			}
			err := checkInterferingRolloutResources(context.Background(), client)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkInterferingRolloutResources error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil {
				return
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("checkInterferingRolloutResources error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestMissingPrivileges(t *testing.T) {
	tests := []struct {
		name      string
		entity    types.EntityPrivilege
		requested []string
		want      []string
	}{
		{
			name: "returns nil when all privileges granted",
			entity: types.EntityPrivilege{
				PrivAvailability: []types.PrivilegeAvailability{
					{PrivId: "b", IsGranted: true},
					{PrivId: "a", IsGranted: true},
				},
			},
			requested: []string{"a", "b"},
			want:      nil,
		},
		{
			name: "returns sorted missing privileges when some are absent",
			entity: types.EntityPrivilege{
				PrivAvailability: []types.PrivilegeAvailability{
					{PrivId: "b", IsGranted: true},
				},
			},
			requested: []string{"c", "a", "b"},
			want:      []string{"a", "c"},
		},
		{
			name: "returns all requested when none are granted",
			entity: types.EntityPrivilege{
				PrivAvailability: nil,
			},
			requested: []string{"b", "a"},
			want:      []string{"a", "b"},
		},
		{
			name: "returns nil for empty requested privileges",
			entity: types.EntityPrivilege{
				PrivAvailability: []types.PrivilegeAvailability{
					{PrivId: "a", IsGranted: true},
				},
			},
			requested: nil,
			want:      nil,
		},
		{
			name: "treats explicitly denied privilege as missing",
			entity: types.EntityPrivilege{
				PrivAvailability: []types.PrivilegeAvailability{
					{PrivId: "a", IsGranted: false},
					{PrivId: "b", IsGranted: true},
				},
			},
			requested: []string{"a", "b"},
			want:      []string{"a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := missingPrivileges(tt.entity, tt.requested)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("missingPrivileges() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateUniqueFailureDomainNames(t *testing.T) {
	tests := []struct {
		name         string
		failureAreas []configv1.VSpherePlatformFailureDomainSpec
		wantErr      bool
		wantContains string
	}{
		{
			name: "passes when names are unique",
			failureAreas: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "fd-a"},
				{Name: "fd-b"},
			},
			wantErr: false,
		},
		{
			name: "fails when names are duplicated",
			failureAreas: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "fd-a"},
				{Name: "fd-a"},
				{Name: "fd-b"},
				{Name: "fd-b"},
			},
			wantErr:      true,
			wantContains: "duplicate failure domain names are not allowed: fd-a, fd-b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateUniqueFailureDomainNames(tt.failureAreas)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateUniqueFailureDomainNames error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tt.wantContains) {
				t.Fatalf("validateUniqueFailureDomainNames error = %q, want substring %q", err.Error(), tt.wantContains)
			}
		})
	}
}

func TestRunPreflightChecks(t *testing.T) {
	ctx := context.Background()
	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("Create simulator model: %v", err)
	}
	defer model.Remove()

	model.Service.TLS = new(tls.Config)
	model.Service.RegisterEndpoints = true

	server := model.Service.NewServer()
	defer server.Close()

	ctx = model.Service.Context
	username := simulator.DefaultLogin.Username()
	password, ok := simulator.DefaultLogin.Password()
	if !ok {
		t.Fatal("simulator default login missing password")
	}

	vsphere.ClearSessions(ctx)
	defer vsphere.ClearSessions(ctx)

	inventory := discoverPreflightTestInventory(ctx, t, server.URL, username, password)

	tests := []struct {
		name                      string
		version                   string
		gateVersion               string // defaults to version if empty
		gateEnabled               bool
		progressing               bool
		pvs                       []runtime.Object
		dynamicObjects            []runtime.Object
		omitDefaultStorageOps     bool
		extraConfigObjects        []runtime.Object
		machineObjects            []runtime.Object
		mutateInfrastructure      func(*configv1.Infrastructure)
		mutateMigration           func(*migrationv1alpha1.VmwareCloudFoundationMigration)
		wantMessageContains       string
		wantErrContains           string
		wantTransient             bool
		wantTargetSecretReadCount int
	}{
		{
			name:                      "happy path passes when gate enabled",
			version:                   "5.0.0",
			gateEnabled:               true,
			wantMessageContains:       "Preflight validation passed",
			wantTargetSecretReadCount: 2,
		},
		{
			name:                  "storage planning permits CSI volumes with Managed driver",
			version:               "5.0.0",
			gateEnabled:           true,
			omitDefaultStorageOps: true,
			dynamicObjects: []runtime.Object{
				newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Managed"),
				newManagementStateResource("Storage", "cluster", "Managed"),
			},
			pvs: []runtime.Object{&corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "vsphere-csi-pv"},
				Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: vsphereCSIDriverName},
				}},
			}},
			mutateMigration: func(m *migrationv1alpha1.VmwareCloudFoundationMigration) {
				m.Spec.MigrateStorage = true
			},
			wantMessageContains:       "Preflight validation passed",
			wantTargetSecretReadCount: 2,
		},
		{
			name:        "rejects source ComputeCluster region affinity",
			version:     "5.0.0",
			gateEnabled: true,
			mutateInfrastructure: func(infra *configv1.Infrastructure) {
				infra.Spec.PlatformSpec.VSphere.FailureDomains = []configv1.VSpherePlatformFailureDomainSpec{{
					Name:           "source-fd",
					RegionAffinity: &configv1.VSphereFailureDomainRegionAffinity{Type: configv1.ComputeClusterFailureDomainRegion},
				}}
			},
			wantErrContains:           `failure domain "source-fd" uses unsupported region type "ComputeCluster"`,
			wantTargetSecretReadCount: 1,
		},
		{
			name:        "rejects source HostGroup zone affinity",
			version:     "5.0.0",
			gateEnabled: true,
			mutateInfrastructure: func(infra *configv1.Infrastructure) {
				infra.Spec.PlatformSpec.VSphere.FailureDomains = []configv1.VSpherePlatformFailureDomainSpec{{
					Name:         "source-fd",
					ZoneAffinity: &configv1.VSphereFailureDomainZoneAffinity{Type: configv1.HostGroupFailureDomainZone},
				}}
			},
			wantErrContains:           `failure domain "source-fd" uses unsupported zone type "HostGroup"`,
			wantTargetSecretReadCount: 1,
		},
		{
			name:        "rejects target ComputeCluster region affinity",
			version:     "5.0.0",
			gateEnabled: true,
			mutateMigration: func(migration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				migration.Spec.FailureDomains[0].RegionAffinity = &configv1.VSphereFailureDomainRegionAffinity{Type: configv1.ComputeClusterFailureDomainRegion}
			},
			wantErrContains:           `failure domain "fd-a" uses unsupported region type "ComputeCluster"`,
			wantTargetSecretReadCount: 0,
		},
		{
			name:        "rejects target HostGroup zone affinity",
			version:     "5.0.0",
			gateEnabled: true,
			mutateMigration: func(migration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				migration.Spec.FailureDomains[0].ZoneAffinity = &configv1.VSphereFailureDomainZoneAffinity{Type: configv1.HostGroupFailureDomainZone}
			},
			wantErrContains:           `failure domain "fd-a" uses unsupported zone type "HostGroup"`,
			wantTargetSecretReadCount: 0,
		},
		{
			name:                      "blocks when gate disabled",
			version:                   "5.0.0",
			gateEnabled:               false,
			wantErrContains:           "feature gate VSphereMultiVCenterDay2 is not enabled",
			wantTargetSecretReadCount: 1,
		},
		{
			name:        "csi pv blocker short circuits before target validation",
			version:     "5.0.0",
			gateEnabled: true,
			pvs: []runtime.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "vsphere-csi-pv"},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{Driver: vsphereCSIDriverName},
						},
					},
				},
			},
			mutateMigration: func(migration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				migration.Spec.FailureDomains[0].Topology.Datastore = missingDatastorePath
			},
			wantErrContains:           "vsphere-csi-pv",
			wantTargetSecretReadCount: 1,
		},
		{
			name:                  "csi managed blocker short circuits before target validation",
			version:               "5.0.0",
			gateEnabled:           true,
			omitDefaultStorageOps: true,
			dynamicObjects: []runtime.Object{
				newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Managed"),
				newManagementStateResource("Storage", "cluster", "Removed"),
			},
			mutateMigration: func(migration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				migration.Spec.FailureDomains[0].Topology.Datastore = missingDatastorePath
			},
			wantErrContains:           "ClusterCSIDriver/" + vsphereCSIDriverName,
			wantTargetSecretReadCount: 1,
		},
		{
			name:        "interfering resource blocker short circuits before target validation",
			version:     "5.0.0",
			gateEnabled: true,
			dynamicObjects: []runtime.Object{
				newUnstructuredResource("machine.openshift.io/v1beta1", "MachineHealthCheck", "openshift-machine-api", "worker-mhc"),
			},
			mutateMigration: func(migration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				migration.Spec.FailureDomains[0].Topology.Datastore = missingDatastorePath
			},
			wantErrContains:           "MachineHealthCheck resources: openshift-machine-api/worker-mhc",
			wantTargetSecretReadCount: 1,
		},
		{
			name:                      "cluster upgrade in progress blocks migration",
			version:                   "5.0.0",
			gateEnabled:               true,
			progressing:               true,
			wantErrContains:           "cluster upgrade is in progress",
			wantTransient:             true,
			wantTargetSecretReadCount: 1,
		},
		{
			name:        "degraded cluster operator blocks migration",
			version:     "5.0.0",
			gateEnabled: true,
			extraConfigObjects: []runtime.Object{
				&configv1.ClusterOperator{
					ObjectMeta: metav1.ObjectMeta{Name: "kube-apiserver"},
					Status: configv1.ClusterOperatorStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{Type: configv1.OperatorAvailable, Status: configv1.ConditionTrue},
							{Type: configv1.OperatorDegraded, Status: configv1.ConditionTrue},
						},
					},
				},
			},
			wantErrContains:           "cluster operators are not healthy",
			wantTransient:             true,
			wantTargetSecretReadCount: 1,
		},
		{
			name:        "missing target folder fails during target validation",
			version:     "5.0.0",
			gateEnabled: true,
			mutateMigration: func(migration *migrationv1alpha1.VmwareCloudFoundationMigration) {
				migration.Spec.FailureDomains[0].Topology.Folder = "/missing-folder"
			},
			wantErrContains:           `target folder "/missing-folder"`,
			wantTargetSecretReadCount: 2,
		},
		{
			name:                      "feature gate version mismatch surfaces error",
			version:                   "5.0.0",
			gateVersion:               "4.19.0",
			gateEnabled:               true,
			wantErrContains:           "checking cluster readiness",
			wantTargetSecretReadCount: 1,
		},
		{
			name:                  "storage managed warns but does not block",
			version:               "5.0.0",
			gateEnabled:           true,
			omitDefaultStorageOps: true,
			dynamicObjects: []runtime.Object{
				newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Removed"),
				newManagementStateResource("Storage", "cluster", "Managed"),
			},
			wantMessageContains:       "Storage/cluster managementState is Managed",
			wantTargetSecretReadCount: 2,
		},
		{
			name:        "empty topology.template is rejected before vCenter work",
			version:     "5.0.0",
			gateEnabled: true,
			mutateMigration: func(m *migrationv1alpha1.VmwareCloudFoundationMigration) {
				m.Spec.FailureDomains[0].Topology.Template = ""
			},
			wantErrContains:           "topology.template is required",
			wantTargetSecretReadCount: 0,
		},
		{
			name:        "later failure domain missing template is rejected before vCenter work",
			version:     "5.0.0",
			gateEnabled: true,
			mutateMigration: func(m *migrationv1alpha1.VmwareCloudFoundationMigration) {
				m.Spec.FailureDomains[1].Topology.Template = ""
			},
			wantErrContains:           `spec.failureDomains[1].topology.template is required (failure domain "fd-b")`,
			wantTargetSecretReadCount: 0,
		},
		{
			name:        "rejects destination failure domains spanning two vCenter servers",
			version:     "5.0.0",
			gateEnabled: true,
			mutateMigration: func(m *migrationv1alpha1.VmwareCloudFoundationMigration) {
				m.Spec.FailureDomains[1].Server = "second-target.example.com"
			},
			wantErrContains:           "must use a single vCenter server",
			wantTargetSecretReadCount: 0,
		},
		{
			name:        "rejects more than one source vCenter",
			version:     "5.0.0",
			gateEnabled: true,
			mutateInfrastructure: func(infra *configv1.Infrastructure) {
				infra.Spec.PlatformSpec.VSphere.VCenters = append(infra.Spec.PlatformSpec.VSphere.VCenters, configv1.VSpherePlatformVCenterSpec{Server: "second-source.example.com"})
			},
			wantErrContains:           "source infrastructure must have exactly one vCenter, found 2",
			wantTargetSecretReadCount: 1,
		},
		{
			name:        "rejects overlapping source failure domain topology",
			version:     "5.0.0",
			gateEnabled: true,
			mutateInfrastructure: func(infra *configv1.Infrastructure) {
				fds := infra.Spec.PlatformSpec.VSphere.FailureDomains
				fds[1].Topology.ResourcePool = fds[0].Topology.ResourcePool
			},
			wantErrContains:           "overlapping topology in datacenter",
			wantTargetSecretReadCount: 1,
		},
		{
			name:        "rejects omitted mappings when source and destination are not one-to-one",
			version:     "5.0.0",
			gateEnabled: true,
			mutateMigration: func(m *migrationv1alpha1.VmwareCloudFoundationMigration) {
				m.Spec.FailureDomainMappings = nil
			},
			wantErrContains:           "provide mappings",
			wantTargetSecretReadCount: 1,
		},
		{
			name:        "rejects unknown source failure domain in mappings",
			version:     "5.0.0",
			gateEnabled: true,
			mutateMigration: func(m *migrationv1alpha1.VmwareCloudFoundationMigration) {
				m.Spec.FailureDomainMappings[0].Source = "nope"
			},
			wantErrContains:           `unknown source failure domain "nope"`,
			wantTargetSecretReadCount: 1,
		},
		{
			name:        "fails preflight when a destination MachineSet name collides with an existing MachineSet",
			version:     "5.0.0",
			gateEnabled: true,
			machineObjects: []runtime.Object{
				&machinev1beta1.MachineSet{ObjectMeta: metav1.ObjectMeta{Namespace: openshift.MachineAPINamespace, Name: workerMachineSetName("test-infra", "fd-a")}},
			},
			wantErrContains:           "collides with existing MachineSet; rename the destination failure domain",
			wantTargetSecretReadCount: 1,
		},
		{
			name:        "passes preflight after the destination failure domain is renamed",
			version:     "5.0.0",
			gateEnabled: true,
			machineObjects: []runtime.Object{
				&machinev1beta1.MachineSet{ObjectMeta: metav1.ObjectMeta{Namespace: openshift.MachineAPINamespace, Name: workerMachineSetName("test-infra", "fd-a")}},
			},
			mutateMigration: func(m *migrationv1alpha1.VmwareCloudFoundationMigration) {
				m.Spec.FailureDomains[0].Name = "fd-renamed"
				m.Spec.FailureDomainMappings[0].Destination = "fd-renamed"
			},
			wantMessageContains:       "Preflight validation passed",
			wantTargetSecretReadCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vsphere.ClearSessions(ctx)

			kubeObjects := []runtime.Object{
				newTargetCredentialsSecret("default", "target-vcenter-creds", server.URL.Host, username, password),
				newTargetCredentialsSecret(openshift.VSphereCredsSecretNamespace, openshift.VSphereCredsSecretName, server.URL.Host, username, password),
			}
			kubeObjects = append(kubeObjects, tt.pvs...)
			kubeClient := fakekube.NewClientset(kubeObjects...)

			// Count reads of the migration target-credentials secret. The happy path
			// performs 2 reads total: 1 existence check plus 1 credential lookup for
			// the shared target server. Early-blocker cases should remain at 1, proving
			// preflight returned before the failure-domain credential loop ran.
			targetSecretReadCount := 0
			kubeClient.PrependReactor("get", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
				getAction, ok := action.(ktesting.GetAction)
				if ok && action.GetNamespace() == "default" && getAction.GetName() == "target-vcenter-creds" {
					targetSecretReadCount++
				}
				return false, nil, nil
			})

			gateVersion := tt.version
			if tt.gateVersion != "" {
				gateVersion = tt.gateVersion
			}
			infra := newInfrastructureForPreflight(server.URL.Host, inventory.datacenterName)
			if tt.mutateInfrastructure != nil {
				tt.mutateInfrastructure(infra)
			}
			configObjects := []runtime.Object{
				infra,
				newClusterVersionForPreflight(tt.version, tt.progressing),
				newFeatureGateForPreflight(gateVersion, tt.gateEnabled),
			}
			configObjects = append(configObjects, tt.extraConfigObjects...)
			configClient := configfake.NewClientset(configObjects...)

			scheme := runtime.NewScheme()
			dynamicObjects := tt.dynamicObjects
			if !tt.omitDefaultStorageOps {
				dynamicObjects = append([]runtime.Object{
					newManagementStateResource("ClusterCSIDriver", vsphereCSIDriverName, "Removed"),
					newManagementStateResource("Storage", "cluster", "Removed"),
				}, dynamicObjects...)
			}
			dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, preflightListKinds(), dynamicObjects...)

			reconciler := &VmwareCloudFoundationMigrationReconciler{
				KubeClient:    kubeClient,
				ConfigClient:  configClient,
				DynamicClient: dynamicClient,
				MachineClient: machinefake.NewSimpleClientset(tt.machineObjects...),
				checkNetworkingViaProbeVMsFunc: func(context.Context, *migrationv1alpha1.VmwareCloudFoundationMigration, *configv1.VSpherePlatformVCenterSpec) error {
					return nil
				},
			}

			migration := newMigrationForPreflight(server.URL.Host, inventory)
			if tt.mutateMigration != nil {
				tt.mutateMigration(migration)
			}

			message, err := reconciler.runPreflightChecks(ctx, migration)
			if tt.wantErrContains != "" {
				if err == nil {
					t.Fatalf("runPreflightChecks succeeded, want error containing %q", tt.wantErrContains)
				}
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("runPreflightChecks error = %q, want substring %q", err.Error(), tt.wantErrContains)
				}
				if got := isTransientError(err); got != tt.wantTransient {
					t.Fatalf("isTransientError(err) = %v, want %v", got, tt.wantTransient)
				}
			} else {
				if err != nil {
					t.Fatalf("runPreflightChecks: %v", err)
				}
				if !strings.Contains(message, tt.wantMessageContains) {
					t.Fatalf("runPreflightChecks message = %q, want substring %q", message, tt.wantMessageContains)
				}
			}

			if targetSecretReadCount != tt.wantTargetSecretReadCount {
				t.Fatalf("target credentials secret read count = %d, want %d", targetSecretReadCount, tt.wantTargetSecretReadCount)
			}
		})
	}
}

func newUnstructuredResource(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(apiVersion)
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

const missingDatastorePath = "/missing-datastore"

func newManagementStateResource(kind, name, managementState string) *unstructured.Unstructured {
	obj := newUnstructuredResource("operator.openshift.io/v1", kind, "", name)
	if err := unstructured.SetNestedField(obj.Object, managementState, "spec", "managementState"); err != nil {
		panic(fmt.Sprintf("newManagementStateResource(%s/%s): setting spec.managementState: %v", kind, name, err))
	}
	return obj
}

func preflightListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		machineHealthCheckGVR: "MachineHealthCheckList",
		clusterAutoscalerGVR:  "ClusterAutoscalerList",
		machineAutoscalerGVR:  "MachineAutoscalerList",
		clusterCSIDriverGVR:   "ClusterCSIDriverList",
		storageOperatorGVR:    "StorageList",
	}
}

func TestHasTargetVCenterConfiguration(t *testing.T) {
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{Name: "fd-a", Server: "target-a.example.com"},
				{Name: "fd-b", Server: "target-b.example.com"},
			},
		},
	}

	tests := []struct {
		name  string
		spec  migrationv1alpha1.VmwareCloudFoundationMigrationSpec
		infra *configv1.Infrastructure
		want  bool
	}{
		{
			name: "returns false when migration has no failure domains",
			spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{},
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{
						Type: configv1.VSpherePlatformType,
						VSphere: &configv1.VSpherePlatformSpec{
							VCenters: []configv1.VSpherePlatformVCenterSpec{
								{Server: "target-a.example.com"},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "returns false when target vcenters missing",
			spec: migration.Spec,
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{
						Type: configv1.VSpherePlatformType,
						VSphere: &configv1.VSpherePlatformSpec{
							VCenters: []configv1.VSpherePlatformVCenterSpec{
								{Server: "source.example.com"},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "returns false when target failure domains missing",
			spec: migration.Spec,
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{
						Type: configv1.VSpherePlatformType,
						VSphere: &configv1.VSpherePlatformSpec{
							VCenters: []configv1.VSpherePlatformVCenterSpec{
								{Server: "target-a.example.com"},
								{Server: "target-b.example.com"},
							},
							FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
								{Name: "fd-a", Server: "target-a.example.com"},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "returns true when all target vcenters and failure domains present",
			spec: migration.Spec,
			infra: &configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
				Spec: configv1.InfrastructureSpec{
					PlatformSpec: configv1.PlatformSpec{
						Type: configv1.VSpherePlatformType,
						VSphere: &configv1.VSpherePlatformSpec{
							VCenters: []configv1.VSpherePlatformVCenterSpec{
								{Server: "source.example.com"},
								{Server: "target-a.example.com"},
								{Server: "target-b.example.com"},
							},
							FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
								{Name: "fd-a", Server: "target-a.example.com"},
								{Name: "fd-b", Server: "target-b.example.com"},
							},
						},
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler := &VmwareCloudFoundationMigrationReconciler{
				ConfigClient: configfake.NewClientset(tt.infra),
			}
			got, err := reconciler.hasTargetVCenterConfiguration(context.Background(), &migrationv1alpha1.VmwareCloudFoundationMigration{
				Spec: tt.spec,
			})
			if err != nil {
				t.Fatalf("hasTargetVCenterConfiguration: %v", err)
			}
			if got != tt.want {
				t.Fatalf("hasTargetVCenterConfiguration = %v, want %v", got, tt.want)
			}
		})
	}
}

type preflightTestInventory struct {
	datacenterName string
	clusterPath    string
	datastorePath  string
	networkPath    string
	folderPath     string
	templatePath   string
}

func discoverPreflightTestInventory(ctx context.Context, t *testing.T, serverURL *url.URL, username, password string) preflightTestInventory {
	t.Helper()

	client, err := govmomi.NewClient(ctx, serverURL, true)
	if err != nil {
		t.Fatalf("creating govmomi client: %v", err)
	}
	defer func() {
		if err := client.Logout(ctx); err != nil {
			t.Fatalf("logging out govmomi client: %v", err)
		}
	}()

	finder := find.NewFinder(client.Client, true)
	datacenter, err := finder.DefaultDatacenter(ctx)
	if err != nil {
		t.Fatalf("finding default datacenter: %v", err)
	}
	finder.SetDatacenter(datacenter)

	clusters, err := finder.ClusterComputeResourceList(ctx, "*")
	if err != nil || len(clusters) == 0 {
		t.Fatalf("listing clusters: %v", err)
	}
	datastores, err := finder.DatastoreList(ctx, "*")
	if err != nil || len(datastores) == 0 {
		t.Fatalf("listing datastores: %v", err)
	}
	networks, err := finder.NetworkList(ctx, "*")
	if err != nil || len(networks) == 0 {
		t.Fatalf("listing networks: %v", err)
	}

	session, err := vsphere.GetOrCreate(ctx, vsphere.Params{
		Server:     serverURL.Host,
		Datacenter: datacenter.Name(),
		Username:   username,
		Password:   password,
		Insecure:   true,
	})
	if err != nil {
		t.Fatalf("creating test vSphere session: %v", err)
	}

	folder, err := vsphere.CreateVMFolder(ctx, session, "preflight-test-folder")
	if err != nil {
		t.Fatalf("creating preflight test folder: %v", err)
	}

	dcFolders, err := datacenter.Folders(ctx)
	if err != nil {
		t.Fatalf("getting datacenter folders: %v", err)
	}
	rootPool, err := finder.ResourcePool(ctx, clusters[0].InventoryPath+"/Resources")
	if err != nil {
		t.Fatalf("finding root resource pool: %v", err)
	}
	vmSpec := types.VirtualMachineConfigSpec{
		Name:  "preflight-test-template",
		Files: &types.VirtualMachineFileInfo{VmPathName: fmt.Sprintf("[%s]", datastores[0].Name())},
	}
	createTask, err := dcFolders.VmFolder.CreateVM(ctx, vmSpec, rootPool, nil)
	if err != nil {
		t.Fatalf("creating preflight test template VM: %v", err)
	}
	if _, err := createTask.WaitForResult(ctx); err != nil {
		t.Fatalf("waiting for preflight test template VM: %v", err)
	}
	templateVM, err := finder.VirtualMachine(ctx, "preflight-test-template")
	if err != nil {
		t.Fatalf("finding preflight test template VM: %v", err)
	}

	return preflightTestInventory{
		datacenterName: datacenter.Name(),
		clusterPath:    clusters[0].InventoryPath,
		datastorePath:  datastores[0].InventoryPath,
		networkPath:    networks[0].GetInventoryPath(),
		folderPath:     folder.InventoryPath,
		templatePath:   templateVM.InventoryPath,
	}
}

func newMigrationForPreflight(server string, inventory preflightTestInventory) *migrationv1alpha1.VmwareCloudFoundationMigration {
	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "migration",
			Namespace: "default",
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{
				Name:      "target-vcenter-creds",
				Namespace: "default",
			},
			FailureDomainMappings: []migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-a", Destination: "fd-a"},
				{Source: "source-fd-b", Destination: "fd-b"},
			},
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{
					Name:   "fd-a",
					Region: "region-a",
					Zone:   "zone-a",
					Server: server,
					Topology: configv1.VSpherePlatformTopology{
						Template:       inventory.templatePath,
						Datacenter:     inventory.datacenterName,
						ComputeCluster: inventory.clusterPath,
						Datastore:      inventory.datastorePath,
						Networks:       []string{inventory.networkPath},
						Folder:         inventory.folderPath,
					},
				},
				{
					Name:   "fd-b",
					Region: "region-b",
					Zone:   "zone-b",
					Server: server,
					Topology: configv1.VSpherePlatformTopology{
						Template:       inventory.templatePath,
						Datacenter:     inventory.datacenterName,
						ComputeCluster: inventory.clusterPath,
						Datastore:      inventory.datastorePath,
						Networks:       []string{inventory.networkPath},
						Folder:         inventory.folderPath,
					},
				},
			},
		},
	}
}

func newTargetCredentialsSecret(namespace, name, server, username, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Data: map[string][]byte{
			server + ".username": []byte(username),
			server + ".password": []byte(password),
		},
	}
}

func newInfrastructureForPreflight(server, datacenter string) *configv1.Infrastructure {
	return &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: openshift.InfrastructureName},
		Spec: configv1.InfrastructureSpec{
			PlatformSpec: configv1.PlatformSpec{
				Type: configv1.VSpherePlatformType,
				VSphere: &configv1.VSpherePlatformSpec{
					VCenters: []configv1.VSpherePlatformVCenterSpec{
						{
							Server:      server,
							Port:        443,
							Datacenters: []string{datacenter},
						},
					},
					FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
						{
							Name:   "source-fd-a",
							Server: server,
							Topology: configv1.VSpherePlatformTopology{
								Datacenter:   datacenter,
								ResourcePool: "/" + datacenter + "/host/cluster-a/Resources/pool-a",
							},
						},
						{
							Name:   "source-fd-b",
							Server: server,
							Topology: configv1.VSpherePlatformTopology{
								Datacenter:   datacenter,
								ResourcePool: "/" + datacenter + "/host/cluster-b/Resources/pool-b",
							},
						},
					},
				},
			},
		},
		Status: configv1.InfrastructureStatus{InfrastructureName: "test-infra"},
	}
}

func newClusterVersionForPreflight(version string, progressing bool) *configv1.ClusterVersion {
	clusterVersion := &configv1.ClusterVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "version"},
		Status: configv1.ClusterVersionStatus{
			Desired: configv1.Release{Version: version},
		},
	}
	if progressing {
		clusterVersion.Status.Conditions = append(clusterVersion.Status.Conditions, configv1.ClusterOperatorStatusCondition{
			Type:   configv1.OperatorProgressing,
			Status: configv1.ConditionTrue,
		})
	}
	return clusterVersion
}

func newFeatureGateForPreflight(version string, enabled bool) *configv1.FeatureGate {
	details := configv1.FeatureGateDetails{Version: version}
	if enabled {
		details.Enabled = []configv1.FeatureGateAttributes{
			{Name: configv1.FeatureGateName("VSphereMultiVCenterDay2")},
		}
	}

	return &configv1.FeatureGate{
		ObjectMeta: metav1.ObjectMeta{Name: testInfraName},
		Status: configv1.FeatureGateStatus{
			FeatureGates: []configv1.FeatureGateDetails{details},
		},
	}
}

func TestFDFailureDomainTemplateMissing(t *testing.T) {
	setFD := func(name, tpl string) configv1.VSpherePlatformFailureDomainSpec {
		return configv1.VSpherePlatformFailureDomainSpec{Name: name, Topology: configv1.VSpherePlatformTopology{Template: tpl}}
	}
	if err := fdTemplateMissing([]configv1.VSpherePlatformFailureDomainSpec{setFD("a", "/DC0/vm/t"), setFD("b", "/DC0/vm/u")}); err != nil {
		t.Fatalf("expected nil when every template is set, got: %v", err)
	}
	for _, tt := range []struct {
		name string
		fds  []configv1.VSpherePlatformFailureDomainSpec
		want string
	}{
		{"first missing", []configv1.VSpherePlatformFailureDomainSpec{setFD("a", ""), setFD("b", "/DC0/vm/u")}, `spec.failureDomains[0].topology.template is required (failure domain "a")`},
		{"second missing", []configv1.VSpherePlatformFailureDomainSpec{setFD("a", "/DC0/vm/t"), setFD("b", "")}, `spec.failureDomains[1].topology.template is required (failure domain "b")`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := fdTemplateMissing(tt.fds); err == nil || err.Error() != tt.want {
				t.Fatalf("fdTemplateMissing() = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDCVMFolderMissingVMPFolder(t *testing.T) {
	ctx := context.Background()
	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("Create simulator model: %v", err)
	}
	defer model.Remove()

	model.Service.TLS = new(tls.Config)
	model.Service.RegisterEndpoints = true
	server := model.Service.NewServer()
	defer server.Close()

	client, err := govmomi.NewClient(ctx, server.URL, true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Absent the vmFolder property: Folders() still returns a non-nil
	// VmFolder, but with a zero managed-object reference.
	simDC, ok := model.Service.Context.Map.Any("Datacenter").(*simulator.Datacenter)
	if !ok {
		t.Fatal("TestDCVMFolderMissingVMPFolder: simulator datacenter not found")
	}
	simDC.VmFolder = types.ManagedObjectReference{}

	finder := find.NewFinder(client.Client)
	datacenter, err := finder.Datacenter(ctx, testSourceDC)
	if err != nil {
		t.Fatalf("Finder.Datacenter: %v", err)
	}

	if _, err := dcVMFolder(ctx, datacenter); err == nil {
		t.Fatal("dcVMFolder: expected error for datacenter without vmFolder property")
	} else if !strings.Contains(err.Error(), "has no VM folder") {
		t.Fatalf("dcVMFolder: unexpected error: %v", err)
	}
}

// TestValidatePreflightSourceCoverage covers the preflight source MachineSet
// coverage gate: worker capacity on the source vCenter that no mapped source
// failure domain covers must be rejected before the mapping is published and
// the control plane is retargeted, while zero-replica and empty cases pass.
func TestValidateMigrationFailureDomainsStatusCardinality(t *testing.T) {
	ctx := context.Background()
	sourceFD := newSourceFailureDomainForMapping("source-fd-1", "dc-source", "rp-a")
	destFD := newTargetFailureDomain("target-fd-1")
	mappings := []migrationv1alpha1.FailureDomainMapping{{Source: sourceFD.Name, Destination: destFD.Name}}

	tests := []struct {
		name              string
		sourceMachineSets int
		wantErr           string
	}{
		{
			name:              "generated names at the status limit pass",
			sourceMachineSets: maxDestinationWorkerMachineSets,
		},
		{
			name:              "generated names above the status limit fail",
			sourceMachineSets: maxDestinationWorkerMachineSets + 1,
			wantErr:           fmt.Sprintf("failure domain mapping would generate %d destination MachineSets, exceeding the %d supported by status.workerMachineSetNames", maxDestinationWorkerMachineSets+1, maxDestinationWorkerMachineSets),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Preflight resolves the source failure domains from an Infrastructure
			// that still has a single vCenter.
			infra := newInfrastructureForMapping(sourceFD)
			infra.Spec.PlatformSpec.VSphere.VCenters = []configv1.VSpherePlatformVCenterSpec{{Server: sourceFD.Server}}
			objects := []runtime.Object{infra}
			for i := 0; i < tt.sourceMachineSets; i++ {
				objects = append(objects, newSourceMachineSetForMapping(fmt.Sprintf("source-worker-%03d", i), "dc-source", "rp-a", 1, 4))
			}
			reconciler := newRolloutReconciler(t, objects)
			migration := newMappingTestMigration([]configv1.VSpherePlatformFailureDomainSpec{destFD}, mappings)

			_, err := reconciler.validateMigrationFailureDomains(ctx, migration)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateMigrationFailureDomains: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateMigrationFailureDomains error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidateMigrationFailureDomainsStatusCardinalityAcrossMappings covers the
// aggregate limit: status.workerMachineSetNames holds every destination name of
// the whole migration, so two mappings that each fit must still be rejected
// together before any destination MachineSet is created.
func TestValidateMigrationFailureDomainsStatusCardinalityAcrossMappings(t *testing.T) {
	ctx := context.Background()
	sourceFDs := []configv1.VSpherePlatformFailureDomainSpec{
		newSourceFailureDomainForMapping("source-fd-1", "dc-source-1", "rp-a"),
		newSourceFailureDomainForMapping("source-fd-2", "dc-source-2", "rp-b"),
	}
	destFDs := []configv1.VSpherePlatformFailureDomainSpec{
		newTargetFailureDomain("target-fd-1"),
		newTargetFailureDomain("target-fd-2"),
	}
	mappings := []migrationv1alpha1.FailureDomainMapping{
		{Source: "source-fd-1", Destination: "target-fd-1"},
		{Source: "source-fd-2", Destination: "target-fd-2"},
	}

	tests := []struct {
		name    string
		counts  [2]int
		wantErr string
	}{
		{
			name:    "generated names summed across mappings above the limit fail",
			counts:  [2]int{129, 128},
			wantErr: fmt.Sprintf("failure domain mapping would generate %d destination MachineSets, exceeding the %d supported by status.workerMachineSetNames", 257, maxDestinationWorkerMachineSets),
		},
		{
			name:   "generated names summed across mappings at the limit pass",
			counts: [2]int{128, 128},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Preflight resolves the source failure domains from an Infrastructure
			// that still has a single vCenter.
			infra := newInfrastructureForMapping(sourceFDs...)
			infra.Spec.PlatformSpec.VSphere.VCenters = []configv1.VSpherePlatformVCenterSpec{{Server: "source.example.com"}}
			objects := []runtime.Object{infra}
			for i := 0; i < tt.counts[0]; i++ {
				objects = append(objects, newSourceMachineSetForMapping(fmt.Sprintf("source-a-%03d", i), "dc-source-1", "rp-a", 1, 4))
			}
			for i := 0; i < tt.counts[1]; i++ {
				objects = append(objects, newSourceMachineSetForMapping(fmt.Sprintf("source-b-%03d", i), "dc-source-2", "rp-b", 1, 4))
			}
			reconciler := newRolloutReconciler(t, objects)
			creates := 0
			reconciler.MachineClient.(*machinefake.Clientset).PrependReactor("create", "machinesets",
				func(ktesting.Action) (bool, runtime.Object, error) {
					creates++
					return false, nil, nil
				})
			migration := newMappingTestMigration(destFDs, nil)
			migration.Spec.FailureDomainMappings = mappings

			got, err := reconciler.validateMigrationFailureDomains(ctx, migration)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateMigrationFailureDomains: %v", err)
				}
				if !reflect.DeepEqual(got, mappings) {
					t.Fatalf("resolved mappings = %+v, want %+v", got, mappings)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateMigrationFailureDomains error = %v, want containing %q", err, tt.wantErr)
				}
				if got != nil {
					t.Fatalf("mappings published alongside the rejection: %+v", got)
				}
			}
			if creates != 0 {
				t.Fatalf("destination MachineSet creates = %d, want 0 (the count is checked before creation)", creates)
			}
		})
	}
}

func TestValidatePreflightSourceCoverage(t *testing.T) {
	ctx := context.Background()
	mappings := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}}

	tests := []struct {
		name    string
		objects []runtime.Object
		wantErr string
	}{
		{
			name: "mapped source MachineSet passes",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newSourceMachineSetForRollout(1),
			},
		},
		{
			name: "no source MachineSets passes",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
			},
		},
		{
			name: "zero-replica out-of-topology MachineSet passes",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newOutOfTopologySourceMachineSetForRollout("source-worker-other", 0),
			},
		},
		{
			name: "positive-replica out-of-topology MachineSet fails",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newOutOfTopologySourceMachineSetForRollout("source-worker-other", 1),
			},
			wantErr: "does not match any source failure domain",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRolloutReconciler(t, tt.objects)
			err := r.validatePreflightSourceCoverage(ctx, mappings)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validatePreflightSourceCoverage: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validatePreflightSourceCoverage: nil error, want containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validatePreflightSourceCoverage: %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
