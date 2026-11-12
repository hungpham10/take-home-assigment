#!/usr/bin/env bash
#
# bump_test.sh — regression tests for scripts/bump.sh (plan 38).
#
# Each case runs against a throwaway git repository seeded with specific tags,
# so the real repo history never influences the assertions.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUMP="$ROOT/scripts/bump.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

PASS=0
FAIL=0

say() { printf '\n=== %s ===\n' "$1"; }

ok() {
    PASS=$((PASS + 1))
    printf 'ok   %s\n' "$1"
}

bad() {
    FAIL=$((FAIL + 1))
    printf 'FAIL %s\n' "$1" >&2
}

# new_repo <dir> — fresh git repo with an initial commit and no tags.
new_repo() {
    local dir="$1"
    mkdir -p "$dir"
    git -C "$dir" init -q
    git -C "$dir" config user.name "bump-test"
    git -C "$dir" config user.email "bump-test@example.com"
    git -C "$dir" config commit.gpgsign false
    git -C "$dir" config tag.gpgsign false
    git -C "$dir" commit -q --allow-empty -m init
    printf 'tracked\n' >"$dir/tracked.txt"
    git -C "$dir" add tracked.txt
    git -C "$dir" commit -q -m "add tracked file"
}

tag() {
    git -C "$1" tag "$2"
}

commit() {
    git -C "$1" commit -q --allow-empty -m "change $2"
}

# expect_out <name> <dir> <expected> <args...>
expect_out() {
    local name="$1" dir="$2" want="$3"
    shift 3
    local got
    if ! got="$(cd "$dir" && "$BUMP" "$@" 2>/dev/null)"; then
        bad "$name: bump.sh $* exited non-zero (want '$want')"
        return
    fi
    if [[ "$got" == "$want" ]]; then
        ok "$name"
    else
        bad "$name: got '$got', want '$want'"
    fi
}

# expect_fail <name> <dir> <stderr-substring> <args...>
expect_fail() {
    local name="$1" dir="$2" want_err="$3"
    shift 3
    local out rc
    set +e
    out="$(cd "$dir" && "$BUMP" "$@" 2>&1)"
    rc=$?
    set -e
    if [[ $rc -eq 0 ]]; then
        bad "$name: expected failure, got success ('$out')"
        return
    fi
    if [[ -n "$want_err" && "$out" != *"$want_err"* ]]; then
        bad "$name: stderr '$out' does not contain '$want_err'"
        return
    fi
    ok "$name"
}

say "no tags (bootstrap)"
R="$WORK/no-tags"
new_repo "$R"
expect_fail "no tags + patch fails with guidance" "$R" "no release tag found" patch
expect_fail "no tags + minor fails" "$R" "no release tag found" minor
expect_out "no tags + --set bootstraps" "$R" "0.1.0" --set 0.1.0
expect_out "--print without tags is dev" "$R" "dev" --print

say "bump levels"
R="$WORK/levels"
new_repo "$R"
tag "$R" v1.2.3
expect_out "patch" "$R" "1.2.4" patch
expect_out "minor" "$R" "1.3.0" minor
expect_out "major" "$R" "2.0.0" major
expect_out "--print on tagged commit" "$R" "1.2.3" --print

say "tag ordering"
R="$WORK/ordering"
new_repo "$R"
tag "$R" v1.2.3
tag "$R" v1.10.0
tag "$R" v1.9.9
expect_out "numeric ordering (1.10.0 wins)" "$R" "1.10.1" patch

say "non-semver tags are ignored"
R="$WORK/junk-tags"
new_repo "$R"
tag "$R" v1.2.3
tag "$R" nightly
tag "$R" v1.2.4-rc.1
tag "$R" release-candidate
expect_out "rc/nightly ignored" "$R" "1.2.4" patch
expect_out "--set ignores rc in comparison" "$R" "1.2.4" --set 1.2.4

say "guards"
R="$WORK/guards"
new_repo "$R"
tag "$R" v1.2.3
expect_fail "--set same version rejected" "$R" "must be greater" --set 1.2.3
expect_fail "--set downgrade rejected" "$R" "must be greater" --set 1.0.0
expect_fail "--set malformed rejected" "$R" "MAJOR.MINOR.PATCH" --set 1.2
expect_fail "--set with extra args rejected" "$R" "exactly one" --set 1.2.3 1.2.4
expect_fail "unknown bump type rejected" "$R" "unknown argument" hotfix
expect_fail "missing argument rejected" "$R" "Usage" ""
expect_fail "non-git directory rejected" "$WORK" "not a git repository" patch

say "dirty working tree"
R="$WORK/dirty"
new_repo "$R"
tag "$R" v1.2.3
commit "$R" after-tag
echo dirty >>"$R/tracked.txt"
expect_out "bump ignores dirty state" "$R" "1.2.4" patch
got="$(cd "$R" && "$BUMP" --print)"
if [[ "$got" == 1.2.3-1-g*-dirty ]]; then
    ok "--print marks commits after tag as 1.2.3-1-g<sha>-dirty"
else
    bad "--print after tag = '$got', want 1.2.3-1-g<sha>-dirty"
fi

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
