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
	configv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MigrationState represents the overall state of the migration workflow.
type MigrationState string

const (
	// MigrationStatePending indicates the migration has not started.
	MigrationStatePending MigrationState = "Pending"
	// MigrationStateRunning indicates the migration is actively progressing.
	MigrationStateRunning MigrationState = "Running"
	// MigrationStatePaused indicates the migration is paused by the user.
	MigrationStatePaused MigrationState = "Paused"
)

// MigrationPhase represents the current phase of the migration workflow.
type MigrationPhase string

const (
	// PhasePending indicates the migration is waiting to start.
	PhasePending MigrationPhase = "Pending"
	// PhaseInfrastructurePrepared indicates preflight checks and path selection are complete.
	PhaseInfrastructurePrepared MigrationPhase = "InfrastructurePrepared"
	// PhaseDestinationInitialized indicates target vCenter assets have been created.
	PhaseDestinationInitialized MigrationPhase = "DestinationInitialized"
	// PhaseMultiSiteConfigured indicates the cluster recognizes both vCenters.
	PhaseMultiSiteConfigured MigrationPhase = "MultiSiteConfigured"
	// PhaseWorkloadMigrated indicates compute has migrated to the target vCenter.
	PhaseWorkloadMigrated MigrationPhase = "WorkloadMigrated"
	// PhaseSourceCleaned indicates the source vCenter has been removed.
	PhaseSourceCleaned MigrationPhase = "SourceCleaned"
	// PhaseCompleted indicates the entire migration workflow completed successfully.
	PhaseCompleted MigrationPhase = "Completed"
	// PhaseFailed indicates a fatal error occurred during migration.
	PhaseFailed MigrationPhase = "Failed"
	// PhasePaused indicates the migration was paused by the user.
	PhasePaused MigrationPhase = "Paused"
)

// MigrationProgress contains real-time counters and details of resource migration.
type MigrationProgress struct {
	// Workers tracks worker machine and node migration progress.
	// +optional
	Workers *WorkerMigrationProgress `json:"workers,omitempty"`

	// ControlPlane tracks ControlPlaneMachineSet rollout progress.
	// +optional
	ControlPlane *ControlPlaneProgress `json:"controlPlane,omitempty"`
}

// WorkerMigrationProgress surfaces worker machine and node counts across source and target.
type WorkerMigrationProgress struct {
	// TargetMachinesTotal is the desired number of worker machines in target failure domains.
	TargetMachinesTotal int32 `json:"targetMachinesTotal"`
	// TargetMachinesReady is the number of target worker machines in Running phase with a NodeRef.
	TargetMachinesReady int32 `json:"targetMachinesReady"`
	// TargetNodesReady is the number of target worker nodes reporting NodeReady=True.
	TargetNodesReady int32 `json:"targetNodesReady"`
	// SourceMachinesRemaining is the number of source worker machines still existing.
	SourceMachinesRemaining int32 `json:"sourceMachinesRemaining"`
}

// ControlPlaneProgress surfaces CPMS rollout counts.
type ControlPlaneProgress struct {
	// Replicas is the total desired control plane replicas.
	Replicas int32 `json:"replicas"`
	// UpdatedReplicas is the number of control plane replicas matching the target spec.
	UpdatedReplicas int32 `json:"updatedReplicas"`
	// ReadyReplicas is the number of control plane replicas running and ready.
	ReadyReplicas int32 `json:"readyReplicas"`
}

// SingletonName is the only object name the operator will reconcile. Since a
// single OpenShift cluster can only ever have one active vCenter migration,
// this follows OpenShift's singleton resource pattern (e.g.
// infrastructures.config.openshift.io/cluster), but as a namespaced resource
// whose singleton behavior is enforced at reconcile time via the Accepted
// condition rather than cluster-scoped singleton=true.
const SingletonName = "cluster"

