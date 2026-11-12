# Plan 16 — Harness Testing: Integrity, ACID & Fault Tolerance

## Mục tiêu
Thiết lập **harness test** chứng minh metadata/control-plane **an toàn và ổn định** dưới lỗi: crash, exception, network lỗi, timeout, retry, concurrency, partial outage. Harness bổ sung (không thay thế) unit/integration/E2E/k6 perf; tập trung vào **ACID/invariant/idempotency/fail-closed/recovery**.

## Dependencies
- Nền tảng: Plan 03–07 (SQL/schema/invariants), Plan 08–13 (controller/runner/reconciler/idempotency/outbox/authz).
- Tham chiếu: `docs/harness-testing.md` (contract), `tests/sql/*` (pattern hiện có), `plans/14-local-stack-e2e.md`.

## Deliverables

### Docs (đã/đồng bộ)
- `docs/harness-testing.md` — taxonomy, ID matrix, ACID mapping, fault-injection controls, CI gates.
- Link từ `docs/architecture.md` / `plans/README.md` nếu cần (update khi implement).

### SQL harness (mở rộng `tests/sql/`)
- Bổ sung case cho invariant chưa cover: idempotency request-hash/conflict policy (theo impl), outbox+state same-tx rollback proof (qua app tx test bổ trợ), reservation concurrent commit/release, terminal-state/event append-only theo domain, cross-tenant FK/scoping nơi áp dụng.
- Giữ pattern transaction + `RAISE EXCEPTION`/`DO $$` như harness hiện có; chạy trong CI `sql-migrations`.

### Go application harness
- `pkgs/*` interfaces/fakes cho clock, runner, K8s client adapter, outbox dispatcher, ref resolver (test-only; build tag `harness` hoặc injected interfaces).
- Integration harness (Postgres thật): `API-IDEM-*`, `API-CONC-*`, `API-AUTHZ-*` fail-closed, `API-VAL-*`, `API-CRASH-*`/outbox duplicate delivery simulation.
- Runner harness: fake K8s (`already exists`, timeout, stale status), LocalRunner process crash/timeout/stop idempotency; assert transition events + no false completion.

### K8s/NetworkPolicy harness (cluster-capable, phased)
- `tests/harness/k8s/` (`//go:build harness_k8s`): verify run-scoped egress **NetworkPolicy default deny** + allowed targets/deps/mocks/DNS only; runner has no K8s/platform creds. Chạy khi có cluster/CNI; bắt buộc trước claim production runner isolation.

### CI/reporting
- Job/unit targets: `test-sql-harness`, `test-harness` (race where practical), optional `test-harness-k8s`.
- Harness failure output: IDs/tenant/state/version/idempotency key (redacted) — no secrets.

## Verification (acceptance)
- TDD cho mỗi harness case mới: case fail khi invariant bị phá (hoặc mô phỏng fault) và pass khi behavior đúng.
- SQL harness xanh; API/runner harness xanh trên core matrix (`DB-TXN-01..05`, `API-IDEM-01`, `API-AUTHZ-01/02`, `API-CONC-01`, `RUN-STOP/TIMEOUT/IDEM` tối thiểu).
- K8s egress harness có bằng chứng chạy (CI cluster hoặc manual artifact) cho thay đổi chạm runner/network isolation.

## Ghi chú / rủi ro
- **Distributed ACID caveat:** không claim ACID xuyên DB+K8s+S3; chứng minh bằng outbox + idempotent reconcile + cleanup.
- External S3/nginx chaos trong MVP dùng fake/resolver fault injection; full network chaos (tc/Chaos Mesh) là phase sau.
- Mock-service runtime harness phụ thuộc snapshot contract finalization (architecture open item).

## CodeGraph state review (2026-11-07)

