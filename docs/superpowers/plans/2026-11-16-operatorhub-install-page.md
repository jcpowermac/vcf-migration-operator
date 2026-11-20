# OperatorHub Install Page Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Improve VCF Migration Operator's native OperatorHub details and install-page presentation with accurate, complete CSV metadata.

**Architecture:** Keep OperatorHub's ClusterServiceVersion as the only UI source. Edit the scaffold source CSV, then use the existing bundle generator to propagate and validate the generated catalog artifact.

**Tech Stack:** OLM ClusterServiceVersion YAML, Operator SDK, Kustomize

**Spec:** `docs/superpowers/specs/2026-11-16-operatorhub-install-page-design.md`

## Global Constraints

- Do not change operator runtime behavior, RBAC, CRD schema, install modes, or channels.
- Keep `capabilities: Basic Install` and suggested namespace `openshift-vcf-migration`.
- Use only existing public repository and documentation URLs.
- Retain the empty scaffold icon entry; do not add branded image data without an approved asset.

## Review Focus

- Placeholder URLs must not survive in source or generated CSV.
- Source and generated CSV metadata must match after bundle generation.
- Description must not claim VMotion execution or vSphere CSI support that current release lacks.
- Existing deployment, permissions, install modes, and channel metadata must remain unchanged.
- Bundle must pass Operator SDK validation.

---

### Task 1: Polish OperatorHub CSV metadata

**Files:**
- Modify: `api/v1alpha1/vmwarecloudfoundationmigration_types.go`
- Modify: `config/manifests/bases/vcf-migration-operator.clusterserviceversion.yaml`
- Modify (generated): `config/crd/bases/migration.openshift.io_vmwarecloudfoundationmigrations.yaml`
- Modify (generated): `bundle/manifests/vcf-migration-operator.clusterserviceversion.yaml`
- Modify (generated): `bundle/manifests/migration.openshift.io_vmwarecloudfoundationmigrations.yaml`

**Interfaces:**
- Consumes: Existing Operator SDK `make bundle` generation path.
- Produces: Catalog-ready CSV metadata rendered by OpenShift OperatorHub.

- [ ] **Step 1: Record failing metadata checks**

Verify current source CSV lacks `operatorframework.io/suggested-namespace`, `repository`, and `support`, and contains `https://vcf-migration-operator.domain`.

- [ ] **Step 2: Update source CSV metadata**

Add accurate summary annotations, richer Markdown `spec.description`, repository/documentation links, provider wording, and focused keywords. Improve the API type godoc that generates the owned-CR description. Retain the empty scaffold icon entry.

- [ ] **Step 3: Regenerate bundle**

Run: `make bundle`

Expected: source metadata appears in `bundle/manifests/vcf-migration-operator.clusterserviceversion.yaml` and generation succeeds.

- [ ] **Step 4: Verify content and generated scope**

Run checks that both CSVs contain suggested namespace, repository, support, and no placeholder domain; inspect `git diff` to ensure no runtime/RBAC/install-mode changes.

Expected: metadata checks pass and diff is limited to design/plan plus CSV/bundle presentation artifacts.

- [ ] **Step 5: Validate bundle**

Run: `bin/operator-sdk bundle validate ./bundle` (or `make bundle`, whose recipe includes validation).

Expected: validation exits 0.

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/specs/2026-11-16-operatorhub-install-page-design.md \
  docs/superpowers/plans/2026-11-16-operatorhub-install-page.md \
  api/v1alpha1/vmwarecloudfoundationmigration_types.go \
  config/manifests/bases/vcf-migration-operator.clusterserviceversion.yaml \
  config/crd/bases/migration.openshift.io_vmwarecloudfoundationmigrations.yaml \
  bundle/manifests/vcf-migration-operator.clusterserviceversion.yaml \
  bundle/manifests/migration.openshift.io_vmwarecloudfoundationmigrations.yaml \
  bundle/metadata/annotations.yaml bundle.Dockerfile
git commit -m "docs: polish OperatorHub install page"
```