// SecretReference references a secret by name and namespace.
type SecretReference struct {
	// name is the secret name.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// namespace is the secret namespace. When omitted, defaults to the namespace
	// of the VmwareCloudFoundationMigration object.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// VmwareCloudFoundationMigrationSpec defines the desired state of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationSpec struct {
	// state controls the workflow: Pending, Running, Paused.
	// The migration workflow only executes when state is Running.
	// When omitted, defaults to Pending.
	// +optional
	// +kubebuilder:validation:Enum=Pending;Running;Paused
	// +kubebuilder:default=Pending
	State MigrationState `json:"state,omitempty"`

	// targetVCenterCredentialsSecret references the secret containing target vCenter credentials.
	// The secret must contain keys: {target-vcenter-fqdn}.username and {target-vcenter-fqdn}.password.
	// +required
	TargetVCenterCredentialsSecret SecretReference `json:"targetVCenterCredentialsSecret"`

	// failureDomains defines failure domains for the target vCenter. The embedded
	// element type intentionally tracks config.openshift.io/v1 VSpherePlatformFailureDomainSpec
	// (including Name, Region, Zone, Server, and Topology) to align with OpenShift API
	// failure domain semantics; the schema is re-inlined on "make manifests" after
	// dependency bumps.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	FailureDomains []configv1.VSpherePlatformFailureDomainSpec `json:"failureDomains"`

	// image controls RHCOS OVA resolution and import into destination vCenter.
	// When set, the operator downloads and imports the OVA as a VM template
	// for each failure domain and populates topology.template automatically.
	// When omitted, topology.template must be set manually in each failure domain.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`
}

// DiskProvisioningMode defines the disk provisioning type for imported VM templates.
type DiskProvisioningMode string

const (
	// DiskProvisioningModeThin allocates storage on demand as blocks are written.
	DiskProvisioningModeThin DiskProvisioningMode = "thin"
	// DiskProvisioningModeThick pre-allocates all storage at creation time.
	DiskProvisioningModeThick DiskProvisioningMode = "thick"
	// DiskProvisioningModeEagerZeroedThick pre-allocates all storage and zeroes blocks at creation time.
	DiskProvisioningModeEagerZeroedThick DiskProvisioningMode = "eagerZeroedThick"
)

// ImageSpec controls RHCOS OVA import behavior.
type ImageSpec struct {
	// ovaUrl is a direct URL to the RHCOS OVA file. The URL must use https:// and
	// end in .ova (an optional query string is allowed for proxy tokens or
	// integrity digests appended by stream metadata tooling). When set, the operator
	// downloads from this URL instead of resolving via the coreos-bootimages
	// ConfigMap delivered by CVO.
	// Required for air-gapped environments: point to an internal HTTPS mirror.
	// +optional
	// +kubebuilder:validation:Pattern=`^https://.*\.ova(\?.*)?$`
	OVAUrl string `json:"ovaUrl,omitempty"`

	// diskProvisioning controls the VMDK disk provisioning type when importing
	// the OVA (thin, thick, eagerZeroedThick). Matches the installer's behavior.
	// When omitted, vSphere defaults to the provisioning type specified in the
	// OVF descriptor.
	// +optional
	// +kubebuilder:validation:Enum=thin;thick;eagerZeroedThick
	DiskProvisioning DiskProvisioningMode `json:"diskProvisioning,omitempty"`
}

// VmwareCloudFoundationMigrationStatus defines the observed state of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationStatus struct {
	// Phase indicates the current stage of the migration workflow.
	// +optional
	Phase MigrationPhase `json:"phase,omitempty"`

	// Progress surfaces real-time counts for worker and control-plane migration.
	// +optional
	Progress *MigrationProgress `json:"progress,omitempty"`

	// conditions represent the current state of the migration.
	// Known conditions are:
	// - Accepted: admission gate; True is normal for the single reconciled instance (cluster),
	//   while False indicates an unsupported object name.
	// - InfrastructurePrepared: preflight validation and migration path selection.
	// - DestinationInitialized: destination vCenter folders and tags created.
	// - DestinationImageImported: RHCOS OVA imported as a VM template on destination vCenter.
	// - MultiSiteConfigured: cluster configured for both source and target vCenters.
	// - WorkloadMigrated: workloads migrated to destination vCenter machine sets.
	// - SourceCleaned: source vCenter references removed and cleaned up.
	// - Ready: aggregate condition indicating migration is complete and cluster is healthy.
	// For stage conditions (InfrastructurePrepared through Ready), False is normal while
	// progressing; True indicates the stage has completed. Stages execute in order.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// startTime is when the migration started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// lastUpdateTime is when the migration status was last updated.
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// completionTime is when the migration completed.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// image reports the RHCOS OVA import state.
	// +optional
	Image *ImageStatus `json:"image,omitempty"`
}

// ImageURLSource describes how the resolved OVA URL was obtained.
type ImageURLSource string

const (
	// ImageURLSourceUser indicates the OVA URL was user-specified in spec.image.ovaUrl.
	ImageURLSourceUser ImageURLSource = "user"
	// ImageURLSourceAuto indicates the OVA URL was auto-resolved from stream metadata.
	ImageURLSourceAuto ImageURLSource = "auto"
)