### High
- `loadtest.create` gọi `Provision` rồi persist `runner_ref` và update trực tiếp `status=PROVISIONING`, nhưng không gọi `Runner.Start`. Với `LocalRunner`, memory `procTable` không có process; reconciler có thể advance `RUNNING` trước, rồi poll sau trả `STOPPED/no such run` và đánh dấu `FAILED`. Harness cần test "DB state vs runner memory" sau crash/restart và trước/sau khi start.
- `QUEUED → PROVISIONING` bypass `fn_loadtest_transition` qua raw GORM update, nên thiếu lock/version check/event append. Kết hợp với `runner_ref` và `timeout_at` được ghi ở hai update riêng: nếu một update sau fail, DB có thể rơi vào trạng thái thiếu ref hoặc thiếu timeout và không thể recover tự động.
- Script lifecycle persist `VALIDATING` trước khi enqueue vào validation queue in-memory. `Queue.Submit` bounded và drop khi full; nếu queue restart hoặc overflow, script bị kẹt `VALIDATING`. Harness cần fault-inject queue full/restart và assert timeout/requeue/fail-closed.

### Medium
- Reconciler advance `RUNNING` rồi update `started_at` ngoài transition; advance terminal rồi update `finished_at` ngoài transaction. Fault injection cần assert metadata time không bị mất khi process crash giữa hai bước.
- Service spec update đặt `ready_for_loadtest=false` nhưng vẫn giữ namespace `status=READY`; state memory/model và effective readiness có thể lệch khi API/controller đọc khác thời điểm. Harness cần test concurrent spec update/readiness/loadtest creation.
- `ValidateWith` đọc staging file trước, rồi transition `VALIDATING` và ghi kết quả trong transaction; staging file mất giữa việc persist intent và validation khiến script kẹt `VALIDATING`. Harness cần case missing/unreadable staged content.

### Harness additions
- Thêm `STATE-MEM-01`: loadtest provisioning phải không advance nếu runner memory/backend chưa chứa run; restart controller phải discover/reconcile run từ persisted ref, không phụ thuộc in-process map.
- Thêm `STATE-DB-01`: mọi state transition phải đi qua DB function trong một transaction chứa ref/metadata liên quan; assert event trail và version monotonic khi fault-inject từng update.
- Thêm `STATE-QUEUE-01`: validation intent phải recover được sau overflow/restart; nếu không enqueue được, trạng thái phải còn `PENDING_VALIDATION` hoặc có timeout/requeue dẫn tới terminal state.

## Resource declaration gap (2026-11-07)

### Current API coverage
- Namespace `POST` chỉ nhận `name`, `description`, `ttl_expires_at`; không nhận full desired `spec`, services, databases, ingress, dependency policy hay namespace quota/resource limits.
- Service `POST` chỉ nhận `name` + OpenAPI `spec`; docs định nghĩa thêm deploy/runtime metadata, replicas, port, health path, mock metadata, database bindings và dependencies nhưng handler chưa parse/persist.
- Load-test `POST` nhận được `load_config.vusers`, `load_config.duration`, `load_config.timeout_minutes`; chưa khai báo runner CPU/memory/storage hoặc resource class.
- `tbl_reservation_ledger` đã có `vusers`, `concurrent_loadtests`, `mock_pool`, nhưng load-test creation chưa reserve/check quota.

### Missing declaration surface
- Namespace-level resource quota: CPU, memory, storage, component limits và ephemeral TTL guardrails.
- Service runtime resources: image/version, replicas, port, health path, CPU/memory requests/limits, environment/config/secret references.
- Database runtime resources: engine version, storage size, instances, connection limits và restore policy.
- Ingress/dependencies/database bindings/mock capacity; đây là input cần thiết để render K8s object và NetworkPolicy.
- Runner resources: CPU/memory requests/limits, storage/staging limits, run class và concurrency limit.

