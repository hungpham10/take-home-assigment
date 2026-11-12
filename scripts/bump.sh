#!/usr/bin/env bash
#
# bump.sh — compute the next release version from the latest git tag.
#
# The git tag is the single source of truth (docs/versioning.md). This script
# has NO side effects: it never writes files, commits, tags or pushes. The
# release workflow owns those steps so they stay visible in one place.
#
# Usage:
#   ./scripts/bump.sh patch|minor|major   # print the next version
#   ./scripts/bump.sh --set X.Y.Z         # validate + print an explicit version
#   ./scripts/bump.sh --print             # print the current build version
#
# Output contract: stdout is the version only (machine-readable, e.g.
# `NEXT=$(./scripts/bump.sh patch)`). Diagnostics go to stderr.

set -euo pipefail

# No `cd`: git already resolves the enclosing repository from any subdirectory,
# and staying in the caller's directory keeps the script testable against a
# throwaway repository (scripts/bump_test.sh).

SEMVER_RE='^[0-9]+\.[0-9]+\.[0-9]+$'

die() {
    echo "bump: $*" >&2
    exit 1
}

usage() {
    cat >&2 <<'EOF'
Usage: ./scripts/bump.sh <patch|minor|major>
       ./scripts/bump.sh --set X.Y.Z
       ./scripts/bump.sh --print

The git tag (vMAJOR.MINOR.PATCH) is the source of truth; this script only
computes a version. The release workflow creates the tag and pushes images.
EOF
}

# latest_version prints the highest strict semver tag without the leading "v".
# Non-semver tags (nightly, v1.2.3-rc.1, ...) are ignored on purpose: they must
# never influence the next release number.
latest_version() {
    git tag --list 'v[0-9]*' 2>/dev/null \
        | sed -n 's/^v\([0-9][0-9]*\)\.\([0-9][0-9]*\)\.\([0-9][0-9]*\)$/\1 \2 \3/p' \
        | sort -k1,1n -k2,2n -k3,3n \
        | tail -n 1 \
        | tr ' ' '.'
}

# current_version mirrors the Makefile: git describe, without the "v" prefix.
# No tag reachable on this commit → "dev": the exact revision still travels in
# the Commit field, and the banner never claims a release that does not exist.
current_version() {
    local described
    described="$(git describe --tags --always --dirty --match 'v[0-9]*' 2>/dev/null || true)"
    if [[ "$described" == v[0-9]* ]]; then
        echo "${described#v}"
    else
        echo "dev"
    fi
}

# version_gt A B succeeds when A > B (numeric semver comparison).
version_gt() {
    awk -v a="$1" -v b="$2" 'BEGIN {
        split(a, x, "."); split(b, y, ".")
        for (i = 1; i <= 3; i++) {
            if (x[i] + 0 > y[i] + 0) exit 0
            if (x[i] + 0 < y[i] + 0) exit 1
        }
        exit 1
    }'
}

validate_semver() {
    [[ "$1" =~ $SEMVER_RE ]] || die "version '$1' is not MAJOR.MINOR.PATCH"
}

require_git_repo() {
    git rev-parse --git-dir >/dev/null 2>&1 || die "not a git repository (the version comes from git tags)"
}

case "${1:-}" in
    --print)
        require_git_repo
        current_version
        exit 0
        ;;
    --help | -h | help)
        usage
        exit 0
        ;;
    --set)
        require_git_repo
        [[ $# -eq 2 ]] || die "--set requires exactly one version argument"
        validate_semver "$2"
        latest="$(latest_version)"
        if [[ -n "$latest" ]] && ! version_gt "$2" "$latest"; then
            die "--set $2 must be greater than the latest tag ($latest); tags are immutable"
        fi
        echo "$2"
        exit 0
        ;;
    patch | minor | major)
        require_git_repo
        latest="$(latest_version)"
        if [[ -z "$latest" ]]; then
            die "no release tag found. The first release is bootstrapped explicitly:
  gh workflow run release.yml -f bump=none -f version=0.1.0
  (local: ./scripts/bump.sh --set 0.1.0)"
        fi
        IFS='.' read -r MAJOR MINOR PATCH <<< "$latest"
        case "$1" in
            major) MAJOR=$((MAJOR + 1)); MINOR=0; PATCH=0 ;;
            minor) MINOR=$((MINOR + 1)); PATCH=0 ;;
            patch) PATCH=$((PATCH + 1)) ;;
        esac
        echo "${MAJOR}.${MINOR}.${PATCH}"
        exit 0
        ;;
    "")
        usage
        exit 2
        ;;
    *)
        die "unknown argument '$1' (expected patch|minor|major, --set X.Y.Z or --print)"
        ;;
esac
