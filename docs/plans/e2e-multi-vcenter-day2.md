# Plan: Add E2E Test for vSphere Multi-vCenter Day 2 (Add 2nd vCenter)

## Context

The vSphere Multi-vCenter Day 2 feature (`VSphereMultiVCenterDay2` feature gate) allows adding/removing vCenters from a running OpenShift cluster. The vcf-migration-operator already implements the full migration workflow — adding a 2nd vCenter is one of its core stages (`ensureMultiSiteConfigured`). We want an e2e test that exercises this flow on a real cluster, verifying that the operator correctly updates the Infrastructure CR, cloud-provider-config, and that CCCMO picks up the change and produces the correct `kube-cloud-config` in `openshift-config-managed`.

This test uses `openshift-tests-extension` (OTE) so it can be discovered and run by the centralized `openshift-tests` runner. The feature gate `[OCPFeatureGate:VSphereMultiVCenterDay2]` is embedded in the Describe block so the test only runs when the gate is enabled.

The test assumes a `VmwareCloudFoundationMigration` CR already exists in the cluster (pre-created by CI/test infrastructure) with target failure domains and a credentials secret populated. The test sets the CR to Running and monitors the operator-driven multi-site configuration, then verifies the full pipeline: `cloud-provider-config` (source of truth in `openshift-config`), `kube-cloud-config` (CCCMO-managed in `openshift-config-managed`), `cloud-conf` (CCM-consumed in `openshift-cloud-controller-manager`), and CCCMO pod health.

---

## Implementation Steps

### 1. Add `openshift-tests-extension` dependency

Run `go get github.com/openshift-eng/openshift-tests-extension@latest` to add the OTE dependency to `go.mod`.

Also add the OpenShift Ginkgo fork replacement in `go.mod` (required for `[OCPFeatureGate:...]` parsing):
```text
replace github.com/onsi/ginkgo/v2 => github.com/openshift/onsi-ginkgo/v2 <version>
```

Note: We need to check the latest version of the openshift/onsi-ginkgo fork. We can look at what MAO or cluster-api-actuator-pkg uses.

### 2. Create OTE entry point: `cmd/openshift-tests-extension/main.go`

Following the OTE pattern from the example:
- Create extension registry
- Create extension: `e.NewExtension("openshift", "payload", "vcf-migration-operator")`
- Add suite with parent `openshift/conformance/parallel`
- Build specs from Ginkgo: `g.BuildExtensionTestSpecsFromOpenShiftGinkgoSuite()`
- Add platform filter: `specs.Select(et.NameContains("[platform:vsphere]")).Include(et.PlatformEquals("vsphere"))`
- Register and wire Cobra CLI

### 3. Create the e2e test file: `test/e2e/vsphere/multi_vcenter_day2.go`

**Package**: `vsphere` (not `vsphere_test` — OTE discovers non-`_test.go` files via blank import)

**Describe block**:
```go
var _ = Describe("[sig-cluster-lifecycle][OCPFeatureGate:VSphereMultiVCenterDay2][platform:vsphere] Adding a second vCenter via VmwareCloudFoundationMigration",
    Label("Conformance"), func() {
```

**BeforeEach** setup:
- Load kubeconfig via `controller-runtime` client config (this project uses controller-runtime, not `k8s.io/kubernetes/test/e2e/framework`)
- Create Kubernetes clientset, OpenShift config client, and controller-runtime client
- Fetch Infrastructure CR (`cluster`)
- Skip if not vSphere platform
- Skip if `VSphereMultiVCenterDay2` feature gate is not enabled (reuse `openshift.GetVSphereMultiVCenterSupport()`)
- Fetch existing VmwareCloudFoundationMigration CR (expect exactly one; skip if none found)
- Record initial state: number of vCenters in Infrastructure, content of cloud-provider-config

**Test case: "should configure the cluster for multi-vCenter operation"**:

1. **Capture baseline**: Record the current Infrastructure CR vCenters count (expect 1), read `cloud-provider-config` from `openshift-config`, read `kube-cloud-config` from `openshift-config-managed`

2. **Trigger migration**: Set the Migration CR's `spec.state` to `Running` and update it

3. **Wait for MultiSiteConfigured condition**: Poll the Migration CR status until `MultiSiteConfigured` condition is `True` (timeout ~10 minutes). Use `Eventually` with a polling interval.

4. **Verify Infrastructure CR**: Fetch Infrastructure CR, assert:
   - `spec.platformSpec.vsphere.vcenters` has 2+ entries
   - The new vCenter server matches the migration spec's failure domain server
   - New failure domains are present