### Harness additions
- Thêm `RESOURCE-DECL-01`: request chứa field tài nguyên không được silently drop; unsupported/unknown fields phải trả validation error hoặc được persist theo contract, sau đó response/readiness phản ánh đúng declared state.
- Thêm `RESOURCE-DECL-02`: load-test creation phải reserve `vusers`/`concurrent_loadtests` atomically với state transition; fault-inject giữa reserve và transition phải release hết reservation hoặc để lại reservation có TTL/reconciler reclaim.
- Thêm `RESOURCE-DECL-03`: apply/readiness không được trả READY khi declared resources vượt quota hoặc thiếu required runtime metadata; case phải assert blocking reasons và fail-closed.

## AdminAPI vs `docs/admin-oidc-integration-api.md` (2026-11-07)

### Implemented surface
- Nginx-internal endpoints are implemented: `GET /tenant/{host}/id`, `GET /tenant/{host}/auth-config?name=`, and `POST /token/introspect`. The auth-config response declares `id`, `jwt_mode`, `oidc_jwks_url`, `jwt_secret`, `oidc_issuer`, `oidc_client_id`, `oidc_client_secret`, `oidc_expected_alg`, and `session_secret`; secrets resolve from SOPS-backed env, not DB.
- Introspection supports the contract fields `active`, `user_id`, `tenant_id`, `roles`, `is_admin`, `expires_at`; token lookup uses prefix plus constant-time hash comparison, checks revocation/expiry and tenant context.
- Token operations are implemented: issue (`name`, `ttl`, default 30d), list metadata (`id`, `name`, `prefix`, `expires_at`, `revoked_at`), revoke, and an extra reveal endpoint backed by `token_encrypted`.
- Canonical `RequirePrincipal` currently declares and accepts only `X-Tenant-Id`, `X-User-Id`, `X-User-Role`; admin is exactly the literal `admin`.

### Missing vs docs
- All general account/tenant/auth-profile management routes are absent: tenants CRUD/list, tenant host list/bind, auth-profile list/create/get/update/delete, users list/create/get/update, role grant/revoke, and `/me`.
- `/audit` admin trail is absent even though token issue/revoke/reveal writes audit rows. There is no read/filter API for audit events.
- `DELETE /users/{user_id}/tokens/{token_id}` from docs is implemented as `POST /tokens/{tokenID}/revoke`; this is semantically equivalent but path/method differs and must be reconciled or documented as MVP deviation.
- The server principal model ignores documented optional claim headers: `X-Client-Id`, `X-Audience-Id`, `X-User-Email`, `X-Auth-User-Id`, `X-Auth-Email`; nginx already injects several of these. Unknown role values are silently non-admin instead of explicit invalid/unauthorized/forbidden behavior.
- Introspection only looks up one `tbl_user_roles` row; if multi-role support is added, `roles` will be under-reported. The migration currently constrains roles to `admin`/`user`, so this is a future contract risk rather than a current data bug.
- Tenant status, user status/metadata and auth-profile rotation semantics have no admin API, so operator control remains manual DB/bootstrap-only.

### Harness additions
- Thêm `ADMIN-EDGE-01`: tenant resolution, auth config, và introspect fail-closed trên host/profile missing, expired/revoked token, tenant mismatch, invalid token; assert không leak secrets/logs và constant-time behavior.
- Thêm `ADMIN-PRINCIPAL-01`: protected routes phải reject spoofed headers at boundary contract level, normalize `roles`/`is_admin` consistently between JWT and `abt_` paths, và audit cross-owner admin action.
- Thêm `ADMIN-TOKEN-01`: issue-once/list-no-secret/revoke idempotency semantics, expired/revoked introspection, encryption-at-rest availability, và reveal authorization/audit. Nêu rõ contract mismatch giữa `DELETE token` trong docs và `POST /tokens/{id}/revoke` trong code.

## Implementation scope 2026-11-07 (approved)

