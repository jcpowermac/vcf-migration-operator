package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	fakemachineclient "github.com/openshift/client-go/machine/clientset/versioned/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

// newOutOfTopologySourceMachineSetForRollout builds a source-vCenter
// MachineSet placed in a datacenter covered by no source failure domain,
// mirroring newSourceMachineSetForRolloutNamed.
func newOutOfTopologySourceMachineSetForRollout(name string, replicas int32) *machinev1beta1.MachineSet {
	ms := newTargetMachineSetForRollout(name, "source.example.com", replicas)
	ms.Spec.Template.Spec.ProviderSpec.Value = &runtime.RawExtension{Raw: []byte(
		`{"kind":"VSphereMachineProviderSpec","apiVersion":"machine.openshift.io/v1beta1","workspace":{"server":"source.example.com","datacenter":"dc-other","resourcePool":"rp-other"}}`)}
	return ms
}

func TestValidateSourceMachineSetCoverage(t *testing.T) {
	const infraID = "test-infra"
	base := workerMachineSetName(infraID, "target-fd-1")
	mappings := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}}
	sourceFDs := []configv1.VSpherePlatformFailureDomainSpec{newSourceFailureDomainForRollout()}

	// case 7 reuses the out-of-topology MachineSet with nil replicas.
	nilReplica := newOutOfTopologySourceMachineSetForRollout("source-worker-nil", 1)
	nilReplica.Spec.Replicas = nil
	// case 6 reuses a mapped MachineSet whose provider spec cannot be parsed.
	unparseable := newSourceMachineSetForRollout(1)
	unparseable.Spec.Template.Spec.ProviderSpec.Value = nil

	tests := []struct {
		name      string
		ms        *machinev1beta1.MachineSet
		rec       []string
		sourceFDs []configv1.VSpherePlatformFailureDomainSpec
		wantErr   string
	}{
		{
			name:    "mapped >0 with recorded replacement passes",
			ms:      newSourceMachineSetForRollout(1),
			rec:     []string{base, base + "-2"},
			wantErr: "",
		},
		{
			name:    "mapped >0 without recorded replacement errors",
			ms:      newSourceMachineSetForRollout(1),
			rec:     nil,
			wantErr: "no recorded destination",
		},
		{
			name:    "unmapped >0 stops migration",
			ms:      newOutOfTopologySourceMachineSetForRollout("source-worker-b", 1),
			rec:     []string{base, base + "-2"},
			wantErr: "does not match any source failure domain",
		},
		{
			name:    "unmapped zero-replica skipped",
			ms:      newOutOfTopologySourceMachineSetForRollout("source-worker-b", 0),
			rec:     []string{base, base + "-2"},
			wantErr: "",
		},
		{
			name:    "mapped zero-replica skipped",
			ms:      newSourceMachineSetForRollout(0),
			rec:     nil,
			wantErr: "",
		},
		{
			name:    "unparseable provider spec errors",
			ms:      unparseable,
			rec:     []string{base, base + "-2"},
			wantErr: "extracting provider spec",
		},
		{
			name:    "nil replicas treated as >0 and stops migration",
			ms:      nilReplica,
			rec:     []string{base, base + "-2"},
			wantErr: "does not match any source failure domain",
		},
		{
			name: "matching two failure domains errors",
			ms:   newSourceMachineSetForRollout(1),
			rec:  []string{base, base + "-2"},
			sourceFDs: []configv1.VSpherePlatformFailureDomainSpec{
				newSourceFailureDomainForRollout(),
				{Name: "source-fd-2", Server: "source.example.com", Topology: configv1.VSpherePlatformTopology{Datacenter: sourceDatacenter, ResourcePool: ""}},
			},
			wantErr: "matches multiple source failure domains",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fds := tt.sourceFDs
			if fds == nil {
				fds = sourceFDs
			}
			err := validateSourceMachineSetCoverage(context.Background(), []*machinev1beta1.MachineSet{tt.ms}, infraID, mappings, fds, tt.rec)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateSourceMachineSetCoverage: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateSourceMachineSetCoverage: nil error, want containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateSourceMachineSetCoverage: %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestRecoverUnrecordedDestinationNames covers the restart path where
// destination MachineSets were created but a crash before updateStatus lost
// the WorkerMachineSetNames recording: derived names must be re-derived from
// the still-present source MachineSets and verified against the destination
// vCenter before being recorded again.
func TestRecoverUnrecordedDestinationNames(t *testing.T) {
	const infraID = "test-infra"
	base := workerMachineSetName(infraID, "target-fd-1")
	sourceFD := newSourceFailureDomainForRollout()
	destFD := configv1.VSpherePlatformFailureDomainSpec{Name: "target-fd-1", Server: "target.example.com"}
	mappings := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}}

	// Listed out of order: recovery must sort by name so suffixed indices
	// line up with the creation loop's deterministic order.
	msList := []*machinev1beta1.MachineSet{
		newSourceMachineSetForRolloutNamed("source-worker-b", 1),
		newSourceMachineSetForRolloutNamed("source-worker-a", 1),
	}

	newMigration := func(recorded []string) *migrationv1alpha1.VmwareCloudFoundationMigration {
		return &migrationv1alpha1.VmwareCloudFoundationMigration{
			Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
				FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{destFD},
			},
			Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
				WorkerMachineSetNames: recorded,
			},
		}
	}

	tests := []struct {
		name         string
		objects      []runtime.Object
		msList       []*machinev1beta1.MachineSet
		recorded     []string
		sourceFDs    []configv1.VSpherePlatformFailureDomainSpec
		wantErr      string
		wantRecorded []string
	}{
		{
			name: "both destination MachineSets recorded when creation completed",
			objects: []runtime.Object{
				newDestinationMachineSetForSource(base, "source-worker-a", 1),
				newDestinationMachineSetForSource(base+"-2", "source-worker-b", 1),
			},
			msList:       msList,
			recorded:     nil,
			wantErr:      "",
			wantRecorded: []string{base, base + "-2"},
		},
		{
			name: "already recorded name not duplicated",
			objects: []runtime.Object{
				newDestinationMachineSetForSource(base, "source-worker-a", 1),
				newDestinationMachineSetForSource(base+"-2", "source-worker-b", 1),
			},
			msList:       msList,
			recorded:     []string{base},
			wantErr:      "",
			wantRecorded: []string{base, base + "-2"},
		},
		{
			name: "missing destination MachineSet fails closed",
			objects: []runtime.Object{
				newDestinationMachineSetForSource(base, "source-worker-a", 1),
			},
			msList:       msList,
			recorded:     nil,
			wantErr:      "does not exist",
			wantRecorded: nil,
		},
		{
			name: "destination MachineSet on wrong vCenter is a collision",
			objects: []runtime.Object{
				newDestinationMachineSetForSource(base, "source-worker-a", 1),
				newTargetMachineSetForRollout(base+"-2", "other.example.com", 1),
			},
			msList:       msList,
			recorded:     nil,
			wantErr:      "collides",
			wantRecorded: nil,
		},
		{
			name:         "source failure domain absent from infrastructure skips mapping",
			objects:      []runtime.Object{newTargetMachineSetForRollout(base, "target.example.com", 1)},
			msList:       msList,
			recorded:     nil,
			sourceFDs:    []configv1.VSpherePlatformFailureDomainSpec{{Name: "source-fd-gone", Server: "source.example.com"}},
			wantErr:      "",
			wantRecorded: nil,
		},
		{
			name:         "source mapping with no matching MachineSets stays empty",
			objects:      []runtime.Object{newTargetMachineSetForRollout(base, "target.example.com", 1)},
			msList:       []*machinev1beta1.MachineSet{newOutOfTopologySourceMachineSetForRollout("source-worker-other", 1)},
			recorded:     nil,
			wantErr:      "",
			wantRecorded: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fds := tt.sourceFDs
			if fds == nil {
				fds = []configv1.VSpherePlatformFailureDomainSpec{sourceFD}
			}
			r := newRolloutReconciler(t, tt.objects)
			machineMgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, r.DynamicClient)
			migration := newMigration(tt.recorded)
			_, err := recoverUnrecordedDestinationNames(context.Background(), machineMgr, migration, tt.msList, infraID, mappings, fds)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("recoverUnrecordedDestinationNames: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("recoverUnrecordedDestinationNames: nil error, want containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("recoverUnrecordedDestinationNames: %v, want containing %q", err, tt.wantErr)
				}
			}
			if !slices.Equal(migration.Status.WorkerMachineSetNames, tt.wantRecorded) {
				t.Fatalf("WorkerMachineSetNames = %v, want %v", migration.Status.WorkerMachineSetNames, tt.wantRecorded)
			}
		})
	}
}

