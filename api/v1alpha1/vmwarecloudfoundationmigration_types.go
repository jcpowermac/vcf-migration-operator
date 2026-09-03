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

// SingletonName is the only object name the operator will reconcile. Since a
// single OpenShift cluster can only ever have one active vCenter migration,
// this follows OpenShift's singleton config resource convention (e.g.
// infrastructures.config.openshift.io/cluster).
const SingletonName = "cluster"

// SecretReference references a secret by name and namespace.
type SecretReference struct {
	// Name is the secret name.
	// +required
	Name string `json:"name"`

	// Namespace is the secret namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// VmwareCloudFoundationMigrationSpec defines the desired state of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationSpec struct {
	// State controls the workflow: Pending, Running, Paused.
	// The reconciler only acts when State is Running.
	// +optional
	// +kubebuilder:validation:Enum=Pending;Running;Paused
	// +kubebuilder:default=Pending
	State MigrationState `json:"state"`

	// TargetVCenterCredentialsSecret references the secret containing target vCenter credentials.
	// The secret must contain keys: {target-vcenter-fqdn}.username and {target-vcenter-fqdn}.password.
	// +required
	TargetVCenterCredentialsSecret SecretReference `json:"targetVCenterCredentialsSecret"`

	// FailureDomains defines failure domains for the target vCenter.
	// Uses OpenShift's standard VSpherePlatformFailureDomainSpec which includes
	// Name, Region, Zone, Server, and Topology with all necessary fields.
	// +required
	// +kubebuilder:validation:MinItems=1
	FailureDomains []configv1.VSpherePlatformFailureDomainSpec `json:"failureDomains"`

	// Image controls RHCOS OVA resolution and import into destination vCenter.
	// When set, the operator downloads and imports the OVA as a VM template
	// for each failure domain and populates topology.template automatically.
	// When omitted, topology.template must be set manually in each failure domain.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`
}

// ImageSpec controls RHCOS OVA import behavior.
type ImageSpec struct {
	// OVAUrl is a direct URL to the RHCOS OVA file. When set, the operator
	// downloads from this URL instead of resolving via the coreos-bootimages
	// ConfigMap delivered by CVO.
	// Supports both direct .ova URLs and URLs with query parameters (e.g.
	// integrity digests appended by stream metadata tooling).
	// Required for air-gapped environments: point to an internal HTTP(S) mirror.
	// +optional
	// +kubebuilder:validation:Pattern=`^https://.*\.ova(\?.*)?$`
	OVAUrl string `json:"ovaUrl,omitempty"`

	// DiskProvisioning controls the VMDK disk provisioning type when importing
	// the OVA. Matches the installer's behavior.
	// +optional
	// +kubebuilder:validation:Enum=thin;thick;eagerZeroedThick
	DiskProvisioning string `json:"diskProvisioning,omitempty"`
}

// VmwareCloudFoundationMigrationStatus defines the observed state of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationStatus struct {
	// Conditions represent the current state of the migration.
	// Each condition corresponds to a stage of the migration workflow.
	// Standard Kubernetes conditions using metav1.Condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// StartTime is when the migration started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the migration completed.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Image reports the RHCOS OVA import state.
	// +optional
	Image *ImageStatus `json:"image,omitempty"`
}

// ImageStatus reports the RHCOS OVA import progress and results.
type ImageStatus struct {
	// ResolvedOVAUrl is the URL from which the OVA was (or will be) downloaded.
	// +optional
	ResolvedOVAUrl string `json:"resolvedOVAUrl,omitempty"`

	// ResolvedSHA256 is the expected sha256 digest of the OVA file, when
	// resolved from stream metadata. Empty for user-provided URLs.
	// +optional
	ResolvedSHA256 string `json:"resolvedSHA256,omitempty"`

	// DownloadComplete indicates the OVA has been successfully downloaded
	// to the operator's scratch volume.
	// +optional
	DownloadComplete *bool `json:"downloadComplete,omitempty"`

	// ImportedTemplates maps failure domain names to the inventory paths
	// of imported VM templates.
	// +optional
	ImportedTemplates map[string]string `json:"importedTemplates,omitempty"`

	// OperatorImportedTemplates records the OVA URL each failure domain's
	// template was imported from by the operator. It is populated only for
	// operator imports; user-pre-configured templates are not recorded here.
	// Used to detect a changed OVA URL and re-import only operator-managed
	// templates without touching user-provided ones.
	// +optional
	OperatorImportedTemplates map[string]string `json:"operatorImportedTemplates,omitempty"`

	// URLSource records how ResolvedOVAUrl was populated: "" (unresolved),
	// "user" (user-specified), or "auto" (auto-resolved). Used to tell a
	// deliberate user-clear of spec.image.ovaUrl apart from an empty
	// auto-resolution when deciding whether to clear ResolvedOVAUrl.
	// +optional
	URLSource string `json:"urlSource,omitempty"`
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
	ReasonPaused      = "Paused"
	ReasonPending     = "Pending"

	// ReasonUnsupportedName indicates the object's name is not SingletonName,
	// so the operator is ignoring it.
	ReasonUnsupportedName = "UnsupportedName"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=vmwarecloudfoundationmigrations,scope=Namespaced,shortName=vcfm
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VmwareCloudFoundationMigration is the Schema for the vmwarecloudfoundationmigrations API.
// It orchestrates migration of an OpenShift cluster from one vCenter to another.
type VmwareCloudFoundationMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec VmwareCloudFoundationMigrationSpec `json:"spec,omitempty"`

	// +optional
	Status VmwareCloudFoundationMigrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VmwareCloudFoundationMigrationList contains a list of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	// +required
	Items []VmwareCloudFoundationMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VmwareCloudFoundationMigration{}, &VmwareCloudFoundationMigrationList{})
}
