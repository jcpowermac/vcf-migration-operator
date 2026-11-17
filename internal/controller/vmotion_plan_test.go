package controller

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	machinefake "github.com/openshift/client-go/machine/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

func TestVMotionDispatchBlocksBeforeLegacyPreflight(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       migrationv1alpha1.SingletonName,
			Namespace:  migrationv1alpha1.SingletonNamespace,
			UID:        "migration-uid",
			Finalizers: []string{migrationv1alpha1.FinalizerName},
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			State: migrationv1alpha1.MigrationStateRunning,
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
				Name:   "target-fd",
				Server: "target.example.com",
				Topology: configv1.VSpherePlatformTopology{
					Datacenter: "TargetDC", ComputeCluster: "/TargetDC/host/TargetCluster",
					Datastore: "/TargetDC/datastore/TargetDS", Networks: []string{"VM Network"},
				},
			}},
			NodeMigration: &migrationv1alpha1.NodeMigrationSpec{
				Workers: migrationv1alpha1.RoleMigrationSpec{
					Type:    migrationv1alpha1.NodeMigrationTypeVMotion,
					VMotion: &migrationv1alpha1.VMotionSpec{Mode: migrationv1alpha1.VMotionModeHot},
				},
				ControlPlane: migrationv1alpha1.RoleMigrationSpec{
					Type:    migrationv1alpha1.NodeMigrationTypeVMotion,
					VMotion: &migrationv1alpha1.VMotionSpec{Mode: migrationv1alpha1.VMotionModeHot},
				},
			},
		},
	}

	apiClient := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(migration).WithStatusSubresource(migration).Build()
	legacyProbeCalled := false
	r := &VmwareCloudFoundationMigrationReconciler{
		Client:     apiClient,
		APIReader:  apiClient,
		KubeClient: fakekube.NewClientset(),
		Recorder:   events.NewFakeRecorder(10),
		checkNetworkingViaProbeVMsFunc: func(context.Context, *migrationv1alpha1.VmwareCloudFoundationMigration, *configv1.VSpherePlatformVCenterSpec) error {
			legacyProbeCalled = true
			return nil
		},
	}

	key := client.ObjectKeyFromObject(migration)
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := apiClient.Get(ctx, key, got); err != nil {
		t.Fatalf("getting migration: %v", err)
	}
	condition := apimeta.FindStatusCondition(got.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if condition == nil || condition.Status != metav1.ConditionFalse || !strings.Contains(condition.Message, "exactly three control-plane Nodes") {
		t.Fatalf("InfrastructurePrepared = %+v, want actionable unsupported-node-count block", condition)
	}
	if strings.Contains(condition.Message, "template") {
		t.Fatalf("VMotion dispatch reached legacy template validation: %q", condition.Message)
	}
	if legacyProbeCalled {
		t.Fatal("legacy probe path was called for explicit VMotion")
	}
}

func TestMixedExplicitVMotionSelectionDoesNotFallThroughToRecreate(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	migration := newVMotionPlanMigration()
	migration.Spec.NodeMigration.ControlPlane.Type = migrationv1alpha1.NodeMigrationTypeRecreate
	migration.Spec.NodeMigration.ControlPlane.VMotion = nil
	apiClient := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(migration).WithStatusSubresource(migration).Build()
	r := &VmwareCloudFoundationMigrationReconciler{
		Client: apiClient, APIReader: apiClient, KubeClient: fakekube.NewClientset(), Recorder: events.NewFakeRecorder(10),
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(migration)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := apiClient.Get(ctx, client.ObjectKeyFromObject(migration), got); err != nil {
		t.Fatalf("getting migration: %v", err)
	}
	condition := apimeta.FindStatusCondition(got.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if condition == nil || condition.Status != metav1.ConditionFalse || !strings.Contains(condition.Message, "both roles must explicitly select VMotion") {
		t.Fatalf("InfrastructurePrepared = %+v, want unsupported mixed VMotion selection block", condition)
	}
}

func TestParseVMotionProviderID(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		want       string
		wantErr    bool
	}{
		{name: "canonical", providerID: "vsphere://11111111-1111-4111-8111-111111111111", want: "11111111-1111-4111-8111-111111111111"},
		{name: "case insensitive", providerID: "VSPHERE://11111111-1111-4111-8111-111111111111", want: "11111111-1111-4111-8111-111111111111"},
		{name: "missing scheme", providerID: "11111111-1111-4111-8111-111111111111", wantErr: true},
		{name: "malformed UUID", providerID: "vsphere://not-a-uuid", wantErr: true},
		{name: "noncanonical UUID", providerID: "vsphere://11111111111141118111111111111111", wantErr: true},
		{name: "zero UUID", providerID: "vsphere://00000000-0000-0000-0000-000000000000", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVMotionProviderID(tt.providerID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseVMotionProviderID(%q) error = %v, wantErr %v", tt.providerID, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("parseVMotionProviderID(%q) = %q, want %q", tt.providerID, got, tt.want)
			}
		})
	}
}

func TestSameVMotionVCenter(t *testing.T) {
	if !sameVMotionVCenter("https://SOURCE.example.com/sdk", cleanupTestSource) {
		t.Fatal("case and URL variants of the same vCenter must compare equal")
	}
	if sameVMotionVCenter(cleanupTestSource, "target.example.com") {
		t.Fatal("different vCenters must compare unequal")
	}
}

