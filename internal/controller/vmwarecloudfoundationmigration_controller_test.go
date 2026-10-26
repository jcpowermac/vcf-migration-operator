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
	"fmt"
	"reflect"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

func TestConditionOrderSkipsImageImport(t *testing.T) {
	for i, condition := range conditionOrder {
		if condition == migrationv1alpha1.ConditionDestinationInitialized {
			if i+1 >= len(conditionOrder) {
				t.Fatal("DestinationInitialized has no successor in condition order")
			}
			if conditionOrder[i+1] != migrationv1alpha1.ConditionMultiSiteConfigured {
				t.Fatalf("condition after DestinationInitialized = %q, want MultiSiteConfigured", conditionOrder[i+1])
			}
			return
		}
	}
	t.Fatal("DestinationInitialized missing from condition order")
}

var _ = Describe("required failure-domain template admission", func() {
	It("rejects absent and empty templates in every failure domain", func() {
		for _, tt := range []struct {
			index int
			empty bool
		}{
			{index: 0}, {index: 1}, {index: 0, empty: true}, {index: 1, empty: true},
		} {
			By(fmt.Sprintf("checking index %d, explicitly empty %t", tt.index, tt.empty))
			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "required-template-", Namespace: "default"},
				Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
					State:                          migrationv1alpha1.MigrationStatePending,
					TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{Name: "target-vcenter-creds"},
					FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
						{
							Name: "fd-a", Region: "region-a", Zone: "zone-a", Server: "vcenter.example.com",
							Topology: configv1.VSpherePlatformTopology{Datacenter: testTargetDC, ComputeCluster: "/DC1/host/Cluster1", Datastore: "/DC1/datastore/DS1", Networks: []string{"VM Network"}, Template: "/DC1/vm/template-a"},
						},
						{
							Name: "fd-b", Region: "region-b", Zone: "zone-b", Server: "vcenter.example.com",
							Topology: configv1.VSpherePlatformTopology{Datacenter: testTargetDC, ComputeCluster: "/DC1/host/Cluster1", Datastore: "/DC1/datastore/DS1", Networks: []string{"VM Network"}, Template: "/DC1/vm/template-b"},
						},
					},
				},
			}
			resource.Spec.FailureDomains[tt.index].Topology.Template = ""
			if tt.empty {
				// Go's encoder omits empty fields; an unstructured create exercises explicit empty JSON.
				obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(resource)
				Expect(err).NotTo(HaveOccurred())
				fd := obj["spec"].(map[string]interface{})["failureDomains"].([]interface{})[tt.index].(map[string]interface{})
				fd["topology"].(map[string]interface{})["template"] = ""
				unstructuredResource := &unstructured.Unstructured{Object: obj}
				unstructuredResource.SetAPIVersion("migration.openshift.io/v1alpha1")
				unstructuredResource.SetKind("VmwareCloudFoundationMigration")
				Expect(k8sClient.Create(ctx, unstructuredResource)).To(MatchError(ContainSubstring("topology.template is required")))
			} else {
				Expect(k8sClient.Create(ctx, resource)).To(MatchError(ContainSubstring("topology.template is required")))
			}
		}
	})

	It("accepts explicit templates for every failure domain", func() {
		resource := &migrationv1alpha1.VmwareCloudFoundationMigration{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "provided-template-", Namespace: "default"},
			Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
				TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{Name: "target-vcenter-creds"},
				FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
					Name: "fd-a", Region: "region-a", Zone: "zone-a", Server: "vcenter.example.com",
					Topology: configv1.VSpherePlatformTopology{Datacenter: testTargetDC, ComputeCluster: "/DC1/host/Cluster1", Datastore: "/DC1/datastore/DS1", Networks: []string{"VM Network"}, Template: "/DC1/vm/template-a"},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, resource)).To(Succeed()) })
		Expect(resource.Spec.State).To(Equal(migrationv1alpha1.MigrationStatePending))

		By("accepting Planning on update and create")
		resource.Spec.State = migrationv1alpha1.MigrationStatePlanning
		Expect(k8sClient.Update(ctx, resource)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name, Namespace: resource.Namespace}, resource)).To(Succeed())
		Expect(resource.Spec.State).To(Equal(migrationv1alpha1.MigrationStatePlanning))

		planned := resource.DeepCopy()
		planned.Name = ""
		planned.ResourceVersion = ""
		planned.UID = ""
		Expect(k8sClient.Create(ctx, planned)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, planned)).To(Succeed()) })
		Expect(planned.Spec.State).To(Equal(migrationv1alpha1.MigrationStatePlanning))
	})
})

var _ = Describe("storage configuration admission", func() {
	newResource := func() *migrationv1alpha1.VmwareCloudFoundationMigration {
		return &migrationv1alpha1.VmwareCloudFoundationMigration{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "storage-admission-", Namespace: "default"},
			Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
				TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{Name: "target-vcenter-creds"},
				Storage:                        &migrationv1alpha1.StorageMigrationSpec{},
				FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
					Name: "fd-a", Region: "region-a", Zone: "zone-a", Server: "vcenter.example.com",
					Topology: configv1.VSpherePlatformTopology{Datacenter: "DC1", ComputeCluster: "/DC1/host/Cluster1", Datastore: "/DC1/datastore/DS1", Networks: []string{"VM Network"}, Template: "/DC1/vm/template-a"},
				}},
			},
		}
	}

	It("rejects storage when migrateStorage is not enabled", func() {
		Expect(k8sClient.Create(ctx, newResource())).To(MatchError(ContainSubstring("storage can only be set when migrateStorage is true")))
	})

	It("accepts storage when migrateStorage is enabled and rejects disabling it", func() {
		resource := newResource()
		resource.Spec.MigrateStorage = true
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, resource)).To(Succeed()) })

		resource.Spec.MigrateStorage = false
		Expect(k8sClient.Update(ctx, resource)).To(MatchError(ContainSubstring("storage can only be set when migrateStorage is true")))
	})
})

