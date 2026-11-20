package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"

	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	machinefake "github.com/openshift/client-go/machine/clientset/versioned/fake"
	"github.com/vmware/govmomi/simulator"
	_ "github.com/vmware/govmomi/vapi/simulator"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakekube "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

// reactorPrepender is implemented by every fake clientset used in these tests.
type reactorPrepender interface {
	PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
}

// watchClientsetWrites records mutation verbs submitted through fake
// clientsets so a denied execution can be proven to have produced no cluster
// side effects.
func watchClientsetWrites(clients ...reactorPrepender) *[]k8stesting.Action {
	var writes []k8stesting.Action
	for _, c := range clients {
		c.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
			switch action.GetVerb() {
			case "create", "update", "patch", "delete":
				writes = append(writes, action)
			}
			return false, nil, nil
		})
	}
	return &writes
}

// crWriteCountingClient wraps a controller-runtime client and counts every
// write verb, including status subresource writes, which route through
// separate interceptors rather than the ordinary Update/Patch callbacks.
func crWriteCountingClient(c client.WithWatch) (client.Client, *int) {
	writes := 0
	return interceptor.NewClient(c, interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			writes++
			return underlying.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes++
			return underlying.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, underlying client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			writes++
			return underlying.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			writes++
			return underlying.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, underlying client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return underlying.Status().Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, underlying client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			writes++
			return underlying.Status().Patch(ctx, obj, patch, opts...)
		},
	}), &writes
}