func TestValidateVMotionNodeRoles(t *testing.T) {
	valid := []corev1.Node{
		newVMotionPlanNode("control-plane-a", "control-plane", "vsphere://11111111-1111-4111-8111-111111111111", "uid-a", false),
		newVMotionPlanNode("control-plane-b", "master", "vsphere://22222222-2222-4222-8222-222222222222", "uid-b", false),
		newVMotionPlanNode("control-plane-c", "control-plane", "vsphere://33333333-3333-4333-8333-333333333333", "uid-c", false),
		newVMotionPlanNode("worker-a", "worker", "vsphere://44444444-4444-4444-8444-444444444444", "uid-d", false),
	}
	valid[0].Labels["node-role.kubernetes.io/master"] = ""
	if err := validateVMotionNodeRoles(valid); err != nil {
		t.Fatalf("validateVMotionNodeRoles(valid): %v", err)
	}
	dual := newVMotionPlanNode("dual", "worker", "vsphere://55555555-5555-4555-8555-555555555555", "uid-e", false)
	dual.Labels["node-role.kubernetes.io/control-plane"] = ""
	tests := []struct {
		name    string
		nodes   []corev1.Node
		wantErr string
	}{
		{name: "dual role", nodes: append(append([]corev1.Node(nil), valid...), dual), wantErr: "dual-role"},
		{name: "unclassified", nodes: append(append([]corev1.Node(nil), valid...), corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unknown"}}), wantErr: "unclassified"},
		{name: "compact cluster", nodes: valid[1:], wantErr: "exactly three control-plane Nodes"},
		{name: "no workers", nodes: valid[:3], wantErr: "at least one worker Node"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVMotionNodeRoles(tt.nodes)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateVMotionNodeRoles error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestSelectVMotionSourceFailureDomainByOwner(t *testing.T) {
	fds := []configv1.VSpherePlatformFailureDomainSpec{
		{Name: "source-a", Zone: "zone-a"},
		{Name: "source-b", Zone: "zone-b"},
	}
	refA := vsphere.VMotionManagedObjectReference{VCenter: cleanupTestSource, Datacenter: "SourceDC", Type: "ComputeResource", Value: "domain-11"}
	refB := vsphere.VMotionManagedObjectReference{VCenter: cleanupTestSource, Datacenter: "SourceDC", Type: "ClusterComputeResource", Value: "domain-22"}
	refs := map[string]vsphere.VMotionManagedObjectReference{"source-a": refA, "source-b": refB}
	tests := []struct {
		name    string
		node    *corev1.Node
		owner   vsphere.VMotionManagedObjectReference
		refs    map[string]vsphere.VMotionManagedObjectReference
		fds     []configv1.VSpherePlatformFailureDomainSpec
		want    string
		wantErr string
	}{
		{name: "single FD still resolves resource-pool owner", owner: refA, refs: refs, fds: fds[:1], want: "source-a"},
		{name: "single FD still requires matching owner", owner: refB, refs: refs, fds: fds[:1], wantErr: "does not match a configured source failure domain"},
		{name: "multi FD matches owner type and ID", owner: refB, refs: refs, fds: fds, want: "source-b"},
		{name: "missing zone label is allowed", owner: refA, refs: refs, fds: fds, want: "source-a"},
		{name: "zone label cross-checks matched FD", node: zoneLabeledNode("zone-a"), owner: refA, refs: refs, fds: fds, want: "source-a"},
		{name: "zone mismatch blocks", node: zoneLabeledNode("zone-b"), owner: refA, refs: refs, fds: fds, wantErr: "conflicts with the matched source failure domain"},
		{name: "reference type mismatch blocks", owner: vsphere.VMotionManagedObjectReference{VCenter: refA.VCenter, Datacenter: refA.Datacenter, Type: "ClusterComputeResource", Value: refA.Value}, refs: refs, fds: fds, wantErr: "does not match a configured source failure domain"},
		{name: "reference ID mismatch blocks", owner: vsphere.VMotionManagedObjectReference{VCenter: refA.VCenter, Datacenter: refA.Datacenter, Type: refA.Type, Value: "other"}, refs: refs, fds: fds, wantErr: "does not match a configured source failure domain"},
		{name: "vCenter mismatch blocks", owner: vsphere.VMotionManagedObjectReference{VCenter: "other.example.com", Datacenter: refA.Datacenter, Type: refA.Type, Value: refA.Value}, refs: refs, fds: fds, wantErr: "different vCenter"},
		{name: "datacenter mismatch blocks", owner: vsphere.VMotionManagedObjectReference{VCenter: refA.VCenter, Datacenter: "OtherDC", Type: refA.Type, Value: refA.Value}, refs: refs, fds: fds, wantErr: "outside the configured source datacenter"},
		{name: "missing owner blocks", owner: vsphere.VMotionManagedObjectReference{}, refs: refs, fds: fds, wantErr: "resource pool owner reference is incomplete"},
		{name: "duplicate resolved refs block", owner: refA, refs: map[string]vsphere.VMotionManagedObjectReference{"source-a": refA, "source-b": refA}, fds: fds, wantErr: "matches multiple source failure domains"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := tt.node
			if node == nil {
				node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
			}
			got, err := selectVMotionSourceFailureDomainByOwner(node, tt.owner, cleanupTestSource, vmotionPlanTestSourceDatacenter, tt.fds, tt.refs)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("selectVMotionSourceFailureDomainByOwner error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got.Name != tt.want {
				t.Fatalf("selected source failure domain = %+v, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func zoneLabeledNode(zone string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Labels: map[string]string{corev1.LabelTopologyZone: zone}}}
}

func TestUniqueVMotionVMForNode(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	vm := vsphere.VMotionVM{BIOSUUID: id, InventoryPath: "/DC/vm/node"}
	tests := []struct {
		name    string
		matches map[string][]vsphere.VMotionVM
		wantErr string
	}{
		{name: "unique VM", matches: map[string][]vsphere.VMotionVM{id: {vm}}},
		{name: "missing VM", matches: map[string][]vsphere.VMotionVM{}, wantErr: "no source vSphere VM"},
		{name: "duplicate UUID is ambiguous", matches: map[string][]vsphere.VMotionVM{id: {vm, {BIOSUUID: id, InventoryPath: "/DC/vm/duplicate"}}}, wantErr: "matches 2 source vSphere VMs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := uniqueVMotionVMForNode("worker-a", id, tt.matches)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("uniqueVMotionVMForNode error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got.InventoryPath != "/DC/vm/node" {
				t.Fatalf("uniqueVMotionVMForNode = %+v, %v, want unique VM /DC/vm/node", got, err)
			}
		})
	}
}

const vmotionPlanTestSourceDatacenter = "SourceDC"
const vmotionPlanTestSecondTargetFD = "target-fd-2"

type vmotionPlanHarness struct {
	migration              *migrationv1alpha1.VmwareCloudFoundationMigration
	apiClient              client.Client
	kubeClient             kubernetes.Interface
	reconciler             *VmwareCloudFoundationMigrationReconciler
	key                    client.ObjectKey
	providerIDs            []string
	expectedInventoryCount int
	inventoryCalls         int
	sourceFDReference      vsphere.VMotionManagedObjectReference
	resourcePoolOwner      vsphere.VMotionManagedObjectReference
}

func newVMotionPlanHarness(t *testing.T) *vmotionPlanHarness {
	t.Helper()
	const sourceServer = cleanupTestSource
	const sourceCluster = "/SourceDC/host/SourceCluster"
	providerIDs := []string{
		"vsphere://11111111-1111-4111-8111-111111111111",
		"vsphere://22222222-2222-4222-8222-222222222222",
		"vsphere://33333333-3333-4333-8333-333333333333",
		"vsphere://44444444-4444-4444-8444-444444444444",
		"vsphere://55555555-5555-4555-8555-555555555555",
		"vsphere://66666666-6666-4666-8666-666666666666",
	}
	nodes := []corev1.Node{
		newVMotionPlanNode("worker-b", "worker", providerIDs[4], "node-uid-5", true),
		newVMotionPlanNode("control-plane-c", "control-plane", providerIDs[2], "node-uid-3", false),
		newVMotionPlanNode("worker-a", "worker", providerIDs[3], "node-uid-4", false),
		newVMotionPlanNode("control-plane-a", "control-plane", providerIDs[0], "node-uid-1", false),
		newVMotionPlanNode("control-plane-b", "control-plane", providerIDs[1], "node-uid-2", true),
	}
	migration := newVMotionPlanMigration()
	migration.UID = types.UID("migration-uid")
	migration.Spec.FailureDomainMappings = []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd", Destination: "target-fd"}}
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	apiClient := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(migration).WithStatusSubresource(migration).Build()
	infrastructure := newInfrastructureForReadyTest([]string{sourceServer})
	infrastructure.Spec.PlatformSpec.VSphere.VCenters[0].Datacenters = []string{vmotionPlanTestSourceDatacenter}
	infrastructure.Spec.PlatformSpec.VSphere.FailureDomains = []configv1.VSpherePlatformFailureDomainSpec{{
		Name: "source-fd", Server: sourceServer, Region: "source-region", Zone: "source-zone",
		Topology: configv1.VSpherePlatformTopology{
			Datacenter: vmotionPlanTestSourceDatacenter, ComputeCluster: sourceCluster,
			ResourcePool: "/SourceDC/host/SourceCluster/Resources",
		},
	}}
	kubeClient := fakekube.NewClientset(&nodes[0], &nodes[1], &nodes[2], &nodes[3], &nodes[4])
	harness := &vmotionPlanHarness{
		migration: migration, apiClient: apiClient, kubeClient: kubeClient,
		key: client.ObjectKeyFromObject(migration), providerIDs: providerIDs, expectedInventoryCount: len(nodes),
	}
	harness.sourceFDReference = vsphere.VMotionManagedObjectReference{VCenter: sourceServer, Datacenter: vmotionPlanTestSourceDatacenter, Type: "ComputeResource", Value: "domain-source"}
	harness.resourcePoolOwner = harness.sourceFDReference
	harness.reconciler = &VmwareCloudFoundationMigrationReconciler{
		Client: apiClient, APIReader: apiClient, KubeClient: kubeClient,
		ConfigClient: configfake.NewClientset(infrastructure), MachineClient: machinefake.NewClientset(),
		Recorder: events.NewFakeRecorder(10),
		vmotionInventoryFunc: func(_ context.Context, sourceVC *configv1.VSpherePlatformVCenterSpec, liveServer string, sourceFDs []configv1.VSpherePlatformFailureDomainSpec, ids []string) (vsphere.VMotionInventory, error) {
			harness.inventoryCalls++
			if !sameVMotionVCenter(sourceVC.Server, sourceServer) || !sameVMotionVCenter(liveServer, sourceServer) || len(ids) != harness.expectedInventoryCount {
				t.Fatalf("VM inventory request = source %q live %q and %d identities, want source %q live %q and %d identities", sourceVC.Server, liveServer, len(ids), sourceServer, sourceServer, harness.expectedInventoryCount)
			}
			fdRefs := make(map[string]vsphere.VMotionManagedObjectReference, len(sourceFDs))
			for i := range sourceFDs {
				fdRefs[sourceFDs[i].Name] = harness.sourceFDReference
			}
			vms := make([]vsphere.VMotionVM, 0, len(ids))
			for _, id := range ids {
				uuid := strings.TrimPrefix(id, "vsphere://")
				vms = append(vms, vsphere.VMotionVM{
					BIOSUUID: uuid, InventoryPath: "/SourceDC/vm/" + uuid,
					ResourcePoolRef:   vsphere.VMotionManagedObjectReference{VCenter: sourceVC.Server, Datacenter: vmotionPlanTestSourceDatacenter, Type: "ResourcePool", Value: "resgroup-1"},
					ResourcePoolOwner: harness.resourcePoolOwner, NetworkAdapterCount: 1,
				})
			}
			return vsphere.VMotionInventory{VMs: vms, SourceFailureDomainReferences: fdRefs}, nil
		},
	}
	return harness
}

func (h *vmotionPlanHarness) reconcile(t *testing.T) {
	t.Helper()
	if _, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := h.apiClient.Get(context.Background(), h.key, h.migration); err != nil {
		t.Fatalf("getting migration: %v", err)
	}
}

func (h *vmotionPlanHarness) reconcileUntilFrozen(t *testing.T) {
	t.Helper()
	for range 3 {
		h.reconcile(t)
		if h.migration.Status.NodeMigration != nil && h.migration.Status.NodeMigration.FrozenPlan != nil {
			return
		}
	}
	t.Fatalf("nodeMigration status = %+v, want a frozen plan", h.migration.Status.NodeMigration)
}

func TestVMotionReconcileFreezesAllNodeIdentitiesAndPlacement(t *testing.T) {
	h := newVMotionPlanHarness(t)
	h.reconcileUntilFrozen(t)
	assertVMotionFrozenPlan(t, h)

	status := h.migration.Status.NodeMigration
	executionID, fingerprint := status.ExecutionID, status.PlanFingerprint
	h.reconcile(t)
	if h.migration.Status.NodeMigration.ExecutionID != executionID || h.migration.Status.NodeMigration.PlanFingerprint != fingerprint {
		t.Fatalf("unchanged reconcile replaced execution %q/fingerprint %q with %q/%q", executionID, fingerprint, h.migration.Status.NodeMigration.ExecutionID, h.migration.Status.NodeMigration.PlanFingerprint)
	}
	assertVMotionNodeAdditionBlocked(t, h, executionID)
}

func TestVMotionReconcileBlocksMissingEffectiveMappingBeforeInventory(t *testing.T) {
	h := newVMotionPlanHarness(t)
	secondFD := h.migration.Spec.FailureDomains[0]
	secondFD.Name = vmotionPlanTestSecondTargetFD
	h.migration.Spec.FailureDomains = append(h.migration.Spec.FailureDomains, secondFD)
	h.migration.Spec.FailureDomainMappings = nil
	if err := h.apiClient.Update(context.Background(), h.migration); err != nil {
		t.Fatalf("updating migration fixture: %v", err)
	}
	h.reconcile(t)
	condition := apimeta.FindStatusCondition(h.migration.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if condition == nil || condition.Status != metav1.ConditionFalse || !strings.Contains(condition.Message, "spec.failureDomainMappings is required") {
		t.Fatalf("InfrastructurePrepared = %+v, want missing effective mapping block", condition)
	}
	if h.inventoryCalls != 0 || h.migration.Status.NodeMigration != nil || len(h.migration.Status.FailureDomainMappings) != 0 {
		t.Fatalf("ambiguous mapping caused plan/inventory side effects: calls=%d status=%+v", h.inventoryCalls, h.migration.Status)
	}
}

func TestVMotionPreflightRetainsSupportedFailureDomainTypeChecks(t *testing.T) {
	h := newVMotionPlanHarness(t)
	infrastructure, err := h.reconciler.ConfigClient.ConfigV1().Infrastructures().Get(context.Background(), openshift.InfrastructureName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Infrastructure: %v", err)
	}
	infrastructure.Spec.PlatformSpec.VSphere.FailureDomains[0].RegionAffinity = &configv1.VSphereFailureDomainRegionAffinity{
		Type: configv1.ComputeClusterFailureDomainRegion,
	}
	if _, err := h.reconciler.ConfigClient.ConfigV1().Infrastructures().Update(context.Background(), infrastructure, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating Infrastructure fixture: %v", err)
	}
	h.reconcile(t)
	condition := apimeta.FindStatusCondition(h.migration.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if condition == nil || condition.Status != metav1.ConditionFalse || !strings.Contains(condition.Message, "unsupported region type") {
		t.Fatalf("InfrastructurePrepared = %+v, want unsupported source failure-domain type block", condition)
	}
	if h.inventoryCalls != 0 || len(h.migration.Status.FailureDomainMappings) != 0 {
		t.Fatalf("unsupported failure-domain type caused preflight side effects: inventory calls %d, mappings %+v", h.inventoryCalls, h.migration.Status.FailureDomainMappings)
	}
}

func TestVMotionReconcileBlocksChangedMappingBeforeInventory(t *testing.T) {
	h := newVMotionPlanHarness(t)
	h.reconcileUntilFrozen(t)
	secondFD := h.migration.Spec.FailureDomains[0]
	secondFD.Name = vmotionPlanTestSecondTargetFD
	h.migration.Spec.FailureDomains = append(h.migration.Spec.FailureDomains, secondFD)
	h.migration.Spec.FailureDomainMappings[0].Destination = vmotionPlanTestSecondTargetFD
	if err := h.apiClient.Update(context.Background(), h.migration); err != nil {
		t.Fatalf("updating migration fixture: %v", err)
	}
	h.reconcile(t)
	condition := apimeta.FindStatusCondition(h.migration.Status.Conditions, migrationv1alpha1.ConditionAccepted)
	if condition == nil || condition.Status != metav1.ConditionFalse || !strings.Contains(condition.Message, "immutable after the migration starts") {
		t.Fatalf("Accepted = %+v, want changed mapping block", condition)
	}
	if h.inventoryCalls != 1 || h.migration.Status.NodeMigration.Workers.Total != 2 {
		t.Fatalf("changed mapping altered frozen plan or queried vSphere: calls=%d, status=%+v", h.inventoryCalls, h.migration.Status.NodeMigration)
	}
}

func TestVMotionReconcileBlocksChangedResourcePoolOwnerWithoutRemapping(t *testing.T) {
	h := newVMotionPlanHarness(t)
	h.reconcileUntilFrozen(t)
	original := h.migration.Status.NodeMigration.DeepCopy()
	h.sourceFDReference.Value = "domain-replaced"
	h.resourcePoolOwner.Value = "domain-replaced"
	h.reconcile(t)
	condition := apimeta.FindStatusCondition(h.migration.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != migrationv1alpha1.ReasonFailed ||
		!strings.Contains(condition.Message, "frozen VMotion") {
		t.Fatalf("InfrastructurePrepared after owner change = %+v, want frozen-plan block", condition)
	}
	if h.migration.Status.NodeMigration.PlanFingerprint != original.PlanFingerprint ||
		h.migration.Status.NodeMigration.Workers.Nodes[0].TargetFailureDomain != original.Workers.Nodes[0].TargetFailureDomain {
		t.Fatalf("owner drift changed the frozen plan: got %+v, want %+v", h.migration.Status.NodeMigration, original)
	}
}

func assertVMotionFrozenPlan(t *testing.T, h *vmotionPlanHarness) {
	t.Helper()
	status := h.migration.Status.NodeMigration
	if status == nil || status.FrozenPlan == nil {
		t.Fatalf("nodeMigration status = %+v, want a frozen plan", status)
	}
	if status.Type != migrationv1alpha1.NodeMigrationTypeVMotion || status.MigrationUID != h.migration.UID || len(status.PlanFingerprint) != 64 {
		t.Fatalf("nodeMigration identity = %+v, want VMotion, matching UID, and SHA-256 fingerprint", status)
	}
	if status.FrozenPlan.SourceVCenter != cleanupTestSource || status.FrozenPlan.SourceDatacenter != vmotionPlanTestSourceDatacenter ||
		status.FrozenPlan.DestinationVCenter != "target.example.com" || status.FrozenPlan.Mode != migrationv1alpha1.VMotionModeHot {
		t.Fatalf("frozen plan = %+v, want source/destination/mode identities", status.FrozenPlan)
	}
	if status.ControlPlane == nil || status.ControlPlane.Total != 3 || status.Workers == nil || status.Workers.Total != 2 {
		t.Fatalf("role progress = controlPlane %+v, workers %+v, want totals 3 and 2", status.ControlPlane, status.Workers)
	}
	if status.ControlPlane.Nodes[0].Name != "control-plane-a" || status.Workers.Nodes[0].Name != "worker-a" {
		t.Fatalf("node order = controlPlane %q, workers %q, want stable role order", status.ControlPlane.Nodes[0].Name, status.Workers.Nodes[0].Name)
	}
	assertVMotionNodeTargets(t, append(status.ControlPlane.Nodes, status.Workers.Nodes...))
	if len(h.migration.Status.FailureDomainMappings) != 1 || h.migration.Status.FailureDomainMappings[0] != (migrationv1alpha1.FailureDomainMapping{Source: "source-fd", Destination: "target-fd"}) {
		t.Fatalf("effective status mapping = %+v, want source-fd to target-fd", h.migration.Status.FailureDomainMappings)
	}
	if h.inventoryCalls != 1 {
		t.Fatalf("source inventory calls = %d, want 1", h.inventoryCalls)
	}
}

func assertVMotionNodeTargets(t *testing.T, nodes []migrationv1alpha1.NodeMigrationProgress) {
	t.Helper()
	for _, progress := range nodes {
		if progress.NodeUID == "" || progress.ProviderID == "" || progress.VMBIOSUUID == "" || progress.SourceInventoryPath == "" || progress.RequestedMode != migrationv1alpha1.VMotionModeHot {
			t.Fatalf("Node %q frozen identity = %+v, want Node/VM IDs and Hot mode", progress.Name, progress)
		}
		if progress.TargetFailureDomain != "target-fd" || progress.TargetResourcePool != "/TargetDC/host/TargetCluster/Resources" ||
			progress.TargetDatastore != "/TargetDC/datastore/TargetDS" || progress.TargetNetwork != "/TargetDC/network/VM Network" {
			t.Fatalf("Node %q target placement = %+v, want frozen FD/pool/datastore/network", progress.Name, progress)
		}
		if progress.TargetHost != "" {
			t.Fatalf("Node %q target host = %q, want host unpinned before relocation", progress.Name, progress.TargetHost)
		}
		shouldBeUnschedulable := progress.Name == "control-plane-b" || progress.Name == "worker-b"
		if shouldBeUnschedulable != (progress.OriginalSchedulingState == migrationv1alpha1.NodeMigrationSchedulingStateUnschedulable) {
			t.Fatalf("Node %q original scheduling state = %q, want unschedulable %t", progress.Name, progress.OriginalSchedulingState, shouldBeUnschedulable)
		}
	}
}

func assertVMotionNodeAdditionBlocked(t *testing.T, h *vmotionPlanHarness, executionID string) {
	t.Helper()
	newNode := newVMotionPlanNode("worker-c", "worker", h.providerIDs[5], "node-uid-6", false)
	if _, err := h.kubeClient.CoreV1().Nodes().Create(context.Background(), &newNode, metav1.CreateOptions{}); err != nil {
		t.Fatalf("adding Node after plan freeze: %v", err)
	}
	h.expectedInventoryCount++
	h.reconcile(t)
	condition := apimeta.FindStatusCondition(h.migration.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != migrationv1alpha1.ReasonFailed ||
		!strings.Contains(condition.Message, "frozen VMotion") {
		t.Fatalf("InfrastructurePrepared after Node drift = %+v, want frozen-plan block", condition)
	}
	if h.migration.Status.NodeMigration.ExecutionID != executionID || h.migration.Status.NodeMigration.Workers.Total != 2 {
		t.Fatalf("Node drift changed frozen plan execution or worker count: %+v", h.migration.Status.NodeMigration)
	}
}

func TestVMotionMachineEligibilityUsesPerNodeAssociations(t *testing.T) {
	providerID := "vsphere://11111111-1111-4111-8111-111111111111"
	node := newVMotionPlanNode("worker-a", "worker", providerID, "node-uid-a", false)
	newMachine := func() machinev1beta1.Machine {
		phase := machinev1beta1.PhaseFailed
		return machinev1beta1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: openshift.MachineAPINamespace, Name: "machine-a"}, Status: machinev1beta1.MachineStatus{Phase: &phase}}
	}
	tests := []struct {
		name     string
		node     corev1.Node
		machines []machinev1beta1.Machine
		wantErr  string
	}{
		{name: "unrelated inactive Machine does not block", node: node, machines: []machinev1beta1.Machine{newMachine()}},
		{name: "dangling Node annotation does not block", node: withMachineAnnotation(node, "missing-machine"), wantErr: ""},
		{name: "NodeRef UID marks Machine-managed", node: node, machines: []machinev1beta1.Machine{func() machinev1beta1.Machine {
			m := newMachine()
			m.Status.NodeRef = &corev1.ObjectReference{Name: node.Name, UID: node.UID}
			return m
		}()}, wantErr: "Machine-managed"},
		{name: "providerID marks Machine-managed", node: node, machines: []machinev1beta1.Machine{func() machinev1beta1.Machine { m := newMachine(); m.Spec.ProviderID = &providerID; return m }()}, wantErr: "Machine-managed"},
		{name: "live Node annotation marks Machine-managed", node: withMachineAnnotation(node, "machine-a"), machines: []machinev1beta1.Machine{newMachine()}, wantErr: "Machine-managed"},
		{name: "NodeRef name without UID is indeterminate", node: node, machines: []machinev1beta1.Machine{func() machinev1beta1.Machine {
			m := newMachine()
			m.Status.NodeRef = &corev1.ObjectReference{Name: node.Name}
			return m
		}()}, wantErr: "without a UID"},
		{name: "NodeRef name and UID conflict is indeterminate", node: node, machines: []machinev1beta1.Machine{func() machinev1beta1.Machine {
			m := newMachine()
			m.Status.NodeRef = &corev1.ObjectReference{Name: "other-node", UID: node.UID}
			return m
		}()}, wantErr: "conflicting name"},
		{name: "provisioning Machine blocks even when unrelated", node: node, machines: []machinev1beta1.Machine{func() machinev1beta1.Machine {
			m := newMachine()
			phase := machinev1beta1.PhaseProvisioning
			m.Status.Phase = &phase
			return m
		}()}, wantErr: "active provisioning phase"},
		{name: "provisioned Machine blocks", node: node, machines: []machinev1beta1.Machine{func() machinev1beta1.Machine {
			m := newMachine()
			phase := machinev1beta1.PhaseProvisioned
			m.Status.Phase = &phase
			return m
		}()}, wantErr: "active provisioning phase"},
		{name: "deleting Machine blocks", node: node, machines: []machinev1beta1.Machine{func() machinev1beta1.Machine {
			m := newMachine()
			phase := machinev1beta1.PhaseDeleting
			m.Status.Phase = &phase
			return m
		}()}, wantErr: "active provisioning phase"},
		{name: "deletion timestamp blocks", node: node, machines: []machinev1beta1.Machine{func() machinev1beta1.Machine {
			m := newMachine()
			deletionTimestamp := metav1.Now()
			m.DeletionTimestamp = &deletionTimestamp
			return m
		}()}, wantErr: "active provisioning phase"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, err := collectVMotionNodeIdentities([]corev1.Node{tt.node})
			if err != nil {
				t.Fatalf("collectVMotionNodeIdentities: %v", err)
			}
			err = validateVMotionMachineEligibility(input, tt.machines)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateVMotionMachineEligibility: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateVMotionMachineEligibility error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func withMachineAnnotation(node corev1.Node, value string) corev1.Node {
	node.Annotations = map[string]string{vmotionMachineAnnotation: value}
	return node
}

func TestVMotionSourceVCenterUsesFrozenPlanIdentity(t *testing.T) {
	migration := newVMotionPlanMigration()
	migration.Status.NodeMigration = &migrationv1alpha1.NodeMigrationStatus{
		FrozenPlan: &migrationv1alpha1.NodeMigrationFrozenPlan{
			SourceVCenter: cleanupTestSource, SourceDatacenter: vmotionPlanTestSourceDatacenter,
			DestinationVCenter: "target.example.com", Mode: migrationv1alpha1.VMotionModeHot,
		},
	}
	infrastructure := newInfrastructureForReadyTest([]string{cleanupTestSource, "target.example.com"})
	infrastructure.Spec.PlatformSpec.VSphere.VCenters[0].Datacenters = []string{vmotionPlanTestSourceDatacenter}
	infrastructure.Spec.PlatformSpec.VSphere.VCenters[1].Datacenters = []string{"TargetDC"}

	got, liveServer, err := vmotionSourceVCenter(migration, infrastructure)
	if err != nil {
		t.Fatalf("vmotionSourceVCenter: %v", err)
	}
	if got.Server != cleanupTestSource || len(got.Datacenters) != 1 || got.Datacenters[0] != vmotionPlanTestSourceDatacenter {
		t.Fatalf("source vCenter = %+v, want frozen source identity", got)
	}
	if liveServer != cleanupTestSource {
		t.Fatalf("live server = %q, want %q", liveServer, cleanupTestSource)
	}
	emptyServerInfrastructure := newInfrastructureForReadyTest([]string{""})
	emptyServerInfrastructure.Spec.PlatformSpec.VSphere.VCenters[0].Datacenters = []string{vmotionPlanTestSourceDatacenter}
	if _, _, err := vmotionSourceVCenter(newVMotionPlanMigration(), emptyServerInfrastructure); err == nil || !strings.Contains(err.Error(), "server must not be empty") {
		t.Fatalf("empty source server error = %v, want missing server error", err)
	}
}

func TestVMotionSourceVCenterMatchesEquivalentServers(t *testing.T) {
	tests := []struct {
		name         string
		frozenServer string
		statusServer string
		servers      []string
		wantErr      string
	}{
		{name: "live case changed", frozenServer: cleanupTestSource, servers: []string{"Source.Example.Com"}},
		{name: "live URL variant", statusServer: cleanupTestSource, servers: []string{"https://SOURCE.example.com/sdk"}},
		{name: "persisted case differs", frozenServer: cleanupTestSource, statusServer: "Source.Example.Com", servers: []string{cleanupTestSource}},
		{name: "persisted URL differs", frozenServer: "https://SOURCE.example.com/sdk", statusServer: cleanupTestSource, servers: []string{cleanupTestSource}},
		{name: "persisted identities disagree", frozenServer: cleanupTestSource, statusServer: "other.example.com", servers: []string{cleanupTestSource}, wantErr: "persisted source identities disagree"},
		{name: "different live server", frozenServer: cleanupTestSource, servers: []string{"other.example.com"}, wantErr: "no Infrastructure entry matches"},
		{name: "equivalent duplicate entries", frozenServer: cleanupTestSource, servers: []string{cleanupTestSource, "Source.Example.Com"}, wantErr: "multiple Infrastructure entries match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			migration := newVMotionPlanMigration()
			migration.Status.SourceVCenter = tt.statusServer
			if tt.frozenServer != "" {
				migration.Status.NodeMigration = &migrationv1alpha1.NodeMigrationStatus{
					FrozenPlan: &migrationv1alpha1.NodeMigrationFrozenPlan{
						SourceVCenter: tt.frozenServer, SourceDatacenter: vmotionPlanTestSourceDatacenter,
					},
				}
			}
			infrastructure := newInfrastructureForReadyTest(tt.servers)
			for i := range infrastructure.Spec.PlatformSpec.VSphere.VCenters {
				infrastructure.Spec.PlatformSpec.VSphere.VCenters[i].Datacenters = []string{vmotionPlanTestSourceDatacenter}
			}
			got, liveServer, err := vmotionSourceVCenter(migration, infrastructure)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("vmotionSourceVCenter error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("vmotionSourceVCenter: %v", err)
			}
			if got.Datacenters[0] != vmotionPlanTestSourceDatacenter {
				t.Fatalf("source vCenter = %+v, want matching source datacenter", got)
			}
			wantServer := tt.frozenServer
			if wantServer == "" {
				wantServer = tt.statusServer
			}
			if got.Server != wantServer {
				t.Fatalf("source server = %q, want persisted spelling %q", got.Server, wantServer)
			}
			if liveServer != tt.servers[0] {
				t.Fatalf("live server = %q, want Infrastructure spelling %q", liveServer, tt.servers[0])
			}
			if infrastructure.Spec.PlatformSpec.VSphere.VCenters[0].Server != tt.servers[0] {
				t.Fatal("resolving source server mutated Infrastructure")
			}
		})
	}
}

func TestVMotionReconcilePreservesPlanAfterSourceServerCaseChange(t *testing.T) {
	h := newVMotionPlanHarness(t)
	h.reconcileUntilFrozen(t)
	original := h.migration.Status.NodeMigration.DeepCopy()
	infrastructure, err := h.reconciler.ConfigClient.ConfigV1().Infrastructures().Get(context.Background(), openshift.InfrastructureName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting Infrastructure: %v", err)
	}
	infrastructure.Spec.PlatformSpec.VSphere.VCenters[0].Server = "Source.Example.Com"
	if _, err := h.reconciler.ConfigClient.ConfigV1().Infrastructures().Update(context.Background(), infrastructure, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating Infrastructure: %v", err)
	}
	h.reconcile(t)
	condition := apimeta.FindStatusCondition(h.migration.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
	if condition == nil || condition.Reason != migrationv1alpha1.ReasonProgressing {
		t.Fatalf("InfrastructurePrepared = %+v, want Progressing after equivalent source server edit", condition)
	}
	if h.migration.Status.NodeMigration.ExecutionID != original.ExecutionID || h.migration.Status.NodeMigration.PlanFingerprint != original.PlanFingerprint {
		t.Fatal("equivalent source server edit replaced the frozen plan")
	}
}

func TestValidateVMotionVCenterScope(t *testing.T) {
	tests := []struct {
		name    string
		servers []string
		nilSpec bool
		wantErr string
	}{
		{name: "no vSphere configuration", nilSpec: true, wantErr: "no vSphere configuration"},
		{name: "source only", servers: []string{cleanupTestSource}},
		{name: "source and destination", servers: []string{cleanupTestSource, "target.example.com"}},
		{name: "no entries", wantErr: "found 0 source and 0 destination entries"},
		{name: "missing source", servers: []string{"target.example.com"}, wantErr: "found 0 source and 1 destination entries"},
		{name: "duplicate source", servers: []string{cleanupTestSource, "SOURCE.example.com"}, wantErr: "found 2 source and 0 destination entries"},
		{name: "unexpected vCenter", servers: []string{cleanupTestSource, "target.example.com", "other.example.com"}, wantErr: `unexpected vCenter "other.example.com"`},
		{name: "duplicate destination", servers: []string{cleanupTestSource, "target.example.com", "TARGET.example.com"}, wantErr: "found 1 source and 2 destination entries"},
		{name: "equivalent source and destination", servers: []string{"https://SOURCE.example.com/sdk", "TARGET.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infrastructure := newInfrastructureForReadyTest(tt.servers)
			if tt.nilSpec {
				infrastructure.Spec.PlatformSpec.VSphere = nil
			}
			err := validateVMotionVCenterScope(infrastructure, cleanupTestSource, "target.example.com")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateVMotionVCenterScope: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateVMotionVCenterScope error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestVMotionSourceFailureDomains(t *testing.T) {
	sourceFD := configv1.VSpherePlatformFailureDomainSpec{
		Name: "source-a", Server: cleanupTestSource,
		Topology: configv1.VSpherePlatformTopology{Datacenter: vmotionPlanTestSourceDatacenter},
	}
	secondFD := sourceFD
	secondFD.Name, secondFD.Server = "source-b", "https://SOURCE.example.com/sdk"
	destinationFD := configv1.VSpherePlatformFailureDomainSpec{Name: "target-fd", Server: "TARGET.example.com"}
	wrongDatacenter := sourceFD
	wrongDatacenter.Topology.Datacenter = "OtherDC"
	unexpectedFD := configv1.VSpherePlatformFailureDomainSpec{Name: "unexpected-fd", Server: "other.example.com"}
	tests := []struct {
		name    string
		fds     []configv1.VSpherePlatformFailureDomainSpec
		want    []string
		wantErr string
	}{
		{name: "no source match", fds: []configv1.VSpherePlatformFailureDomainSpec{destinationFD}, wantErr: "no source failure domains"},
		{name: "one source", fds: []configv1.VSpherePlatformFailureDomainSpec{sourceFD}, want: []string{"source-a"}},
		{name: "multiple sources", fds: []configv1.VSpherePlatformFailureDomainSpec{sourceFD, secondFD}, want: []string{"source-a", "source-b"}},
		{name: "destination excluded", fds: []configv1.VSpherePlatformFailureDomainSpec{destinationFD, sourceFD}, want: []string{"source-a"}},
		{name: "unexpected vCenter", fds: []configv1.VSpherePlatformFailureDomainSpec{sourceFD, unexpectedFD}, wantErr: `failure domain "unexpected-fd" references unexpected vCenter`},
		{name: "datacenter mismatch", fds: []configv1.VSpherePlatformFailureDomainSpec{wrongDatacenter}, wantErr: `datacenter "OtherDC" differs from configured source datacenter`},
		{name: "empty failure domains", wantErr: "no source failure domains"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infrastructure := newInfrastructureForReadyTest([]string{cleanupTestSource})
			infrastructure.Spec.PlatformSpec.VSphere.FailureDomains = tt.fds
			sourceVC := &configv1.VSpherePlatformVCenterSpec{Server: cleanupTestSource, Datacenters: []string{vmotionPlanTestSourceDatacenter}}
			got, err := vmotionSourceFailureDomains(newVMotionPlanMigration(), infrastructure, sourceVC)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || got != nil {
					t.Fatalf("vmotionSourceFailureDomains = %+v, %v, want no partial result and error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || len(got) != len(tt.want) {
				t.Fatalf("vmotionSourceFailureDomains = %+v, %v, want %v", got, err, tt.want)
			}
			for i, name := range tt.want {
				if got[i].Name != name || got[i].Topology.Datacenter != vmotionPlanTestSourceDatacenter {
					t.Fatalf("source FD %d = %+v, want %q in source datacenter", i, got[i], name)
				}
			}
		})
	}
}

func TestVMotionMachineEligibilityBlocksConflictingBindings(t *testing.T) {
	nodeA := newVMotionPlanNode("worker-a", "worker", "vsphere://11111111-1111-4111-8111-111111111111", "node-uid-a", false)
	nodeB := newVMotionPlanNode("worker-b", "worker", "vsphere://22222222-2222-4222-8222-222222222222", "node-uid-b", false)
	identities := []vmotionNodeIdentity{
		{node: nodeA, role: vmotionWorkerRole, biosUUID: "11111111-1111-4111-8111-111111111111"},
		{node: nodeB, role: vmotionWorkerRole, biosUUID: "22222222-2222-4222-8222-222222222222"},
	}
	providerID := nodeB.Spec.ProviderID
	machine := machinev1beta1.Machine{
		ObjectMeta: metav1.ObjectMeta{Namespace: openshift.MachineAPINamespace, Name: "machine-a"},
		Spec:       machinev1beta1.MachineSpec{ProviderID: &providerID},
		Status:     machinev1beta1.MachineStatus{NodeRef: &corev1.ObjectReference{Name: nodeA.Name, UID: nodeA.UID}},
	}
	if err := validateVMotionMachineEligibility(identities, []machinev1beta1.Machine{machine}); err == nil || !strings.Contains(err.Error(), "multiple planned Nodes") {
		t.Fatalf("conflicting NodeRef/providerID error = %v, want multiple planned Nodes", err)
	}

	first := machine.DeepCopy()
	first.Spec.ProviderID = nil
	second := first.DeepCopy()
	second.Name = "machine-b"
	if err := validateVMotionMachineEligibility(identities, []machinev1beta1.Machine{*first, *second}); err == nil || !strings.Contains(err.Error(), "multiple Machines") {
		t.Fatalf("duplicate Machine binding error = %v, want multiple Machines", err)
	}
}

func TestVMotionInventoryUsesLiveServerSpelling(t *testing.T) {
	for _, variant := range []string{"case", "URL"} {
		t.Run(variant, func(t *testing.T) {
			helloNames := make(chan string, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			server.TLS = &tls.Config{
				GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					helloNames <- hello.ServerName
					return nil, nil
				},
			}
			server.StartTLS()
			defer server.Close()
			_, port, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatalf("SplitHostPort: %v", err)
			}
			liveServer := net.JoinHostPort("localhost", port)
			frozenServer := strings.ToUpper(liveServer)
			if variant == "URL" {
				frozenServer = "https://" + liveServer + "/sdk"
			}
			migration := newVMotionPlanMigration()
			migration.Status.NodeMigration = &migrationv1alpha1.NodeMigrationStatus{
				FrozenPlan: &migrationv1alpha1.NodeMigrationFrozenPlan{
					SourceVCenter: frozenServer, SourceDatacenter: vmotionPlanTestSourceDatacenter,
				},
			}
			infrastructure := newInfrastructureForReadyTest([]string{liveServer})
			infrastructure.Spec.PlatformSpec.VSphere.VCenters[0].Datacenters = []string{vmotionPlanTestSourceDatacenter}
			sourceVC, connectionServer, err := vmotionSourceVCenter(migration, infrastructure)
			if err != nil {
				t.Fatalf("vmotionSourceVCenter: %v", err)
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: openshift.VSphereCredsSecretName, Namespace: openshift.VSphereCredsSecretNamespace},
				Data: map[string][]byte{
					liveServer + ".username": []byte("user"),
					liveServer + ".password": []byte("password"),
				},
			}
			r := &VmwareCloudFoundationMigrationReconciler{KubeClient: fakekube.NewClientset(secret)}
			_, err = r.listVMotionVMInventory(context.Background(), sourceVC, connectionServer, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "tls: failed to verify certificate") {
				t.Fatalf("listVMotionVMInventory error = %v, want live credentials accepted followed by untrusted TLS rejection", err)
			}
			select {
			case name := <-helloNames:
				if name != "localhost" {
					t.Fatalf("TLS server name = %q, want live server spelling localhost", name)
				}
			default:
				t.Fatal("inventory did not reach the live TLS endpoint")
			}
			if sourceVC.Server != frozenServer {
				t.Fatalf("source identity = %q, want frozen spelling %q", sourceVC.Server, frozenServer)
			}
		})
	}
}

func TestVMotionInventoryRejectsUntrustedSourceCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	serverName := strings.TrimPrefix(server.URL, "https://")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: openshift.VSphereCredsSecretName, Namespace: openshift.VSphereCredsSecretNamespace},
		Data: map[string][]byte{
			serverName + ".username": []byte("user"),
			serverName + ".password": []byte("password"),
		},
	}
	r := &VmwareCloudFoundationMigrationReconciler{KubeClient: fakekube.NewClientset(secret)}
	_, err := r.listVMotionVMInventory(
		context.Background(),
		&configv1.VSpherePlatformVCenterSpec{Server: serverName, Datacenters: []string{vmotionPlanTestSourceDatacenter}},
		serverName,
		nil,
		[]string{"11111111-1111-4111-8111-111111111111"},
	)
	if err == nil || !strings.Contains(err.Error(), "certificate signed by unknown authority") {
		t.Fatalf("listVMotionVMInventory error = %v, want untrusted TLS certificate rejection", err)
	}
}

func TestListVMotionVMInventoryUsesBoundedContext(t *testing.T) {
	called := false
	r := &VmwareCloudFoundationMigrationReconciler{
		vmotionInventoryFunc: func(ctx context.Context, _ *configv1.VSpherePlatformVCenterSpec, _ string, _ []configv1.VSpherePlatformFailureDomainSpec, _ []string) (vsphere.VMotionInventory, error) {
			called = true
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("inventory context has no deadline")
			}
			if remaining := time.Until(deadline); remaining <= 0 || remaining > preflightVSphereTimeout {
				t.Fatalf("inventory deadline is %s from now, want a positive duration no longer than %s", remaining, preflightVSphereTimeout)
			}
			return vsphere.VMotionInventory{}, nil
		},
	}

	_, err := r.listVMotionVMInventory(
		context.Background(),
		&configv1.VSpherePlatformVCenterSpec{Server: cleanupTestSource, Datacenters: []string{vmotionPlanTestSourceDatacenter}},
		cleanupTestSource,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("listVMotionVMInventory: %v", err)
	}
	if !called {
		t.Fatal("inventory function was not called")
	}
}

func TestVMotionResumeClearsPausedReadyCondition(t *testing.T) {
	migration := newVMotionPlanMigration()
	apimeta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
		Type: migrationv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
		Reason: migrationv1alpha1.ReasonPaused, Message: "paused", ObservedGeneration: migration.Generation,
	})
	r, c := reconcileErrReconciler(t, migration)

	if _, err := r.reconcileWorkflow(context.Background(), migration, *migration.Status.DeepCopy()); err != nil {
		t.Fatalf("reconcileWorkflow: %v", err)
	}

	got := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(migration), got); err != nil {
		t.Fatalf("getting migration: %v", err)
	}
	ready := apimeta.FindStatusCondition(got.Status.Conditions, migrationv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != migrationv1alpha1.ReasonProgressing || ready.Message != "Migration is running" {
		t.Fatalf("Ready = %+v, want Progressing after VMotion resume", ready)
	}
}

func TestVMotionMappingResolutionFailsClosed(t *testing.T) {
	sourceFDs := []configv1.VSpherePlatformFailureDomainSpec{
		{Name: "source-a"},
		{Name: "source-b"},
	}
	destinationFDs := []configv1.VSpherePlatformFailureDomainSpec{{Name: "target-a"}, {Name: "target-b"}, {Name: "target-shared"}}
	tests := []struct {
		name     string
		mappings []migrationv1alpha1.FailureDomainMapping
		wantErr  string
	}{
		{name: "effective source mapping", mappings: []migrationv1alpha1.FailureDomainMapping{{Source: "source-a", Destination: "target-a"}}},
		{name: "unknown source", mappings: []migrationv1alpha1.FailureDomainMapping{{Source: "missing", Destination: "target-a"}}, wantErr: "source \"missing\" is not configured"},
		{name: "unknown destination", mappings: []migrationv1alpha1.FailureDomainMapping{{Source: "source-a", Destination: "missing"}}, wantErr: "destination \"missing\" is not configured"},
		{name: "duplicate source mapping", mappings: []migrationv1alpha1.FailureDomainMapping{{Source: "source-a", Destination: "target-a"}, {Source: "source-a", Destination: "target-b"}}, wantErr: "source \"source-a\" has multiple destinations"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVMotionMappings(tt.mappings, sourceFDs, destinationFDs)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateVMotionMappings error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateVMotionMappings: %v", err)
			}
		})
	}

	if _, err := mappedVMotionDestination("source-a", nil, nil); err == nil || !strings.Contains(err.Error(), "no destination") {
		t.Fatalf("missing destination mapping error = %v, want no destination error", err)
	}
	duplicate := []migrationv1alpha1.FailureDomainMapping{
		{Source: "source-a", Destination: "target-a"}, {Source: "source-a", Destination: "target-b"},
	}
	if _, err := mappedVMotionDestination("source-a", duplicate, nil); err == nil || !strings.Contains(err.Error(), "multiple destinations") {
		t.Fatalf("ambiguous destination mapping error = %v, want multiple destinations error", err)
	}

	sharedTarget := []migrationv1alpha1.FailureDomainMapping{
		{Source: "source-a", Destination: "target-shared"},
		{Source: "source-b", Destination: "target-shared"},
	}
	if err := validateVMotionMappings(sharedTarget, sourceFDs, destinationFDs); err != nil {
		t.Fatalf("many-to-one effective mapping should remain valid: %v", err)
	}
}

func TestValidateVMotionSourceFailureDomainReferences(t *testing.T) {
	const (
		vcenter    = cleanupTestSource
		datacenter = "SourceDC"
	)
	fds := []configv1.VSpherePlatformFailureDomainSpec{{Name: "source-a"}, {Name: "source-b"}}
	refA := vsphere.VMotionManagedObjectReference{VCenter: vcenter, Datacenter: datacenter, Type: "ComputeResource", Value: "domain-1"}
	refB := vsphere.VMotionManagedObjectReference{VCenter: vcenter, Datacenter: datacenter, Type: "ComputeResource", Value: "domain-2"}
	tests := []struct {
		name    string
		refs    map[string]vsphere.VMotionManagedObjectReference
		wantErr string
	}{
		{name: "unique references", refs: map[string]vsphere.VMotionManagedObjectReference{"source-a": refA, "source-b": refB}},
		{name: "missing reference", refs: map[string]vsphere.VMotionManagedObjectReference{"source-a": refA}, wantErr: "no resolved reference"},
		{name: "duplicate reference", refs: map[string]vsphere.VMotionManagedObjectReference{"source-a": refA, "source-b": refA}, wantErr: "share compute-resource reference"},
		{name: "different vCenter", refs: map[string]vsphere.VMotionManagedObjectReference{"source-a": refA, "source-b": {VCenter: "other.example.com", Datacenter: datacenter, Type: "ComputeResource", Value: "domain-2"}}, wantErr: "different vCenter"},
		{name: "different datacenter", refs: map[string]vsphere.VMotionManagedObjectReference{"source-a": refA, "source-b": {VCenter: vcenter, Datacenter: "OtherDC", Type: "ComputeResource", Value: "domain-2"}}, wantErr: "outside the source datacenter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateVMotionSourceFailureDomainReferences(fds, tt.refs, vcenter, datacenter)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateVMotionSourceFailureDomainReferences error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateVMotionSourceFailureDomainReferences: %v", err)
			}
		})
	}
}

func TestBuildVMotionPlanRejectsMissingOrAmbiguousVM(t *testing.T) {
	const biosUUID = "11111111-1111-4111-8111-111111111111"
	migration := newVMotionPlanMigration()
	migration.UID = types.UID("migration-uid")
	sourceVC := &configv1.VSpherePlatformVCenterSpec{Server: cleanupTestSource, Datacenters: []string{vmotionPlanTestSourceDatacenter}}
	sourceFD := configv1.VSpherePlatformFailureDomainSpec{
		Name: "source-fd", Server: sourceVC.Server, Zone: "source-zone",
		Topology: configv1.VSpherePlatformTopology{Datacenter: vmotionPlanTestSourceDatacenter, ComputeCluster: "/SourceDC/host/SourceCluster"},
	}
	identity := vmotionNodeIdentity{
		node: newVMotionPlanNode("worker-a", "worker", "vsphere://"+biosUUID, "node-uid", false),
		role: vmotionWorkerRole, biosUUID: biosUUID,
	}
	mapping := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd", Destination: "target-fd"}}
	owner := vsphere.VMotionManagedObjectReference{VCenter: sourceVC.Server, Datacenter: sourceVC.Datacenters[0], Type: "ComputeResource", Value: "domain-11"}
	pool := vsphere.VMotionManagedObjectReference{VCenter: sourceVC.Server, Datacenter: sourceVC.Datacenters[0], Type: "ResourcePool", Value: "resgroup-1"}
	vm := vsphere.VMotionVM{BIOSUUID: biosUUID, InventoryPath: "/SourceDC/vm/worker-a", ResourcePoolRef: pool, ResourcePoolOwner: owner, NetworkAdapterCount: 1}
	build := func(vms []vsphere.VMotionVM) (*migrationv1alpha1.NodeMigrationStatus, error) {
		inventory := vsphere.VMotionInventory{VMs: vms, SourceFailureDomainReferences: map[string]vsphere.VMotionManagedObjectReference{"source-fd": owner}}
		return buildVMotionPlan(migration, sourceVC, "target.example.com", []configv1.VSpherePlatformFailureDomainSpec{sourceFD}, mapping, migration.Spec.FailureDomains, []vmotionNodeIdentity{identity}, inventory)
	}
	if _, err := build(nil); err == nil || !strings.Contains(err.Error(), "no source vSphere VM") {
		t.Fatalf("missing VM error = %v, want no source VM error", err)
	}
	duplicate := []vsphere.VMotionVM{vm, {BIOSUUID: biosUUID, InventoryPath: "/SourceDC/vm/duplicate", ResourcePoolRef: pool, ResourcePoolOwner: owner, NetworkAdapterCount: 1}}
	if _, err := build(duplicate); err == nil || !strings.Contains(err.Error(), "matches 2 source vSphere VMs") {
		t.Fatalf("duplicate VM error = %v, want ambiguous UUID error", err)
	}
	withoutNIC := vm
	withoutNIC.NetworkAdapterCount = 0
	if _, err := build([]vsphere.VMotionVM{withoutNIC}); err == nil || !strings.Contains(err.Error(), "exactly one network adapter") {
		t.Fatalf("missing NIC error = %v, want single-adapter eligibility error", err)
	}
	withMultipleNICs := vm
	withMultipleNICs.NetworkAdapterCount = 2
	if _, err := build([]vsphere.VMotionVM{withMultipleNICs}); err == nil || !strings.Contains(err.Error(), "exactly one network adapter") {
		t.Fatalf("multiple NIC error = %v, want single-adapter eligibility error", err)
	}
	baseline, err := build([]vsphere.VMotionVM{vm})
	if err != nil {
		t.Fatalf("building baseline plan: %v", err)
	}
	migration.Spec.FailureDomains[0].Topology.ComputeCluster = "/TargetDC/host/ChangedCluster"
	changedPlacement, err := build([]vsphere.VMotionVM{vm})
	if err != nil {
		t.Fatalf("building plan after destination compute-cluster change: %v", err)
	}
	if baseline.PlanFingerprint == changedPlacement.PlanFingerprint {
		t.Fatal("destination compute-cluster change did not change frozen plan fingerprint")
	}
	migration.Spec.FailureDomains[0].Topology.ComputeCluster = "/TargetDC/host/TargetCluster"
	withoutResourcePool := vm
	withoutResourcePool.ResourcePoolRef = vsphere.VMotionManagedObjectReference{}
	if _, err := build([]vsphere.VMotionVM{withoutResourcePool}); err == nil || !strings.Contains(err.Error(), "resource pool reference is missing") {
		t.Fatalf("missing resource pool reference error = %v, want fail-closed reference error", err)
	}
	if _, err := buildVMotionPlan(migration, sourceVC, "target.example.com", []configv1.VSpherePlatformFailureDomainSpec{sourceFD}, nil, migration.Spec.FailureDomains, []vmotionNodeIdentity{identity}, vsphere.VMotionInventory{VMs: []vsphere.VMotionVM{vm}, SourceFailureDomainReferences: map[string]vsphere.VMotionManagedObjectReference{"source-fd": owner}}); err == nil || !strings.Contains(err.Error(), "effective status.failureDomainMappings is empty") {
		t.Fatalf("missing effective mapping error = %v, want missing status mapping error", err)
	}
	ambiguousMapping := append(append([]migrationv1alpha1.FailureDomainMapping(nil), mapping...), migrationv1alpha1.FailureDomainMapping{Source: "source-fd", Destination: "other-target"})
	if _, err := buildVMotionPlan(migration, sourceVC, "target.example.com", []configv1.VSpherePlatformFailureDomainSpec{sourceFD}, ambiguousMapping, migration.Spec.FailureDomains, []vmotionNodeIdentity{identity}, vsphere.VMotionInventory{VMs: []vsphere.VMotionVM{vm}, SourceFailureDomainReferences: map[string]vsphere.VMotionManagedObjectReference{"source-fd": owner}}); err == nil || !strings.Contains(err.Error(), "multiple destinations") {
		t.Fatalf("ambiguous effective mapping error = %v, want multiple destination error", err)
	}

	invalidNetworkMigration := migration.DeepCopy()
	invalidNetworkMigration.Spec.FailureDomains[0].Topology.Networks = []string{"network-a", "network-b"}
	if _, err := buildVMotionPlan(invalidNetworkMigration, sourceVC, "target.example.com", []configv1.VSpherePlatformFailureDomainSpec{sourceFD}, mapping, invalidNetworkMigration.Spec.FailureDomains, []vmotionNodeIdentity{identity}, vsphere.VMotionInventory{VMs: []vsphere.VMotionVM{vm}, SourceFailureDomainReferences: map[string]vsphere.VMotionManagedObjectReference{"source-fd": owner}}); err == nil || !strings.Contains(err.Error(), "exactly one topology.networks entry") {
		t.Fatalf("ambiguous target network error = %v, want single-network placement requirement", err)
	}
	withoutPool := migration.DeepCopy()
	withoutPool.Spec.FailureDomains[0].Topology.ResourcePool = ""
	status, err := buildVMotionPlan(withoutPool, sourceVC, "target.example.com", []configv1.VSpherePlatformFailureDomainSpec{sourceFD}, mapping, withoutPool.Spec.FailureDomains, []vmotionNodeIdentity{identity}, vsphere.VMotionInventory{VMs: []vsphere.VMotionVM{vm}, SourceFailureDomainReferences: map[string]vsphere.VMotionManagedObjectReference{"source-fd": owner}})
	if err != nil {
		t.Fatalf("buildVMotionPlan without a destination resource pool: %v", err)
	}
	if got := status.Workers.Nodes[0].TargetResourcePool; got != "/TargetDC/host/TargetCluster/Resources" {
		t.Fatalf("derived target resource pool = %q, want the compute cluster root pool", got)
	}
}

func newVMotionPlanNode(name, role, providerID, uid string, unschedulable bool) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, UID: types.UID(uid), Labels: map[string]string{"node-role.kubernetes.io/" + role: ""},
		},
		Spec: corev1.NodeSpec{ProviderID: providerID, Unschedulable: unschedulable},
	}
}

func newVMotionPlanMigration() *migrationv1alpha1.VmwareCloudFoundationMigration {
	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name: migrationv1alpha1.SingletonName, Namespace: migrationv1alpha1.SingletonNamespace,
			UID: "migration-uid", Finalizers: []string{migrationv1alpha1.FinalizerName},
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			State: migrationv1alpha1.MigrationStateRunning,
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
				Name: "target-fd", Region: "target-region", Zone: "target-zone", Server: "target.example.com",
				Topology: configv1.VSpherePlatformTopology{
					Datacenter: "TargetDC", ComputeCluster: "/TargetDC/host/TargetCluster",
					Datastore: "/TargetDC/datastore/TargetDS", ResourcePool: "/TargetDC/host/TargetCluster/Resources",
					Networks: []string{"/TargetDC/network/VM Network"},
				},
			}},
			NodeMigration: &migrationv1alpha1.NodeMigrationSpec{
				Workers: migrationv1alpha1.RoleMigrationSpec{
					Type:    migrationv1alpha1.NodeMigrationTypeVMotion,
					VMotion: &migrationv1alpha1.VMotionSpec{Mode: migrationv1alpha1.VMotionModeHot},
				},
				ControlPlane: migrationv1alpha1.RoleMigrationSpec{
					Type:    migrationv1alpha1.NodeMigrationTypeVMotion,
					VMotion: &migrationv1alpha1.VMotionSpec{Mode: migrationv1alpha1.VMotionModeHot},
				},
			},
		},
	}
}