func TestSanitizeRFC1123(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "already valid", input: "zone-a", want: "zone-a"},
		{name: "underscore", input: "funny_solomon", want: "funny-solomon"},
		{name: "multiple underscores", input: "a_b_c", want: "a-b-c"},
		{name: "uppercase", input: "Zone-A", want: "zone-a"},
		{name: "spaces", input: "zone a", want: "zone-a"},
		{name: "consecutive invalid chars", input: "a__b", want: "a-b"},
		{name: "leading invalid", input: "_zone", want: zoneName},
		{name: "trailing invalid", input: "zone_", want: zoneName},
		{name: "mixed invalid", input: "My Zone!@#1", want: "my-zone-1"},
		{name: "empty string", input: "", want: ""},
		{name: "all invalid", input: "___", want: ""},
		{name: "only hyphens", input: "---", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeRFC1123(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeRFC1123(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestWorkerMachineSetName(t *testing.T) {
	tests := []struct {
		name    string
		infraID string
		fdName  string
		want    string
	}{
		{
			name:    "simple name",
			infraID: "ci-op-abc-12345",
			fdName:  "zone-a",
			want:    "ci-op-abc-12345-worker-zone-a",
		},
		{
			name:    "underscore replaced with hyphen",
			infraID: "ci-op-abc-12345",
			fdName:  "funny_solomon",
			want:    "ci-op-abc-12345-worker-funny-solomon",
		},
		{
			name:    "all invalid chars falls back to default",
			infraID: "ci-op-abc-12345",
			fdName:  "___",
			want:    "ci-op-abc-12345-worker-default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workerMachineSetName(tt.infraID, tt.fdName)
			if got != tt.want {
				t.Errorf("workerMachineSetName(%q, %q) = %q, want %q", tt.infraID, tt.fdName, got, tt.want)
			}
		})
	}
}

var _ = Describe("VmwareCloudFoundationMigration Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = migrationv1alpha1.SingletonName

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: migrationv1alpha1.SingletonNamespace,
		}
		vmwarecloudfoundationmigration := &migrationv1alpha1.VmwareCloudFoundationMigration{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind VmwareCloudFoundationMigration")
			err := k8sClient.Get(ctx, typeNamespacedName, vmwarecloudfoundationmigration)
			if err != nil && errors.IsNotFound(err) {
				resource := &migrationv1alpha1.VmwareCloudFoundationMigration{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: migrationv1alpha1.SingletonNamespace,
					},
					Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
						State: migrationv1alpha1.MigrationStatePending,
						TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{
							Name:      "target-vcenter-creds",
							Namespace: "default",
						},
						FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
							{
								Name:   "target-fd-1",
								Region: "target-region",
								Zone:   "target-zone-1",
								Server: "vcenter-target.example.com",
								Topology: configv1.VSpherePlatformTopology{
									Datacenter:     "TargetDC",
									ComputeCluster: "/TargetDC/host/TargetCluster",
									Datastore:      "/TargetDC/datastore/TargetDatastore",
									Networks:       []string{"VM Network"},
									ResourcePool:   "/TargetDC/host/TargetCluster/Resources",
									Template:       "/TargetDC/vm/rhcos-template",
									Folder:         "/TargetDC/vm/my-cluster-infra-id",
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance VmwareCloudFoundationMigration")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			releaseMigrationFinalizer(ctx, resource)
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &VmwareCloudFoundationMigrationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// The first reconcile adds the record-retention finalizer and returns;
			// the update event requeues the migration workflow itself.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionReady)).To(BeNil())
			Expect(resource.Status.StartTime).To(BeNil())
			Expect(resource.Status.LastUpdateTime).NotTo(BeNil())
		})

		It("should record a paused Ready condition when in Paused state", func() {
			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.State = migrationv1alpha1.MigrationStatePaused
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			controllerReconciler := &VmwareCloudFoundationMigrationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// The first reconcile adds the record-retention finalizer and returns.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			paused := apimeta.FindStatusCondition(updated.Status.Conditions, migrationv1alpha1.ConditionReady)
			Expect(paused).NotTo(BeNil())
			Expect(paused.Status).To(Equal(metav1.ConditionFalse))
			Expect(paused.Reason).To(Equal(migrationv1alpha1.ReasonPaused))
			Expect(updated.Status.LastUpdateTime).NotTo(BeNil())
		})
	})

	Context("When reconciling a resource that is not named the singleton name", func() {
		const resourceName = "not-cluster"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		BeforeEach(func() {
			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
					State: migrationv1alpha1.MigrationStateRunning,
					TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{
						Name:      "target-vcenter-creds",
						Namespace: "default",
					},
					FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
						{
							Name:   "target-fd-1",
							Region: "target-region",
							Zone:   "target-zone-1",
							Server: "vcenter-target.example.com",
							Topology: configv1.VSpherePlatformTopology{
								Datacenter:     "TargetDC",
								ComputeCluster: "/TargetDC/host/TargetCluster",
								Datastore:      "/TargetDC/datastore/TargetDatastore",
								Networks:       []string{"VM Network"},
								ResourcePool:   "/TargetDC/host/TargetCluster/Resources",
								Template:       "/TargetDC/vm/rhcos-template",
								Folder:         "/TargetDC/vm/my-cluster-infra-id",
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should ignore the resource, mark it as not accepted, and record a warning event", func() {
			fakeRecorder := events.NewFakeRecorder(10)
			controllerReconciler := &VmwareCloudFoundationMigrationReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: fakeRecorder,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(fakeRecorder.Events).To(Receive(SatisfyAll(
				ContainSubstring("Warning"),
				ContainSubstring(migrationv1alpha1.ReasonUnsupportedName),
			)))

			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())

			cond := apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionAccepted)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(migrationv1alpha1.ReasonUnsupportedName))
			Expect(resource.Status.LastUpdateTime).NotTo(BeNil())

			// No workflow conditions should have been set since the resource was never processed.
			Expect(apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)).To(BeNil())
			Expect(resource.Status.StartTime).To(BeNil())
		})
	})

	Context("When reconciling a resource in Paused state", func() {
		const resourceName = migrationv1alpha1.SingletonName

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: migrationv1alpha1.SingletonNamespace,
		}

		BeforeEach(func() {
			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: migrationv1alpha1.SingletonNamespace,
				},
				Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
					State: migrationv1alpha1.MigrationStatePaused,
					TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{
						Name:      "target-vcenter-creds",
						Namespace: "default",
					},
					FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
						{
							Name:   "target-fd-1",
							Region: "target-region",
							Zone:   "target-zone-1",
							Server: "vcenter-target.example.com",
							Topology: configv1.VSpherePlatformTopology{
								Datacenter:     "TargetDC",
								ComputeCluster: "/TargetDC/host/TargetCluster",
								Datastore:      "/TargetDC/datastore/TargetDatastore",
								Networks:       []string{"VM Network"},
								ResourcePool:   "/TargetDC/host/TargetCluster/Resources",
								Template:       "/TargetDC/vm/rhcos-template",
								Folder:         "/TargetDC/vm/my-cluster-infra-id",
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			releaseMigrationFinalizer(ctx, resource)
		})

		It("should set the Ready condition to False with reason Paused and transition to Progressing on resume", func() {
			fakeRecorder := events.NewFakeRecorder(10)
			controllerReconciler := &VmwareCloudFoundationMigrationReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				KubeClient: fakekube.NewClientset(),
				Recorder:   fakeRecorder,
			}

			// The first reconcile adds the record-retention finalizer and returns.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(fakeRecorder.Events).To(Receive(SatisfyAll(
				ContainSubstring("Normal"),
				ContainSubstring(migrationv1alpha1.ReasonPaused),
				ContainSubstring("Migration is paused; set spec.state to Running to resume"),
			)))

			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())

			cond := apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(migrationv1alpha1.ReasonPaused))
			Expect(cond.Message).To(Equal("Migration is paused; set spec.state to Running to resume"))
			Expect(cond.ObservedGeneration).To(Equal(resource.Generation))

			// Workflow conditions should not have started
			Expect(apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)).To(BeNil())
			Expect(resource.Status.StartTime).To(BeNil())

			// Subsequent reconcile while still paused should be idempotent and not re-emit events
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeRecorder.Events).NotTo(Receive())

			// Resume migration by setting state to Running
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.State = migrationv1alpha1.MigrationStateRunning
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			// The fake client has no credentials secret for the resumed workflow,
			// so reconciliation persists the Running condition then fails.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).To(HaveOccurred())

			Expect(fakeRecorder.Events).To(Receive(SatisfyAll(
				ContainSubstring("Normal"),
				ContainSubstring(migrationv1alpha1.ReasonProgressing),
				ContainSubstring("Migration is running"),
			)))

			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())

			cond = apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(migrationv1alpha1.ReasonProgressing))
			Expect(cond.Message).To(Equal("Migration is running"))
			Expect(cond.ObservedGeneration).To(Equal(resource.Generation))
		})
	})
})

