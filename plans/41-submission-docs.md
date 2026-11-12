# Plan 41 — Submission docs: README, WRITEUP, AI-USAGE

**Status:** planned
**Branch:** `feat/41-submission-docs`
**Merge order:** **last** (after plans 42–45). Authored now so the narrative cannot drift;
merged only when the CLI/result commands it documents actually exist.
**Type:** docs only — no production code, no tests.

## Problem

The platform is far ahead of its documentation story:

- `README.MD` is 37 lines, **in Vietnamese**, while `docs/` was translated to English in PR #41.
  It documents `make test/lint/build` and the compose stack, but not the developer journey,
  not the kind path, and not the CLI (which does not exist yet — plan 44).
- `WRITEUP.md` and `AI-USAGE.md` — both required deliverables — do not exist.
- `plans/README.md` (index) stops at plan 26 and still has a duplicated row and a Vietnamese
  footer, so a reviewer cannot navigate plans 27–45.
- `docs/mvp-status.md` section 7 ("Gaps / remaining work") is stale: it lists the K8s runner and
  mock-service as unfinished, and does not mention caps/results work (plans 42, 45).

A reviewer who reads only the repo root concludes this is a partial backend; a reviewer who reads
`docs/mvp-status.md` gets a diff-accurate picture. The gap is presentation, and it is cheap to close.

## Design decisions

1. **README is a walkthrough, not an inventory.** Three run paths (`compose`, `kind`,
   in-cluster E2E) plus the six-command developer journey. Every command must be copy-paste
   runnable; no pseudo-commands.
2. **WRITEUP documents decisions and their costs**, not the feature list. The three decisions that
   actually shaped the codebase:
   - namespace (environment) is the unit of isolation, not the test run;
   - desired-state in Postgres + observed-state in a projection, with generation fencing,
     instead of imperative apply-and-wait;
   - manifests rendered in Go and applied via SSA/kustomize, instead of Helm charts.
   Each gets: the alternative, why it lost, and what it costs today.
3. **AI-USAGE is evidence-first.** Tools used, which plans/PRs were AI-assisted, what was rejected,
   and the harness (`tests/harness/`, `tests/sql/harness_test.sql`) as the mechanism that made
   AI output reviewable rather than trusted.
4. **No new marketing claims.** No "production-ready", no benchmark numbers that were not measured
   (the k6 perf gate has never been run — say so, link the plan).

## Steps

1. **Rewrite `README.MD` in English** (keep the file name; CI/Makefile do not reference it):
   - one-paragraph what/why + architecture diagram (reuse `docs/mvp-status.md` §1 tree);
   - **Quickstart A (local):** `docker compose up -d --build` → `./tests/e2e-smoke.sh`;
   - **Quickstart B (kind):** kustomize apply + in-cluster E2E (`tests/e2e-full-deploy.sh`);
   - **Developer journey:** the six `ltp` commands from plan 44, with expected output lines;
   - **Results:** `ltp run report` sample output (plan 45);
   - `make` targets table, doc index (`docs/`, `plans/`), known-gaps table linking plans 42/45/46;
   - environment-variable table (only what `pkgs/config` actually reads).
2. **`WRITEUP.md`** (~2–3 pages):
   - problem framing and the one-sentence design thesis;
   - architecture: control plane / data plane / gateway / DB (with the state-machine and
     transition-function pattern called out, since it is the codebase's spine);
   - the three decisions above (alternative → why rejected → cost);
   - explicit trade-offs taken under time pressure (quota ledger deferred, k6 summary text parse
     instead of a metrics pipeline, single-tenant-per-host assumption);
   - "what I would do next" in priority order, aligned with the gaps table.
3. **`AI-USAGE.md`**: tooling and division of labour; per-plan AI assistance map; the review
   mechanism (harness red→green, lint-transitions guard, SQL invariant suites); three concrete
   examples of AI output that was **rejected** and why; what I would not delegate.
4. **Refresh `plans/README.md`**: add rows 27–45 (they exist on disk), remove the duplicated row 16,
   translate the header/footer to English, and add the "merge order" note for 41.
5. **Refresh `docs/mvp-status.md`**: repoint §7 at the remaining real gaps only; add a "Phase 2
   (plans 41–45)" subsection pointing at the new plans.
6. **Verify every command** against a running compose stack in a fresh shell; paste real output.

## Tests / verification

Docs-only plan, so verification is executable rather than unit-tested:

- [ ] Every `bash` block in README runs top-to-bottom in a clean checkout (compose path).
- [ ] `ltp` commands in the README match `ltp --help` output verbatim (diff the two).
- [ ] `grep -n "docs/" README.MD` → every linked path exists.
- [ ] No Vietnamese prose remains in `README.MD`; `docs/` links resolve.
- [ ] `WRITEUP.md` claims each traceable to a file/plan (spot-check 5).
- [ ] `plans/README.md` row count == number of plan files (script the check once).

## Risks

| Risk | Mitigation |
|---|---|
| README documents CLI flags that ship differently in plan 44 | Merge order: 41 lands last; step 6 diffs against real `--help` |
| Claims drift into marketing | Every claim cites a file or plan; no unmeasured numbers |
| `mvp-status.md` refresh expands into a full rewrite | Timebox: only §7 + one new subsection |

## Out of scope

- Demo video (plan 46, pending confirmation).
- Any code change; anything found while verifying commands becomes a separate plan.