5. **Verify cloud-provider-config** (source ConfigMap in `openshift-config`):
   - Parse the YAML from `data.config`
   - Assert the `vcenter` map has entries for both old and new servers
   - Each entry has correct datacenters

6. **Verify kube-cloud-config** (CCCMO-managed in `openshift-config-managed`):
   - Poll until the ConfigMap reflects both vCenters (CCCMO needs time to reconcile)
   - Parse the cloud config content
   - Assert it contains entries for both vCenter servers

7. **Verify cloud-conf** (CCM-consumed in `openshift-cloud-controller-manager`):
   - Poll until the ConfigMap in this namespace reflects both vCenters
   - This confirms the full pipeline: operator -> cloud-provider-config -> CCCMO -> kube-cloud-config -> CCM

8. **Verify CCCMO pod health**:
   - List pods in `openshift-cloud-controller-manager-operator` namespace
   - Assert pods are Running and Ready
   - List pods in `openshift-cloud-controller-manager` namespace (CCM pods)
   - Assert pods are Running and Ready

### 4. Create test helpers: `test/e2e/vsphere/helpers.go`

Utility functions for the test:
- `getKubeCloudConfig(ctx, kubeClient)` — fetches `kube-cloud-config` from `openshift-config-managed`
- `getCloudConf(ctx, kubeClient)` — fetches ConfigMap from `openshift-cloud-controller-manager`
- `parseCloudConfig(cm)` — parses the vSphere cloud config YAML and returns vcenter server list
- `getMigrationCR(ctx, client)` — finds the VmwareCloudFoundationMigration CR
- `waitForCondition(ctx, client, name, namespace, conditionType, timeout)` — polls Migration CR for condition
- `checkPodsHealthy(ctx, kubeClient, namespace, labelSelector)` — verifies pods are Running/Ready

Note: Reuse existing helpers where possible — `openshift.ConfigMapManager.parseCloudProviderConfig` pattern for YAML parsing, `openshift.PodManager` patterns for pod checks.

### 5. Wire the test package into OTE entry point

In `cmd/openshift-tests-extension/main.go`, add blank import:
```go
_ "github.com/openshift/vcf-migration-operator/test/e2e/vsphere"
```

### 6. Add Makefile target

Add a `test-e2e-vsphere` target (or similar) to build and run the OTE binary:
```makefile
.PHONY: openshift-tests-extension
openshift-tests-extension:
	go build -o bin/openshift-tests-extension ./cmd/openshift-tests-extension/
```

---

## Files to Create/Modify

| File | Action | Description |
|------|--------|-------------|
| `go.mod` | Modify | Add `openshift-tests-extension` dependency, potentially add ginkgo fork replace |
| `cmd/openshift-tests-extension/main.go` | Create | OTE entry point with registry, extension, suite, specs |
| `test/e2e/vsphere/multi_vcenter_day2.go` | Create | Main test: Describe + BeforeEach + It blocks |
| `test/e2e/vsphere/helpers.go` | Create | Test utility functions |
| `Makefile` | Modify | Add build target for OTE binary |

---

## Key Patterns to Follow

- **Feature gate in Describe**: `[OCPFeatureGate:VSphereMultiVCenterDay2]` — the openshift ginkgo fork parses this and skips if the gate isn't enabled
- **Platform tag**: `[platform:vsphere]` — standard OTE filtering
- **SIG tag**: `[sig-cluster-lifecycle]` — matches the MAO pattern
- **Labels**: `Label("Conformance")` for suite membership
- **Client setup**: Use `controller-runtime` rest config (consistent with the operator itself) rather than `k8s.io/kubernetes/test/e2e/framework` which is a heavy dependency
- **Assertions**: Gomega `Eventually` for polling, `Expect` for immediate checks
- **Logging**: `GinkgoWriter` for test output, `By()` for step documentation
- **Existing helpers**: Reuse types and parsing from `internal/openshift/configmaps.go` (the `cloudProviderConfigYAML` struct and YAML parsing)

---

## Verification

1. **Build**: `go build ./cmd/openshift-tests-extension/` compiles without errors
2. **List tests**: `./bin/openshift-tests-extension list` shows the test with correct tags
3. **Run on cluster**: On a vSphere cluster with the feature gate enabled and a pre-created Migration CR:
   ```
   ./bin/openshift-tests-extension run --suite example/tests
   ```
4. **Lint**: `make lint` passes
5. **Unit tests**: `make test` still passes (no regressions)