func TestGeneratedDestinationNameCollisions(t *testing.T) {
	const target = "target"
	for _, names := range [][]string{{target, "target-2"}, {"target-2", target}, {"Target", target}, {"target-a", "target-b"}} {
		t.Run(names[0]+"_"+names[1], func(t *testing.T) {
			r := newRolloutReconciler(t, nil)
			source := []configv1.VSpherePlatformFailureDomainSpec{{Name: "source-a", Server: cleanupTestSource}, {Name: "source-b", Server: cleanupTestSource}}
			dest := []configv1.VSpherePlatformFailureDomainSpec{{Name: names[0]}, {Name: names[1]}}
			mappings := []migrationv1alpha1.FailureDomainMapping{{Source: "source-a", Destination: names[0]}, {Source: "source-b", Destination: names[1]}}
			err := r.validateMachineSetNameCollisions(context.Background(), "infra", mappings, source, dest)
			wantError := names[0] != "target-a"
			if (err != nil) != wantError {
				t.Fatalf("validateMachineSetNameCollisions = %v, want error %v", err, wantError)
			}
		})
	}
}

func TestWorkerReadinessDesiredReplicas(t *testing.T) {
	for _, replicas := range []int32{0, 1, 2} {
		ms := newTargetMachineSetForRollout(workerMachineSetName("infra", "target"), "target.example.com", replicas)
		machine := newReadyWorkerMachineForRollout(ms.Name)
		r := newRolloutReconciler(t, []runtime.Object{ms, machine, newReadyNodeForRollout(machine.Status.NodeRef.Name)})
		mgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, r.DynamicClient)
		got, err := checkWorkerReadiness(context.Background(), mgr, []configv1.VSpherePlatformFailureDomainSpec{{Name: "target"}}, "infra", nil)
		if err != nil {
			t.Fatalf("checkWorkerReadiness: %v", err)
		}
		if got.Ready != (replicas <= 1) {
			t.Fatalf("replicas %d: Ready = %v", replicas, got.Ready)
		}
	}
}

