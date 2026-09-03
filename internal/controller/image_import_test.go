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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

var _ = Describe("DestinationImageImported Condition", func() {
	const resourceName = "image-import-test"

	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{
		Name:      resourceName,
		Namespace: "default",
	}

	Context("when spec.image is nil", func() {
		BeforeEach(func() {
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
							Name:   "test-fd-1",
							Region: "test-region",
							Zone:   "test-zone",
							Server: "vcenter.example.com",
							Topology: configv1.VSpherePlatformTopology{
								Datacenter:     "DC1",
								ComputeCluster: "/DC1/host/Cluster1",
								Datastore:      "/DC1/datastore/DS1",
								Networks:       []string{"VM Network"},
								ResourcePool:   "/DC1/host/Cluster1/Resources",
								Template:       "/DC1/vm/rhcos-template",
								Folder:         "/DC1/vm/test-folder",
							},
						},
					},
					// Image is nil — should be immediate True.
				},
			}
			err := k8sClient.Get(ctx, typeNamespacedName, &migrationv1alpha1.VmwareCloudFoundationMigration{})
			if err != nil {
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should immediately set condition True", func() {
			migration := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, migration)).To(Succeed())

			reconciler := &VmwareCloudFoundationMigrationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := reconciler.ensureDestinationImageImported(ctx, migration)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// Verify condition is True.
			condition := findCondition(migration, migrationv1alpha1.ConditionDestinationImageImported)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(migrationv1alpha1.ReasonCompleted))
			Expect(condition.Message).To(ContainSubstring("not requested"))
		})
	})

	Context("when spec.image is set with OVA URL", func() {
		BeforeEach(func() {
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
							Name:   "test-fd-1",
							Region: "test-region",
							Zone:   "test-zone",
							Server: "vcenter.example.com",
							Topology: configv1.VSpherePlatformTopology{
								Datacenter:     "DC1",
								ComputeCluster: "/DC1/host/Cluster1",
								Datastore:      "/DC1/datastore/DS1",
								Networks:       []string{"VM Network"},
								ResourcePool:   "/DC1/host/Cluster1/Resources",
								Folder:         "/DC1/vm/test-folder",
							},
						},
					},
					Image: &migrationv1alpha1.ImageSpec{
						OVAUrl: "https://example.com/rhcos.ova",
					},
				},
			}
			err := k8sClient.Get(ctx, typeNamespacedName, &migrationv1alpha1.VmwareCloudFoundationMigration{})
			if err != nil {
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})

		It("should initialize status.image and fail gracefully without ConfigClient", func() {
			migration := &migrationv1alpha1.VmwareCloudFoundationMigration{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, migration)).To(Succeed())

			// Reconciler without ConfigClient — handler should return an error,
			// not panic.
			reconciler := &VmwareCloudFoundationMigrationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.ensureDestinationImageImported(ctx, migration)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ConfigClient"))

			// Verify status.image was initialized before the error.
			Expect(migration.Status.Image).NotTo(BeNil())
		})
	})

	Context("when topology.template is pre-set with spec.image", func() {
		It("should fail gracefully without ConfigClient when all FDs have templates", func() {
			migration := &migrationv1alpha1.VmwareCloudFoundationMigration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "skip-test",
					Namespace: "default",
				},
				Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
					State: migrationv1alpha1.MigrationStatePending,
					TargetVCenterCredentialsSecret: migrationv1alpha1.SecretReference{
						Name: "creds",
					},
					FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
						{
							Name:   "fd-with-template",
							Region: "r",
							Zone:   "z",
							Server: "vc.example.com",
							Topology: configv1.VSpherePlatformTopology{
								Datacenter:     "DC",
								ComputeCluster: "/DC/host/Cluster",
								Datastore:      "/DC/datastore/DS",
								Networks:       []string{"Net"},
								Template:       "/DC/vm/existing-template",
							},
						},
					},
					Image: &migrationv1alpha1.ImageSpec{},
				},
			}

			// Manually simulate having already resolved the URL and downloaded.
			migration.Status.Image = &migrationv1alpha1.ImageStatus{
				ResolvedOVAUrl:    "https://example.com/rhcos.ova",
				DownloadComplete:  true,
				ImportedTemplates: make(map[string]string),
			}

			reconciler := &VmwareCloudFoundationMigrationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Without ConfigClient, the handler will fail at GetInfrastructureID.
			// But it should not panic.
			_, err := reconciler.ensureDestinationImageImported(ctx, migration)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ConfigClient"))
		})
	})

	Context("conditionOrder includes DestinationImageImported", func() {
		It("should be between DestinationInitialized and MultiSiteConfigured", func() {
			initIdx := -1
			imageIdx := -1
			multiIdx := -1
			for i, c := range conditionOrder {
				switch c {
				case migrationv1alpha1.ConditionDestinationInitialized:
					initIdx = i
				case migrationv1alpha1.ConditionDestinationImageImported:
					imageIdx = i
				case migrationv1alpha1.ConditionMultiSiteConfigured:
					multiIdx = i
				}
			}
			Expect(initIdx).To(BeNumerically(">=", 0), "DestinationInitialized not found in conditionOrder")
			Expect(imageIdx).To(BeNumerically(">=", 0), "DestinationImageImported not found in conditionOrder")
			Expect(multiIdx).To(BeNumerically(">=", 0), "MultiSiteConfigured not found in conditionOrder")
			Expect(imageIdx).To(BeNumerically(">", initIdx), "DestinationImageImported should come after DestinationInitialized")
			Expect(imageIdx).To(BeNumerically("<", multiIdx), "DestinationImageImported should come before MultiSiteConfigured")
		})
	})
})

// findCondition returns the condition with the given type from the migration's
// status conditions, or nil if not found.
func findCondition(migration *migrationv1alpha1.VmwareCloudFoundationMigration, condType string) *metav1.Condition {
	for i := range migration.Status.Conditions {
		if migration.Status.Conditions[i].Type == condType {
			return &migration.Status.Conditions[i]
		}
	}
	return nil
}
