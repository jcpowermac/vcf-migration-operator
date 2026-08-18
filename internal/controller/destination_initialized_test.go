/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/tags"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	_ "github.com/vmware/govmomi/vapi/simulator"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/vsphere"
)

// newDestinationInitializedReconciler builds a reconciler wired to a fake
// KubeClient/ConfigClient for the given infraID and target vCenter server.
func newDestinationInitializedReconciler(server, username, password, infraID string) *VmwareCloudFoundationMigrationReconciler {
	return &VmwareCloudFoundationMigrationReconciler{
		KubeClient: fakekube.NewClientset(
			newTargetCredentialsSecret("default", "target-vcenter-creds", server, username, password),
		),
		ConfigClient: configfake.NewClientset(&configv1.Infrastructure{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Status:     configv1.InfrastructureStatus{InfrastructureName: infraID},
		}),
		Recorder: record.NewFakeRecorder(10),
	}
}

func newDestinationInitializedMigration(server string, inventory preflightTestInventory) *migrationv1alpha1.VmwareCloudFoundationMigration {
	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "default"},
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
					},
				},
			},
		},
	}
}

func TestEnsureDestinationInitialized(t *testing.T) {
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

	t.Run("creates folder, ownership tag, and failure domain tags on first reconcile", func(t *testing.T) {
		const infraID = "ci-op-happy-path"
		reconciler := newDestinationInitializedReconciler(server.URL.Host, username, password, infraID)
		migration := newDestinationInitializedMigration(server.URL.Host, inventory)

		if _, err := reconciler.ensureDestinationInitialized(ctx, migration); err != nil {
			t.Fatalf("ensureDestinationInitialized: %v", err)
		}

		cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionDestinationInitialized)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("DestinationInitialized condition = %+v, want True", cond)
		}
	})

	t.Run("skips re-validating the ownership category when the folder already has the tag attached", func(t *testing.T) {
		const infraID = "ci-op-already-done"

		session, err := vsphere.GetOrCreate(ctx, vsphere.Params{
			Server:     server.URL.Host,
			Datacenter: inventory.datacenterName,
			Username:   username,
			Password:   password,
			Insecure:   true,
		})
		if err != nil {
			t.Fatalf("creating test vSphere session: %v", err)
		}

		// Pre-create the ownership category the way a prior, already-succeeded
		// reconcile would have left it, but with associable types that make a
		// *second* strict validation fail (mirroring the CI incident, where a
		// second reconcile's re-validation of the just-created category failed
		// with "missing required associable types").
		categoryName := vsphere.ClusterOwnershipCategoryName(infraID)
		catID, err := session.TagManager.CreateCategory(ctx, &tags.Category{
			Name:            categoryName,
			Description:     vsphere.ClusterOwnershipDescription,
			Cardinality:     "SINGLE",
			AssociableTypes: []string{"Datastore"},
		})
		if err != nil {
			t.Fatalf("pre-creating ownership category: %v", err)
		}
		tagID, err := session.TagManager.CreateTag(ctx, &tags.Tag{
			Name:       infraID,
			CategoryID: catID,
		})
		if err != nil {
			t.Fatalf("pre-creating ownership tag: %v", err)
		}
		folder, err := vsphere.CreateVMFolder(ctx, session, infraID)
		if err != nil {
			t.Fatalf("pre-creating VM folder: %v", err)
		}
		if err := session.TagManager.AttachTag(ctx, tagID, folder.Reference()); err != nil {
			t.Fatalf("pre-attaching ownership tag to folder: %v", err)
		}

		reconciler := newDestinationInitializedReconciler(server.URL.Host, username, password, infraID)
		migration := newDestinationInitializedMigration(server.URL.Host, inventory)

		if _, err := reconciler.ensureDestinationInitialized(ctx, migration); err != nil {
			t.Fatalf("ensureDestinationInitialized: %v (should have skipped re-validating the already-attached ownership tag)", err)
		}

		cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionDestinationInitialized)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("DestinationInitialized condition = %+v, want True", cond)
		}
	})
}