func TestRecordedWorkerMachineSetsRequiredAfterSourceCleanup(t *testing.T) {
	source := newSourceFailureDomainForRollout()
	dest := configv1.VSpherePlatformFailureDomainSpec{Name: "target-fd-1", Server: "target.example.com"}
	destFDs := []configv1.VSpherePlatformFailureDomainSpec{dest}
	mappings := []migrationv1alpha1.FailureDomainMapping{{Source: source.Name, Destination: dest.Name}}
	base := workerMachineSetName("test-infra", dest.Name)
	recorded := []string{base, base + "-2"}

	tests := []struct {
		name         string
		objects      []runtime.Object
		recorded     []string
		wantExist    bool
		wantWorkerFD bool
	}{
		{
			name:         "recorded names still required when source cleaned up and targets deleted",
			objects:      []runtime.Object{newInfrastructureForRollout()},
			recorded:     recorded,
			wantExist:    false,
			wantWorkerFD: true,
		},
		{
			name: "recorded names satisfied when source cleaned up and targets exist",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newTargetMachineSetForRollout(base, "target.example.com", 1),
				newTargetMachineSetForRollout(base+"-2", "target.example.com", 1),
			},
			recorded:     recorded,
			wantExist:    true,
			wantWorkerFD: true,
		},
		{
			name:         "originally empty mapping with nothing recorded stays empty",
			objects:      []runtime.Object{newInfrastructureForRollout()},
			recorded:     nil,
			wantExist:    true,
			wantWorkerFD: false,
		},
		{
			name: "missing recorded copy still fails existence check",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newTargetMachineSetForRollout(base, "target.example.com", 1),
			},
			recorded:     recorded,
			wantExist:    false,
			wantWorkerFD: true,
		},
		{
			// Regression: when source MachineSets still exist, previously
			// recorded names must keep being required even if the current
			// source list would derive fewer names.
			name: "recorded names preserved when fewer source MachineSets remain",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newSourceMachineSetForRollout(1),
				newTargetMachineSetForRollout(base, "target.example.com", 1),
			},
			recorded:     recorded,
			wantExist:    false,
			wantWorkerFD: true,
		},
		{
			name: "merged recorded and derived names satisfied",
			objects: []runtime.Object{
				newInfrastructureForRollout(),
				newSourceMachineSetForRollout(1),
				newTargetMachineSetForRollout(base, "target.example.com", 1),
				newTargetMachineSetForRollout(base+"-2", "target.example.com", 1),
			},
			recorded:     recorded,
			wantExist:    true,
			wantWorkerFD: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRolloutReconciler(t, tt.objects)
			mgr := openshift.NewMachineManager(r.KubeClient, r.MachineClient, r.DynamicClient)
			migration := newMappingTestMigration(destFDs, mappings)
			migration.Status.WorkerMachineSetNames = tt.recorded
			exist, err := r.targetMachineSetsExist(context.Background(), migration, mgr, "test-infra", mappings, []configv1.VSpherePlatformFailureDomainSpec{source})
			if err != nil {
				t.Fatalf("targetMachineSetsExist: %v", err)
			}
			if exist != tt.wantExist {
				t.Fatalf("targetMachineSetsExist = %v, want %v", exist, tt.wantExist)
			}
			workerFDs, err := workerReadinessFailureDomains(context.Background(), mgr, "test-infra", mappings, []configv1.VSpherePlatformFailureDomainSpec{source}, destFDs, tt.recorded)
			if err != nil {
				t.Fatalf("workerReadinessFailureDomains: %v", err)
			}
			if (len(workerFDs) > 0) != tt.wantWorkerFD {
				t.Fatalf("workerReadinessFailureDomains = %v, want contains %v", workerFDs, tt.wantWorkerFD)
			}
		})
	}
}

