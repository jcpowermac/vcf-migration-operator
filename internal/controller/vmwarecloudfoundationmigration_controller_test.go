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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

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
		{name: "leading invalid", input: "_zone", want: "zone"},
		{name: "trailing invalid", input: "zone_", want: "zone"},
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

func TestConditionToPhase(t *testing.T) {
	tests := []struct {
		condition string
		want      migrationv1alpha1.MigrationPhase
	}{
		{
			condition: migrationv1alpha1.ConditionInfrastructurePrepared,
			want:      migrationv1alpha1.PhaseInfrastructurePrepared,
		},
		{
			condition: migrationv1alpha1.ConditionDestinationInitialized,
			want:      migrationv1alpha1.PhaseDestinationInitialized,
		},
		{
			condition: migrationv1alpha1.ConditionMultiSiteConfigured,
			want:      migrationv1alpha1.PhaseMultiSiteConfigured,
		},
		{
			condition: migrationv1alpha1.ConditionWorkloadMigrated,
			want:      migrationv1alpha1.PhaseWorkloadMigrated,
		},
		{
			condition: migrationv1alpha1.ConditionSourceCleaned,
			want:      migrationv1alpha1.PhaseSourceCleaned,
		},
		{
			condition: migrationv1alpha1.ConditionReady,
			want:      migrationv1alpha1.PhaseSourceCleaned,
		},
		{
			condition: "CustomCondition",
			want:      migrationv1alpha1.MigrationPhase("CustomCondition"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.condition, func(t *testing.T) {
			got := conditionToPhase(tt.condition)
			if got != tt.want {
				t.Errorf("conditionToPhase(%q) = %q, want %q", tt.condition, got, tt.want)
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
			Namespace: "default", // TODO(user):Modify as needed
		}
		vmwarecloudfoundationmigration := &migrationv1alpha1.VmwareCloudFoundationMigration{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind VmwareCloudFoundationMigration")
			err := k8sClient.Get(ctx, typeNamespacedName, vmwarecloudfoundationmigration)
			if err != nil && errors.IsNotFound(err) {
				resource := &migrationv1alpha1.VmwareCloudFoundationMigration{
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
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance VmwareCloudFoundationMigration")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource and set phase to Pending", func() {
			By("Reconciling the created resource")
			controllerReconciler := &VmwareCloudFoundationMigrationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionReady)).To(BeNil())
			Expect(resource.Status.StartTime).To(BeNil())
			Expect(resource.Status.Phase).To(Equal(migrationv1alpha1.PhasePending))
			Expect(resource.Status.LastUpdateTime).NotTo(BeNil())
		})

		It("should set status phase to Paused when in Paused state", func() {
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

			updated := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(migrationv1alpha1.PhasePaused))
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
			fakeRecorder := record.NewFakeRecorder(10)
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
			Expect(resource.Status.Phase).To(Equal(migrationv1alpha1.PhaseFailed))
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
			Namespace: "default",
		}

		BeforeEach(func() {
			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
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
		})

		It("should set the Ready condition to False with reason Paused and transition to Progressing on resume", func() {
			fakeRecorder := record.NewFakeRecorder(10)
			controllerReconciler := &VmwareCloudFoundationMigrationReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				KubeClient: fakekube.NewClientset(),
				Recorder:   fakeRecorder,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
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

	It("does not let a stale-generation reconcile overwrite current-generation Phase and Progress", func() {
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

		// A current-generation reconcile commits Phase and Progress.
		migrationCurrent := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migrationCurrent)).To(Succeed())
		baseCurrent := *migrationCurrent.Status.DeepCopy()
		migrationCurrent.Status.Phase = migrationv1alpha1.PhaseWorkloadMigrated
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
		// its own Phase and Progress.
		migrationStale.Status.Phase = migrationv1alpha1.PhaseFailed
		migrationStale.Status.Progress = &migrationv1alpha1.MigrationProgress{
			Workers:      &migrationv1alpha1.WorkerMigrationProgress{SourceMachinesRemaining: 1},
			ControlPlane: &migrationv1alpha1.ControlPlaneProgress{Replicas: 1},
		}
		Expect(reconciler.updateStatus(ctx, migrationStale, baseStale)).To(Succeed())

		final := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, final)).To(Succeed())

		Expect(final.Status.Phase).To(Equal(migrationv1alpha1.PhaseWorkloadMigrated),
			"a stale-generation update must not overwrite a current-generation phase")
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

	It("persists Phase, Progress, and updates LastUpdateTime", func() {
		resource := newStatusTestResource()
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())

		reconciler := &VmwareCloudFoundationMigrationReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		migration := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, migration)).To(Succeed())
		base := *migration.Status.DeepCopy()

		migration.Status.Phase = migrationv1alpha1.PhaseWorkloadMigrated
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

		Expect(final.Status.Phase).To(Equal(migrationv1alpha1.PhaseWorkloadMigrated))
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

	It("persists CompletionTime and PhaseCompleted when migration is finished", func() {
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
		migration.Status.Phase = migrationv1alpha1.PhaseCompleted
		migration.Status.CompletionTime = &now
		reconciler.setCondition(migration, migrationv1alpha1.ConditionReady, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "Migration complete")

		Expect(reconciler.updateStatus(ctx, migration, base)).To(Succeed())

		final := &migrationv1alpha1.VmwareCloudFoundationMigration{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, final)).To(Succeed())

		Expect(final.Status.Phase).To(Equal(migrationv1alpha1.PhaseCompleted))
		Expect(final.Status.CompletionTime).NotTo(BeNil())
		Expect(final.Status.LastUpdateTime).NotTo(BeNil())
		readyCond := apimeta.FindStatusCondition(final.Status.Conditions, migrationv1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
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
