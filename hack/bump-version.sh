#!/usr/bin/env bash

set -euo pipefail

script_name=${0##*/}

usage() {
    printf 'Usage: %s <version>\n   or: VERSION=<version> %s\n' \
        "$script_name" "$script_name" >&2
}

fail() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

literal_count() {
    awk -v needle="$2" '
        {
            remaining = $0
            while ((position = index(remaining, needle)) != 0) {
                count++
                remaining = substr(remaining, position + length(needle))
            }
        }
        END { print count + 0 }
    ' "$1"
}

expect_literal_count() {
    local file=$1
    local needle=$2
    local expected=$3
    local actual

    actual=$(literal_count "$file" "$needle")
    [[ "$actual" -eq "$expected" ]] ||
        fail "expected $expected occurrence(s) of '$needle' in $file, found $actual"
}

replace_field() {
    local file=$1
    local prefix=$2
    local suffix=$3
    local value=$4
    local expected=$5
    local anchored=${6:-false}
    local temporary_file
    local actual

    if [[ "$anchored" == true ]]; then
        actual=$(awk -v prefix="$prefix" 'index($0, prefix) == 1 { count++ } END { print count + 0 }' "$file")
    else
        actual=$(literal_count "$file" "$prefix")
    fi
    [[ "$actual" -eq "$expected" ]] ||
        fail "expected $expected field(s) beginning with '$prefix' in $file, found $actual"

    temporary_file=$(mktemp "${file}.tmp.XXXXXX")
    cp -p "$file" "$temporary_file"
    awk -v prefix="$prefix" -v suffix="$suffix" -v value="$value" -v anchored="$anchored" '
        {
            if (anchored == "true") {
                if (index($0, prefix) == 1) {
                    print prefix value
                } else {
                    print
                }
                next
            }
            remaining = $0
            rewritten = ""
            while ((position = index(remaining, prefix)) != 0) {
                rewritten = rewritten substr(remaining, 1, position - 1) prefix
                remaining = substr(remaining, position + length(prefix))
                if (length(suffix) == 0) {
                    rewritten = rewritten value
                    remaining = ""
                } else {
                    suffix_position = index(remaining, suffix)
                    if (suffix_position == 0) {
                        exit 1
                    }
                    rewritten = rewritten value substr(remaining, suffix_position)
                    remaining = substr(remaining, suffix_position + length(suffix))
                }
            }
            print rewritten remaining
        }
    ' "$file" >"$temporary_file"
    mv "$temporary_file" "$file"
}

replace_cpe_label() {
    local file=$1
    local new_major_minor=$2
    local temporary_file
    local count

    count=$(awk 'index($0, "LABEL cpe=") == 1 { count++ } END { print count + 0 }' "$file")
    [[ "$count" -eq 1 ]] || fail "expected exactly one CPE label in $file, found $count"

    temporary_file=$(mktemp "${file}.tmp.XXXXXX")
    cp -p "$file" "$temporary_file"
    awk -v cpe="LABEL cpe=\"cpe:/a:redhat:vcf_migration_operator:${new_major_minor}::el9\"" '
        index($0, "LABEL cpe=") == 1 { $0 = cpe }
        { print }
    ' "$file" >"$temporary_file"
    mv "$temporary_file" "$file"
}

read_csv_created_at() {
    awk -F '"' '
        /^[[:space:]]*createdAt:/ {
            value = $2
            count++
        }
        END {
            if (count != 1) {
                exit 1
            }
            print value
        }
    ' "$1"
}

run_bundle_generation() {
    local csv_file=bundle/manifests/vcf-migration-operator.clusterserviceversion.yaml
    local created_at
    local generated_created_at
    local sdk_version
    local kustomize_version
    local installed_sdk
    local installed_sdk_version
    local operator_sdk
    local temp_dir=
    local operating_system
    local architecture

    created_at=$(read_csv_created_at "$csv_file") ||
        fail "expected exactly one createdAt timestamp in $csv_file"
    sdk_version=$(awk '$1 == "OPERATOR_SDK_VERSION" && $2 == "?=" { print $3 }' Makefile)
    kustomize_version=$(awk '$1 == "KUSTOMIZE_VERSION" && $2 == "?=" { print $3 }' Makefile)

    installed_sdk=$(command -v operator-sdk || true)
    if [[ -n "$installed_sdk" ]]; then
        installed_sdk_version=$(
            "$installed_sdk" version | awk -F '"' '/^operator-sdk version:/ { print $2 }'
        )
    fi

    if [[ "${installed_sdk_version:-}" == "$sdk_version" ]]; then
        operator_sdk=$installed_sdk
    else
        temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/vcf-version-bump.XXXXXX")
        trap '[[ -z "${temp_dir:-}" ]] || rm -rf "$temp_dir"' EXIT
        operating_system=$(go env GOOS)
        architecture=$(go env GOARCH)
        operator_sdk="$temp_dir/operator-sdk"
        curl -fsSLo "$operator_sdk" \
            "https://github.com/operator-framework/operator-sdk/releases/download/${sdk_version}/operator-sdk_${operating_system}_${architecture}"
        chmod +x "$operator_sdk"
    fi

    env \
        -u BUNDLE_GEN_FLAGS \
        -u BUNDLE_METADATA_OPTS \
        -u CHANNELS \
        -u DEFAULT_CHANNEL \
        -u GNUMAKEFLAGS \
        -u GOFLAGS \
        -u IMAGE_TAG_BASE \
        -u IMG \
        -u KUSTOMIZE \
        -u LOCALBIN \
        -u MAKEFLAGS \
        -u MAKEFILES \
        -u MAKEOVERRIDES \
        -u MFLAGS \
        -u USE_IMAGE_DIGESTS \
        make bundle \
        VERSION="$new_version" \
        OPERATOR_SDK="$operator_sdk" \
        OPERATOR_SDK_VERSION="$sdk_version" \
        KUSTOMIZE_VERSION="$kustomize_version"

    generated_created_at=$(read_csv_created_at "$csv_file") ||
        fail "expected exactly one generated createdAt timestamp in $csv_file"
    if [[ "$created_at" != "$generated_created_at" ]]; then
        replace_field "$csv_file" 'createdAt: "' '"' "$created_at" 1
    fi

    [[ -z "$temp_dir" ]] || rm -rf "$temp_dir"
    trap - EXIT
}

if (($# > 1)); then
    usage
    fail "expected at most one version argument"
fi

new_version=${1:-${VERSION:-}}
if [[ ! "$new_version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    usage
    fail "version must be stable SemVer without a leading 'v' (for example, 0.2.0)"
fi

repo_root=$(git rev-parse --show-toplevel 2>/dev/null) ||
    fail "must be run from inside a Git repository"
cd "$repo_root"

current_version=$(awk '$1 == "VERSION" && $2 == "?=" { print $3 }' Makefile)
[[ -n "$current_version" ]] || fail "unable to read VERSION ?= from Makefile"

new_major_minor=${new_version%.*}

replace_field Makefile "VERSION ?= " "" "$new_version" 1 true
replace_field Dockerfile 'LABEL release="' '"' "$new_version" 1
replace_field Dockerfile 'LABEL version="' '"' "$new_version" 1
replace_cpe_label Dockerfile "$new_major_minor"
replace_field bundle.konflux.Dockerfile 'LABEL release="' '"' "$new_version" 1
replace_field bundle.konflux.Dockerfile 'LABEL version="' '"' "$new_version" 1
replace_cpe_label bundle.konflux.Dockerfile "$new_major_minor"
replace_field docs/dev/development.md 'vcf-migration-operator-bundle:v' '' "$new_version" 1
replace_field docs/dev/development.md 'vcf-migration-operator-catalog:v' '' "$new_version" 1
replace_field docs/dev/development.md '| `VERSION` | `' '` |' "$new_version" 1
replace_field docs/user/install-with-olm.md 'vcf-migration-operator-bundle:v' '' "$new_version" 1
replace_field docs/user/install-with-olm.md 'vcf-migration-operator-catalog:v' '' "$new_version" 2
replace_field test/e2e/e2e_suite_test.go \
    'example.com/vcf-migration-operator:v' '"' "$new_version" 1

run_bundle_generation

csv_file=bundle/manifests/vcf-migration-operator.clusterserviceversion.yaml
expect_literal_count "$csv_file" "vcf-migration-operator.v${new_version}" 1
expect_literal_count "$csv_file" "  version: ${new_version}" 1
expect_literal_count bundle.Dockerfile "LABEL release=\"${new_version}\"" 1
expect_literal_count bundle.Dockerfile "LABEL version=\"${new_version}\"" 1
expect_literal_count bundle.Dockerfile \
    "cpe:/a:redhat:vcf_migration_operator:${new_major_minor}::el9" 1

printf 'Version bump to %s complete. Review the generated changes before committing.\n' \
    "$new_version"