// ImageStatus reports the RHCOS OVA import progress and results.
type ImageStatus struct {
	// resolvedOVAUrl is the URL from which the OVA was (or will be) downloaded.
	// +optional
	ResolvedOVAUrl string `json:"resolvedOVAUrl,omitempty"`

	// resolvedSHA256 is the expected sha256 digest of the OVA file, when
	// resolved from stream metadata. Empty for user-provided URLs.
	// +optional
	ResolvedSHA256 string `json:"resolvedSHA256,omitempty"`

	// importedTemplates maps failure domain names to the inventory paths
	// of imported VM templates.
	// +optional
	ImportedTemplates map[string]string `json:"importedTemplates,omitempty"`

	// operatorImportedTemplates records the OVA URL each failure domain's
	// template was imported from by the operator. It is populated only for
	// operator imports; user-pre-configured templates are not recorded here.
	// Used to detect a changed OVA URL and re-import only operator-managed
	// templates without touching user-provided ones.
	// +optional
	OperatorImportedTemplates map[string]string `json:"operatorImportedTemplates,omitempty"`

	// urlSource records how resolvedOVAUrl was populated: "" (unresolved / no opinion),
	// "user" (user-specified), or "auto" (auto-resolved). The zero value ("")
	// indicates no opinion or unresolved. Used to tell a deliberate user-clear
	// of spec.image.ovaUrl apart from an empty auto-resolution when deciding
	// whether to clear resolvedOVAUrl.
	// +optional
	// +kubebuilder:validation:Enum="";user;auto
	URLSource ImageURLSource `json:"urlSource,omitempty"`
}

// Condition type constants for the migration workflow.
// The reconciler checks conditions in this order; if a condition is not True,
// it executes the work for that condition and returns with RequeueAfter.
const (
	// ConditionInfrastructurePrepared indicates preflight checks passed and the
	// migration path has been selected.
	ConditionInfrastructurePrepared = "InfrastructurePrepared"

	// ConditionDestinationInitialized indicates the target vCenter has all required assets
	// (VM folders, region/zone tags).
	ConditionDestinationInitialized = "DestinationInitialized"

	// ConditionDestinationImageImported indicates the RHCOS OVA has been
	// downloaded and imported as a VM template on all target vCenters.
	// When spec.image is nil, this condition is immediately set to True.
	ConditionDestinationImageImported = "DestinationImageImported"

	// ConditionMultiSiteConfigured indicates the cluster recognizes both vCenters
	// (secrets, Infrastructure CRD, cloud-provider-config updated, pods restarted).
	ConditionMultiSiteConfigured = "MultiSiteConfigured"

	// ConditionWorkloadMigrated indicates compute is running in the new location
	// (new workers created, control plane rolled out, old MachineSets scaled to 0).
	ConditionWorkloadMigrated = "WorkloadMigrated"

	// ConditionSourceCleaned indicates the old vCenter is fully detached
	// (removed from Infrastructure, config, and secrets; CVO is re-enabled when
	// required by the legacy path).
	ConditionSourceCleaned = "SourceCleaned"

	// ConditionReady indicates migration is 100% complete.
	// This is an aggregate condition: all operators healthy, only target vCenters in Infrastructure.
	ConditionReady = "Ready"

	// ConditionAccepted indicates whether this object is the singleton instance
	// (named SingletonName) that the operator will act on.
	ConditionAccepted = "Accepted"
)

// Condition reason constants.
const (
	ReasonProgressing = "Progressing"
	ReasonCompleted   = "Completed"
	ReasonFailed      = "Failed"
	// ReasonPaused indicates migration has been paused because spec.state is set to Paused.
	ReasonPaused = "Paused"

	// ReasonUnsupportedName indicates the object's name is not SingletonName,
	// so the operator is ignoring it.
	ReasonUnsupportedName = "UnsupportedName"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=vmwarecloudfoundationmigrations,scope=Namespaced,shortName=vcfm,categories=migration
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VmwareCloudFoundationMigration is the Schema for the vmwarecloudfoundationmigrations API.
// It orchestrates migration of an OpenShift cluster from one vCenter to another.
type VmwareCloudFoundationMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of VmwareCloudFoundationMigration.
	// +optional
	Spec VmwareCloudFoundationMigrationSpec `json:"spec,omitempty"`

	// status defines the observed state of VmwareCloudFoundationMigration.
	// +optional
	Status VmwareCloudFoundationMigrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VmwareCloudFoundationMigrationList contains a list of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// items is the list of VmwareCloudFoundationMigration objects.
	// +required
	Items []VmwareCloudFoundationMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VmwareCloudFoundationMigration{}, &VmwareCloudFoundationMigrationList{})
}