const (
	// originatingMigrationUID is the UID the reconcile under test starts from.
	originatingMigrationUID types.UID = "uid-originating"
	// replacementMigrationUID is the UID of the object an administrator creates
	// under the same name and namespace after force-removing the old one.
	replacementMigrationUID types.UID = "uid-replacement"
)

// statusWriteMigrationKey is the singleton key the replacement object reuses.
var statusWriteMigrationKey = types.NamespacedName{
	Name:      migrationv1alpha1.SingletonName,
	Namespace: migrationv1alpha1.SingletonNamespace,
}

// newStatusWriteMigration builds a migration parked at the workload phase: every
// earlier condition is True and the destination MachineSet does not exist yet,
// so a reconcile creates it, records its name, and publishes status.
func newStatusWriteMigration(uid types.UID) *migrationv1alpha1.VmwareCloudFoundationMigration {
	start := metav1.Now()
	migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       statusWriteMigrationKey.Name,
			Namespace:  statusWriteMigrationKey.Namespace,
			Generation: 1,
			UID:        uid,
			Finalizers: []string{migrationv1alpha1.FinalizerName},
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			State: migrationv1alpha1.MigrationStateRunning,
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
				Name:   "target-fd-1",
				Server: "target.example.com",
				Topology: configv1.VSpherePlatformTopology{
					Template:       "/dc1/vm/target-template",
					Datacenter:     "dc1",
					Datastore:      "ds1",
					ResourcePool:   "rp1",
					ComputeCluster: "cl1",
				},
			}},
		},
		Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
			StartTime: &start,
			FailureDomainMappings: []migrationv1alpha1.FailureDomainMapping{
				{Source: "source-fd-1", Destination: "target-fd-1"},
			},
		},
	}
	for _, condType := range []string{
		migrationv1alpha1.ConditionInfrastructurePrepared,
		migrationv1alpha1.ConditionDestinationInitialized,
		migrationv1alpha1.ConditionMultiSiteConfigured,
	} {
		apimeta.SetStatusCondition(&migration.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionTrue,
			Reason:             migrationv1alpha1.ReasonCompleted,
			Message:            completedMessage,
			ObservedGeneration: 1,
		})
	}
	return migration
}

