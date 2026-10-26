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
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// StorageWorkloadMigrationPhase identifies a namespace storage migration phase.
// Only Planning is implemented; later phases are added with execution support.
// +kubebuilder:validation:Enum=Planning
type StorageWorkloadMigrationPhase string

const (
	// StorageWorkloadMigrationPhasePlanning indicates inventory and target resolution are in progress.
	StorageWorkloadMigrationPhasePlanning StorageWorkloadMigrationPhase = "Planning"
)

// MigrationObjectReference records the name and identity of an inventoried object.
type MigrationObjectReference struct {
	// Name is the object name.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// UID is the identity of the object; same-name replacements must not be adopted.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Type=string
	UID types.UID `json:"uid"`
}

// StorageWorkloadMigrationSpec identifies the migration that owns this plan.
type StorageWorkloadMigrationSpec struct {
	// MigrationRef identifies the VmwareCloudFoundationMigration that owns this plan.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="migrationRef is immutable"
	MigrationRef MigrationObjectReference `json:"migrationRef"`
}

// StorageWorkloadMigrationSourcePV records the identity of a source PV and CNS volume.
type StorageWorkloadMigrationSourcePV struct {
	// Name is the source PV name.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// UID is the source PV identity.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Type=string
	UID types.UID `json:"uid"`

	// VolumeHandle identifies the source CSI volume.
	// +required
	// +kubebuilder:validation:MinLength=1
	VolumeHandle string `json:"volumeHandle"`
}

// StorageWorkloadMigrationConditionPlanned reports whether the inventory has blockers.
const StorageWorkloadMigrationConditionPlanned = "Planned"

// StorageWorkloadMigrationBlocker explains why a volume cannot be migrated.
type StorageWorkloadMigrationBlocker struct {
	// Reason is a CamelCase blocker identifier.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason"`
	// Message explains the blocker and how to resolve it.
	// +required
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message"`
}

// StorageWorkloadMigrationVolume records a source volume and its planned destination.
type StorageWorkloadMigrationVolume struct {
	// SourcePVC identifies the source PVC in this workload namespace.
	// +required
	SourcePVC MigrationObjectReference `json:"sourcePVC"`

	// SourcePV identifies the bound source PV and CSI volume.
	// +required
	SourcePV StorageWorkloadMigrationSourcePV `json:"sourcePV"`

	// TargetFailureDomain is the resolved target failure domain; absent while unresolved.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	TargetFailureDomain string `json:"targetFailureDomain,omitempty"`

	// Blockers lists reasons this volume cannot be migrated.
	// +optional
	// +listType=map
	// +listMapKey=reason
	Blockers []StorageWorkloadMigrationBlocker `json:"blockers,omitempty"`
}

// StorageWorkloadMigrationConsumer records a workload controller observed consuming
// PVCs in this namespace. It is informational: it tells the customer which workloads
// to stop and resume, and lists the workloads checked for readiness during validation.
// It is not an identity check; quiescence is verified through Pods and VolumeAttachments.
type StorageWorkloadMigrationConsumer struct {
	// APIVersion is the API group and version of the consumer.
	// +required
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`

	// Kind is the consumer resource kind.
	// +required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name is the consumer name in this workload namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// UID is the identity of the consumer when it was observed.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Type=string
	UID types.UID `json:"uid"`
}

// StorageWorkloadMigrationStatus contains operator-owned inventory and progress.
type StorageWorkloadMigrationStatus struct {
	// Phase identifies the current lifecycle phase.
	// +optional
	Phase StorageWorkloadMigrationPhase `json:"phase,omitempty"`

	// Volumes records source identities and resolved destinations.
	// +optional
	// +listType=atomic
	Volumes []StorageWorkloadMigrationVolume `json:"volumes,omitempty"`

	// Consumers lists workload controllers observed consuming PVCs in this namespace.
	// +optional
	// +listType=map
	// +listMapKey=uid
	Consumers []StorageWorkloadMigrationConsumer `json:"consumers,omitempty"`

	// Conditions reports readiness, blockers, retries, and required customer actions.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=storageworkloadmigrations,scope=Namespaced,shortName=swm,categories=migration
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Migration",type=string,JSONPath=`.spec.migrationRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// StorageWorkloadMigration tracks customer-coordinated storage migration in one namespace.
type StorageWorkloadMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec identifies the migration that owns this plan.
	// +required
	Spec StorageWorkloadMigrationSpec `json:"spec"`

	// Status contains operator-owned inventory and progress.
	// +optional
	Status StorageWorkloadMigrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageWorkloadMigrationList contains StorageWorkloadMigration objects.
type StorageWorkloadMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	// Items contains the StorageWorkloadMigration objects.
	// +required
	Items []StorageWorkloadMigration `json:"items"`
}
