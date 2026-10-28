package storageplan

import (
	"context"
	"errors"
	"fmt"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

func validInput() Input {
	return Input{
		Migration: &migrationv1alpha1.VmwareCloudFoundationMigration{
			Spec: migrationv1alpha1.VmwareCloudFoundationMigrationSpec{
				Storage: &migrationv1alpha1.StorageMigrationSpec{},
				FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
					{
						Name:   "target",
						Region: "new-region",
						Zone:   "new-zone",
					},
				},
			},
		},
		Infrastructure: &configv1.Infrastructure{
			Spec: configv1.InfrastructureSpec{
				PlatformSpec: configv1.PlatformSpec{
					VSphere: &configv1.VSpherePlatformSpec{
						FailureDomains: []configv1.VSpherePlatformFailureDomainSpec{
							{
								Name:   "source",
								Region: "old-region",
								Zone:   "old-zone",
							},
						},
					},
				},
			},
		},
		PVs: []corev1.PersistentVolume{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pv",
					UID:  "pv-uid",
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							Driver:       Driver,
							VolumeHandle: "disk",
						},
					},
					ClaimRef: &corev1.ObjectReference{
						Namespace: "app",
						Name:      "data",
						UID:       "pvc-uid",
					},
				},
				Status: corev1.PersistentVolumeStatus{
					Phase: corev1.VolumeBound,
				},
			},
		},
		PVCs: []corev1.PersistentVolumeClaim{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "data",
					Namespace: "app",
					UID:       "pvc-uid",
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					VolumeName:       "pv",
					StorageClassName: ptr.To("class"),
				},
			},
		},
		StorageClasses: []storagev1.StorageClass{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "class",
				},
				VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
				Parameters: map[string]string{
					"storagepolicyname": "policy",
				},
			},
		},
	}
}

func TestVolumeBlockers(t *testing.T) {
	cases := []struct {
		name, reason string
		change       func(*Input)
	}{
		{"valid", "", func(in *Input) {}},
		{"raw block", "RawBlockUnsupported", func(in *Input) { in.PVs[0].Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock) }},
		{"in tree", "", func(in *Input) {
			in.PVs[0].Spec.CSI = nil
			in.PVs[0].Spec.VsphereVolume = &corev1.VsphereVirtualDiskVolumeSource{VolumePath: "disk"}
		}},
		{"migrated", "", func(in *Input) { in.PVs[0].Annotations = map[string]string{"pv.kubernetes.io/migrated-to": Driver} }},
		{"file", "", func(in *Input) { in.PVs[0].Spec.CSI.VolumeHandle = "file:disk" }},
		{"static", "StaticVolumeUnsupported", func(in *Input) { in.PVCs[0].Spec.StorageClassName = nil }},
		{"missing class", "StaticVolumeUnsupported", func(in *Input) { in.StorageClasses = nil }},
		{"immediate", "ImmediateBindingUnsupported", func(in *Input) { in.StorageClasses[0].VolumeBindingMode = nil }},
		{"datastore", "StorageClassPinnedToSource", func(in *Input) { in.StorageClasses[0].Parameters["DatastoreURL"] = "source" }},
		{"topology", "StorageClassPinnedToSource", func(in *Input) { in.StorageClasses[0].AllowedTopologies = []corev1.TopologySelectorTerm{{}} }},
		{"missing policy", "StoragePolicyMissing", func(in *Input) { in.StorageClasses[0].Parameters = nil }},
		{"unresolved", "TargetFailureDomainUnresolved", func(in *Input) {
			in.Migration.Spec.FailureDomains = append(in.Migration.Spec.FailureDomains, configv1.VSpherePlatformFailureDomainSpec{Name: "other"})
		}},
		{"invalid override", "TargetFailureDomainInvalid", func(in *Input) { in.PVCs[0].Annotations = map[string]string{TargetFailureDomainAnnotation: "invalid"} }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			tt.change(&in)
			plan := Build(in)
			v := plan.Namespaces["app"].Volumes[0]
			if tt.reason == "" {
				if len(v.Blockers) != 0 || v.TargetFailureDomain != "target" {
					t.Fatalf("Build: unexpected plan %+v", v)
				}
				return
			}
			for _, b := range v.Blockers {
				if b.Reason == tt.reason {
					return
				}
			}
			t.Fatalf("Build: missing %s in %+v", tt.reason, v.Blockers)
		})
	}
}