var _ = Describe("updateStatus", func() {
	const resourceName = "status-merge-test"

	ctx := context.Background()

	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: "default",
	}

	// newStatusTestResource builds the CR used by the updateStatus merge tests.
	newStatusTestResource := func() *migrationv1alpha1.VmwareCloudFoundationMigration {
		return &migrationv1alpha1.VmwareCloudFoundationMigration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resourceName,
				Namespace: "default",
			},
			Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
				State: migrationv1alpha1.MigrationStatePending,
				TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{
					Name:      "target-vcenter-creds",
					Namespace: "default",
				},
				FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
					{
						Name:   "target-fd-1",
						Region: "target-region",
						Zone:   "target-zone-1",
						Server: "vcenter-target.example.com",
						Topology: configv1.VSpherePlatformTopology{
							Datacenter:     "TargetDC",
							ComputeCluster: "/TargetDC/host/TargetCluster",
							Datastore:      "/TargetDC/datastore/TargetDatastore",
							Networks:       []string{"VM Network"},
							ResourcePool:   "/TargetDC/host/TargetCluster/Resources",
							Template:       "/TargetDC/vm/rhcos-template",
							Folder:         "/TargetDC/vm/my-cluster-infra-id",
						},
					},
				},
			},
		}
	}

	AfterEach(func() {
		resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		if err := k8sClient.Get(ctx, typeNamespacedName, resource); err == nil {
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		}
	})

	It("does not discard a condition committed by another writer in between reads", func() {
		resource := newStatusTestResource()
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		reconciler := &VmwareCloudFoundationMigrationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		// Simulate two reconciles that both started from the same initial
		// state, before either had persisted a status change: each holds its
		// own in-memory copy, unaware of what the other is about to write.
		migrationA := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migrationA)).To(Succeed())
		baseA := *migrationA.Status.DeepCopy()
		migrationB := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migrationB)).To(Succeed())
		baseB := *migrationB.Status.DeepCopy()

		reconciler.setCondition(migrationA, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "preflight passed")
		Expect(reconciler.updateStatus(ctx, migrationA, baseA)).To(Succeed())

		// migrationB's in-memory status predates migrationA's write, so it
		// knows nothing about ConditionInfrastructurePrepared.
		reconciler.setCondition(migrationB, migrationv1alpha1.ConditionDestinationInitialized, metav1.ConditionFalse, migrationv1alpha1.ReasonFailed, "stale failure")
		Expect(reconciler.updateStatus(ctx, migrationB, baseB)).To(Succeed())

		final := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, final)).To(Succeed())

		infraCond := apimeta.FindStatusCondition(final.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
		Expect(infraCond).NotTo(BeNil(), "InfrastructurePrepared condition set by the first writer must survive the second writer's update")
		Expect(infraCond.Status).To(Equal(metav1.ConditionTrue))

		destCond := apimeta.FindStatusCondition(final.Status.Conditions, migrationv1alpha1.ConditionDestinationInitialized)
		Expect(destCond).NotTo(BeNil())
		Expect(destCond.Status).To(Equal(metav1.ConditionFalse))
	})

	It("does not let a stale-generation reconcile overwrite a current-generation condition", func() {
		resource := newStatusTestResource()
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		reconciler := &VmwareCloudFoundationMigrationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		// Capture an in-memory copy from generation 1 before any spec change.
		migrationStale := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migrationStale)).To(Succeed())
		baseStale := *migrationStale.Status.DeepCopy()

		// Bump the resource generation with a spec update.
		current := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, current)).To(Succeed())
		current.Spec.FailureDomains[0].Name = "renamed-fd"
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		// A current-generation reconcile commits an in-progress condition.
		migrationCurrent := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migrationCurrent)).To(Succeed())
		baseCurrent := *migrationCurrent.Status.DeepCopy()
		reconciler.setCondition(migrationCurrent, migrationv1alpha1.ConditionDestinationInitialized, metav1.ConditionFalse, migrationv1alpha1.ReasonProgressing, "current progress")
		Expect(reconciler.updateStatus(ctx, migrationCurrent, baseCurrent)).To(Succeed())

		// The stale reconcile still holds generation 1 and tries to persist a failure.
		reconciler.setCondition(migrationStale, migrationv1alpha1.ConditionDestinationInitialized, metav1.ConditionFalse, migrationv1alpha1.ReasonFailed, "stale failure")
		Expect(reconciler.updateStatus(ctx, migrationStale, baseStale)).To(Succeed())

		final := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, final)).To(Succeed())

		destCond := apimeta.FindStatusCondition(final.Status.Conditions, migrationv1alpha1.ConditionDestinationInitialized)
		Expect(destCond).NotTo(BeNil())
		Expect(destCond.Reason).To(Equal(migrationv1alpha1.ReasonProgressing), "a stale-generation update must not overwrite a current-generation condition")
		Expect(destCond.Message).To(Equal("current progress"))
		Expect(destCond.ObservedGeneration).To(Equal(migrationCurrent.Generation))
	})

	It("does not let a stale-generation reconcile overwrite current-generation Progress", func() {
		resource := newStatusTestResource()
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		reconciler := &VmwareCloudFoundationMigrationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		// Capture an in-memory copy from generation 1 before any spec change.
		migrationStale := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migrationStale)).To(Succeed())
		baseStale := *migrationStale.Status.DeepCopy()

		// Bump the resource generation with a spec update.
		current := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, current)).To(Succeed())
		current.Spec.FailureDomains[0].Name = "renamed-fd"
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		// A current-generation reconcile commits Progress.
		migrationCurrent := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migrationCurrent)).To(Succeed())
		baseCurrent := *migrationCurrent.Status.DeepCopy()
		migrationCurrent.Status.Progress = &migrationv1alpha1.MigrationProgress{
			Workers: &migrationv1alpha1.WorkerMigrationProgress{
				TargetMachinesTotal:     3,
				TargetMachinesReady:     3,
				SourceMachinesRemaining: 2,
			},
			ControlPlane: &migrationv1alpha1.ControlPlaneProgress{Replicas: 3},
		}
		Expect(reconciler.updateStatus(ctx, migrationCurrent, baseCurrent)).To(Succeed())

		// The stale reconcile still holds generation 1 and tries to persist
		// its own Progress.
		migrationStale.Status.Progress = &migrationv1alpha1.MigrationProgress{
			Workers:      &migrationv1alpha1.WorkerMigrationProgress{SourceMachinesRemaining: 1},
			ControlPlane: &migrationv1alpha1.ControlPlaneProgress{Replicas: 1},
		}
		Expect(reconciler.updateStatus(ctx, migrationStale, baseStale)).To(Succeed())

		final := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, final)).To(Succeed())

		Expect(final.Status.Progress).NotTo(BeNil())
		Expect(final.Status.Progress.Workers.TargetMachinesTotal).To(Equal(int32(3)))
		Expect(final.Status.Progress.Workers.SourceMachinesRemaining).To(Equal(int32(2)))
		Expect(final.Status.Progress.ControlPlane.Replicas).To(Equal(int32(3)))
	})

	It("does not let a stale failure overwrite a concurrent success on the same condition", func() {
		resource := newStatusTestResource()
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		reconciler := &VmwareCloudFoundationMigrationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		// Both reconciles start from the same initial snapshot.
		migrationA := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migrationA)).To(Succeed())
		baseA := *migrationA.Status.DeepCopy()
		migrationB := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migrationB)).To(Succeed())
		baseB := *migrationB.Status.DeepCopy()

		// The concurrent success commits first.
		reconciler.setCondition(migrationA, migrationv1alpha1.ConditionDestinationInitialized, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "destination initialized")
		Expect(reconciler.updateStatus(ctx, migrationA, baseA)).To(Succeed())

		// A later, stale reconcile that started from the same snapshot failed
		// and now persists its failure.
		reconciler.setCondition(migrationB, migrationv1alpha1.ConditionDestinationInitialized, metav1.ConditionFalse, migrationv1alpha1.ReasonFailed, "stale failure")
		Expect(reconciler.updateStatus(ctx, migrationB, baseB)).To(Succeed())

		final := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, final)).To(Succeed())

		destCond := apimeta.FindStatusCondition(final.Status.Conditions, migrationv1alpha1.ConditionDestinationInitialized)
		Expect(destCond).NotTo(BeNil())
		Expect(destCond.Status).To(Equal(metav1.ConditionTrue), "a later stale failure must not overwrite a committed success")
		Expect(destCond.Reason).To(Equal(migrationv1alpha1.ReasonCompleted))
	})

	It("persists newly recorded worker MachineSet names across the status update", func() {
		resource := newStatusTestResource()
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		reconciler := &VmwareCloudFoundationMigrationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		migration := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migration)).To(Succeed())
		base := *migration.Status.DeepCopy()

		// Names recorded during the rollout reconcile must be persisted
		// before cleanup can delete the source MachineSets they came from.
		migration.Status.WorkerMachineSetNames = []string{"test-infra-target-fd-1", "test-infra-target-fd-1-2"}
		Expect(reconciler.updateStatus(ctx, migration, base)).To(Succeed())

		first := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, first)).To(Succeed())
		Expect(first.Status.WorkerMachineSetNames).To(ConsistOf("test-infra-target-fd-1", "test-infra-target-fd-1-2"))

		// A later update merges one overlapping and one new name without
		// duplicates.
		base = *first.Status.DeepCopy()
		first.Status.WorkerMachineSetNames = append(first.Status.WorkerMachineSetNames, "test-infra-target-fd-1", "test-infra-target-fd-1-3")
		Expect(reconciler.updateStatus(ctx, first, base)).To(Succeed())

		final := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, final)).To(Succeed())
		Expect(final.Status.WorkerMachineSetNames).To(ConsistOf("test-infra-target-fd-1", "test-infra-target-fd-1-2", "test-infra-target-fd-1-3"))
	})

	It("persists Progress and updates LastUpdateTime", func() {
		resource := newStatusTestResource()
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		reconciler := &VmwareCloudFoundationMigrationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		migration := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migration)).To(Succeed())
		base := *migration.Status.DeepCopy()

		migration.Status.Progress = &migrationv1alpha1.MigrationProgress{
			Workers: &migrationv1alpha1.WorkerMigrationProgress{
				TargetMachinesTotal:     3,
				TargetMachinesReady:     2,
				TargetNodesReady:        2,
				SourceMachinesRemaining: 1,
			},
			ControlPlane: &migrationv1alpha1.ControlPlaneProgress{
				Replicas:        3,
				UpdatedReplicas: 2,
				ReadyReplicas:   2,
			},
		}

		Expect(reconciler.updateStatus(ctx, migration, base)).To(Succeed())

		final := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, final)).To(Succeed())

		Expect(final.Status.LastUpdateTime).NotTo(BeNil())
		Expect(final.Status.Progress).NotTo(BeNil())
		Expect(final.Status.Progress.Workers).NotTo(BeNil())
		Expect(final.Status.Progress.Workers.TargetMachinesTotal).To(Equal(int32(3)))
		Expect(final.Status.Progress.Workers.TargetMachinesReady).To(Equal(int32(2)))
		Expect(final.Status.Progress.Workers.TargetNodesReady).To(Equal(int32(2)))
		Expect(final.Status.Progress.Workers.SourceMachinesRemaining).To(Equal(int32(1)))
		Expect(final.Status.Progress.ControlPlane).NotTo(BeNil())
		Expect(final.Status.Progress.ControlPlane.Replicas).To(Equal(int32(3)))
		Expect(final.Status.Progress.ControlPlane.UpdatedReplicas).To(Equal(int32(2)))
		Expect(final.Status.Progress.ControlPlane.ReadyReplicas).To(Equal(int32(2)))
	})

	It("persists CompletionTime when migration is finished", func() {
		resource := newStatusTestResource()
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		reconciler := &VmwareCloudFoundationMigrationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		migration := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migration)).To(Succeed())
		base := *migration.Status.DeepCopy()

		now := metav1.Now()
		migration.Status.CompletionTime = &now
		reconciler.setCondition(migration, migrationv1alpha1.ConditionReady, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "Migration complete")

		Expect(reconciler.updateStatus(ctx, migration, base)).To(Succeed())

		final := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, final)).To(Succeed())

		Expect(final.Status.CompletionTime).NotTo(BeNil())
		Expect(final.Status.LastUpdateTime).NotTo(BeNil())
		readyCond := apimeta.FindStatusCondition(final.Status.Conditions, migrationv1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("persists FailureDomainMappings recorded at preflight", func() {
		resource := newStatusTestResource()
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		reconciler := &VmwareCloudFoundationMigrationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		migration := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migration)).To(Succeed())
		base := *migration.Status.DeepCopy()

		// Simulate runPreflightChecks recording the effective mapping.
		migration.Status.FailureDomainMappings = []migrationv1alpha1.FailureDomainMapping{
			{Source: "source-fd-a", Destination: "target-fd-1"},
		}
		reconciler.setCondition(migration, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "Preflight validation passed")
		Expect(reconciler.updateStatus(ctx, migration, base)).To(Succeed())

		// A later reconcile holding no mappings must not clear the recorded ones.
		later := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, later)).To(Succeed())
		laterBase := *later.Status.DeepCopy()
		Expect(reconciler.updateStatus(ctx, later, laterBase)).To(Succeed())

		final := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, final)).To(Succeed())

		Expect(final.Status.FailureDomainMappings).To(HaveLen(1))
		Expect(final.Status.FailureDomainMappings[0].Source).To(Equal("source-fd-a"))
		Expect(final.Status.FailureDomainMappings[0].Destination).To(Equal("target-fd-1"))
	})
})

