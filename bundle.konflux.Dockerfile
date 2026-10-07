FROM registry.access.redhat.com/ubi9/go-toolset:latest as builder
ARG IMG=registry.redhat.io/vcf-migration-operator-tech-preview/vcf-migration-rhel9-operator@sha256:ac859669b5d436a0900e12eff08d2d96fec0424a8638be4964bae0a225b3cc0f
ARG ORIGINAL_IMG=registry.ci.openshift.org/origin/vcf-migration-operator:latest
WORKDIR /code
COPY ./ ./

# Fail fast if IMG was not overridden from its placeholder default.
RUN echo "${IMG}" | grep -q '@sha256:placeholder' && \
    { echo "ERROR: IMG contains placeholder digest; override IMG with a valid image reference."; exit 1; } || true

# Replace the bundle image in the repository with the one specified by the IMG build argument.
RUN cp -r bundle /tmp/bundle && \
    find /tmp/bundle -type f -exec sed -i \
    "s|${ORIGINAL_IMG}|${IMG}|g" {} \+; \
    grep -rq "${ORIGINAL_IMG}" /tmp/bundle/ && \
    { echo "Failed to replace image references"; exit 1; } || echo "Image references replaced" && \
    grep -r "${IMG}" /tmp/bundle/

FROM scratch
# Core bundle labels.
LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1=vcf-migration-operator
LABEL operators.operatorframework.io.bundle.channels.v1=dev-preview
LABEL operators.operatorframework.io.bundle.channel.default.v1=dev-preview
LABEL operators.operatorframework.io.metrics.builder=operator-sdk-v1.42.0
LABEL operators.operatorframework.io.metrics.mediatype.v1=metrics+v1
LABEL operators.operatorframework.io.metrics.project_layout=go.kubebuilder.io/v4

# Labels for testing.
LABEL operators.operatorframework.io.test.mediatype.v1=scorecard+v1
LABEL operators.operatorframework.io.test.config.v1=tests/scorecard/

# Copy files to locations specified by labels.
COPY --from=builder --chown=1001:0 /tmp/bundle/manifests /manifests/
COPY --from=builder --chown=1001:0 /tmp/bundle/metadata /metadata/
COPY --from=builder --chown=1001:0 /tmp/bundle/tests/scorecard /tests/scorecard/
USER 1001:0

# Labels from hack/patch-bundle-dockerfile.sh
LABEL com.redhat.component="VCF Migration Operator"
LABEL distribution-scope="public"
LABEL name="vcf-migration-operator-tech-preview/vcf-migration-operator-bundle"
LABEL release="0.1.0"
LABEL version="0.1.0"
LABEL cpe="cpe:/a:redhat:vcf_migration_operator:0.1::el9"
LABEL url="https://github.com/openshift/vcf-migration-operator"
LABEL vendor="Red Hat, Inc."
LABEL description="The VCF Migration Operator automates migrating OpenShift clusters between VMware vCenters \
                   (e.g. VMware Cloud Foundation / VCF), orchestrating infrastructure preparation, multi-site \
                   configuration, machine migration, and source cleanup."
LABEL io.k8s.description="The VCF Migration Operator automates migrating OpenShift clusters between VMware vCenters \
                   (e.g. VMware Cloud Foundation / VCF), orchestrating infrastructure preparation, multi-site \
                   configuration, machine migration, and source cleanup."
LABEL summary="The VCF Migration Operator automates migrating OpenShift clusters between VMware vCenters \
                   (e.g. VMware Cloud Foundation / VCF), orchestrating infrastructure preparation, multi-site \
                   configuration, machine migration, and source cleanup."
LABEL io.k8s.display-name="VCF Migration Operator"
LABEL io.openshift.tags="openshift,operator,vcf,migration,vsphere"
