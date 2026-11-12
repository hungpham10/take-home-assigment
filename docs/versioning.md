# Versioning & Release (plan 38)

The single source of truth for the version is the **git tag** in the form `vMAJOR.MINOR.PATCH`
(annotated). There is no `version.txt` file; no version is ever typed by hand in a
manifest, Dockerfile, or workflow.

```
git tag v0.1.1
   │
   ├─► git describe --tags  ──► Go binary (ldflags)  ──► --version / GET /version / /health
   ├─► --build-arg VERSION  ──► Earthly              ──► ghcr.io/.../serve:0.1.1 + :latest
   ├─► --build-arg VERSION  ──► Dockerfile (k8s)     ──► loadtest-platform-serve:0.1.1
   └─► git describe         ──► scripts/k8s-render.sh ──► release overlay newTag=0.1.1
```

## 1. Rules

| Item | Rule |
|---|---|
| Tag | `v<major>.<minor>.<patch>`, annotated (`git tag -a`), no prerelease/build metadata in the MVP |
| Bump | `patch` = fix, `minor` = backward-compatible feature, `major` = breaking |
| Build version source | `git describe --tags --always --dirty --match 'v[0-9]*'`, strip the `v` prefix |
| Untagged commit | `0.1.1-3-gabc1234` (commits since tag + short SHA) — valid for tracing, **not** used for a release |
| No `.git` (manual Docker build) | `dev` |
| Release | Only from a tag; `:latest` always points at the newest release |

Do not bump the version in the same PR as a feature. The version is only produced at the release
step (`Releases → Run workflow`), except for the first release (bootstrap, see §4).

## 2. Reading the version at runtime

```bash
# binary
./bin/loadtest-controller --version
# loadtest-controller v0.1.1 (1ead5d4, 2026-11-12T09:00:00Z)
./bin/mock-service --version

# HTTP (behind nginx or directly in the pod)
curl -s localhost:8080/version
# {"version":"0.1.1","commit":"1ead5d4","build_date":"...","go_version":"go1.26.x"}
curl -s localhost:8080/health
# {"status":"ok","version":"0.1.1"}
```

`/version` is the **runtime truth**: if the pod runs an old image, `/version` returns the old
version regardless of what the tag in the manifest/Helm values says. Use it to verify a deployment:

```bash
kubectl -n ltp-system exec deploy/serve -c controller -- \
  wget -qO- http://127.0.0.1:8080/version   # or curl through nginx
```

The startup log also prints `version` + `commit` (`slog.Info("starting", …)`), so
`kubectl logs` is enough to know which build the pod is running.

## 3. Release runbook

Preconditions: green CI on the branch being released, `contents: write` +
`packages: write` permissions.

### 3.1 Regular release (bump from the nearest tag)

```bash
gh workflow run release.yml --ref <branch> -f bump=patch   # or minor|major
```

Workflow:
1. `./scripts/bump.sh patch` → compute the next version from the latest tag (writes no file).
2. Fail if `v<next>` already exists, or if HEAD is already tagged.
3. `earthly --push --build-arg VERSION=<next> … +build +all` → push `:<next>` and `:latest`, and write `bin/*-earthly` (plans/40).
4. Verify with `docker buildx imagetools inspect` for both images, and that `bin/*-earthly` are linux/amd64 ELF.
5. `git tag -a v<next>` + push the tag (commits nothing ⇒ nothing is pushed to the branch).
6. Create the GitHub Release + attach the `loadtest-controller`, `mock-service` binaries (linux/amd64).

**The ordering is intentional (plans/39):** the tag is pushed **after** the images and binaries have been
verified. If the build/verify fails, no tag is created ⇒ a tag pointing at a release with no artifacts can
never exist. The toolchain (`earthly`) is installed and checked
(`earthly --version`) at the very start of the job, fail-fast.

**Retracting the tag of a failed release:** only when **no** artifact has been
published (no image in ghcr, no GitHub Release):

```bash
git push origin --delete refs/tags/v0.1.0 && git tag -d v0.1.0
```


### 3.2 Release with an explicit version (`bump=none`)

Used when the version has already been decided elsewhere (release PR, customer requests a specific
number) or for bootstrapping:

```bash
gh workflow run release.yml --ref <branch> -f bump=none -f version=0.1.0
```

`version` is required, must be a valid `X.Y.Z`, and the tag must not exist yet.

### 3.3 Bootstrap (repo has no tags yet)

`bump=patch` fails intentionally ("no tags yet"). For the first release:

```bash
gh workflow run release.yml -f bump=none -f version=0.1.0
```

## 4. Deploying a release to the cluster

```bash
make k8s-render                       # render the release overlay with the newest tag
kubectl apply -f bin/k8s/release.yaml # ghcr.io/<owner>/loadtest-platform/*:<version>
```

Use `make k8s-render VERSION=0.1.1` to pick a specific tag. The `kind` overlay still uses
the `:local` image + `IfNotPresent` for dev; the `release` overlay uses ghcr +
`Always`. If the package is private, create an `imagePullSecret` (e.g. `ghcr-pull`) and
add `imagePullSecrets` to `deploy/k8s/overlays/release/kustomization.yaml`.

## 5. When a release fails midway

| State | How to handle |
|---|---|
| Failed **before** pushing the tag (bump/validate) | Fix the cause, re-run the workflow — nothing was written |
| Tag pushed, image build/push failed | Fix the workflow → re-run the **build job** on the same tag (`Releases → …` or `gh run rerun`). Do **not** bump again: the version has been published |
| Image pushed, GitHub Release failed | Re-run the release-creation job; if that does not work, create the release manually pointing at the right tag |
| Wrong tag (wrong number) | Delete the remote + local tag **only if** nobody uses it yet: `git push --delete origin v0.1.1 && git tag -d v0.1.1`; an already-pushed image must be deleted/overwritten deliberately |

Never "fix" a version by bumping again for the same change — every tag
is a published contract.

## 6. Version checks in CI

- `version` job: `./scripts/bump_test.sh` (sandbox git repo) + the binary's `--version`
  matching `git describe` + no tag pointing at HEAD.
- In-cluster E2E (`run version`): assert that `GET /version` returns the exact version the image
  was built with.

## 7. Related

- `plans/38-version-management.md` — design + fix log.
- `docs/packaging-deployment.md` §1 — image/tag table.
- `.github/workflows/release.yml`, `scripts/bump.sh`, `pkgs/version`.
