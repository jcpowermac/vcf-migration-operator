// Package storageplan inventories source volumes and resolves non-disruptive migration plans.
package storageplan

import (
	"fmt"
	"sort"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

const (
	// Driver is the vSphere CSI driver name.
	Driver = "csi.vsphere.vmware.com"
	// TargetFailureDomainAnnotation overrides the destination of one PVC.
	TargetFailureDomainAnnotation = "migration.openshift.io/target-failure-domain"
)

// Input is a consistent-enough inventory; binding identities are checked before use.
type Input struct {
	Migration          *migrationv1alpha1.VmwareCloudFoundationMigration
	Infrastructure     *configv1.Infrastructure
	PVs                []corev1.PersistentVolume
	PVCs               []corev1.PersistentVolumeClaim
	Pods               []corev1.Pod
	StorageClasses     []storagev1.StorageClass
	TopologyCategories []string
}

// Namespace contains the volume inventory for one workload namespace.
type Namespace struct {
	Volumes   []migrationv1alpha1.StorageWorkloadMigrationVolume
	Consumers []migrationv1alpha1.StorageWorkloadMigrationConsumer
}

// Plan contains namespace inventories and cluster-wide blockers.
type Plan struct {
	Namespaces map[string]*Namespace
	Blockers   []migrationv1alpha1.StorageWorkloadMigrationBlocker
}

// AddBlocker adds one message per reason, matching the API's list-map semantics.
func AddBlocker(volume *migrationv1alpha1.StorageWorkloadMigrationVolume, reason, message string) {
	for _, b := range volume.Blockers {
		if b.Reason == reason {
			return
		}
	}
	volume.Blockers = append(volume.Blockers, blocker(reason, message))
}

func blocker(reason, message string) migrationv1alpha1.StorageWorkloadMigrationBlocker {
	// API messages have a 1024-character limit; inventory names are UTF-8 safe.
	runes := []rune(message)
	if len(runes) > 1024 {
		message = string(runes[:1021]) + "..."
	}
	return migrationv1alpha1.StorageWorkloadMigrationBlocker{Reason: reason, Message: message}
}

// Build computes volume blockers, target placement, and Pod topology compatibility.
func Build(in Input) *Plan {
	plan := &Plan{Namespaces: map[string]*Namespace{}}
	pvcs := map[string]*corev1.PersistentVolumeClaim{}
	for i := range in.PVCs {
		p := &in.PVCs[i]
		pvcs[p.Namespace+"/"+p.Name] = p
	}
	classes := map[string]*storagev1.StorageClass{}
	for i := range in.StorageClasses {
		sc := &in.StorageClasses[i]
		classes[sc.Name] = sc
	}

	var sources []configv1.VSpherePlatformFailureDomainSpec
	if in.Infrastructure.Spec.PlatformSpec.VSphere != nil {
		sources = in.Infrastructure.Spec.PlatformSpec.VSphere.FailureDomains
	}

	for _, target := range in.Migration.Spec.FailureDomains {
		for _, source := range sources {
			if target.Region == source.Region && target.Zone == source.Zone {
				plan.Blockers = append(plan.Blockers, blocker("TargetTopologyOverlapsSource", fmt.Sprintf("target failure domain %s shares region and zone with source %s", target.Name, source.Name)))
			}
		}
	}
	if len(in.Migration.Spec.FailureDomains) == 1 && len(in.TopologyCategories) > 0 && !standardCategories(in.TopologyCategories) {
		plan.Blockers = append(plan.Blockers, blocker("TopologyCategoriesConflict", "use topology categories openshift-zone and openshift-region"))
	}
	for i := range in.PVs {
		pv := &in.PVs[i]
		if !IsVSphereVolume(pv) {
			continue
		}
		if pv.Status.Phase != corev1.VolumeBound {
			plan.Blockers = append(plan.Blockers, blocker("UnboundVolume", fmt.Sprintf("PV %s is %s; delete or bind it before migration", pv.Name, pv.Status.Phase)))
			continue
		}

		ref := pv.Spec.ClaimRef
		var pvc *corev1.PersistentVolumeClaim
		if ref != nil {
			pvc = pvcs[ref.Namespace+"/"+ref.Name]
		}
		if pvc == nil || ref.UID == "" || pvc.UID != ref.UID || pvc.Spec.VolumeName != pv.Name {
			plan.Blockers = append(plan.Blockers, blocker("PVCBindingMismatch", fmt.Sprintf("PV %s does not have a matching PVC binding and UID", pv.Name)))
			continue
		}

		ns := plan.Namespaces[pvc.Namespace]
		if ns == nil {
			ns = &Namespace{}
			plan.Namespaces[pvc.Namespace] = ns
			if PlatformNamespaceUnsupported(pvc.Namespace) {
				plan.Blockers = append(plan.Blockers, blocker("PlatformNamespaceUnsupported", fmt.Sprintf("namespace %s requires an unsupported platform quiesce procedure", pvc.Namespace)))
			}
		}

		v := planVolume(in.Migration, pv, pvc, sources, classes)
		ns.Volumes = append(ns.Volumes, v)
	}

	checkPodTopology(plan, in.Pods)
	for _, ns := range plan.Namespaces {
		sort.Slice(ns.Volumes, func(i, j int) bool { return ns.Volumes[i].SourcePVC.Name < ns.Volumes[j].SourcePVC.Name })
	}
	sort.Slice(plan.Blockers, func(i, j int) bool {
		return plan.Blockers[i].Reason+plan.Blockers[i].Message < plan.Blockers[j].Reason+plan.Blockers[j].Message
	})
	return plan
}

// IsVSphereVolume includes in-tree volumes so unsupported disks cannot be stranded.
func IsVSphereVolume(pv *corev1.PersistentVolume) bool {
	return pv.Spec.CSI != nil && pv.Spec.CSI.Driver == Driver || pv.Spec.VsphereVolume != nil || pv.Annotations["pv.kubernetes.io/migrated-to"] == Driver
}

// supportedPlatformNamespaces lists platform namespaces that have a quiesce plan.
var supportedPlatformNamespaces = map[string]bool{
	"openshift-monitoring":     true,
	"openshift-image-registry": true,
}

// PlatformNamespaceUnsupported identifies platform namespaces without a quiesce plan.
func PlatformNamespaceUnsupported(ns string) bool {
	platformNamespace := strings.HasPrefix(ns, "openshift-") || strings.HasPrefix(ns, "kube-")
	return platformNamespace && !supportedPlatformNamespaces[ns]
}

func standardCategories(categories []string) bool {
	return len(categories) == 2 && (categories[0] == "openshift-zone" && categories[1] == "openshift-region" || categories[1] == "openshift-zone" && categories[0] == "openshift-region")
}

// Parameter reads a CSI parameter case-insensitively.
func Parameter(sc *storagev1.StorageClass, key string) (string, bool) {
	for k, v := range sc.Parameters {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// SourceFailureDomain resolves all affinity terms conservatively; ambiguous placement is unknown.
func SourceFailureDomain(pv *corev1.PersistentVolume, sources []configv1.VSpherePlatformFailureDomainSpec) string {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		if len(sources) == 1 {
			return sources[0].Name
		}
		return ""
	}
	matches := map[string]bool{}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, fd := range sources {
			if matchesTopology(term, fd.Region, fd.Zone) {
				matches[fd.Name] = true
			}
		}
	}
	if len(matches) == 1 {
		for name := range matches {
			return name
		}
	}
	return ""
}

func matchesTopology(term corev1.NodeSelectorTerm, region, zone string) bool {
	values := map[string]string{"topology.csi.vmware.com/openshift-region": region, "topology.csi.vmware.com/openshift-zone": zone, "topology.kubernetes.io/region": region, "topology.kubernetes.io/zone": zone}
	for _, req := range term.MatchExpressions {
		value, ok := values[req.Key]
		if !ok {
			continue
		}
		contains := false
		for _, v := range req.Values {
			if v == value {
				contains = true
			}
		}
		switch req.Operator {
		case corev1.NodeSelectorOpIn:
			if !contains {
				return false
			}
		case corev1.NodeSelectorOpNotIn:
			if contains {
				return false
			}
		case corev1.NodeSelectorOpExists:
		default:
			return false
		}
	}
	return true
}

// targetFailureDomain applies PVC override, source map, and single-target fallback in order.
func targetFailureDomain(m *migrationv1alpha1.VmwareCloudFoundationMigration, pvc *corev1.PersistentVolumeClaim, source string) (string, string) {
	target, override := pvc.Annotations[TargetFailureDomainAnnotation]
	if !override && m.Spec.Storage != nil {
		for _, entry := range m.Spec.Storage.FailureDomainMap {
			if source != "" && entry.Source == source {
				target = entry.Target
				break
			}
		}
	}
	if target == "" && !override && len(m.Spec.FailureDomains) == 1 {
		target = m.Spec.FailureDomains[0].Name
	}
	for _, fd := range m.Spec.FailureDomains {
		if fd.Name == target {
			return target, ""
		}
	}
	if override || target != "" {
		return "", "TargetFailureDomainInvalid"
	}
	return "", "TargetFailureDomainUnresolved"
}

// PodVolumes returns inventory indices mounted by a Pod.
func PodVolumes(pod *corev1.Pod, volumes []migrationv1alpha1.StorageWorkloadMigrationVolume) []int {
	var indices []int
	for i, v := range volumes {
		for _, mount := range pod.Spec.Volumes {
			if mount.PersistentVolumeClaim != nil && mount.PersistentVolumeClaim.ClaimName == v.SourcePVC.Name {
				indices = append(indices, i)
				break
			}
		}
	}
	return indices
}

// ControllerOwner returns only the controlling owner, ignoring incidental owner references.
func ControllerOwner(meta metav1.Object) *metav1.OwnerReference { return metav1.GetControllerOf(meta) }

func planVolume(m *migrationv1alpha1.VmwareCloudFoundationMigration, pv *corev1.PersistentVolume, pvc *corev1.PersistentVolumeClaim, sources []configv1.VSpherePlatformFailureDomainSpec, classes map[string]*storagev1.StorageClass) migrationv1alpha1.StorageWorkloadMigrationVolume {

	handle := ""
	if pv.Spec.CSI != nil {
		handle = pv.Spec.CSI.VolumeHandle
	} else if pv.Spec.VsphereVolume != nil {
		handle = pv.Spec.VsphereVolume.VolumePath
	}

	v := migrationv1alpha1.StorageWorkloadMigrationVolume{
		SourcePVC: migrationv1alpha1.MigrationObjectReference{
			Name: pvc.Name, UID: pvc.UID,
		},
		SourcePV: migrationv1alpha1.StorageWorkloadMigrationSourcePV{
			Name: pv.Name, UID: pv.UID, VolumeHandle: handle,
		},
	}

	target, reason := targetFailureDomain(m, pvc, SourceFailureDomain(pv, sources))

	v.TargetFailureDomain = target
	if reason != "" {
		AddBlocker(&v, reason, "set the PVC target-failure-domain annotation to a target in spec.failureDomains")
	}
	if pv.Spec.VolumeMode != nil && *pv.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		AddBlocker(&v, "RawBlockUnsupported", "raw block volumes cannot be migrated")
	}
	if pv.Spec.VsphereVolume != nil || pv.Annotations["pv.kubernetes.io/migrated-to"] != "" {
		AddBlocker(&v, "InTreeVolumeUnsupported", "in-tree and CSI-migrated in-tree volumes cannot be migrated")
	}
	if strings.HasPrefix(handle, "file:") {
		AddBlocker(&v, "FileVolumeUnsupported", "vSAN file service volumes cannot be migrated")
	}
	var sc *storagev1.StorageClass
	if pvc.Spec.StorageClassName != nil {
		sc = classes[*pvc.Spec.StorageClassName]
	}
	if sc == nil {
		AddBlocker(&v, "StaticVolumeUnsupported", "PVC must reference an existing StorageClass")
	} else {
		if sc.VolumeBindingMode == nil || *sc.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer {
			AddBlocker(&v, "ImmediateBindingUnsupported", "StorageClass must use WaitForFirstConsumer")
		}
		_, pinned := Parameter(sc, "datastoreurl")
		if pinned || len(sc.AllowedTopologies) > 0 {
			AddBlocker(&v, "StorageClassPinnedToSource", "remove datastoreurl and allowedTopologies from the StorageClass")
		}
		policy, _ := Parameter(sc, "storagepolicyname")
		if policy == "" {
			AddBlocker(&v, "StoragePolicyMissing", "StorageClass must specify storagepolicyname")
		}
	}
	return v
}

func checkPodTopology(plan *Plan, pods []corev1.Pod) {
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		ns := plan.Namespaces[pod.Namespace]
		if ns == nil {
			continue
		}
		indices := PodVolumes(&pod, ns.Volumes)
		targets := map[string]bool{}
		for _, i := range indices {
			if fd := ns.Volumes[i].TargetFailureDomain; fd != "" {
				targets[fd] = true
			}
		}
		if len(targets) > 1 {
			for _, i := range indices {
				AddBlocker(&ns.Volumes[i], "IncompatiblePodTopology", fmt.Sprintf("Pod %s mounts PVCs resolving to different target failure domains", pod.Name))
			}
		}
	}
}