var _ = Describe("seedReadyCondition", func() {
	It("seeds Ready as False/Progressing when the condition is absent", func() {
		r := &VmwareCloudFoundationMigrationReconciler{}
		migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
			ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Generation: 1},
		}
		r.seedReadyCondition(migration)

		cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(migrationv1alpha1.ReasonProgressing))
		Expect(cond.ObservedGeneration).To(Equal(int64(1)))
	})

	It("leaves an existing Ready condition untouched", func() {
		r := &VmwareCloudFoundationMigrationReconciler{}
		migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
			ObjectMeta: metav1.ObjectMeta{Name: migrationv1alpha1.SingletonName, Generation: 2},
			Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
				Conditions: []metav1.Condition{{
					Type:   migrationv1alpha1.ConditionReady,
					Status: metav1.ConditionTrue,
					Reason: migrationv1alpha1.ReasonCompleted,
				}},
			},
		}
		r.seedReadyCondition(migration)

		cond := apimeta.FindStatusCondition(migration.Status.Conditions, migrationv1alpha1.ConditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(migrationv1alpha1.ReasonCompleted))
		Expect(migration.Status.Conditions).To(HaveLen(1))
	})
})

// releaseMigrationFinalizer strips the record-retention finalizer after a test
// deletion so envtest actually removes the singleton between specs.
func releaseMigrationFinalizer(ctx context.Context, resource *migrationv1alpha1.VmwareCloudFoundationMigration) {
	if !controllerutil.ContainsFinalizer(resource, migrationv1alpha1.FinalizerName) {
		return
	}
	base := client.MergeFrom(resource.DeepCopy())
	controllerutil.RemoveFinalizer(resource, migrationv1alpha1.FinalizerName)
	ExpectWithOffset(1, k8sClient.Patch(ctx, resource, base)).To(Succeed())
	EventuallyWithOffset(1, func() error {
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(resource), &migrationv1alpha1.VmwareCloudFoundationMigration{})
	}).ShouldNot(Succeed())
}

