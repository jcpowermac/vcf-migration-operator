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
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
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
			configObjects := []runtime.Object{
				newInfrastructureForPreflight(server.URL.Host, inventory.datacenterName),
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

	return preflightTestInventory{
		datacenterName: datacenter.Name(),
		clusterPath:    clusters[0].InventoryPath,
		datastorePath:  datastores[0].InventoryPath,
		networkPath:    networks[0].GetInventoryPath(),
		folderPath:     folder.InventoryPath,
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
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
				{
					Name:   "fd-a",
					Region: "region-a",
					Zone:   "zone-a",
					Server: server,
					Topology: configv1.VSpherePlatformTopology{
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
				},
			},
		},
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
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status: configv1.FeatureGateStatus{
			FeatureGates: []configv1.FeatureGateDetails{details},
		},
	}
}
