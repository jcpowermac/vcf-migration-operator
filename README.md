# vcf-migration-operator

Kubernetes operator and OpenShift Console plugin for migrating OpenShift clusters between VMware vCenters (e.g. VMware Cloud Foundation environments).

The operator reconciles the `VmwareCloudFoundationMigration` custom resource through a multi-phase workflow: preflight validation, destination initialization, multi-site configuration, workload migration (workers + control plane), source cleanup, and final health checks. The optional console plugin provides a web UI for creating migrations, browsing vCenter inventory, and monitoring progress.

## Documentation

- **User Guide**
  - [Install with OLM](docs/user/install-with-olm.md)
  - [Install without OLM](docs/user/install-without-olm.md)

- **Developer Guide**
  - [API Reference](docs/dev/api.md) — CR spec, status, condition types
  - [Architecture](docs/dev/architecture.md) — package layout, reconciliation flow, requeue strategy
  - [Development](docs/dev/development.md) — build, test, deploy, code generation, CI

## Quick Start

```bash
# Build and deploy the operator
make operator-image operator-push IMG=<registry>/vcf-migration-operator:latest
make deploy IMG=<registry>/vcf-migration-operator:latest

# Apply a sample migration
kubectl apply -k config/samples/
```

See the [user docs](docs/user/) for full installation instructions and the [development guide](docs/dev/development.md) for building from source.

## Contributing

Contributions are welcome. Run `make help` for all available targets. See also `AGENTS.md` for build, lint, and test commands.

## License

Copyright 2026. Licensed under the [Apache License, Version 2.0](http://www.apache.org/licenses/LICENSE-2.0).