func TestClusterBlockers(t *testing.T) {
	cases := []struct {
		name, reason string
		change       func(*Input)
	}{
		{"available", "UnboundVolume", func(in *Input) { in.PVs[0].Status.Phase = corev1.VolumeAvailable }},
		{"released", "UnboundVolume", func(in *Input) { in.PVs[0].Status.Phase = corev1.VolumeReleased }},
		{"missing ref", "PVCBindingMismatch", func(in *Input) { in.PVs[0].Spec.ClaimRef = nil }},
		{"missing claim", "PVCBindingMismatch", func(in *Input) { in.PVCs = nil }},
		{"uid mismatch", "PVCBindingMismatch", func(in *Input) { in.PVCs[0].UID = "replacement" }},
		{"volume mismatch", "PVCBindingMismatch", func(in *Input) { in.PVCs[0].Spec.VolumeName = "other" }},
		{"categories", "TopologyCategoriesConflict", func(in *Input) { in.TopologyCategories = []string{"custom"} }},
		{"overlap", "TargetTopologyOverlapsSource", func(in *Input) {
			in.Migration.Spec.FailureDomains[0].Region = "old-region"
			in.Migration.Spec.FailureDomains[0].Zone = "old-zone"
		}},
		{"platform namespace", "PlatformNamespaceUnsupported", func(in *Input) {
			in.PVCs[0].Namespace = "openshift-user-workload-monitoring"
			in.PVs[0].Spec.ClaimRef.Namespace = in.PVCs[0].Namespace
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			tt.change(&in)
			for _, b := range Build(in).Blockers {
				if b.Reason == tt.reason {
					return
				}
			}
			t.Fatalf("Build: missing cluster blocker %s", tt.reason)
		})
	}
	for _, namespace := range []string{"app", "openshift-monitoring", "openshift-image-registry"} {
		if PlatformNamespaceUnsupported(namespace) {
			t.Fatalf("PlatformNamespaceUnsupported(%s)", namespace)
		}
	}
}

func TestFailureDomains(t *testing.T) {
	const secondFD = "second"
	sources := []configv1.VSpherePlatformFailureDomainSpec{{Name: "a", Region: "r", Zone: "z"}, {Name: "b", Region: "r", Zone: "other"}}
	for _, prefix := range []string{"topology.csi.vmware.com/openshift-", "topology.kubernetes.io/"} {
		pv := corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: prefix + "region", Operator: corev1.NodeSelectorOpIn, Values: []string{"r"}}, {Key: prefix + "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"z"}}}}}}}}}
		if got := sourceFailureDomain(&pv, sources); got != "a" {
			t.Fatalf("SourceFailureDomain: %q", got)
		}
		pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[1].Values = append([]string{"z"}, "other")
		if got := sourceFailureDomain(&pv, sources); got != "" {
			t.Fatalf("SourceFailureDomain ambiguous: %q", got)
		}
	}
	pv := &corev1.PersistentVolume{}
	if got := sourceFailureDomain(pv, sources[:1]); got != "a" {
		t.Fatalf("SourceFailureDomain fallback: %q", got)
	}
	if got := sourceFailureDomain(pv, sources); got != "" {
		t.Fatalf("SourceFailureDomain unknown: %q", got)
	}
	in := validInput()
	in.Migration.Spec.FailureDomains = append(in.Migration.Spec.FailureDomains, configv1.VSpherePlatformFailureDomainSpec{Name: secondFD})
	in.Migration.Spec.FailureDomainMappings = []migrationv1alpha1.FailureDomainMapping{{Source: "source", Destination: secondFD}}
	claim := &in.PVCs[0]
	if target, err := targetFailureDomain(in.Migration, claim, "source"); target != secondFD || err != nil {
		t.Fatalf("TargetFailureDomain map: %s %v", target, err)
	}
	claim.Annotations = map[string]string{TargetFailureDomainAnnotation: "target"}
	if target, _ := targetFailureDomain(in.Migration, claim, "source"); target != "target" {
		t.Fatalf("TargetFailureDomain override: %s", target)
	}
	claim.Annotations[TargetFailureDomainAnnotation] = ""
	if _, err := targetFailureDomain(in.Migration, claim, "source"); !errors.Is(err, errTargetFailureDomainInvalid) {
		t.Fatalf("TargetFailureDomain empty override: %v", err)
	}
}

func TestIncompatiblePodTopology(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodRunning, corev1.PodPending, corev1.PodSucceeded, corev1.PodFailed} {
		t.Run(string(phase), func(t *testing.T) {
			in := validInput()
			second := *in.PVCs[0].DeepCopy()
			second.Name = "second"
			second.UID = "second-uid"
			second.Spec.VolumeName = "second-pv"
			second.Annotations = map[string]string{TargetFailureDomainAnnotation: "other"}
			in.PVCs = append(in.PVCs, second)
			pv := *in.PVs[0].DeepCopy()
			pv.Name = "second-pv"
			pv.Spec.ClaimRef.Name = "second"
			pv.Spec.ClaimRef.UID = "second-uid"
			in.PVs = append(in.PVs, pv)
			in.Migration.Spec.FailureDomains = append(in.Migration.Spec.FailureDomains, configv1.VSpherePlatformFailureDomainSpec{Name: "other"})
			in.PVCs[0].Annotations = map[string]string{TargetFailureDomainAnnotation: "target"}
			in.Pods = []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod",
						Namespace: "app",
					},
					Spec: corev1.PodSpec{
						Volumes: []corev1.Volume{
							{
								VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: "data",
									},
								},
							},
							{
								VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: "second",
									},
								},
							},
						},
					},
					Status: corev1.PodStatus{
						Phase: phase,
					},
				},
			}
			plan := Build(in)
			for _, v := range plan.Namespaces["app"].Volumes {
				blocked := len(v.Blockers) > 0
				want := phase != corev1.PodSucceeded && phase != corev1.PodFailed
				if blocked != want {
					t.Fatalf("Build Pod topology: %+v", v)
				}
			}
		})
	}
}

