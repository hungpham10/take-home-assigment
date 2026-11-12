# Plan 39 — Release workflow: robust Earthly install

## Problem

The `Release` workflow (`v0.1.0` bootstrap, runs 43130298760 / 43130593585) failed at

```
curl: (6) Could not resolve host: get.earthly.dev
...
Build & push images (Earthly): earthly: command not found  (exit 127)
```

Two defects:

1. `curl -fsSL https://get.earthly.dev | bash` depends on one redirect host. When
   DNS resolution for that host fails (transient runner/DNS issue, reproduced twice),
   no binary is installed.
2. **The failure is silent.** Without `set -o pipefail` the pipeline's exit status is
   `bash`'s (0 on empty stdin), so the step logs `success`. The run only fails later
   at `earthly --push`, after the git tag has already been pushed. That leaves a tag
   pointing at a release with no images and no GitHub Release.

## Fix

- Download the pinned-form binary directly from GitHub Releases
  (`https://github.com/earthly/earthly/releases/latest/download/earthly-linux-amd64`),
  which is reachable wherever `actions/checkout` works, into `/usr/local/bin`.
- Wrap the download in a small retry loop (3 attempts) to absorb transient network
  blips.
- `set -euo pipefail` + `earthly --version` as a post-condition, so a broken install
  fails **at the install step**, before the tag is created.
- Keep the tag step after the toolchain is verified.

## Why not the installer script

`get.earthly.dev` is a convenience alias; it is not required. Pulling the release
artifact from `github.com` removes the extra DNS dependency and makes the version
pinned by the release asset URL, which is what the workflow actually needs.

## Migration / operations

- No behaviour change for successful releases.
- Retracting a tag pushed by a failed release run is allowed when **no** artifact was
  published (no image in ghcr, no GitHub Release): delete the tag, fix, re-run.
  `v0.1.0` was retracted this way before this fix.

## Verification

- `actionlint`-clean (manual review) — the step now fails fast.
- Re-run `gh workflow run release.yml -f bump=none -f version=0.1.0`: install step
  prints `earthly version ...`, images `:0.1.0` + `:latest` are pushed, GitHub
  Release is created with both binaries.
