# Load Test Manager API — Run Lifecycle (requires READY namespace)

This document defines **load test run management** in the namespace-first model. Endpoint-level contracts live in [`docs/bff-loadtest-api-spec.md`](bff-loadtest-api-spec.md); namespace/component/readiness rules live in [`docs/namespace-management-api.md`](namespace-management-api.md).

**Base path:** `/api/loadtest/v1`.  
**Related:** [`docs/namespace-management-api.md`](namespace-management-api.md), [`docs/k6-operator-go-client.md`](k6-operator-go-client.md), [`docs/admin-oidc-integration-api.md`](admin-oidc-integration-api.md), [`docs/nginx-oidc-auth.md`](nginx-oidc-auth.md), [`docs/architecture.md`](architecture.md).

## 1. Hard prerequisites (must enforce)

A load test **cannot** be defined/triggered unless:
1. **`namespace_id` is provided** and resolves in the same tenant/visibility scope.
2. Namespace is **READY for load testing** (`ready_for_loadtest=true`; e.g., status `READY`, components applied/readiness checks passed). If not ready -> **`409 Conflict`** (`NAMESPACE_NOT_READY`) with readiness details.
3. **Target API(s) are explicit** and valid: `targets[]` references `service_id` operations that exist in the namespace (resolved from each service’s **OpenAPI/Swagger** definition). Reject unknown service/operation/invalid method+path.
4. **JS load test definition is provided** (k6 script): required `script` payload (e.g., `filename` + `content` and/or approved ref). Reject missing/invalid script; enforce size/form guardrails (ConfigMap payload limit ~1 MiB when using ConfigMap; prefer run-scoped storage/ref for larger scripts).

Authz: tenant isolation; owner-only; `admin` cross-owner within tenant. Immutable `tenant_id`, `owner_user_id` on runs.

## 2. Responsibilities

- Validate namespace readiness + target APIs + JS script + guardrails.
- Persist run catalog before provisioning; async `202` + polling; idempotency where supported.
- Provision/resolve execution context (namespace/dependency/NetworkPolicy already defined by namespace; ensure runner access/policies as needed), create script source (ConfigMap or approved mechanism), create **k6-operator `TestRun`** via **Go Kubernetes API (no `kubectl`)** (`docs/k6-operator-go-client.md`).
- Reconcile BFF status with operator/provision/teardown signals; enforce TTL/force stop/cleanup.
- Serve status, logs, metrics/events scoped to authz.

