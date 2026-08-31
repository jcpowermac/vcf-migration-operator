# The builder image is expected to contain /bin/opm (with serve subcommand)
FROM registry.redhat.io/openshift4/ose-operator-registry-rhel9:v4.18 as builder

# Copy FBC root into image at /configs and pre-populate serve cache
COPY catalog /configs
RUN ["/bin/opm", "serve", "/configs", "--cache-dir=/tmp/cache", "--cache-only"]

FROM registry.redhat.io/openshift4/ose-operator-registry-rhel9:v4.18

# Configure the entrypoint and command
ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs", "--cache-dir=/tmp/cache"]

COPY LICENSE /licenses/license.txt
COPY --from=builder /configs /configs
COPY --from=builder /tmp/cache /tmp/cache

# Set FBC-specific label for the location of the FBC root directory in the image
LABEL operators.operatorframework.io.index.configs.v1=/configs
LABEL com.redhat.component="VCF Migration Operator Catalog"
LABEL distribution-scope="public"
LABEL name="vcf-migration/vcf-migration-operator-catalog"
LABEL release="0.0.1"
LABEL version="0.0.1"
LABEL cpe="cpe:/a:redhat:vcf_migration_operator:0.1::el9"
LABEL url="https://github.com/openshift/vcf-migration-operator"
LABEL vendor="Red Hat, Inc."
LABEL description="File-based catalog for the VCF Migration Operator."
LABEL summary="File-based catalog for the VCF Migration Operator."
LABEL io.k8s.display-name="VCF Migration Operator Catalog"
