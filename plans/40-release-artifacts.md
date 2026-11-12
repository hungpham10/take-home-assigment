# Plan 40 — Release workflow: attach the built binaries

## Problem

Release `v0.1.0` (run 43134469376) succeeded for images and tag, but the GitHub
Release was published with **no assets**:

```
🤔 Pattern 'bin/loadtest-controller-earthly' does not match any files.
🤔 Pattern 'bin/mock-service-earthly' does not match any files.
🤔 ... does not include a valid file.
🎉 Release ready at .../releases/tag/v0.1.0
```

Root cause: `Earthfile` declares

```
build:
    ...
    SAVE ARTIFACT /out/loadtest-controller AS LOCAL bin/loadtest-controller-earthly
    SAVE ARTIFACT /out/mock-service        AS LOCAL bin/mock-service-earthly
```

`SAVE ARTIFACT ... AS LOCAL` only materialises files on the host when that target
is **built by the invocation**. The release job runs `earthly --push +all`, and
`+all` only does `BUILD +serve` / `BUILD +mock-service` — the `+build` target is an
intermediate, so nothing is written to `bin/`.

`action-gh-release` treats a non-matching `files:` glob as a warning, not an error,
so the job still reported success and published an asset-less release.

## Fix

1. Build the artifact target explicitly in the same invocation:
   `earthly --push ... +build +all`. `+build` is cached from the `+all` builds, so
   no extra compilation; its `SAVE ARTIFACT AS LOCAL` now writes
   `bin/loadtest-controller-earthly` and `bin/mock-service-earthly`.
2. Add a **fail-fast check** before the tag step: both files must exist and be `ELF`
   (linux/amd64). A future regression fails the release *before* the tag is pushed,
   so we never publish a tag whose release is missing artifacts.
3. Tag ordering from plans/39 is unchanged: `build → verify images → verify
   binaries → tag → GitHub Release`.

## Backfill for v0.1.0

The `v0.1.0` release already existed without assets. Backfilled out of band with
`gh release upload v0.1.0 …` using binaries cross-compiled exactly as the workflow
does (`VERSION=0.1.0`, `Commit=60b8769`, linux/amd64). No tag change.

## Verification

- Re-run `gh workflow run release.yml -f bump=patch`: the "Verify build artifacts"
  step lists both files and the GitHub Release contains 2 assets.
- Local: `earthly +build --VERSION=0.1.1` writes both `bin/*-earthly` files.
