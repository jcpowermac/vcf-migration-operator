ARG BUILD_IMAGE=registry.access.redhat.com/ubi9/go-toolset:latest
ARG RUNTIME_IMAGE=registry.access.redhat.com/ubi9/ubi-micro:latest
FROM ${BUILD_IMAGE} as builder

USER 0
WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
COPY vendor/ vendor/

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build
# VCF Migration Operator only supports x86_64 architecture
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -p=4 -o manager cmd/main.go


# Use ubi-micro as base image to package the manager binary
FROM ${RUNTIME_IMAGE}
WORKDIR /
COPY --from=builder /workspace/manager .
COPY LICENSE /licenses/license.txt

USER 65532:65532
LABEL com.redhat.component="VCF Migration Operator"
LABEL distribution-scope="public"
LABEL name="vcf-migration/vcf-migration-operator"
LABEL release="0.0.1"
LABEL version="0.0.1"
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

ENTRYPOINT ["/manager"]