// statusWriteHarness wires the clients a real Reconcile needs to reach the
// workload phase's status write. fake is the raw migration client so a test can
// replace the object out from under the reconcile.
type statusWriteHarness struct {
	reconciler *VmwareCloudFoundationMigrationReconciler
	fake       client.WithWatch
}

func newStatusWriteHarness(t *testing.T) *statusWriteHarness {
	t.Helper()
	reconciler := newRolloutReconciler(t, []runtime.Object{
		newInfrastructureForRollout(),
		newSourceMachineSetForRollout(2),
	})
	scheme := newMigrationTestScheme(t)
	fakeClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(newStatusWriteMigration(originatingMigrationUID)).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		Build()
	reconciler.Client = fakeClient
	reconciler.Scheme = scheme
	return &statusWriteHarness{reconciler: reconciler, fake: fakeClient}
}

// replaceStatusWriteMigration force-removes the reconciled object and recreates
// the same name and namespace with a different UID, as an administrator
// bypassing the retention finalizer and reapplying the CR does.
func replaceStatusWriteMigration(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	current := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := c.Get(ctx, statusWriteMigrationKey, current); err != nil {
		t.Fatalf("getting migration to replace: %v", err)
	}
	// Force-remove the retention finalizer so the deletion actually completes.
	current.Finalizers = nil
	if err := c.Update(ctx, current); err != nil {
		t.Fatalf("removing retention finalizer: %v", err)
	}
	if err := c.Delete(ctx, current); client.IgnoreNotFound(err) != nil {
		t.Fatalf("deleting migration to replace: %v", err)
	}
	replacement := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       statusWriteMigrationKey.Name,
			Namespace:  statusWriteMigrationKey.Namespace,
			Generation: 1,
			UID:        replacementMigrationUID,
			Finalizers: []string{migrationv1alpha1.FinalizerName},
		},
		Spec: current.Spec,
	}
	if err := c.Create(ctx, replacement); err != nil {
		t.Fatalf("creating replacement migration: %v", err)
	}
}

// getStatusWriteMigration reads the persisted migration and asserts the
// replacement object is still the one under the singleton key.
func getStatusWriteMigration(ctx context.Context, t *testing.T, c client.Client) *migrationv1alpha1.VmwareCloudFoundationMigration {
	t.Helper()
	persisted := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := c.Get(ctx, statusWriteMigrationKey, persisted); err != nil {
		t.Fatalf("getting persisted migration: %v", err)
	}
	return persisted
}