// TestDeniedExecutionBlocksHandlerMutations calls the workflow's mutation
// entry points under an execution context whose permission check denies
// (the live migration object was replaced) and asserts every handler stops
// before submitting any cluster mutation.
func TestDeniedExecutionBlocksHandlerMutations(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) (*VmwareCloudFoundationMigrationReconciler, *migrationv1alpha1.VmwareCloudFoundationMigration, *[]k8stesting.Action, *int, func(*testing.T))
		call  func(ctx context.Context, r *VmwareCloudFoundationMigrationReconciler, m *migrationv1alpha1.VmwareCloudFoundationMigration) error
	}{
		{
			name: "destination initialization creates no folder",
			setup: func(t *testing.T) (*VmwareCloudFoundationMigrationReconciler, *migrationv1alpha1.VmwareCloudFoundationMigration, *[]k8stesting.Action, *int, func(*testing.T)) {
				model := simulator.VPX()
				if err := model.Create(); err != nil {
					t.Fatalf("creating simulator model: %v", err)
				}
				t.Cleanup(model.Remove)
				model.Service.TLS = new(tls.Config)
				model.Service.RegisterEndpoints = true
				server := model.Service.NewServer()
				t.Cleanup(server.Close)
				ctx := model.Service.Context
				vsphere.ClearSessions(ctx)
				t.Cleanup(func() { vsphere.ClearSessions(ctx) })
				username := simulator.DefaultLogin.Username()
				password, ok := simulator.DefaultLogin.Password()
				if !ok {
					t.Fatal("simulator default login missing password")
				}
				inventory := discoverPreflightTestInventory(ctx, t, server.URL, username, password)
				const infraID = "ci-op-denied-init"
				m := newDestinationInitializedMigration(server.URL.Host, inventory)
				m.Finalizers = []string{migrationv1alpha1.FinalizerName}
				r := newDestinationInitializedReconciler(server.URL.Host, username, password, infraID)
				counting, crWrites := crWriteCountingClient(retentionTestClient(t, m))
				r.Client = counting
				verify := func(t *testing.T) {
					session, err := vsphere.GetOrCreate(ctx, vsphere.Params{
						Server:     server.URL.Host,
						Datacenter: inventory.datacenterName,
						Username:   username,
						Password:   password,
						Insecure:   true,
					})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := vsphere.GetVMFolder(ctx, session, infraID); err == nil {
						t.Fatal("ownership folder was created despite denied execution")
					}
				}
				return r, m, &[]k8stesting.Action{}, crWrites, verify
			},
			call: func(ctx context.Context, r *VmwareCloudFoundationMigrationReconciler, m *migrationv1alpha1.VmwareCloudFoundationMigration) error {
				_, err := r.ensureDestinationInitialized(ctx, m)
				return err
			},
		},
		{
			name: "multi-site configuration adds no credentials",
			setup: func(t *testing.T) (*VmwareCloudFoundationMigrationReconciler, *migrationv1alpha1.VmwareCloudFoundationMigration, *[]k8stesting.Action, *int, func(*testing.T)) {
				migration := newCleanupTestMigration()
				kubeClient := fakekube.NewSimpleClientset(
					newCleanupTestCredsSecret(cleanupTestSource),
					newCleanupTestConfigMap(cleanupTestSource),
					newCleanupTestTargetSecret(),
				)
				cfgClient := configfake.NewClientset(newCleanupTestInfrastructure(cleanupTestSource))
				r, crClient := newCleanupTestReconciler(t, migration, cfgClient, kubeClient)
				counting, crWrites := crWriteCountingClient(crClient.(client.WithWatch))
				r.Client = counting
				return r, migration, watchClientsetWrites(kubeClient, cfgClient), crWrites, nil
			},
			call: func(ctx context.Context, r *VmwareCloudFoundationMigrationReconciler, m *migrationv1alpha1.VmwareCloudFoundationMigration) error {
				_, err := r.ensureMultiSiteConfigured(ctx, m)
				return err
			},
		},
		{
			name: "workload rollout submits no scale-down",
			setup: func(t *testing.T) (*VmwareCloudFoundationMigrationReconciler, *migrationv1alpha1.VmwareCloudFoundationMigration, *[]k8stesting.Action, *int, func(*testing.T)) {
				m := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStateRunning)
				m.Finalizers = []string{migrationv1alpha1.FinalizerName}
				// The scale-down coverage gate requires the preflight mapping and
				// recorded worker MachineSets on any migration reaching Step 6.
				m.Status.FailureDomainMappings = []migrationv1alpha1.FailureDomainMapping{
					{Source: "source-fd-1", Destination: "target-fd-1"},
				}
				m.Status.WorkerMachineSetNames = []string{workerMachineSetName("test-infra", "target-fd-1")}
				destination := newTargetMachineSetForRollout(m.Status.WorkerMachineSetNames[0], "vcenter-target.example.com", 2)
				machines := machinefake.NewClientset(newCPMSForRollout(true, true), newSourceMachineSetForRollout(2), destination)
				kubeClient := fakekube.NewClientset()
				counting, crWrites := crWriteCountingClient(retentionTestClient(t, m))
				r := &VmwareCloudFoundationMigrationReconciler{
					Client:        counting,
					KubeClient:    kubeClient,
					ConfigClient:  configfake.NewClientset(newInfrastructureForRollout()),
					MachineClient: machines,
					Recorder:      events.NewFakeRecorder(10),
				}
				return r, m, watchClientsetWrites(machines, kubeClient), crWrites, nil
			},
			call: func(ctx context.Context, r *VmwareCloudFoundationMigrationReconciler, m *migrationv1alpha1.VmwareCloudFoundationMigration) error {
				_, err := r.ensureWorkloadMigratedRolloutAndScaleDown(ctx, m)
				return err
			},
		},
		{
			name: "source cleanup removes nothing",
			setup: func(t *testing.T) (*VmwareCloudFoundationMigrationReconciler, *migrationv1alpha1.VmwareCloudFoundationMigration, *[]k8stesting.Action, *int, func(*testing.T)) {
				migration := newCleanupTestMigration()
				// Pre-record the source identity so the persistSourceVCenter
				// status write is not part of this denied-execution path; a
				// write slipping through would still trip the CR write counter.
				migration.Status.SourceVCenter = cleanupTestSource
				cfgClient := configfake.NewClientset(newCleanupTestInfrastructure(cleanupTestSource, cleanupTestDest))
				kubeClient := fakekube.NewSimpleClientset(
					newCleanupTestConfigMap(cleanupTestSource, cleanupTestDest),
					newCleanupTestCredsSecret(cleanupTestSource, cleanupTestDest),
					newCleanupTestTargetSecret(),
				)
				r, crClient := newCleanupTestReconciler(t, migration, cfgClient, kubeClient)
				counting, crWrites := crWriteCountingClient(crClient.(client.WithWatch))
				r.Client = counting
				return r, migration, watchClientsetWrites(kubeClient, cfgClient), crWrites, nil
			},
			call: func(ctx context.Context, r *VmwareCloudFoundationMigrationReconciler, m *migrationv1alpha1.VmwareCloudFoundationMigration) error {
				_, err := r.ensureSourceCleaned(ctx, m)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			r, m, writes, crWrites, verify := tt.setup(t)
			// Deny permission by replacing the live migration's identity.
			live := m.DeepCopy()
			live.UID = types.UID("replacement-live-uid")
			r.APIReader = retentionTestClient(t, live)
			if err := tt.call(r.stateContext(ctx, m, migrationv1alpha1.MigrationStateRunning), r, m); !errors.Is(err, errExecutionStopped) {
				t.Fatalf("handler did not stop on denied execution: %v", err)
			}
			if len(*writes) != 0 {
				t.Fatalf("handler submitted %d cluster mutations while execution was denied: %v", len(*writes), *writes)
			}
			if *crWrites != 0 {
				t.Fatalf("handler submitted %d migration-resource writes (including status subresource) while execution was denied", *crWrites)
			}
			if verify != nil {
				verify(t)
			}
		})
	}
}