// newSingletonSpecResource builds a minimal valid singleton candidate in the
// given namespace.
func newSingletonSpecResource(namespace string, state migrationv1alpha1.MigrationState) *migrationv1alpha1.VmwareCloudFoundationMigration {
	return &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migrationv1alpha1.SingletonName,
			Namespace: namespace,
		},
		Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
			State: state,
			TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{
				Name:      "target-vcenter-creds",
				Namespace: "default",
			},
			FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{{
				Name:   "target-fd-1",
				Region: "target-region",
				Zone:   "target-zone-1",
				Server: "vcenter-target.example.com",
				Topology: configv1.VSpherePlatformTopology{
					Datacenter:     "TargetDC",
					ComputeCluster: "/TargetDC/host/TargetCluster",
					Datastore:      "/TargetDC/datastore/TargetDatastore",
					Networks:       []string{"VM Network"},
					Template:       "/TargetDC/vm/rhcos-template",
				},
			}},
		},
	}
}

var _ = Describe("Singleton namespace enforcement", func() {
	ctx := context.Background()

	It("ignores a resource named cluster in a non-singleton namespace", func() {
		typeNamespacedName := types.NamespacedName{
			Name:      migrationv1alpha1.SingletonName,
			Namespace: "default",
		}
		resource := newSingletonSpecResource("default", migrationv1alpha1.MigrationStateRunning)
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		DeferCleanup(func() {
			leftover := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			if err := k8sClient.Get(ctx, typeNamespacedName, leftover); err != nil {
				return
			}
			Expect(k8sClient.Delete(ctx, leftover)).To(Succeed())
			releaseMigrationFinalizer(ctx, leftover)
		})

		fakeRecorder := events.NewFakeRecorder(10)
		controllerReconciler := &VmwareCloudFoundationMigrationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: fakeRecorder,
		}

		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())

		Expect(fakeRecorder.Events).To(Receive(SatisfyAll(
			ContainSubstring("Warning"),
			ContainSubstring(migrationv1alpha1.ReasonUnsupportedNamespace),
		)))

		Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		cond := apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionAccepted)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(migrationv1alpha1.ReasonUnsupportedNamespace))

		By("not running any migration work")
		Expect(resource.Status.StartTime).To(BeNil())
		Expect(apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)).To(BeNil())
		Expect(controllerutil.ContainsFinalizer(resource, migrationv1alpha1.FinalizerName)).To(BeFalse())
	})
})

