#!/bin/bash

# Extract major.minor version for CPE label (e.g., 0.1.0 -> 0.1)
MAJOR_MINOR=$(echo "${VERSION:-0.1.0}" | sed -E 's/^([0-9]+\.[0-9]+).*/\1/')

# shellcheck disable=SC2016
# shellcheck disable=SC1004
CONTENT='# Labels from hack/patch-bundle-dockerfile.sh
LABEL com.redhat.component="VCF Migration Operator"
LABEL distribution-scope="public"
LABEL name="vcf-migration/vcf-migration-operator-bundle"
LABEL release="'"${VERSION:-0.1.0}"'"
LABEL version="'"${VERSION:-0.1.0}"'"
LABEL cpe="cpe:/a:redhat:vcf_migration_operator:'"${MAJOR_MINOR}"'::el9"
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
LABEL io.openshift.tags="openshift,operator,vcf,migration,vsphere"'

# Remove the content of the bundle.Dockerfile starting from the line with the comment "# Labels from hack/patch-bundle-dockerfile.sh"
if [[ "$(uname)" == "Darwin" ]]; then
    # macOS BSD sed
    sed -i '' '/# Labels from hack\/patch-bundle-dockerfile.sh/,$d' bundle.Dockerfile
else
    # Linux GNU sed
    sed -i '/# Labels from hack\/patch-bundle-dockerfile.sh/,$d' bundle.Dockerfile
fi

# Append the content to the bundle.Dockerfile
cat <<EOF >>bundle.Dockerfile

$CONTENT
EOF