// TestEnsureDestinationInitializedConcurrency verifies that folder and
// ownership tag initialization is safe under repeated and concurrent
// reconciles — the interleaving that failed the periodic E2E job, where a
// second reconcile re-entered the ownership-tag path after the first had
// already configured the target. Safety comes from idempotency against vCenter
// state, not from an in-process lock: overlapping reconciles occur across
// processes during a leader handoff, which a mutex could not serialize.
func TestEnsureDestinationInitializedConcurrency(t *testing.T) {
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

	t.Run("a repeat reconcile after full initialization still succeeds", func(t *testing.T) {
		const infraID = "ci-op-repeat"
		reconciler := newDestinationInitializedReconciler(server.URL.Host, username, password, infraID)

		// First reconcile fully initializes the target: folder created,
		// ownership category/tag created, tag attached to the folder.
		first := newDestinationInitializedMigration(server.URL.Host, inventory)
		if _, err := reconciler.ensureDestinationInitialized(ctx, first); err != nil {
			t.Fatalf("first ensureDestinationInitialized: %v", err)
		}

		// A second reconcile re-enters the ownership-tag path after the first
		// already configured the target — the exact CI interleaving. It must
		// observe the completed state and skip, not re-validate the category
		// and fail.
		second := newDestinationInitializedMigration(server.URL.Host, inventory)
		if _, err := reconciler.ensureDestinationInitialized(ctx, second); err != nil {
			t.Fatalf("repeat ensureDestinationInitialized: %v (should have skipped the already-configured target)", err)
		}

		cond := apimeta.FindStatusCondition(second.Status.Conditions, migrationv1alpha1.ConditionDestinationInitialized)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("DestinationInitialized condition = %+v, want True", cond)
		}
	})

	t.Run("two concurrent reconciles from a cold state both succeed", func(t *testing.T) {
		const infraID = "ci-op-concurrent"

		type result struct {
			err  error
			cond *metav1.Condition
		}
		outcomes := make(chan result, 2)
		for range 2 {
			go func() {
				reconciler := newDestinationInitializedReconciler(server.URL.Host, username, password, infraID)
				migration := newDestinationInitializedMigration(server.URL.Host, inventory)
				_, err := reconciler.ensureDestinationInitialized(ctx, migration)
				outcomes <- result{
					err:  err,
					cond: apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionDestinationInitialized),
				}
			}()
		}
		for range 2 {
			select {
			case res := <-outcomes:
				if res.err != nil {
					t.Fatalf("concurrent ensureDestinationInitialized: %v", res.err)
				}
				if res.cond == nil || res.cond.Status != metav1.ConditionTrue {
					t.Fatalf("DestinationInitialized condition = %+v, want True", res.cond)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("concurrent reconciles did not finish")
			}
		}

		// The target must end up fully configured: folder exists with the
		// ownership tag attached.
		session, err := vsphere.GetOrCreate(ctx, vsphere.Params{
			Server:     server.URL.Host,
			Datacenter: inventory.datacenterName,
			Username:   username,
			Password:   password,
			Insecure:   true,
		})
		if err != nil {
			t.Fatalf("creating test vSphere session: %v", err)
		}
		folder, err := vsphere.GetVMFolder(ctx, session, infraID)
		if err != nil {
			t.Fatalf("looking up folder after concurrent reconciles: %v", err)
		}
		hasOwnership, err := vsphere.ObjectHasTagInCategory(ctx, session, vsphere.ClusterOwnershipCategoryName(infraID), folder)
		if err != nil {
			t.Fatalf("checking ownership tag after concurrent reconciles: %v", err)
		}
		if !hasOwnership {
			t.Fatal("ownership tag not attached to folder after concurrent reconciles")
		}
	})
}
