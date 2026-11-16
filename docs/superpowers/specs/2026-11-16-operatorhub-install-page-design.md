# OperatorHub Install Page Design

## Intent

Make VCF Migration Operator's OpenShift OperatorHub details and installation pages complete and credible, following metadata patterns used by Red Hat OpenShift GitOps. This is presentation metadata only; operator runtime behavior and permissions remain unchanged.

## Goals

- Replace placeholder CSV copy and links with accurate product metadata.
- Add standard OpenShift annotations for description, repository, support, and suggested namespace.
- Present clear capabilities, keywords, provider, maintainer, and CR description.
- Regenerate and validate the checked-in OLM bundle.

## Non-goals

- No console plugin or custom web UI.
- No runtime, RBAC, CRD schema, channel, or install-mode changes.
- No unsupported feature claims.
- No invented product logo; retain the empty scaffold icon entry until an approved brand asset exists.

## Constraints

- Source of truth is `config/manifests/bases/vcf-migration-operator.clusterserviceversion.yaml`.
- Generated `bundle/manifests/vcf-migration-operator.clusterserviceversion.yaml` must stay in sync through `make bundle`.
- Links must use existing public repository and documentation locations.
- Suggested namespace is `openshift-vcf-migration`.
- Capability remains `Basic Install`; migration workflow visibility does not justify GitOps' `Deep Insights` classification.

## Approaches

1. **CSV metadata only (selected):** Update supported annotations and CSV fields, then regenerate bundle. Smallest change, native OperatorHub rendering, no runtime risk.
2. **Add an embedded branded icon:** Improves card branding but requires an approved Red Hat/product asset. Rejected until such an asset is supplied; keep the empty icon entry.
3. **Build a console plugin:** Maximum UI control, but adds a deployed frontend, permissions, maintenance, and security surface. Rejected because OperatorHub metadata provides requested install-page polish.

## Design

Add concise OperatorHub summary annotations (`description`, `repository`, `support`, `operatorframework.io/suggested-namespace`) and replace placeholder link with repository and documentation links. Expand `spec.description` into short Markdown explaining purpose, migration stages, prerequisites, and where to start. Use `Red Hat, Inc.` as provider, preserve SPLAT maintainer contact, use descriptive VMware/OpenShift migration keywords, and improve owned-CR wording.

Generated bundle remains authoritative for catalog publication and receives the same metadata through the existing `make bundle` workflow.