### Composition-root refactor (review feedback)
- Pattern: **`Mount(r chi.Router, d Deps)`** thay `Router(d) http.Handler` cho namespace/script/loadtest/adminapi — module KHÔNG tự tạo router; `pkgs/server/server.go` là **composition root duy nhất** (middleware auth/RBAC/audit chen tập trung).
- **`pkgs/testutil`** (test-only): `NewServer` dựng server composition THẬT; harness/test đi qua server thật — **không tái tạo auth trong tests** (xoá wrap tay `RequirePrincipal(Router(d))` ×3).
- Xoá helpers trùng: withKey ×3, do/doScript/doLT ×3, testDeps ×4, fixtures ×4 (test); requireIdempotencyKey ×3 + replay → `pkgs/idempotency`; loadNamespace ×2 + loadOwned → `store.LoadNamespaceScoped`; strPtr/jsonBytes/actorJSON → store helpers.

### Authz harness matrix (lấp 5 gap review)
| ID | Case | Status |
|---|---|---|
| API-AUTHZ-01 | fail-closed integration qua server THẬT trên mọi route group (ns/services/scripts/loadtests/admin operator) | implement |
| API-AUTHZ-02 | isolation per-resource: cross-owner GET/list → 404 no-leak cho namespace + **script + loadtest** | implement |
| API-AUTHZ-03 | admin cùng tenant mutation (PATCH/revalidate/stop/token ops) + cross-tenant deny + **assert audit row** mỗi cross-owner action | implement |
| API-AUTHZ-04 | role verify **DB server-side**: `X-User-Role` chỉ là hint; spoof header admin với DB role=user → chặn ở mutation | implement |
| API-CONC-01 | 2 goroutine PATCH cùng If-Match → 1×200/1×412; transition đồng thời tuần tự, version monotonic | implement |
| API-IDEM-01 | replay cùng object; conflict payload → 422; in-flight → 409 | implement |

### Fix list (từ CodeGraph review ở trên) — scope đợt này
- **High-1** loadtest.create: transition `QUEUED→PROVISIONING` qua fn trong tx (runner_ref + timeout_at cùng tx) + gọi `Runner.Start` sau commit
- **High-2** Queue.Submit → `accepted bool`; không kẹt VALIDATING (giữ status cũ + 429 khi full); staging file mất → VALIDATION_FAILED
- **High-3** Reconciler: runner ref "no such run" sau restart → FAILED (fail-closed, không kẹt)
- **Medium**: metadata (started_at/finished_at) gộp transition transaction khi có thể; spec update → ready_for_loadtest=false là effective gate (đã đúng, thêm test)
- **Authz**: RequirePrincipal verify role từ `tbl_user_roles` (DB server-side)

### Implemented (2026-11-07 — PR #20 + #21)
- [x] Composition-root refactor: Mount pattern + pkgs/testutil (NewServer thật) + pkgs/idempotency
- [x] Fix High-1/2/3 (loadtest create transition+Start, queue accepted bool, reconciler Missing→FAILED)
- [x] API-AUTHZ-01..04 + API-CONC-01 + API-IDEM-01 + API-VAL-01 + STATE-QUEUE/DB/MEM-01 + RUN-STOP/TIMEOUT
- [x] SQL harness DB-TXN-01..05 + IDEM/RESERVATION/TERMINAL/APPEND
- [x] K8s scaffold (build tag harness_k8s)
- [x] **Bonus bug thật**: adminapi issueToken/listTokens resolve target tenant từ DB (trước: admin tenant B issue token trong tenant A) — bắt bởi API-AUTHZ-03
- [x] TDD proof: bỏ RequireSelfOrAdmin → 2 case đỏ → restore xanh
- Prod tweak: queue-full → 429 QUOTA_EXCEEDED

### DEFER (ghi rõ, không làm đợt này)
- Admin CRUD surface: tenants/users/auth-profiles CRUD, `/audit` read API, `/me` — plan riêng
- RESOURCE-DECL-*: chỉ skeleton test silent-drop; full declaration surface theo docs là phase sau
- K8s egress harness: scaffold + `//go:build harness_k8s` (chạy khi có cluster)
- MVP deviation đã ghi: `POST /tokens/{id}/revoke` thay `DELETE /users/{uid}/tokens/{tid}` (docs/admin-oidc-integration-api.md)