func TestConsumers(t *testing.T) {
	in := validInput()
	volumes := Build(in).Namespaces["app"].Volumes
	owner := func(api, kind, name, uid string) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: api, Kind: kind, Name: name, UID: types.UID(uid), Controller: ptr.To(true)}
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod",
			Namespace: "app",
			UID:       "pod-uid",
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "data",
						},
					},
				},
			},
		},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", UID: "rs-uid", OwnerReferences: []metav1.OwnerReference{owner("apps/v1", "Deployment", "deploy", "deploy-uid")}}}
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "deploy", UID: "deploy-uid"}}
	lookup := func(_ context.Context, _ string, ref metav1.OwnerReference) (metav1.Object, error) {
		switch ref.Kind {
		case "ReplicaSet":
			return rs, nil
		case "Deployment":
			return deploy, nil
		default:
			return nil, fmt.Errorf("unexpected lookup %s", ref.Kind)
		}
	}
	for _, tt := range []struct {
		name string
		ref  *metav1.OwnerReference
		want string
	}{{"bare", nil, "Pod"}, {"deployment", ptr.To(owner("apps/v1", "ReplicaSet", "rs", "rs-uid")), "Deployment"}, {"operator", ptr.To(owner("monitoring.coreos.com/v1", "Prometheus", "prom", "prom-uid")), "Prometheus"}} {
		t.Run(tt.name, func(t *testing.T) {
			p := *pod.DeepCopy()
			if tt.ref != nil {
				p.OwnerReferences = []metav1.OwnerReference{*tt.ref}
			}
			got, err := Consumers(context.Background(), []corev1.Pod{p, p}, "app", volumes, lookup)
			if err != nil || len(got) != 1 || got[0].Kind != tt.want {
				t.Fatalf("Consumers: %+v, %v", got, err)
			}
		})
	}
}

func TestConsumerOwnerChains(t *testing.T) {
	for _, tt := range []struct{ name, kind, api, parentKind, parentAPI string }{
		{"stateful", "StatefulSet", "apps/v1", "Prometheus", "monitoring.coreos.com/v1"},
		{"daemon", "DaemonSet", "apps/v1", "", ""},
		{"scheduled job", "Job", "batch/v1", "CronJob", "batch/v1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			volumes := Build(in).Namespaces["app"].Volumes
			ref := metav1.OwnerReference{APIVersion: tt.api, Kind: tt.kind, Name: "owner", UID: "owner-uid", Controller: ptr.To(true)}
			pod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "app",
					UID:       "pod-uid",
					OwnerReferences: []metav1.OwnerReference{
						ref,
					},
				},
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "data",
								},
							},
						},
					},
				},
			}
			lookup := func(_ context.Context, _ string, owner metav1.OwnerReference) (metav1.Object, error) {
				meta := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, UID: owner.UID}}
				if owner.Kind == tt.kind && tt.parentKind != "" {
					meta.OwnerReferences = []metav1.OwnerReference{{APIVersion: tt.parentAPI, Kind: tt.parentKind, Name: "parent", UID: "parent-uid", Controller: ptr.To(true)}}
				}
				return meta, nil
			}
			got, err := Consumers(context.Background(), []corev1.Pod{pod}, "app", volumes, lookup)
			want := tt.kind
			if tt.parentKind != "" {
				want = tt.parentKind
			}
			if err != nil || len(got) != 1 || got[0].Kind != want {
				t.Fatalf("Consumers: %+v %v", got, err)
			}
			_, err = Consumers(context.Background(), []corev1.Pod{pod}, "app", volumes, func(context.Context, string, metav1.OwnerReference) (metav1.Object, error) {
				return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{UID: "replacement"}}, nil
			})
			if err == nil {
				t.Fatal("Consumers accepted a replaced owner")
			}
		})
	}
}

func TestAffinityAlternativeWithoutTopology(t *testing.T) {
	sources := []configv1.VSpherePlatformFailureDomainSpec{{Name: "a", Region: "r", Zone: "a"}, {Name: "b", Region: "r", Zone: "b"}}
	pv := &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
		{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "topology.kubernetes.io/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}}},
		{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"node"}}}},
	}}}}}
	if got := sourceFailureDomain(pv, sources); got != "" {
		t.Fatalf("SourceFailureDomain ignored alternative: %s", got)
	}
}