// assertNothingPublishedToReplacement verifies a superseded reconcile published
// no names, progress, conditions, completion record, or timestamp.
func assertNothingPublishedToReplacement(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	persisted := getStatusWriteMigration(ctx, t, c)
	if persisted.UID != replacementMigrationUID {
		t.Fatalf("persisted UID = %q, want %q (the replacement object)", persisted.UID, replacementMigrationUID)
	}
	if len(persisted.Status.Conditions) != 0 {
		t.Fatalf("superseded reconcile published conditions to the replacement object: %+v", persisted.Status.Conditions)
	}
	if persisted.Status.Progress != nil {
		t.Fatalf("superseded reconcile published progress to the replacement object: %+v", persisted.Status.Progress)
	}
	if len(persisted.Status.WorkerMachineSetNames) != 0 {
		t.Fatalf("superseded reconcile published MachineSet names to the replacement object: %v", persisted.Status.WorkerMachineSetNames)
	}
	if persisted.Status.CompletionTime != nil || persisted.Status.CompletionSpecHash != "" {
		t.Fatalf("superseded reconcile published a completion record to the replacement object: %v/%q", persisted.Status.CompletionTime, persisted.Status.CompletionSpecHash)
	}
	if persisted.Status.LastUpdateTime != nil {
		t.Fatalf("superseded reconcile published lastUpdateTime %v to the replacement object", persisted.Status.LastUpdateTime)
	}
}

// assertStatusPublished verifies a same-identity reconcile published the names
// it recorded, its progress, and its condition.
func assertStatusPublished(ctx context.Context, t *testing.T, c client.Client, wantNames []string) {
	t.Helper()
	persisted := getStatusWriteMigration(ctx, t, c)
	if persisted.UID != originatingMigrationUID {
		t.Fatalf("persisted UID = %q, want %q (the originating object)", persisted.UID, originatingMigrationUID)
	}
	if !slices.Equal(persisted.Status.WorkerMachineSetNames, wantNames) {
		t.Fatalf("persisted WorkerMachineSetNames = %v, want %v", persisted.Status.WorkerMachineSetNames, wantNames)
	}
	if persisted.Status.Progress == nil {
		t.Fatal("persisted progress = nil, want the workload progress of the reconcile")
	}
	cond := apimeta.FindStatusCondition(persisted.Status.Conditions, migrationv1alpha1.ConditionWorkloadMigrated)
	if cond == nil {
		t.Fatalf("condition %q not published", migrationv1alpha1.ConditionWorkloadMigrated)
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != migrationv1alpha1.ReasonProgressing {
		t.Fatalf("condition %q = %s/%s, want %s/%s", migrationv1alpha1.ConditionWorkloadMigrated, cond.Status, cond.Reason, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing)
	}
	if persisted.Status.LastUpdateTime == nil {
		t.Fatal("persisted lastUpdateTime = nil, want the reconcile's timestamp")
	}
}

// TestReconcileSkipsStatusPublicationForSupersededObject covers deleting and
// recreating the migration (same name and namespace, new UID) while a reconcile
// for the old object is still running. Both inventory writers merge by key, so
// without a UID fence the old reconcile's destination MachineSet names, progress
// and conditions land on an object that never computed them.
func TestReconcileSkipsStatusPublicationForSupersededObject(t *testing.T) {
	ctx := context.Background()
	wantNames := []string{workerMachineSetName("test-infra", "target-fd-1")}
	req := ctrl.Request{NamespacedName: statusWriteMigrationKey}

	t.Run("replacement before the status refetch publishes nothing", func(t *testing.T) {
		harness := newStatusWriteHarness(t)
		// The administrator's delete/recreate lands after the destination
		// MachineSet is created, so the reconcile's own work is already done and
		// only the status refetch sees the replacement. The first armed read is
		// the execution-gate check, which must still see the originating object;
		// the second is updateStatus's refetch.
		var armed bool
		armedReads := 0
		harness.reconciler.Client = interceptor.NewClient(harness.fake, interceptor.Funcs{
			Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isMigration := obj.(*migrationv1alpha1.VmwareCloudFoundationMigration); isMigration && armed {
					armedReads++
					if armedReads == 1 {
						if err := underlying.Get(ctx, key, obj, opts...); err != nil {
							return err
						}
						replaceStatusWriteMigration(ctx, t, harness.fake)
						return nil
					}
				}
				return underlying.Get(ctx, key, obj, opts...)
			},
		})
		harness.reconciler.MachineClient.(*fakemachineclient.Clientset).PrependReactor("create", "machinesets",
			func(k8stesting.Action) (bool, runtime.Object, error) {
				armed = true
				return false, nil, nil
			})

		result, err := harness.reconciler.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if armedReads < 2 {
			t.Fatalf("armed migration reads = %d, want 2 (the execution check and the status refetch)", armedReads)
		}
		if result.RequeueAfter != 30*time.Second {
			t.Fatalf("Reconcile requeueAfter = %s, want %s", result.RequeueAfter, 30*time.Second)
		}
		assertNothingPublishedToReplacement(ctx, t, harness.fake)
	})

	t.Run("replacement between the refetch and the write publishes nothing", func(t *testing.T) {
		harness := newStatusWriteHarness(t)
		// The replacement lands between the status refetch and the write, so the
		// update conflicts and RetryOnConflict refetches the replacement.
		statusWrites := 0
		harness.reconciler.Client = interceptor.NewClient(harness.fake, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, underlying client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				statusWrites++
				replaceStatusWriteMigration(ctx, t, harness.fake)
				return apierrors.NewConflict(schema.GroupResource{
					Group:    migrationv1alpha1.GroupVersion.Group,
					Resource: "vmwarecloudfoundationmigrations",
				}, obj.GetName(), fmt.Errorf("injected conflict"))
			},
		})

		result, err := harness.reconciler.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if result.RequeueAfter != 30*time.Second {
			t.Fatalf("Reconcile requeueAfter = %s, want %s", result.RequeueAfter, 30*time.Second)
		}
		if statusWrites != 1 {
			t.Fatalf("status writes = %d, want 1 (the retry must not write to the replacement)", statusWrites)
		}
		assertNothingPublishedToReplacement(ctx, t, harness.fake)
	})

	t.Run("same object keeps publishing", func(t *testing.T) {
		harness := newStatusWriteHarness(t)

		if _, err := harness.reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertStatusPublished(ctx, t, harness.fake, wantNames)
	})
}

