# Remove OVA Import — Design

## Intent and scope

The operator has not been released to customers. Remove the entire operator-managed RHCOS OVA download, resolution and vSphere import feature rather than maintaining backward-compatible API/status or migration shims. Target failure domains must supply an existing `topology.template` inventory path. Preserve the rest of the vCenter migration workflow, especially destination folders/tags, template existence checks, multi-site configuration, source cleanup and shared ConfigMap access. No OVA artifacts need automatic cleanup; the removal does not delete existing vSphere templates. This design covers one coherent feature removal across its implementation and public surfaces.

## Architecture and behavior

The workflow goes directly from `DestinationInitialized` to `MultiSiteConfigured`. Delete the `DestinationImageImported` condition and its controller handler, timeout, downloader/importer helpers, status/spec mutations and image-only vCenter privilege checks. Preflight always rejects any failure domain with an empty template and continues to check that the configured VM/template exists on the target vCenter. The configured template is never deleted or rewritten by this operator. All other workflow phases and operational privileges remain.

Remove `spec.image`, `status.image`, their dedicated types and the image condition from the Go API and both CRD schemas. There are no compatibility shims or deprecation aliases: this operator has not shipped. Reject absent/empty `failureDomains[*].topology.template` at Kubernetes admission as well as preflight. The embedded `configv1.VSpherePlatformFailureDomainSpec` has an optional upstream `template`; keep that type rather than forking it, and put a repeatable Kubebuilder XValidation CEL marker on the `FailureDomains` list (`self.all(fd, has(fd.topology.template) && fd.topology.template != '')`). Verify that controller-gen emits the rule under the field and that Kubernetes accepts it. If controller-gen fails to attach the marker to the embedded schema, use a scoped, reproducible CRD post-generation step for both base and bundle schemas rather than manually patching generated YAML with no regeneration path.

Remove OVA scratch mounts/volumes from the manager deployment and OLM CSV; remove image-only dependency `github.com/coreos/stream-metadata-go` using the repository's vendoring process, without removing govmomi or unrelated dependencies. Remove the obsolete image phase from metrics, image-specific tests, docs and vCenter privilege instructions. Preserve `configmaps` RBAC (cloud-provider-config) and MCO namespace usage (pod restart); don't remove generic operator container-image configuration. Existing sample migration already supplies a manual template.

## Error handling, verification and documentation

Keep a preflight error identifying the index and name of the first failure domain with an empty template; return before vCenter access. Validate all failure domains, including the second and later. Update API/user/install/architecture documentation to say templates are provided by users, and remove image URL/import examples and import-only privilege lists. Historical docs/plans remain historical unless presented as current user guidance. Confirm the base and bundle CRDs have matching schema and the CSV matches manager deployment. Prove that absent/empty templates are rejected at admission, a valid template is accepted, and preflight/phase tests continue to pass. Verify build, vet, non-e2e Go tests with envtest assets, generated-file consistency, dependency cleanup and no first-party OVA feature references. Avoid unrelated refactors.

## Decisions

- Complete removal is safe because no customer release exists; no upgrade compatibility behavior.
- `topology.template` is required by both CRD admission and runtime preflight; retain the upstream embedded failure-domain type.
- This is a plan-first change in `.worktrees/remove-ova-feature` based on `upstream/main` `6cb35bf331c1d50e529954a18b0a6b7595e70e2f`. No implementation is approved yet.
