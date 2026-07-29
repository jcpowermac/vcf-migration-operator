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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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

			// The migration never started (State stayed Pending), so
			// deletion is not blocked; one more reconcile removes the
			// protection finalizer and lets the object actually go away.
			controllerReconciler := &VmwareCloudFoundationMigrationReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &migrationv1alpha1.VmwareCloudFoundationMigration{}))).To(BeTrue())
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
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
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

			// No workflow conditions should have been set since the resource was never processed.
			Expect(apimeta.FindStatusCondition(resource.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)).To(BeNil())
			Expect(resource.Status.StartTime).To(BeNil())
		})
	})

	Context("When the singleton migration is deleted", func() {
		const resourceName = migrationv1alpha1.SingletonName

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		newReconciler := func() *VmwareCloudFoundationMigrationReconciler {
			return &VmwareCloudFoundationMigrationReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}
		}

		newResource := func() *migrationv1alpha1.VmwareCloudFoundationMigration {
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
			// Force any still-blocked deletion through so a failed assertion
			// earlier in the test doesn't hang the suite. Re-fetches on every
			// attempt so a resourceVersion conflict just causes a retry.
			reconciler := newReconciler()
			Eventually(func() bool {
				resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
				if err := k8sClient.Get(ctx, typeNamespacedName, resource); err != nil {
					return errors.IsNotFound(err)
				}

				if resource.Annotations[migrationv1alpha1.ForceDeleteAnnotation] != "true" {
					if resource.Annotations == nil {
						resource.Annotations = map[string]string{}
					}
					resource.Annotations[migrationv1alpha1.ForceDeleteAnnotation] = "true"
					if err := k8sClient.Update(ctx, resource); err != nil {
						return false
					}
				}

				if resource.DeletionTimestamp.IsZero() {
					if err := k8sClient.Delete(ctx, resource); err != nil {
						return false
					}
					if err := k8sClient.Get(ctx, typeNamespacedName, resource); err != nil {
						return errors.IsNotFound(err)
					}
				}

				if _, _, err := reconciler.handleFinalizer(ctx, resource); err != nil {
					return false
				}
				return errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &migrationv1alpha1.VmwareCloudFoundationMigration{}))
			}).WithTimeout(10 * time.Second).WithPolling(500 * time.Millisecond).Should(BeTrue())
		})

		It("adds the protection finalizer on first reconcile", func() {
			Expect(k8sClient.Create(ctx, newResource())).To(Succeed())
			_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(resource, migrationv1alpha1.Finalizer)).To(BeTrue())
		})

		It("deletes immediately when the migration never started", func() {
			Expect(k8sClient.Create(ctx, newResource())).To(Succeed())
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &migrationv1alpha1.VmwareCloudFoundationMigration{}))).To(BeTrue())
		})

		Context("deletion blocking", func() {
			var resource *migrationv1alpha1.VmwareCloudFoundationMigration
			var reconciler *VmwareCloudFoundationMigrationReconciler

			BeforeEach(func() {
				// Exercised directly against handleFinalizer (rather than the
				// full Reconcile) for the deletion steps: once deletion is
				// blocked, Reconcile deliberately falls through to keep driving
				// the migration's state machine, which needs the real vSphere/
				// Kubernetes clients this test doesn't wire up.
				resource = newResource()
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
				reconciler = newReconciler()
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				now := metav1.Now()
				resource.Status.StartTime = &now
				Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())

				Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

				Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			})

			It("blocks deletion while running and not yet Ready", func() {
				fakeRecorder := record.NewFakeRecorder(10)
				reconciler.Recorder = fakeRecorder
				result, done, err := reconciler.handleFinalizer(ctx, resource)
				Expect(err).NotTo(HaveOccurred())
				Expect(done).To(BeFalse())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))

				Expect(fakeRecorder.Events).To(Receive(SatisfyAll(
					ContainSubstring("Warning"),
					ContainSubstring(reasonDeletionBlocked),
				)))

				blocked := &migrationv1alpha1.VmwareCloudFoundationMigration{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, blocked)).To(Succeed())
				Expect(controllerutil.ContainsFinalizer(blocked, migrationv1alpha1.Finalizer)).To(BeTrue())
				Expect(blocked.DeletionTimestamp.IsZero()).To(BeFalse())

				cond := apimeta.FindStatusCondition(blocked.Status.Conditions, migrationv1alpha1.ConditionReady)
				Expect(cond).NotTo(BeNil())
				Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				Expect(cond.Reason).To(Equal(reasonDeletionBlocked))
			})

			It("does not emit a duplicate warning event on a second pass", func() {
				// First call to establish the blocked condition.
				fakeRecorder1 := record.NewFakeRecorder(10)
				reconciler.Recorder = fakeRecorder1
				_, _, err := reconciler.handleFinalizer(ctx, resource)
				Expect(err).NotTo(HaveOccurred())

				// A second call while still blocked must NOT emit another event.
				fakeRecorder2 := record.NewFakeRecorder(10)
				reconciler.Recorder = fakeRecorder2
				Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				_, done, err := reconciler.handleFinalizer(ctx, resource)
				Expect(err).NotTo(HaveOccurred())
				Expect(done).To(BeFalse())
				Expect(fakeRecorder2.Events).ToNot(Receive())
			})

			It("unblocks on force-delete annotation", func() {
				// First call to establish the blocked condition.
				fakeRecorder := record.NewFakeRecorder(10)
				reconciler.Recorder = fakeRecorder
				_, _, err := reconciler.handleFinalizer(ctx, resource)
				Expect(err).NotTo(HaveOccurred())

				// Force it through.
				Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				resource.Annotations = map[string]string{migrationv1alpha1.ForceDeleteAnnotation: "true"}
				Expect(k8sClient.Update(ctx, resource)).To(Succeed())

				Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
				_, done, err := reconciler.handleFinalizer(ctx, resource)
				Expect(err).NotTo(HaveOccurred())
				Expect(done).To(BeTrue())
				Expect(errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &migrationv1alpha1.VmwareCloudFoundationMigration{}))).To(BeTrue())
			})
		})

		It("deletes immediately once the migration has reached Ready", func() {
			resource := newResource()
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			reconciler := newReconciler()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			now := metav1.Now()
			resource.Status.StartTime = &now
			apimeta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
				Type:    migrationv1alpha1.ConditionReady,
				Status:  metav1.ConditionTrue,
				Reason:  migrationv1alpha1.ReasonCompleted,
				Message: "migration complete",
			})
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())

			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &migrationv1alpha1.VmwareCloudFoundationMigration{}))).To(BeTrue())
		})
	})
})
