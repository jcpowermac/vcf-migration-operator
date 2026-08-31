#!/usr/bin/env bash
set -euo pipefail

VERSION="${VERSION:-0.0.1}"
PACKAGE_NAME="${PACKAGE_NAME:-vcf-migration-operator}"
DEFAULT_CHANNEL="${DEFAULT_CHANNEL:-dev-preview}"
CHANNELS="${CHANNELS:-dev-preview}"
CATALOG_DIR="${CATALOG_DIR:-catalog}"
BUNDLE_DIR="${BUNDLE_DIR:-bundle}"
OPM="${OPM:-bin/opm}"

PACKAGE_DIR="${CATALOG_DIR}/${PACKAGE_NAME}"
mkdir -p "${PACKAGE_DIR}"

CATALOG_FILE="${PACKAGE_DIR}/catalog.json"
rm -f "${CATALOG_FILE}"

# Generate olm.package
cat <<EOF > "${CATALOG_FILE}"
{
  "schema": "olm.package",
  "name": "${PACKAGE_NAME}",
  "defaultChannel": "${DEFAULT_CHANNEL}",
  "description": "The VCF Migration Operator automates migrating OpenShift clusters between VMware vCenters (e.g. VMware Cloud Foundation / VCF), orchestrating infrastructure preparation, multi-site configuration, machine migration, and source cleanup."
}
EOF

# Generate olm.channel for each channel
IFS=',' read -ra ADDR <<< "${CHANNELS}"
for channel in "${ADDR[@]}"; do
  cat <<EOF >> "${CATALOG_FILE}"
{
  "schema": "olm.channel",
  "package": "${PACKAGE_NAME}",
  "name": "${channel}",
  "entries": [
    {
      "name": "${PACKAGE_NAME}.v${VERSION}"
    }
  ]
}
EOF
done

# Render bundle objects into catalog.json
"${OPM}" render "${BUNDLE_DIR}" --output=json >> "${CATALOG_FILE}"

# Validate the generated catalog
"${OPM}" validate "${CATALOG_DIR}"
echo "File-based catalog generated and validated at ${CATALOG_FILE}"
