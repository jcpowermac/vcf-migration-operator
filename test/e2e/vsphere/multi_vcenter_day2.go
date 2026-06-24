package vsphere

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	configv1 "github.com/openshift/api/config/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
	"github.com/openshift/vcf-migration-operator/internal/openshift"
)

var _ = Describe(
	"[sig-cluster-lifecycle][OCPFeatureGate:VSphereMultiVCenterDay2]"+
		"[platform:vsphere] Adding a second vCenter via VmwareCloudFoundationMigration",
	Label("Conformance"), func() {
		ctx := context.Background()

		var (
			kubeClient   kubernetes.Interface
			configClient configclient.Interface
			crClient     client.Client

			infra     *configv1.Infrastructure
			migration *migrationv1alpha1.VmwareCloudFoundationMigration

			initialVCenterCount int
			targetServers       map[string]bool
		)

		BeforeEach(func() {
			cfg, err := config.GetConfig()
			Expect(err).NotTo(HaveOccurred(), "failed to get kubeconfig")

			kubeClient, err = kubernetes.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred(), "failed to create kubernetes client")

			configClient, err = configclient.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred(), "failed to create config client")

			scheme := runtime.NewScheme()
			err = migrationv1alpha1.AddToScheme(scheme)
			Expect(err).NotTo(HaveOccurred(), "failed to add migration scheme")
			err = configv1.Install(scheme)
			Expect(err).NotTo(HaveOccurred(), "failed to install configv1 scheme")

			crClient, err = client.New(cfg, client.Options{Scheme: scheme})
			Expect(err).NotTo(HaveOccurred(), "failed to create controller-runtime client")

			By("Fetching Infrastructure CR")
			infra, err = configClient.ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "failed to get Infrastructure CR")

			if infra.Status.PlatformStatus == nil || infra.Status.PlatformStatus.Type != configv1.VSpherePlatformType {
				Skip("skipping: cluster is not vSphere platform")
			}

			By("Checking VSphereMultiVCenterDay2 feature gate")
			support, err := openshift.GetVSphereMultiVCenterSupport(ctx, configClient)
			Expect(err).NotTo(HaveOccurred(), "failed to check feature gate support")
			if !support.FeatureGateEnabled {
				Skip("skipping: VSphereMultiVCenterDay2 feature gate is not enabled")
			}

			By("Finding VmwareCloudFoundationMigration CR")
			migrationList := &migrationv1alpha1.VmwareCloudFoundationMigrationList{}
			err = crClient.List(ctx, migrationList)
			Expect(err).NotTo(HaveOccurred(), "failed to list VmwareCloudFoundationMigration CRs")
			if len(migrationList.Items) == 0 {
				Skip("skipping: no VmwareCloudFoundationMigration CR found in cluster")
			}
			migration = &migrationList.Items[0]

			By("Recording initial state")
			Expect(infra.Spec.PlatformSpec.VSphere).NotTo(BeNil(), "infrastructure vSphere spec is nil")
			initialVCenterCount = len(infra.Spec.PlatformSpec.VSphere.VCenters)

			targetServers = make(map[string]bool)
			for i := range migration.Spec.FailureDomains {
				targetServers[migration.Spec.FailureDomains[i].Server] = true
			}
			Expect(targetServers).NotTo(BeEmpty(), "migration CR has no failure domains")
		})

		It("should configure the cluster for multi-vCenter operation", func() {
			By("Capturing baseline cloud configs")
			baselineCM, err := kubeClient.CoreV1().ConfigMaps("openshift-config").Get(
				ctx, "cloud-provider-config", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "failed to get baseline cloud-provider-config")
			baselineServers, err := parseVCenterServersFromConfig(baselineCM, "config")
			Expect(err).NotTo(HaveOccurred(), "failed to parse baseline cloud-provider-config")
			GinkgoWriter.Printf("Baseline vCenters in cloud-provider-config: %v\n", baselineServers)
			GinkgoWriter.Printf("Baseline vCenters in Infrastructure CR: %d\n", initialVCenterCount)

			By("Setting Migration CR state to Running")
			migration.Spec.State = migrationv1alpha1.MigrationStateRunning
			err = crClient.Update(ctx, migration)
			Expect(err).NotTo(HaveOccurred(), "failed to set migration state to Running")

			By("Waiting for MultiSiteConfigured condition to be True")
			Eventually(func(g Gomega) {
				updated := &migrationv1alpha1.VmwareCloudFoundationMigration{}
				err := crClient.Get(ctx, client.ObjectKeyFromObject(migration), updated)
				g.Expect(err).NotTo(HaveOccurred())

				var found bool
				for _, cond := range updated.Status.Conditions {
					if cond.Type == migrationv1alpha1.ConditionMultiSiteConfigured {
						found = true
						GinkgoWriter.Printf(
							"MultiSiteConfigured: status=%s reason=%s message=%s\n",
							cond.Status, cond.Reason, cond.Message)
						g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
							fmt.Sprintf("MultiSiteConfigured not True: reason=%s message=%s", cond.Reason, cond.Message))
						break
					}
				}
				g.Expect(found).To(BeTrue(), "MultiSiteConfigured condition not found on migration CR")
			}, 10*time.Minute, 15*time.Second).Should(Succeed())

			By("Verifying Infrastructure CR has multiple vCenters")
			infra, err = configClient.ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "failed to get updated Infrastructure CR")
			Expect(len(infra.Spec.PlatformSpec.VSphere.VCenters)).To(BeNumerically(">", initialVCenterCount),
				"Infrastructure CR should have more vCenters after multi-site configuration")

			infraServers := make(map[string]bool)
			for _, vc := range infra.Spec.PlatformSpec.VSphere.VCenters {
				infraServers[vc.Server] = true
			}
			for server := range targetServers {
				Expect(infraServers).To(HaveKey(server),
					fmt.Sprintf("target vCenter %s not found in Infrastructure CR", server))
			}
			GinkgoWriter.Printf("Infrastructure CR vCenters after update: %v\n", infraServers)

			infraFDs := make(map[string]bool)
			for _, fd := range infra.Spec.PlatformSpec.VSphere.FailureDomains {
				infraFDs[fd.Name] = true
			}
			for i := range migration.Spec.FailureDomains {
				Expect(infraFDs).To(HaveKey(migration.Spec.FailureDomains[i].Name),
					fmt.Sprintf("failure domain %s not found in Infrastructure CR", migration.Spec.FailureDomains[i].Name))
			}

			By("Verifying cloud-provider-config in openshift-config")
			cm, err := kubeClient.CoreV1().ConfigMaps("openshift-config").Get(
				ctx, "cloud-provider-config", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "failed to get updated cloud-provider-config")
			configServers, err := parseVCenterServersFromConfig(cm, "config")
			Expect(err).NotTo(HaveOccurred(), "failed to parse updated cloud-provider-config")
			for server := range targetServers {
				Expect(configServers).To(ContainElement(server),
					fmt.Sprintf("target vCenter %s not found in cloud-provider-config", server))
			}
			GinkgoWriter.Printf("cloud-provider-config vCenters: %v\n", configServers)

			By("Verifying kube-cloud-config in openshift-config-managed reflects both vCenters")
			Eventually(func(g Gomega) {
				kcCM, err := getKubeCloudConfig(ctx, kubeClient)
				g.Expect(err).NotTo(HaveOccurred())

				// kube-cloud-config uses "cloud.conf" key
				servers, err := parseVCenterServersFromConfig(kcCM, "cloud.conf")
				g.Expect(err).NotTo(HaveOccurred())

				for server := range targetServers {
					g.Expect(servers).To(ContainElement(server),
						fmt.Sprintf("target vCenter %s not in kube-cloud-config", server))
				}
				GinkgoWriter.Printf("kube-cloud-config vCenters: %v\n", servers)
			}, 5*time.Minute, 15*time.Second).Should(Succeed())

			By("Verifying cloud-conf ConfigMap in openshift-cloud-controller-manager")
			Eventually(func(g Gomega) {
				cms, err := getCloudConfConfigMaps(ctx, kubeClient)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(cms.Items).NotTo(BeEmpty(), "no ConfigMaps found in openshift-cloud-controller-manager")

				var foundConfig bool
				for i := range cms.Items {
					cm := &cms.Items[i]
					for key := range cm.Data {
						servers, err := parseVCenterServersFromConfig(cm, key)
						if err != nil {
							continue
						}
						if len(servers) == 0 {
							continue
						}
						foundConfig = true
						for server := range targetServers {
							g.Expect(servers).To(ContainElement(server),
								fmt.Sprintf("target vCenter %s not in %s/%s key %s", server, cm.Namespace, cm.Name, key))
						}
						GinkgoWriter.Printf("cloud-conf %s/%s key=%s vCenters: %v\n", cm.Namespace, cm.Name, key, servers)
					}
				}
				g.Expect(foundConfig).To(BeTrue(),
					"no cloud config found in openshift-cloud-controller-manager")
			}, 5*time.Minute, 15*time.Second).Should(Succeed())

			By("Verifying CCCMO operator pods are healthy")
			Eventually(func() error {
				return checkPodsHealthy(ctx, kubeClient, cccmoOperatorNamespace)
			}, 3*time.Minute, 15*time.Second).Should(Succeed())

			By("Verifying CCM pods are healthy")
			Eventually(func() error {
				return checkPodsHealthy(ctx, kubeClient, ccmNamespace)
			}, 3*time.Minute, 15*time.Second).Should(Succeed())

			GinkgoWriter.Println("Multi-vCenter Day 2 configuration verified successfully")
		})
	})