// TestPersistWorkerMachineSetNamesSkipsSupersededObject covers the dedicated
// persistence path Step 6 uses before destructive cleanup: it retries on
// conflict by key, so it must fence on the originating object identity itself.
func TestPersistWorkerMachineSetNamesSkipsSupersededObject(t *testing.T) {
	ctx := context.Background()
	name := workerMachineSetName("test-infra", "target-fd-1")

	fakeClient := clientfake.NewClientBuilder().
		WithScheme(newMigrationTestScheme(t)).
		WithObjects(newStatusWriteMigration(replacementMigrationUID)).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		Build()
	reconciler := &VmwareCloudFoundationMigrationReconciler{Client: fakeClient}

	originating := newStatusWriteMigration(originatingMigrationUID)
	originating.Status.WorkerMachineSetNames = []string{name}
	if err := reconciler.persistWorkerMachineSetNames(ctx, originating); err != nil {
		t.Fatalf("persistWorkerMachineSetNames: %v", err)
	}
	if got := getStatusWriteMigration(ctx, t, fakeClient).Status.WorkerMachineSetNames; len(got) != 0 {
		t.Fatalf("persisted WorkerMachineSetNames = %v, want none published to the replacement object", got)
	}

	// Control: the same object still gets its names persisted.
	current := newStatusWriteMigration(replacementMigrationUID)
	current.Status.WorkerMachineSetNames = []string{name}
	if err := reconciler.persistWorkerMachineSetNames(ctx, current); err != nil {
		t.Fatalf("persistWorkerMachineSetNames: %v", err)
	}
	if got := getStatusWriteMigration(ctx, t, fakeClient).Status.WorkerMachineSetNames; !slices.Equal(got, []string{name}) {
		t.Fatalf("persisted WorkerMachineSetNames = %v, want %v", got, []string{name})
	}
}
