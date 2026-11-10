#!/usr/bin/env bash
#
# Maintain the OLM upgrade graph in the FBC catalogs: add the channel entry for
# <version> and point its `replaces` at the newest entry already in the channel.
#
# The bundle itself must already be in catalog.yaml (see hack/build-catalog.sh);
# this script only maintains the olm.channel document's entries list.
set -euo pipefail

PACKAGE="vcf-migration-operator"
CHANNEL="dev-preview"

if [[ $# -ne 1 ]]; then
    echo "Usage: $0 <version>    # e.g. $0 0.1.1 or $0 v0.1.1" >&2
    exit 2
fi

INPUT_VERSION="$1"
NEW_ENTRY="${PACKAGE}.v${INPUT_VERSION#v}"

# Print the version part of a bundle name: vcf-migration-operator.v0.1.1 -> 0.1.1
strip_version() {
    sed -E "s/^${PACKAGE}\.v?//"
}

# Version-compare two dotted versions; exits 0 when $1 > $2.
version_gt() {
    local IFS='.'
    # shellcheck disable=SC2206  # intentional word splitting on '.'
    local -a a=($1) b=($2)
    local i
    for i in 0 1 2 3; do
        local x="${a[$i]:-0}" y="${b[$i]:-0}"
        if ((10#$x > 10#$y)); then return 0; fi
        if ((10#$x < 10#$y)); then return 1; fi
    done
    return 1
}

# Highest semver among the channel entries of one catalog.
latest_channel_entry() {
    local catalog="$1" entry version latest="" latest_version=""
    while IFS= read -r entry; do
        [[ -n "$entry" ]] || continue
        version="$(printf '%s' "$entry" | strip_version)"
        if [[ -z "$latest" ]] || version_gt "$version" "$latest_version"; then
            latest="$entry"
            latest_version="$version"
        fi
    done < <(awk '
        /^schema: olm\.channel$/ { in_doc = 1; next }
        in_doc && /^schema:/ { in_doc = 0 }
        in_doc && /^entries:/ { in_entries = 1; next }
        in_doc && in_entries && /^  - name: / { sub(/^  - name: /, ""); print }
        in_doc && in_entries && /^[^ ]/ { in_entries = 0 }
    ' "$catalog")
    printf '%s' "$latest"
}

found_catalog=false
for dir in fbc-v*-*; do
    [[ -d "$dir" ]] || continue
    catalog="$dir/catalog/${PACKAGE}/catalog.yaml"
    if [[ ! -f "$catalog" ]]; then
        echo "Skipping $dir (no $catalog)"
        continue
    fi
    found_catalog=true
    echo "Processing $catalog"

    if ! grep -qx "^schema: olm.channel$" "$catalog"; then
        echo "Error: $catalog has no olm.channel document; not inventing one" >&2
        exit 1
    fi

    # The bundle must already be in the catalog, otherwise the channel would
    # point at a bundle that does not exist.
    if ! grep -qx "name: ${NEW_ENTRY}" "$catalog"; then
        echo "Error: ${NEW_ENTRY} is not in $catalog; add the bundle first" >&2
        echo "       (make build-index SNAPSHOT=<snapshot-or-bundle-image>)" >&2
        exit 1
    fi

    if awk -v entry="  - name: ${NEW_ENTRY}" '$0 == entry { found = 1 } END { exit !found }' "$catalog"; then
        echo "  ${NEW_ENTRY} is already in channel ${CHANNEL}; nothing to do"
        continue
    fi

    previous="$(latest_channel_entry "$catalog")"
    if [[ -z "$previous" ]]; then
        echo "Error: channel ${CHANNEL} in $catalog has no entries to replace" >&2
        exit 1
    fi

    echo "  ${NEW_ENTRY} replaces ${previous}"
    tmp="$(mktemp)"
    awk -v entry="${NEW_ENTRY}" -v replaces="${previous}" '
        /^schema: olm\.channel$/ { in_doc = 1 }
        in_doc && /^entries:/ {
            print
            print "  - name: " entry
            print "    replaces: " replaces
            inserted = 1
            in_entries = 1
            next
        }
        { print }
        END { if (!inserted) exit 1 }
    ' "$catalog" > "$tmp"
    chmod --reference="$catalog" "$tmp" 2>/dev/null || chmod "$(stat -f '%Lp' "$catalog")" "$tmp"
    mv "$tmp" "$catalog"
done

if [[ "$found_catalog" != true ]]; then
    echo "Error: no catalog.yaml files found under fbc-v* directories" >&2
    exit 1
fi

echo "Done."