var _ = Describe("Deletion finalizer", func() {
	ctx := context.Background()

	It("retains an incomplete migration and only releases it via the admin annotation", func() {
		typeNamespacedName := types.NamespacedName{
			Name:      migrationv1alpha1.SingletonName,
			Namespace: migrationv1alpha1.SingletonNamespace,
		}
		resource := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStatePending)
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		DeferCleanup(func() {
			leftover := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			if err := k8sClient.Get(ctx, typeNamespacedName, leftover); err != nil {
				return
			}
			Expect(k8sClient.Delete(ctx, leftover)).To(Succeed())
			releaseMigrationFinalizer(ctx, leftover)
		})

		fakeRecorder := events.NewFakeRecorder(10)
		controllerReconciler := &VmwareCloudFoundationMigrationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: fakeRecorder,
		}

		By("adding the record-retention finalizer on the first reconcile")
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(resource, migrationv1alpha1.FinalizerName)).To(BeTrue())

		By("deleting an incomplete migration")
		Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())

		Expect(fakeRecorder.Events).To(Receive(SatisfyAll(
			ContainSubstring("Warning"),
			ContainSubstring(migrationv1alpha1.ReasonDeletionBlocked),
		)))

		By("keeping the record while migration work is outstanding")
		Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		Expect(resource.DeletionTimestamp).NotTo(BeNil())
		Expect(controllerutil.ContainsFinalizer(resource, migrationv1alpha1.FinalizerName)).To(BeTrue())
		cond := apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionAccepted)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(migrationv1alpha1.ReasonDeletionBlocked))

		By("running no migration steps for a deleting resource")
		Expect(resource.Status.StartTime).To(BeNil())
		Expect(apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)).To(BeNil())

		By("holding the record when the annotation has a non-true value")
		Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		resource.Annotations = map[string]string{migrationv1alpha1.AnnotationAllowDeletion: "false"}
		Expect(k8sClient.Update(ctx, resource)).To(Succeed())
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(resource, migrationv1alpha1.FinalizerName)).To(BeTrue())

		By("releasing the record once the administrator allows deletion")
		Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		resource.Annotations = map[string]string{migrationv1alpha1.AnnotationAllowDeletion: "true"}
		Expect(k8sClient.Update(ctx, resource)).To(Succeed())
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			return k8sClient.Get(ctx, typeNamespacedName, &migrationv1alpha1.VmwareCloudFoundationMigration{})
		}).ShouldNot(Succeed())
	})

	It("releases the finalizer when a completed migration is deleted", func() {
		typeNamespacedName := types.NamespacedName{
			Name:      migrationv1alpha1.SingletonName,
			Namespace: migrationv1alpha1.SingletonNamespace,
		}
		resource := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStatePending)
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		DeferCleanup(func() {
			leftover := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			if err := k8sClient.Get(ctx, typeNamespacedName, leftover); err != nil {
				return
			}
			Expect(k8sClient.Delete(ctx, leftover)).To(Succeed())
			releaseMigrationFinalizer(ctx, leftover)
		})

		fakeRecorder := events.NewFakeRecorder(10)
		controllerReconciler := &VmwareCloudFoundationMigrationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: fakeRecorder,
		}

		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())

		By("marking the migration complete")
		Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		completionTime := metav1.Now()
		resource.Status.CompletionTime = &completionTime
		resource.Status.CompletionSpecHash = completionHashForTest(GinkgoT(), resource.Spec)
		apimeta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
			Type: migrationv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: migrationv1alpha1.ReasonCompleted, ObservedGeneration: resource.Generation,
			LastTransitionTime: completionTime,
		})
		Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())

		By("deleting the completed migration removes the record")
		Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		Expect(apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionReady).ObservedGeneration).To(Equal(resource.Generation - 1))
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			return k8sClient.Get(ctx, typeNamespacedName, &migrationv1alpha1.VmwareCloudFoundationMigration{})
		}).ShouldNot(Succeed())
	})

	It("keeps committed workflow progress while reporting deletion as blocked", func() {
		typeNamespacedName := types.NamespacedName{
			Name:      migrationv1alpha1.SingletonName,
			Namespace: migrationv1alpha1.SingletonNamespace,
		}
		resource := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStateRunning)
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		DeferCleanup(func() {
			leftover := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			if err := k8sClient.Get(ctx, typeNamespacedName, leftover); err != nil {
				return
			}
			Expect(k8sClient.Delete(ctx, leftover)).To(Succeed())
			releaseMigrationFinalizer(ctx, leftover)
		})

		fakeRecorder := events.NewFakeRecorder(10)
		controllerReconciler := &VmwareCloudFoundationMigrationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: fakeRecorder,
		}

		By("adding the record-retention finalizer on the first reconcile")
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(resource, migrationv1alpha1.FinalizerName)).To(BeTrue())

		By("committing a workflow condition for the running migration")
		// The object's generation advances between the seed write and the
		// deletion reconcile; capture the generation the condition is committed
		// against so retention is compared to the commit-time value.
		seedGeneration := resource.Generation
		// Truncate to whole seconds: the apiserver persists timestamps at
		// second precision, so the retained value must compare equal exactly.
		startTime := metav1.NewTime(time.Now().Truncate(time.Second))
		resource.Status.StartTime = &startTime
		apimeta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
			Type:               migrationv1alpha1.ConditionInfrastructurePrepared,
			Status:             metav1.ConditionTrue,
			Reason:             migrationv1alpha1.ReasonCompleted,
			Message:            "Preflight validation passed",
			ObservedGeneration: seedGeneration,
			LastTransitionTime: startTime,
		})
		Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())

		By("deleting the running migration while work is outstanding")
		Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		Expect(resource.DeletionTimestamp).NotTo(BeNil())
		cond := apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionAccepted)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(migrationv1alpha1.ReasonDeletionBlocked))
		Expect(cond.ObservedGeneration).To(Equal(resource.Generation))

		By("keeping the committed progress record")
		kept := apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
		Expect(kept).NotTo(BeNil())
		Expect(kept.Status).To(Equal(metav1.ConditionTrue))
		Expect(kept.Reason).To(Equal(migrationv1alpha1.ReasonCompleted))
		Expect(kept.ObservedGeneration).To(Equal(seedGeneration))
		Expect(resource.Status.StartTime).NotTo(BeNil())
		Expect(resource.Status.StartTime.Equal(&startTime)).To(BeTrue())
		Expect(controllerutil.ContainsFinalizer(resource, migrationv1alpha1.FinalizerName)).To(BeTrue())
	})
})