## 3. API summary

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/loadtest/v1/namespaces/{namespace_id}/loadtests` | Create & trigger (`201`). Requires ready namespace + `script_id` (validated) + `load_config`. Targets are derived server-side from service OpenAPI. |
| `GET` | `/api/loadtest/v1/loadtests` | List runs (`namespace_id`, `service_id`, `status`, owner admin-only). |
| `GET` | `/api/loadtest/v1/loadtests/{id}` | Run detail, targets, namespace/service snapshot, metrics. |
| `POST` | `/api/loadtest/v1/loadtests/{id}/stop` | Force close/teardown. |
| `GET` | `/api/loadtest/v1/loadtests/{id}/logs` | Logs (redacted). |
| (inferred) | events/metrics/artifacts/rerun/delete | As needed, authz-scoped. |

## 4. Create/trigger flow

1. Authn context + authz (namespace visible/usable).
2. **Check namespace readiness** (`ready_for_loadtest`).
3. Resolve and validate **`targets[]`** against namespace service OpenAPI operations.
4. Validate **JS script** (presence, size/type/allowed form, guardrails).
5. Apply guardrails (vusers/duration/TTL/concurrency/quotas).
6. Persist run (`PROVISIONING`/`QUEUED`, `namespace_id`, `targets`, script ref/hash, `expires_at`).
7. Provision async: bind execution to namespace context; ensure **runner NetworkPolicy/egress** allows only resolved targets/approved deps + required platform endpoints (targets are derived server-side, not advisory); create script source (run-scoped name, e.g., `run-<test_id>-script` — the `run-` prefix guarantees the K8s object name is DNS-1035 compliant even when `<test_id>` is a UUID starting with a digit; set owner references/cleanup on failure; respect ConfigMap size limits); create k6 `TestRun`; store `k8s_refs`. Runner must have no platform/K8s credentials. The run-scoped NetworkPolicy is **three policies per run** — egress (runner → target), ingress (target ← runner), and api (k6-operator controller ns → runner TCP 6565) — selected by label `loadtest.io/run-id`, specified in [`docs/network-policy.md`](network-policy.md) §3; NetworkPolicy is L3/L4 only (`service` + `port`).
8. Return `202` + `status_url`; reconcile to canonical statuses per `docs/bff-loadtest-api-spec.md` (MVP: use `STOPPED` for stop/cancel outcomes; `CANCELED` reserved unless explicitly distinguished).

## 5. Target API and script (clarity)

- **Target API(s):** explicit list of impacted operations. Preferred fields: `service_id` + `operation_id`; also accept `method`+`path` when resolvable. Stored on the run for audit/UI/validation.
- **JS script (required):** executable k6 definition of the load test. Supported forms (MVP policy to finalize):
  - `inline`: `filename` + `content` (+ `content_type`). Subject to size guardrails.
  - `approved_ref`: vetted object-storage/reference (e.g. S3 URI, internal artifact ref). **Do not pass arbitrary remote URLs directly to execution**; resolve only approved refs with scoped credentials/presigned access.
  - `multipart upload`: optional convenience; stored as managed artifact/ref.

### 5.1 Remote script loading (S3/object storage) and staging
k6-operator `TestRun.spec.script` supports **`configMap`**, **`volumeClaim`**, or **`localFile`** only (no direct S3/HTTP source). Therefore remote scripts (e.g., S3) must be handled by the platform:
1. **Register/reference script**: BFF stores script metadata (`script_id`, source ref, checksum/hash, size, owner/tenant, status).
2. **Validate before use** (see 5.2): create validation job and require `VALIDATED` (or policy-approved) before allowing `TestRun` execution, especially for approved refs / CI flows.
3. **Stage for execution**: resolve approved ref in a controlled path, then create a run-scoped script source:
   - **ConfigMap** (`data["test.js"]`): simple for small scripts; **ConfigMap value limit ~1 MiB**. Enforce hard size limit.
   - **Volume/PVC/archive** (`volumeClaim`): for larger scripts/archives; platform stages content to a PVC/object-backed volume using a controlled init/staging Job or approved CSI/object mechanism.
   - **Baked image** (`localFile`): only for vetted runner images, not user-supplied arbitrary remote code.
4. **Create `TestRun`** referencing the staged `configMap`/`volumeClaim`/`localFile`. Use run-scoped names/labels, owner refs/cleanup on failure.

### 5.2 Validation before execution (separate K8s Job + watcher)
Remote/registered scripts should be validated before dispatching load:
- **Validation record**: `PENDING_VALIDATION | VALIDATING | VALIDATED | INVALID | VALIDATION_FAILED` with diagnostics (syntax/import/archive errors, guardrail violations, checksum mismatch), timestamps, validator image/version.
- **K8s Job (isolated)**: BFF (or reconciler) creates a **run/validation-scoped Kubernetes Job** that executes controlled validation (e.g., `k6 inspect`/`k6 archive`/syntax/module checks) against the approved script source. Job runs with **least privilege**: no platform/K8s credentials, scoped read-only access to the approved object/script ref, resource/time limits, network egress restricted to approved artifact endpoints as needed.
- **Status observation / notify**: use a **Kubernetes watch/informer** (preferred, level-triggered reconciler) on the validation Job/Pod status; on transitions update projection and emit notification state. Expose to users via:
  - `GET /api/loadtest/v1/scripts/{script_id}` or `/validations/{validation_id}` (polling-friendly, projection-backed),
  - events/audit entries,
  - optional future push (SSE/webhook) — not required for MVP.
- **Execution gate**: `POST /loadtests` rejects unvalidated/invalid script refs when policy requires validation (`409 SCRIPT_NOT_VALIDATED` or `422` as finalized). Inline small scripts may use lightweight synchronous validation + guardrails if fast/safe; large/remote refs use async Job validation.

### 5.3 Security/ops guardrails
- Never trust arbitrary user-provided URLs/credentials; use approved refs, scoped short-lived credentials/presigned reads, and checksum verification.
- Validate script size/type, reject unsafe archive/module patterns per policy, and record audit.
- Validation Jobs and staged artifacts are TTL-cleaned; reaper removes orphans.

## 6. Security notes (must address before implementation)
- **Untrusted JS:** k6 script is arbitrary code. Enforce network-level egress restrictions for runners (resolved targets only + required allowances), least-privilege SA (no K8s/platform secrets), quotas (vusers, duration, script size, ConfigMap 1 MiB), and safe target resolution.
- **Rollback:** if `TestRun` creation fails after script source creation, clean up or use owner references to avoid orphans.
- **K8s naming:** use run-scoped names/labels prefixed with `run-` (e.g. `run-<test_id>-script`), not `<name>-script` without unique scope; object `metadata.name` must be DNS-1035 (start with a lowercase letter), which a digit-leading `<test_id>` UUID would violate.
- **k6-operator version:** pin tested `k6.io` API version (e.g., `v1alpha1`) and operator compatibility.

## 7. Decisions / open items
- **Locked (MVP):** 1 platform Environment = 1 Kubernetes Namespace. Runner execution uses network-enforced egress (NetworkPolicy from resolved targets/approved deps/mocks + DNS; targets derived server-side). The run-scoped policy (**three** policies per run: egress runner→target, ingress target←runner, api k6-operator ns→runner TCP 6565, label `loadtest.io/run-id`) and the L3/L4 limit are specified in [`docs/network-policy.md`](network-policy.md). POC uses OIDC/nginx auth; nginx reflects permissions from OIDC/claims into trusted headers, Go middleware normalizes principal/enforces RBAC; `abt_` is service-account token for CLI/automation; `tenant_id` scopes org/multi-domain boundary.
- Target strictness: targets must match what the script exercises (advisory/metadata) vs enforceable mapping (limited). `operation_id`/`path` cannot be enforced by NetworkPolicy (L3/L4 only); L7 enforcement is post-MVP.
- Script forms allowed in MVP and storage (inline vs object storage/ref).

## 8. Runner mode + result surface (MVP)

`RUNNER_MODE` chọn backend thực thi (`local` mặc định dev; `k8s` trên cluster):

- `local`: `pkgs/runner.Local` chạy `k6` binary trên host controller. Dùng cho dev/CI-local; **không** cần cluster. Mỗi run là một OS process; `Provision`/`Start`/`Stop`/`Status` ánh xạ trạng thái process sang model chung (`QUEUED`/`PROVISIONING`/`RUNNING`/`COMPLETED`/`FAILED`/`STOPPED`/`TIMED_OUT`).
- `k8s`: `pkgs/runner.K8sTestRunRunner` tạo `TestRun` (k6.io/v1alpha1) qua Go Kubernetes API (không `kubectl`), script staged vào ConfigMap run-scoped (≤1 MiB), runner pod chạy trong `lt-env-<namespace_id>` và chịu `default-deny` + run-scoped NetworkPolicy (`docs/network-policy.md` §3).

**Kết quả run (result surface, MVP):**
- `GET /loadtests/{id}`: `status`, `runner_ref`, `started_at`, `finished_at`, `timeout_at`, `targets`, `unreliable`.
- `GET /loadtests/{id}/events`: chuỗi transition (`from_status` → `to_status`) + `reason`, kèm `unreliable` flag (namespace `DEGRADED` sau khi run bắt đầu → `unreliable: true`, run vẫn tiếp tục).
- **K6 metrics/summary** (pod log parse → `result_summary`, `thresholds_failed`, `result_collected_at`) là deferred sang benchmark plan (k6 OSS không có endpoint metrics mặc định; cần cài k6 + parse pod log).

### Status source of truth (k8s runner)

`Status()` (implemented in `pkgs/runner/k8s.go`) does **not** treat `TestRun.status.stage` as the sole source of truth — the k6-operator stage machine (`initialized → created → started → finished`) can stall without emitting events. Resolution order:

1. Missing TestRun → `STOPPED` + `Missing: true`.
2. **Job `-1` conditions (source of truth for result):** `Complete` → `COMPLETED`; `Failed`/`Error` → `FAILED`.
3. `status.stage` mapping: `started` → `RUNNING`; `finished` → `COMPLETED`; `stopped` → `STOPPED`; `error` → `FAILED`; empty/initialization/initialized/created → `PROVISIONING`.
4. Fallback: a pod labeled `loadtest.io/run-id=<ref>` with `runner=true` and phase `Running` → `RUNNING`.

Render (`Start()`) sets `spec.paused = "false"` and `spec.initializer.disabled = "true"` so k6 runs without the 6565 starter/Service handshake, and sets explicit `runner.readinessProbe`/`runner.livenessProbe` (not the operator's 1s default) so a loaded node cannot kill the runner via probe.

**Lưu ý:** `operation_id`/`method`/`path` trong `targets[]` chỉ validate/audit ở API layer — NetworkPolicy là L3/L4 (`service` + `port`) và không thể enforce L7.
