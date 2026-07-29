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

// Finalizer is set on the singleton VmwareCloudFoundationMigration while a
// migration is in progress, so the object is not removed out from under an
// interrupted migration (e.g. a test timeout or accidental delete), leaving
// the cluster split across vCenters with nothing left to track or finish it.
const Finalizer = "migration.openshift.io/vcfm-protection"

// ForceDeleteAnnotation, when set to "true" on the singleton
// VmwareCloudFoundationMigration, allows the object to be deleted even while
// a migration is in progress, abandoning it deliberately.
const ForceDeleteAnnotation = "migration.openshift.io/force-delete"

// SecretReference references a secret by name and namespace.
type SecretReference struct {
	// Name is the secret name.
	Name string `json:"name"`

	// Namespace is the secret namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// VmwareCloudFoundationMigrationSpec defines the desired state of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationSpec struct {
	// State controls the workflow: Pending, Running, Paused.
	// The reconciler only acts when State is Running.
	// +kubebuilder:validation:Enum=Pending;Running;Paused
	// +kubebuilder:default=Pending
	State MigrationState `json:"state"`

	// TargetVCenterCredentialsSecret references the secret containing target vCenter credentials.
	// The secret must contain keys: {target-vcenter-fqdn}.username and {target-vcenter-fqdn}.password.
	TargetVCenterCredentialsSecret SecretReference `json:"targetVCenterCredentialsSecret"`

	// FailureDomains defines failure domains for the target vCenter.
	// Uses OpenShift's standard VSpherePlatformFailureDomainSpec which includes
	// Name, Region, Zone, Server, and Topology with all necessary fields.
	// +kubebuilder:validation:MinItems=1
	FailureDomains []configv1.VSpherePlatformFailureDomainSpec `json:"failureDomains"`
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

	Spec   VmwareCloudFoundationMigrationSpec   `json:"spec,omitempty"`
	Status VmwareCloudFoundationMigrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VmwareCloudFoundationMigrationList contains a list of VmwareCloudFoundationMigration.
type VmwareCloudFoundationMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VmwareCloudFoundationMigration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VmwareCloudFoundationMigration{}, &VmwareCloudFoundationMigrationList{})
}