// TestReconcileAddsRecordRetentionFinalizer covers the finalizer-add path
// without envtest, using a fake client.
func TestReconcileAddsRecordRetentionFinalizer(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	migration := newSingletonSpecResource(migrationv1alpha1.SingletonNamespace, migrationv1alpha1.MigrationStatePending)
	fakeClient := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(migration).
		WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
		Build()
	reconciler := &VmwareCloudFoundationMigrationReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(migration)}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	persisted := &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), persisted); err != nil {
		t.Fatalf("getting migration after first reconcile: %v", err)
	}
	if !controllerutil.ContainsFinalizer(persisted, migrationv1alpha1.FinalizerName) {
		t.Fatalf("finalizer %s not added", migrationv1alpha1.FinalizerName)
	}

	// The second reconcile must not duplicate the finalizer and must proceed
	// past the finalizer gate to the state gate (Pending: no workflow work).
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	persisted = &migrationv1alpha1.VmwareCloudFoundationMigration{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(migration), persisted); err != nil {
		t.Fatalf("getting migration after second reconcile: %v", err)
	}
	if got := persisted.Finalizers; len(got) != 1 || got[0] != migrationv1alpha1.FinalizerName {
		t.Fatalf("finalizers = %v, want exactly [%s]", got, migrationv1alpha1.FinalizerName)
	}
	if persisted.Status.StartTime != nil {
		t.Fatalf("startTime = %v, want nil for a Pending migration", persisted.Status.StartTime)
	}
}

