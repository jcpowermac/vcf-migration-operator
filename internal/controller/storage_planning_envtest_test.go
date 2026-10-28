package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/storageplan"
)

var _ = Describe("Storage planning against the API server", func() {
	It("persists blocker changes and revalidates Planning to Running before disruption", func() {
		r, m := planningFixture(GinkgoT())
		targetFD := m.Spec.FailureDomains[0].Name
		r.Client = k8sClient
		m.UID = ""
		m.ResourceVersion = ""
		m.Generation = 0
		m.Status = migrationv1alpha1.VmwareCloudFoundationMigrationStatus{}
		Expect(k8sClient.Create(ctx, m)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), m)).To(Succeed())
			Expect(k8sClient.Delete(ctx, m)).To(Succeed())
			releaseMigrationFinalizer(ctx, m)
		})
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}}
		if err := k8sClient.Create(ctx, namespace); err != nil {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}
		r.setCondition(m, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "preflight validated")
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
		result, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(planningResync))
		Expect(k8sClient.Get(ctx, req.NamespacedName, m)).To(Succeed())
		Expect(m.Status.StartTime).To(BeNil())
		Expect(apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionReady).Reason).To(Equal(migrationv1alpha1.ReasonPlanComplete))
		Expect(apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionDestinationInitialized)).To(BeNil())
		swm := &migrationv1alpha1.StorageWorkloadMigration{}
		swmKey := client.ObjectKey{Namespace: "app", Name: storagePlanName(m)}
		Expect(k8sClient.Get(ctx, swmKey, swm)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, swm)).To(Succeed()) })
		Expect(swm.Status.Phase).To(Equal(migrationv1alpha1.StorageWorkloadMigrationPhasePlanning))
		Expect(swm.Status.Volumes).To(HaveLen(1))
		pvc, err := r.KubeClient.CoreV1().PersistentVolumeClaims("app").Get(ctx, "data", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		pvc.Annotations = map[string]string{storageplan.TargetFailureDomainAnnotation: "missing"}
		_, err = r.KubeClient.CoreV1().PersistentVolumeClaims("app").Update(ctx, pvc, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, req.NamespacedName, m)).To(Succeed())
		Expect(apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionStoragePlanned).Status).To(Equal(metav1.ConditionFalse))
		Expect(k8sClient.Get(ctx, swmKey, swm)).To(Succeed())
		Expect(swm.Status.Volumes[0].Blockers[0].Reason).To(Equal("TargetFailureDomainInvalid"))
		pvc.Annotations[storageplan.TargetFailureDomainAnnotation] = targetFD
		_, err = r.KubeClient.CoreV1().PersistentVolumeClaims("app").Update(ctx, pvc, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, req.NamespacedName, m)).To(Succeed())
		Expect(apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionStoragePlanned).Status).To(Equal(metav1.ConditionTrue))
		oldGeneration := m.Generation
		m.Spec.State = migrationv1alpha1.MigrationStateRunning
		Expect(k8sClient.Update(ctx, m)).To(Succeed())
		Expect(m.Generation).To(BeNumerically(">", oldGeneration))
		_, err = r.Reconcile(ctx, req)
		Expect(err).To(MatchError(ContainSubstring("target credentials secret")))
		Expect(k8sClient.Get(ctx, req.NamespacedName, m)).To(Succeed())
		preflight := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionInfrastructurePrepared)
		Expect(preflight.Status).To(Equal(metav1.ConditionFalse))
		Expect(preflight.ObservedGeneration).To(Equal(m.Generation))
		Expect(m.Status.StartTime).To(BeNil())
		// Once infrastructure revalidation passes, storage must also re-run for the new generation.
		r.setCondition(m, migrationv1alpha1.ConditionInfrastructurePrepared, metav1.ConditionTrue, migrationv1alpha1.ReasonCompleted, "revalidated")
		Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())
		pv, err := r.KubeClient.CoreV1().PersistentVolumes().Get(ctx, "pv", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		pv.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
		_, err = r.KubeClient.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, req.NamespacedName, m)).To(Succeed())
		storage := apimeta.FindStatusCondition(m.Status.Conditions, migrationv1alpha1.ConditionStoragePlanned)
		Expect(storage.Status).To(Equal(metav1.ConditionFalse))
		Expect(storage.ObservedGeneration).To(Equal(m.Generation))
		Expect(m.Status.StartTime).To(BeNil())
	})
})
