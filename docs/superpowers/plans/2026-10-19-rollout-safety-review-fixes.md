# Rollout safety review fixes

## Approved scope

Reject stale MCP status, establish restart-safe evidence that MCO consumed migration configuration before CPMS updates, add regression coverage, and correct the collision-risk analysis. Do not change the API or use time-based delays as evidence.

## Implementation sequence

1. Add manager tests for an unobserved MCP generation and mismatched desired/current rendered configuration. Verify failures, then tighten CheckPoolsConverged; both workload and final readiness callers inherit the checks.
2. Trace the MCO ControllerConfig -> template -> render pipeline for the supported OpenShift version. Compare the current cloud-provider ConfigMap to ControllerConfig.spec.cloudProviderConfig and require observed generation and completed synchronization. Determine and test evidence that generated MachineConfigs have propagated into the MCP desired configuration; ControllerConfig synchronization alone must not be assumed to prove render completion.
3. Gate CPMS mutation on that propagation evidence and current pool convergence. Add controller regressions that hold CPMS unchanged for stale status, unconsumed migration config, and incomplete render propagation, and permit updates once all evidence agrees. Include missing-resource, API-error and restart behavior.
4. Correct failure-domain-mapping-analysis.md to explain that source Machines can satisfy name/label-based readiness and allow destructive source scale-down; distinguish this pre-existing issue from the branch changes.
5. Run focused openshift and controller tests, format/lint affected code, inspect diff, and request independent review only if separately authorized.

## Safety constraint

A fresh healthy MCP snapshot is necessary but not sufficient: a pending upstream render can leave both spec and status on the old configuration. Do not describe freshness checks alone as closing the rollout race.