// TestUpdateStatusSkipsStaleMappingPublication covers the generation/identity
// race: a spec edit (or delete/recreate) that lands between this reconcile's
// fetch and its status write must not receive the mapping validated for the
// older generation, which would lock the newer valid spec out via
// ensureMappingImmutable.
func TestUpdateStatusSkipsStaleMappingPublication(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := migrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}

	mappings := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-1"}}
	previous := []migrationv1alpha1.FailureDomainMapping{{Source: "source-fd-1", Destination: "target-fd-0"}}
	// inFlight is the copy this reconcile fetched and validated: generation 1.
	inFlight := &migrationv1alpha1.VmwareCloudFoundationMigration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       migrationv1alpha1.SingletonName,
			Namespace:  migrationv1alpha1.SingletonNamespace,
			Generation: 1,
		},
	}
	baseStatus := inFlight.Status.DeepCopy()

	tests := []struct {
		name           string
		latestGen      int64
		latestMappings []migrationv1alpha1.FailureDomainMapping
		latestStarted  bool
		want           []migrationv1alpha1.FailureDomainMapping
	}{
		{
			name:      "same generation publishes mapping",
			latestGen: 1,
			want:      mappings,
		},
		{
			name:      "newer generation does not receive stale mapping",
			latestGen: 2,
		},
		{
			name:           "replanning before start replaces recorded mapping",
			latestGen:      1,
			latestMappings: previous,
			want:           mappings,
		},
		{
			name:           "started migration keeps recorded mapping",
			latestGen:      1,
			latestMappings: previous,
			latestStarted:  true,
			want:           previous,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latest := &migrationv1alpha1.VmwareCloudFoundationMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:       migrationv1alpha1.SingletonName,
					Namespace:  migrationv1alpha1.SingletonNamespace,
					Generation: tt.latestGen,
				},
				Status: migrationv1alpha1.VmwareCloudFoundationMigrationStatus{
					FailureDomainMappings: tt.latestMappings,
				},
			}
			if tt.latestStarted {
				started := metav1.Now()
				latest.Status.StartTime = &started
			}
			fakeClient := clientfake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(latest).
				WithStatusSubresource(&migrationv1alpha1.VmwareCloudFoundationMigration{}).
				Build()
			r := &VmwareCloudFoundationMigrationReconciler{
				Client:   fakeClient,
				Scheme:   scheme,
				Recorder: events.NewFakeRecorder(10),
			}

			// Simulate preflight resolving the mapping on the in-flight copy.
			inFlight.Status.FailureDomainMappings = mappings
			if err := r.updateStatus(ctx, inFlight, *baseStatus); err != nil {
				t.Fatalf("updateStatus: %v", err)
			}

			persisted := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(latest), persisted); err != nil {
				t.Fatalf("getting persisted migration: %v", err)
			}
			if got := persisted.Status.FailureDomainMappings; !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("persisted mappings = %+v, want %+v", got, tt.want)
			}
		})
	}
}
