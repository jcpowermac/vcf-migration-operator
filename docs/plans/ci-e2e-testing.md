# CI E2E Testing Plan for vcf-migration-operator

## Overview

This document describes the design and implementation plan for adding end-to-end CI testing to the vcf-migration-operator. The tests provision an OpenShift cluster on vCenter A (source), deploy the operator, execute a full vCenter-to-vCenter migration to vCenter B (target), and verify the cluster is healthy post-migration.

The testing infrastructure is built within the OpenShift CI step-registry in the [openshift/release](https://github.com/openshift/release) repository, leveraging the existing vSphere IPI installation workflow and the vSphere Capacity Manager (VCM) for multi-pool resource allocation.

## Architecture

```text
                            VCM Lease (POOL_COUNT=2)
                                    |
                   +----------------+----------------+
                   |                                 |
              Pool A (Source)                   Pool B (Target)
              vCenter A                         vCenter B
                   |                                 |
    +--------------+--------------+                  |
    |                             |                  |
  platform.json              govc.sh           vcf-migration-target-fds.json
  (modified: Pool A only)    (Pool A)          vcf-migration-target-creds.json
    |                             |                  |
    v                             v                  v
  ipi-conf-vsphere-vcm      Cluster Install     Migration Steps
  (install-config with       on vCenter A        (deploy, execute, verify)
   only source FDs)                |                  |
                                   v                  v
                             OCP Cluster  ------>  Migrated Cluster
                             on vCenter A          on vCenter B
```

### Why Not Extend the Existing Multi-vCenter Workflow?

The existing `openshift-e2e-vsphere-multi-vcenter` workflow installs a cluster SPANNING multiple vCenters using failure domains from all pools. VCF migration needs the opposite: install on ONE vCenter, then migrate to the other. This fundamental difference requires a custom configuration chain that intercepts `platform.json` at the data layer rather than forking every downstream step.

### Flow Summary

```text
Phase 1: CONFIGURE (vcf-migration-conf chain)
  1. ipi-conf-vsphere-check          - Validate vSphere environment
  2. ipi-conf-vsphere-check-vcm      - Create VCM Lease with POOL_COUNT=2, wait for fulfillment
  3. vcf-migration-conf-target [NEW]  - Split pools: extract Pool B as target, trim platform.json to Pool A only
  4. ipi-conf-vsphere-vips            - Reserve VIPs (from Pool A's network)
  5. ipi-conf-vsphere-vips-vcm        - VCM VIP allocation
  6. ipi-conf-vsphere-dns             - Create DNS records
  7. ipi-conf                         - Generate base install-config
  8. ipi-conf-telemetry               - Configure telemetry
  9. ipi-conf-vsphere                 - Legacy platform config (no-op for elastic)
 10. ipi-conf-vsphere-vcm             - Generate install-config with platform spec from platform.json (Pool A only)
 11. ipi-install-monitoringpvc        - Configure monitoring PVC

Phase 2: INSTALL (ipi-install-vsphere chain)
  1. ipi-install-rbac                 - Set up RBAC
  2. openshift-cluster-bot-rbac       - Cluster bot RBAC
  3. ipi-install-hosted-loki          - Hosted Loki setup
  4. ipi-install-install              - Run openshift-install
  5. ipi-install-vsphere-registry     - Registry config

Phase 3: TEST
  1. vcf-migration-deploy [NEW]       - Deploy operator with CI-built image
  2. vcf-migration-execute [NEW]      - Create CR and wait for migration
  3. vcf-migration-verify [NEW]       - Verify post-migration health

Phase 4: TEARDOWN (ipi-vsphere-post chain)
  1. gather                           - Collect logs and must-gather
  2. ipi-deprovision-vsphere-diags    - Diagnostics
  3. ipi-deprovision-vsphere-diags-vcm - VCM diagnostics
  4. ipi-deprovision-deprovision      - Destroy cluster
  5. ipi-deprovision-vsphere-dns      - Remove DNS records
  6. ipi-deprovision-vsphere-lb       - Remove load balancer
  7. ipi-deprovision-vsphere-lease    - Release VCM lease
```

## Custom Step Registry Components

All new files are created under `ci-operator/step-registry/vcf-migration/` in the openshift/release repository.

### 1. vcf-migration-conf-target (ref step)

**Purpose**: Separate a multi-pool VCM lease into source (install) and target (migration) pools.

**Location**: `ci-operator/step-registry/vcf-migration/conf/target/`

**What it does**:
1. Reads `${SHARED_DIR}/platform.json` (contains ALL pools' failure domains and vCenters)
2. Identifies the first pool's vCenter as the "source" (install target)
3. Extracts all failure domains NOT on the source vCenter as "target" migration destinations
4. Saves target data to SHARED_DIR:
   - `vcf-migration-target-fds.json` -- JSON array of target failure domain specs
   - `vcf-migration-target-creds.json` -- Target vCenter username and password
   - `vcf-migration-target-vcenter.txt` -- Target vCenter FQDN
5. Modifies `platform.json` in place to contain ONLY the source vCenter's failure domains and vCenter entry
6. Regenerates `platform.yaml` from the modified `platform.json`
7. Copies the target pool's govc file to `${SHARED_DIR}/govc_target.sh` for convenience

**Critical design note**: This step MUST run after `ipi-conf-vsphere-check-vcm` (which creates platform.json with all pools) and BEFORE `ipi-conf-vsphere-vcm` (which reads platform.json to generate install-config). By modifying platform.json in between, we ensure the cluster installs on only the source vCenter.

**Source-pool identification**: The step must use `govc.sh` (or `vsphere_context.sh`) to determine the source vCenter FQDN rather than assuming `platform.json`'s first failure domain is the source. The `check-vcm` step writes `govc.sh` from `LEASE_single.json`, which is the authoritative source pool. This ensures alignment between the pool used for installation and the pool kept in `platform.json`.

**Key logic for extracting target pool credentials**:
```bash
# Determine source vCenter from govc.sh (authoritative — written from LEASE_single.json)
source "${SHARED_DIR}/govc.sh"
source_server="${GOVC_URL}"

# The check-vcm step creates per-pool govc files named govc_{sanitized_pool_name}.sh
# Each contains GOVC_USERNAME, GOVC_PASSWORD, and other env vars for that pool
# We iterate through lease poolInfo to find the non-source pool and extract its credentials

for _leaseJSON in "${SHARED_DIR}"/LEASE*; do
  [[ "${_leaseJSON}" =~ "single" ]] && continue
  pool_count=$(jq -r '.status.poolInfo | length' < "${_leaseJSON}")
  for ((idx = 0; idx < pool_count; idx++)); do
    pool_server=$(jq -r ".status.poolInfo[${idx}].server" < "${_leaseJSON}")
    if [ "${pool_server}" != "${source_server}" ]; then
      pool_name=$(jq -r ".status.poolInfo[${idx}].name" < "${_leaseJSON}")
      # Extract only the expected GOVC keys for this pool; do not source lease content
      pool_envvars=$(jq -c --arg pool_name "${pool_name}" '.status.envVarsMap[$pool_name]' < "${_leaseJSON}")
      GOVC_USERNAME=$(jq -r '.GOVC_USERNAME // empty' <<< "${pool_envvars}")
      GOVC_PASSWORD=$(jq -r '.GOVC_PASSWORD // empty' <<< "${pool_envvars}")
      GOVC_URL=$(jq -r '.GOVC_URL // empty' <<< "${pool_envvars}")
      GOVC_INSECURE=$(jq -r '.GOVC_INSECURE // empty' <<< "${pool_envvars}")

      for required_var in GOVC_USERNAME GOVC_PASSWORD GOVC_URL GOVC_INSECURE; do
        if [ -z "${!required_var}" ]; then
          echo "ERROR: missing ${required_var} for pool ${pool_name} in ${_leaseJSON}"
          exit 1
        fi
      done

      jq -n \
        --arg username "${GOVC_USERNAME}" \
        --arg password "${GOVC_PASSWORD}" \
        '{username: $username, password: $password}' > "${SHARED_DIR}/vcf-migration-target-creds.json"
      printf '%s\n' "${GOVC_URL}" > "${SHARED_DIR}/vcf-migration-target-vcenter.txt"
    fi
  done
done

# Fail fast if both pools are on the same vCenter
target_server=$(cat "${SHARED_DIR}/vcf-migration-target-vcenter.txt")
if [ "${source_server}" == "${target_server}" ]; then
  echo "ERROR: source and target vCenters are the same (${source_server}). Need distinct vCenters."
  exit 1
fi
```

**Template/folder enrichment**: The operator requires `topology.template` in target failure domains. The `vcf-migration-conf-target` step must enrich the target failure domain data with a template value. The RHCOS template path is available from the source cluster's existing MachineSets (or from the `SHARED_DIR` install artifacts). Since the target vCenter will have a matching RHCOS template (provisioned by CI infrastructure), the step should construct the template path using the target datacenter and a known convention (e.g., `/<datacenter>/vm/<template-name>`). Alternatively, the execute step can query the target vCenter via govc to discover available templates.

**Image**: `upi-installer` (has `jq`, `oc`, `govc`).

**Credentials needed**: `vsphere-ibmcloud-ci` (for TLS certs and VCM kubeconfig).

### 2. vcf-migration-deploy (ref step)

**Purpose**: Deploy the vcf-migration-operator to the test cluster.

**Location**: `ci-operator/step-registry/vcf-migration/deploy/`

**What it does**:
1. Sets `KUBECONFIG=${SHARED_DIR}/kubeconfig`
2. Uses the operator source tree (available via `from: src`)
3. Runs `make install` to apply CRDs to the cluster
4. Runs `make deploy IMG=${VCF_MIGRATION_OPERATOR_IMAGE}` to deploy the operator with the CI-built image
5. Waits for the deployment `vcf-migration-operator-controller-manager` in namespace `openshift-vcf-migration` to become Available

**Image**: `src` (contains operator source code, Go toolchain, make).

**Dependencies**:
- `pipeline:vcf-migration-operator` -> env var `VCF_MIGRATION_OPERATOR_IMAGE`

This pattern is standard for operator CI testing. The CI-operator builds the operator image from the `Dockerfile` (configured in the ci-operator config `images` section), and the step injects the resulting image reference via a dependency declaration.

### 3. vcf-migration-execute (ref step)

**Purpose**: Create the migration CR and wait for completion.

**Location**: `ci-operator/step-registry/vcf-migration/execute/`

**What it does**:
1. Sets `KUBECONFIG=${SHARED_DIR}/kubeconfig`
2. Reads target vCenter info from SHARED_DIR artifacts
3. Creates the target vCenter credentials Secret in the operator namespace:
   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: target-vcenter-creds
     namespace: openshift-vcf-migration
   type: Opaque
   data:
     ${target_vcenter}.username: <base64>
     ${target_vcenter}.password: <base64>
   ```
4. Constructs and applies a `VmwareCloudFoundationMigration` CR:
   ```yaml
   apiVersion: migration.openshift.io/v1alpha1
   kind: VmwareCloudFoundationMigration
   metadata:
     name: vcf-migration-e2e
     namespace: openshift-vcf-migration
   spec:
     state: Running
     targetVCenterCredentialsSecret:
       name: target-vcenter-creds
       namespace: openshift-vcf-migration
     failureDomains:
       # Populated from vcf-migration-target-fds.json
       - name: <target-fd-name>
         region: <target-region>
         zone: <target-zone>
         server: <target-vcenter-fqdn>
         topology:
           datacenter: <target-dc>
           computeCluster: <target-cluster>
           datastore: <target-datastore>
           networks:
             - <target-portgroup>
           resourcePool: <target-resource-pool>
           template: /<target-dc>/vm/<target-rhcos-template>
   ```
5. Waits for each of the 6 conditions sequentially:
   - `InfrastructurePrepared`
   - `DestinationInitialized`
   - `MultiSiteConfigured`
   - `WorkloadMigrated`
   - `SourceCleaned`
   - `Ready`
6. On timeout or failure, dumps operator logs and CR status for debugging

**Image**: `upi-installer` (has `oc`, `jq`).

**Environment variables**:
- `VCF_MIGRATION_TIMEOUT` (default: `"7200"`) -- timeout in seconds for migration completion.

### 4. vcf-migration-verify (ref step)

**Purpose**: Verify post-migration cluster health and correctness.

**Location**: `ci-operator/step-registry/vcf-migration/verify/`

**What it does**:
1. Sets `KUBECONFIG=${SHARED_DIR}/kubeconfig`
2. Reads target vCenter FQDN from `${SHARED_DIR}/vcf-migration-target-vcenter.txt`
3. Verifies:
   - All ClusterOperators are Available=True, Progressing=False, Degraded=False
   - All nodes are Ready
   - The Infrastructure CR (`oc get infrastructure cluster`) references ONLY the target vCenter in `spec.platformSpec.vsphere.vcenters`
   - All Machines in `openshift-machine-api` have `spec.providerSpec.value.workspace.server` pointing to the target vCenter
   - The VmwareCloudFoundationMigration CR has `Ready=True` condition
4. Outputs a summary of the verification results

**Image**: `upi-installer` (has `oc`, `jq`).

### 5. vcf-migration-conf (chain)

**Location**: `ci-operator/step-registry/vcf-migration/conf/`

Identical to the standard `ipi-conf-vsphere` chain with `vcf-migration-conf-target` inserted after `ipi-conf-vsphere-check-vcm`:

1. `ipi-conf-vsphere-check`
2. `ipi-conf-vsphere-check-vcm` (with `POOL_COUNT=2` from workflow env)
3. **`vcf-migration-conf-target`** (NEW)
4. `ipi-conf-vsphere-vips`
5. `ipi-conf-vsphere-vips-vcm`
6. `ipi-conf-vsphere-dns`
7. `ipi-conf`
8. `ipi-conf-telemetry`
9. `ipi-conf-vsphere`
10. `ipi-conf-vsphere-vcm`
11. `ipi-install-monitoringpvc`

### 6. vcf-migration-pre (chain)

**Location**: `ci-operator/step-registry/vcf-migration/pre/`

1. `vcf-migration-conf` chain
2. `rhcos-conf-osstream`
3. `ipi-install-vsphere` chain

### 7. vcf-migration-e2e (workflow)

**Location**: `ci-operator/step-registry/vcf-migration/e2e/`

```yaml
workflow:
  as: vcf-migration-e2e
  steps:
    pre:
    - chain: vcf-migration-pre
    test:
    - ref: vcf-migration-deploy
    - ref: vcf-migration-execute
    - ref: vcf-migration-verify
    post:
    - chain: gather-network
    - chain: gather-core-dump
    - chain: ipi-vsphere-post
    env:
      FEATURE_GATES: '["VSphereMultiVCenterDay2=true"]'
      FEATURE_SET: CustomNoUpgrade
      POOLS: >-
        vcenter.ci.ibmc.devcluster.openshift.com-cidatacenter-cicluster
        vcenter-1.ci.ibmc.devcluster.openshift.com-cidatacenter-2-cicluster-3
```

### Feature Gate: VSphereMultiVCenterDay2

The operator's preflight checks (`internal/controller/preflight.go`) require the `VSphereMultiVCenterDay2` feature gate to be enabled on the cluster. Without it, the API server's validation rules prevent modifying the Infrastructure CR's `vcenters` list post-install (adding the target vCenter, later removing the source). This is a different feature gate from `VSphereMultiVCenters` (day-1), which is about installing a cluster spanning multiple vCenters at install time.

The `ipi-conf` step reads `FEATURE_SET` and `FEATURE_GATES` env vars and writes them into `install-config.yaml`. Setting `FEATURE_SET: CustomNoUpgrade` with `FEATURE_GATES: '["VSphereMultiVCenterDay2=true"]'` enables the gate at install time so the operator can proceed with migration.

### Pool Selection: Explicit POOLS (not POOL_COUNT)

Existing multi-vCenter jobs use explicit `POOLS` with named pools to guarantee cross-vCenter coverage. `POOL_COUNT=2` alone does NOT guarantee different vCenters -- VCM's underutilized allocation strategy might assign both pools from the same vCenter. By naming two pools known to be on different vCenters, we get a deterministic source/target split:

- `vcenter.ci.ibmc.devcluster.openshift.com-cidatacenter-cicluster` -- vCenter 1 (source, install target)
- `vcenter-1.ci.ibmc.devcluster.openshift.com-cidatacenter-2-cicluster-3` -- vCenter 2 (migration target)

This matches the pattern used by `openshift-e2e-vsphere-multi-vcenter` workflows in nightly release configs.

## CI-Operator Config Changes

### File: `ci-operator/config/openshift/vcf-migration-operator/openshift-vcf-migration-operator-main.yaml`

Add these test definitions:

```yaml
# Presubmit (optional, rehearsable)
- as: e2e-vsphere-vcf-migration
  optional: true
  skip_if_only_changed: ^docs/|\.md$|^(?:.*/)?(?:\.gitignore|OWNERS|PROJECT|LICENSE)$
  steps:
    cluster_profile: vsphere-elastic
    dependencies:
      VCF_MIGRATION_OPERATOR_IMAGE: pipeline:vcf-migration-operator
    workflow: vcf-migration-e2e

# Periodic (runs Monday and Thursday at 06:00 UTC)
- as: e2e-vsphere-vcf-migration-periodic
  cron: 0 6 * * 1,4
  steps:
    cluster_profile: vsphere-elastic
    dependencies:
      VCF_MIGRATION_OPERATOR_IMAGE: pipeline:vcf-migration-operator
    workflow: vcf-migration-e2e
```

The `cluster_profile: vsphere-elastic` triggers VCM-based resource allocation. The workflow sets `POOLS` with two named pools on different vCenters, `FEATURE_SET: CustomNoUpgrade`, and `FEATURE_GATES` with `VSphereMultiVCenterDay2=true`. The `dependencies` mapping wires the CI-built operator image to the `VCF_MIGRATION_OPERATOR_IMAGE` env var consumed by the deploy step.

## SHARED_DIR Artifact Map

| Artifact | Created By | Consumed By | Description |
|----------|-----------|-------------|-------------|
| `platform.json` | `check-vcm` (then modified by `conf-target`) | `ipi-conf-vsphere-vcm` | Platform spec with ONLY source pool failure domains |
| `platform.yaml` | `conf-target` (regenerated) | `ipi-conf-vsphere-vcm` | YAML version of platform.json |
| `vcf-migration-target-fds.json` | `conf-target` | `execute` | Target failure domain(s) array |
| `vcf-migration-target-creds.json` | `conf-target` | `execute` | Target vCenter username and password |
| `vcf-migration-target-vcenter.txt` | `conf-target` | `execute`, `verify` | Target vCenter FQDN |
| `govc_target.sh` | `conf-target` | (optional future use) | govc env for target vCenter |
| `govc.sh` | `check-vcm` | various | govc env for source (first) pool |
| `govc_{pool_name}.sh` | `check-vcm` | `conf-target` | Per-pool govc environments |
| `LEASE_*.json` | `check-vcm` | `conf-target` | Lease details with poolInfo array |
| `kubeconfig` | `ipi-install-install` | `deploy`, `execute`, `verify` | Cluster kubeconfig |

## File Listing

All files to be created in the openshift/release repository under `ci-operator/step-registry/`:

```text
vcf-migration/
  conf/
    target/
      vcf-migration-conf-target-commands.sh
      vcf-migration-conf-target-ref.yaml
      vcf-migration-conf-target-ref.metadata.json
      OWNERS
    vcf-migration-conf-chain.yaml
    vcf-migration-conf-chain.metadata.json
    OWNERS
  pre/
    vcf-migration-pre-chain.yaml
    vcf-migration-pre-chain.metadata.json
    OWNERS
  deploy/
    vcf-migration-deploy-commands.sh
    vcf-migration-deploy-ref.yaml
    vcf-migration-deploy-ref.metadata.json
    OWNERS
  execute/
    vcf-migration-execute-commands.sh
    vcf-migration-execute-ref.yaml
    vcf-migration-execute-ref.metadata.json
    OWNERS
  verify/
    vcf-migration-verify-commands.sh
    vcf-migration-verify-ref.yaml
    vcf-migration-verify-ref.metadata.json
    OWNERS
  e2e/
    vcf-migration-e2e-workflow.yaml
    vcf-migration-e2e-workflow.metadata.json
    OWNERS
```

Plus the CI-operator config update:
```text
ci-operator/config/openshift/vcf-migration-operator/openshift-vcf-migration-operator-main.yaml
```

## Environment Variables

| Variable | Default | Set Where | Used By | Description |
|----------|---------|-----------|---------|-------------|
| `POOLS` | -- | Workflow env | `check-vcm` | Two named pools on different vCenters |
| `FEATURE_SET` | -- | Workflow env | `ipi-conf` | Must be `CustomNoUpgrade` for feature gate |
| `FEATURE_GATES` | -- | Workflow env | `ipi-conf` | `'["VSphereMultiVCenterDay2=true"]'` |
| `CLUSTER_PROFILE_NAME` | -- | CI config `cluster_profile` | `check-vcm` | Must be `vsphere-elastic` for VCM |
| `VCF_MIGRATION_OPERATOR_IMAGE` | -- | Dependency resolution | `deploy` | CI-built operator image pullspec |
| `VCF_MIGRATION_TIMEOUT` | `"7200"` | Step env | `execute` | Migration timeout in seconds |

## Design Decisions

### Why modify platform.json in place?

All downstream steps (`ipi-conf-vsphere-vcm`, etc.) read `platform.json` to generate install-config.yaml. Rather than forking every downstream step, we intercept at the data layer: the `vcf-migration-conf-target` step modifies `platform.json` to only contain source pool data before the downstream steps run.

### Why use from: src for the deploy step?

The operator uses kustomize (via Makefile) for deployment. Using `from: src` gives us the full source tree including `config/` manifests and `Makefile`. The CI-built image is injected via the `VCF_MIGRATION_OPERATOR_IMAGE` dependency, which is resolved by ci-operator from the `pipeline:vcf-migration-operator` imagestream tag.

### Why separate deploy/execute/verify steps?

Separation provides:
1. Clear attribution of failures (deployment vs. migration vs. verification)
2. Individual step timeouts appropriate to each phase
3. Reusability -- the verify step could be used in other contexts
4. Debuggability -- each step's logs are separately available in Prow artifacts

## Review Comments (Resolved)

These review notes came from checking the current `openshift/release` vSphere step-registry implementation and the current `vcf-migration-operator` controller behavior. All have been addressed in the plan above.

1. **Failure domain `template` is a blocking input** -- RESOLVED: The `vcf-migration-conf-target` step description now includes template/folder enrichment. The step will construct or discover the RHCOS template path on the target vCenter. This remains the trickiest implementation detail and should be validated early.

2. **CI config missing dependency wiring** -- RESOLVED: The CI config examples now include `steps.dependencies` mapping `VCF_MIGRATION_OPERATOR_IMAGE: pipeline:vcf-migration-operator`.

3. **Feature-gate enablement** -- RESOLVED: The workflow now sets `FEATURE_SET: CustomNoUpgrade` and `FEATURE_GATES: '["VSphereMultiVCenterDay2=true"]'`. This is a day-2 feature gate distinct from the day-1 `VSphereMultiVCenters` used by existing multi-vCenter install jobs.

4. **Source-pool alignment with VCM artifacts** -- RESOLVED: The `vcf-migration-conf-target` step now keys source identification off `govc.sh` (which is written from `LEASE_single.json` by `check-vcm`) rather than assuming `platform.json[0]`. This guarantees alignment with downstream steps.

5. **`POOL_COUNT=2` insufficient for distinct vCenters** -- RESOLVED: The workflow now uses explicit `POOLS` with two named pools on different vCenters, matching the pattern used by existing nightly multi-vCenter release configs. A fail-fast check in `vcf-migration-conf-target` validates the pools are on different vCenters.

## Open Questions / Future Work

1. **Template/folder discovery**: The operator requires `topology.template` in target failure domains. The `vcf-migration-conf-target` step needs a concrete strategy: construct the path from a naming convention, or query the target vCenter via govc to discover available RHCOS templates. This is a blocking implementation detail.

2. **Gather step**: Should we add a `vcf-migration-gather` step to the post phase that collects operator logs, CR status, and events from the `openshift-vcf-migration` namespace before teardown?

3. **Migration timeout tuning**: The default 2-hour timeout may need adjustment based on observed migration times.

4. **Network type**: Does VCF migration testing require a specific network type (e.g., `single-tenant` vs `multi-tenant`)?

5. **Upgrade testing**: Future consideration -- testing migration as part of an upgrade workflow.

6. **Periodic frequency**: The proposed periodic runs Monday and Thursday at 06:00 UTC. Adjust based on resource availability and team preferences.

7. **CustomNoUpgrade implications**: Using `FEATURE_SET: CustomNoUpgrade` prevents cluster upgrades. If future testing needs to combine migration with upgrade workflows, a different approach to enabling the feature gate may be needed (or wait for the gate to graduate to GA).
