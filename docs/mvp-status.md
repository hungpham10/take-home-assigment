# MVP Status — Tóm tắt tích hợp & Review

> Trạng thái sau khi hoàn thành plans 01–15 + 3 hardening/refactor PR.
> Dùng làm entry point cho **integration test** và **code review**.

## 1. Kiến trúc đã build

```
nginx (conf/nginx — có sẵn, chưa đụng)
  └─ /api/admin/v1    ─┐
  └─ /api/loadtest/v1 ─┴─► loadtest-controller (go-chi, :8080)
                             ├─ pkgs/authz          trusted headers → Principal, fail-closed, RBAC
                             ├─ pkgs/adminapi       tenant-id, auth-config, introspect abt_, issue/reveal/revoke token
                             ├─ pkgs/namespace      CRUD + Idempotency-Key + ETag + validate/apply/readiness
                             ├─ pkgs/serviceopenapi kin-openapi validate + targets[] index
                             ├─ pkgs/script         upload ≤1MiB + async validation workers (k6 inspect/structural)
                             ├─ pkgs/loadtest       gates 409/422 + lifecycle + reconciler (2s poll)
                             ├─ pkgs/runner         Runner interface: Local (k6 binary) | K8s (k6-operator TestRun qua Go client, RUNNER_MODE=k8s)
                             ├─ pkgs/secrets        SOPS env resolver + AES-256-GCM field-level
                             └─ pkgs/store          GORM models + migration runner + Transition (Raw duy nhất)
                                    ▼
                            PostgreSQL (13 migrations, tbl_schema_migrations tracked)
```

## 2. API surface (đã implement)

### `/api/admin/v1` — nginx-internal (trusted channel, không principal)
| Method | Path | Ghi chú |
|---|---|---|
| GET | `/tenant/{host}/id` | plain UUID (Lua `read_body` → header) |
| GET | `/tenant/{host}/auth-config?name=` | shape `{auth:{...}}`; secrets từ `LTP_TENANT_<SLUG>_*` env |
| POST | `/token/introspect` | `{active,user_id,tenant_id,roles,is_admin}` — prefix lookup + constant-time hash |

### `/api/admin/v1` — operator (RequirePrincipal + RequireSelfOrAdmin)
| Method | Path | Ghi chú |
|---|---|---|
| POST | `/users/{uid}/tokens` | issue `abt_` — plaintext 1 lần; encrypted at rest |
| GET | `/users/{uid}/tokens` | list metadata (không plaintext) |
| POST | `/tokens/{id}/revoke` | self-or-admin |
| POST | `/tokens/{id}/reveal` | decrypt AES-GCM (cần `LTP_MASTER_KEY`), audit-logged |

### `/api/loadtest/v1` — RequirePrincipal (fail-closed 401)
| Method | Path | Ghi chú |
|---|---|---|
| POST | `/namespaces` | Idempotency-Key bắt buộc, replay cached response |
| GET/PATCH/DELETE | `/namespaces[/{id}]` | owner-only; admin cross-owner cùng tenant; 404 chống leak; ETag/If-Match → 412 |
| GET | `/namespaces/{id}/events` | transition history |
| POST | `/namespaces/{id}/services` | OpenAPI validate → 422 + reason; spec change → ready reset |
| PUT | `/namespaces/{id}/services/{svc}/spec` | same-content → unchanged |
| POST | `/namespaces/{id}/validate` | →VALIDATING→(INVALID+422 checklist \| APPLYING) |
| POST | `/namespaces/{id}/apply` | Idempotency-Key; resolve deps → network_policies. **K8S_ENABLED=true:** chỉ bump `desired_generation`, trả 202 APPLYING — Env Reconciler apply K8s, Status Watcher projection đóng vòng APPLYING→READY (plans/21). **Không có cluster (legacy):** giữ nhánh sync `→DATA_LOADING→READY` cho stack MVP |
| GET | `/namespaces/{id}/readiness` | checklist gates (services, policies, mock_capacity hook) |
| POST | `/namespaces/{id}/scripts` | ≤1MiB; async validate → VALIDATED/INVALID |
| POST | `/namespaces/{id}/scripts/{sid}/validate` | revalidate |
| GET | `/namespaces/{id}/scripts/{sid}/validations` | attempts + errors JSONB |
| POST | `/namespaces/{id}/loadtests` | gates: NAMESPACE_NOT_READY/SCRIPT_NOT_VALIDATED → 409; targets rỗng → 422 |
| POST | `/namespaces/{id}/loadtests/{lt}/stop` | →STOPPING (reaper/reconciler hoàn tất) |
| GET | `/namespaces/{id}/loadtests/{lt}/events` | transition trail |

Health: `/health`, `/ready` (fail-closed DB), `/metrics` (prometheus).

## 3. DB schema (13 migrations — nguồn chân lý `sql/`)

| Migration | Nội dung |
|---|---|
| 0001 | tenants, tenant_hosts, auth_profiles, users, external_identities, user_roles, access_tokens (hash), audit_log |
| 0002 | **pattern**: `tbl_status_transitions` + `fn_is_transition_allowed` + `fn_status_transition` |
| 0003 | namespaces + events + `fn_namespace_transition` + services/databases/ingress/dependencies/network_policies |
| 0004 | scripts + events + validations, load_tests + events + `fn_loadtest_transition`, test_events |
| 0005 | idempotency_keys, outbox_events, reservation_ledger, datasets + events |
| 0006–0009 | seed transitions bổ sung, strip secrets khỏi DB, token_encrypted, revalidate VALIDATED |

**Invariants đã kiểm bằng SQL test (CI job sql-migrations):**
- Mọi status change qua `fn_*_transition` (FOR UPDATE → check map → status+version+1 → event, 1 transaction)
- Events append-only; `tbl_<object>` là nguồn đọc (JOIN + index)
- 4 bộ test: `tests/sql/{transition,namespace_transition,loadtest_transition,plumbing}_test.sql`

## 4. Security checklist

- ✅ Fail-closed 401 khi thiếu identity headers; 404 (không 403) chống existence leak
- ✅ `RequireSelfOrAdmin`: self ✓, admin cùng tenant ✓, admin khác tenant ✗, role spoof ✗
- ✅ `abt_` chỉ lưu SHA-256 hash + prefix; plaintext encrypted AES-256-GCM (`LTP_MASTER_KEY` từ SOPS) cho reveal
- ✅ Secrets không nằm trong DB (migration 0007 strip); SOPS file-level (age) + field-level (pattern sops.rs)
- ✅ Auth-config response chứa secret — trusted channel only, warn log không kèm value
- ✅ Path traversal chặn khi đọc staged scripts
- ⚠️ Known gap (docs đã ghi): nginx chỉ set `X-User-Role` trên Bearer-JWT path — RBAC dựa thêm vào `tbl_user_roles` server-side

## 5. Testing

| Tầng | Nội dung | Chạy ở đâu |
|---|---|---|
| Unit | authz (6 case), ETag, idempotency middleware, secrets resolver/crypto, serviceopenapi | `go test ./...` — không cần DB |
| Integration (GORM + Postgres 16) | adminapi, namespace, script, loadtest, reconciler, store migration runner | CI job `test` (postgres service); local skip khi không có DB, **hard-fail khi `CI=true`** |
| SQL | transition pattern 4 bộ | CI job `sql-migrations` |
| E2E | `tests/e2e-smoke.sh` — seed → ns → service → validate → apply → script → loadtest → events → delete namespace (**PASS đã verify**) | local: `docker compose up -d` + chạy script |
| Perf | `tests/perf/control-plane.js` (40rps reads p95<100ms), `admin.js` (80rps introspect) | k6 run — **chưa chạy gate lần nào** (k6 binary chưa cài) |

Tổng: 17 test files, 12/12 packages pass với `-race`.

## 6. Chạy cục bộ

```bash
docker compose up -d --build          # postgres + controller
./tests/e2e-smoke.sh                  # e2e pass/fail
earthly +controller-image             # build image (pattern opsense-rs)
```

CI (GitHub Actions): lint (golangci-lint v2) + test (`-race`, postgres service) + build + sql-migrations — **4/4 xanh trên main**.

## 7. Gap / việc còn lại

1. **K8s runner đã implement** — `runner.Runner` interface + `K8sTestRunRunner` khi `RUNNER_MODE=k8s` (tạo `k6.io/v1alpha1 TestRun` qua Go client, script qua ConfigMap). Integration test qua `tests/e2e-full-deploy.sh` (kind + k6-operator) ✅ plan 27.
2. **k6 perf gate chưa chạy** — cần cài k6 và chạy `tests/perf/*` ít nhất 1 lần, ghi số liệu vào README
3. **mock-service** — binary tồn tại tại `cmds/mock-service` (data-plane riêng, không dùng pkgs/controller); endpoint mock-status/mock-pools còn stub, metadata CRUD qua namespace components.
4. **Outbox worker** — bảng + ghi outbox đã có, chưa có dispatcher loop
5. **Quota gate 429** — `tbl_reservation_ledger` có schema, chưa wire vào loadtest create
6. **CodeGraph final review + bump version** (`scripts/bump.sh`) — theo AGENTS.md trước khi coi là release
7. Plans 16 (harness-testing-integrity) — do user tạo, chưa implement

## 7b. Harness (plans/16 — PR #20/#21, 2026-11-07)
- Composition root tập trung: `Mount(r chi.Router, d Deps)`, server.go duy nhất configure middleware; tests qua `pkgs/testutil.NewServer` (không tái tạo auth)
- Fix High: loadtest create transition-in-tx + Runner.Start; queue-full → 429 (không kẹt VALIDATING); reconciler runner-lost → FAILED
- Role verify DB server-side (`RequirePrincipalWithRole`) — spoof header chặn
- Harness matrix: `tests/harness/` (AUTHZ-01..04, IDEM, CONC, VAL, STATE-*, RUN-*) + `tests/sql/harness_test.sql` (DB-TXN-01..05...) — **TDD proof: phá invariant → đỏ**
- Bonus fix: cross-tenant token issue hole (bắt bởi harness)
- Deviation ghi nhận: `POST /tokens/{id}/revoke` thay `DELETE /users/{uid}/tokens/{tid}`; admin CRUD surface defer

## 8. Điểm nên review kỹ khi code review

1. `pkgs/store/transitions.go` — điểm Raw duy nhất; mọi status change phải đi qua đây
2. `pkgs/store/models.go` — `BeforeCreate` UUID (GORM insert '' đè DEFAULT nếu thiếu); json tags
3. `pkgs/authz/selforadmin.go` + `pkgs/adminapi/handlers.go` — trust boundary, constant-time compare, secret handling
4. `pkgs/loadtest/reconciler.go` — transition-first (không UPDATE status trực tiếp), idempotent reap
5. `pkgs/namespace/handlers.go` — idempotency replay (captureWriter), ETag, 404-vs-403
6. `pkgs/secrets/crypto.go` — format nonce(12)||ciphertext, master key lifecycle
7. `sql/0002` — transition function template (dùng `%I` dynamic SQL — review injection surface)

### plans/22 — Watcher hardening (2026-11-08)

- `GET /readiness` **projection-backed khi K8S mode**: đọc `tbl_component_status` + `blocking_reasons` (fall back về readinessGates khi projection trống); legacy mode giữ compute on-the-fly.
- Watcher event source: `k8s.Client.Watch` per GVK → workqueue dedup/rate-limited (client-go workqueue), workers gọi `ReconcileStatus`; polling resync giữ làm safety net.
- `apply_deadline` enforce: namespace kẹt APPLYING/DATA_LOADING quá deadline → DEGRADED reason `apply_deadline_exceeded`; đủ 5 knobs `WATCHER_*` wired.
- DEGRADED-with-running-run: event `namespace_degraded_at` metadata; GET loadtest + events trả `unreliable: true` khi namespace DEGRADED sau start của run (run không bị stop).
- Harness WATCH-01..06: fake-client cases (CI `test` job) + `//go:build harness_k8s` trên kind (CI job `watcher-harness`) / colima local.

### plans/25 — Codebase layering PH1 (2026-11-08)

- §5.4 Stage 1 ✅: tách `pkgs/readiness` (Gates/ProjectionReport/BlockNameOf + dataset/ingress helpers) khỏi `pkgs/namespace` — handlers giờ gọi `readiness.*`; readiness là DESIRED-side logic, watcher giữ OBSERVED-side evaluators.
- §5.3 amendment: `B → D(pure helpers only)` (mockcontrol.DecodeMockDef, envreconciler render) + `E → F, httpx` — khớp §5.2.1 "shared code is pure helpers only".
- Stage 2/3 giữ baseline (trigger chưa bắn): split loadtest chờ K8s runner/quota; ~55 gorm refs in handlers ghi trong §5.4 để refactor khi có second query consumer.

### plans/26 — Full deploy E2E + apply quản lý đầy đủ objects (2026-11-08)

- **Apply orchestrate đầy đủ K8s objects** per component (Go-render, giữ ADR-004): service → ConfigMap (env lớn) + Secret (Opaque, env qua `valueFrom.secretKeyRef` — plaintext KHÔNG bao giờ vào manifest, fix vi phạm docs §7:84) + Deployment + Service + HPA (autoscaling/v2) + ServiceAccount; database → operator CR theo cột `operator` (`cloudnative-pg` | `elastic`/ECK — unknown fail closed trước khi apply). Prune mở rộng cho 4 kind mới.
- **Full deploy E2E** (`tests/e2e-full-deploy.sh` + CI job `full-deploy-e2e`): create-with-spec 1 call (database CNPG demo-db + echo-server) → validate → apply 202 → READY trên kind → kubectl verify `cluster,hpa,cm,secret,deploy,svc` → port-forward curl echo trả JSON → DELETE namespace (202/DELETING) + verify K8s namespace `lt-env-*` bị xoá.
- **Bỏ fakeWatcher** (plans/26): `pkgs/k8s` Fake giữ Apply/Get/List/Delete cho unit fault-injection; watch path test real trên kind (`TestWATCH08WatchEventsTriggerReconcile`). Fix 2 bug lộ ra: consumeWatch tight-spin khi channel đóng (starve reconcile), Tick stall khi 1 namespace lỗi chặn các ns khác (per-ns log-and-continue).
- **202 + status_url** cho create-with-spec + `apply:true` ở K8S mode (docs namespace-management-api.md:93) — apply/reconcile BẮT ĐẦU chứ không hoàn thành; legacy giữ 201 READY.
- Tests mới: INT-NS-02b (202+Location), INT-NS-34 (combined flow), render ConfigMap/Secret/HPA/SA/ECK + unknown-operator fail-closed.

- **Hierarchical apply (plans/26 bổ sung)**: `Applier` interface (`Kind/Name/Apply/Children`) — apply cây ngoài→trong qua `ApplyTree`; `applyNamespace` (K8S) gọi `ApplyNow` ngay sau desired-state commit (best-effort, watcher + loop tự converge). Tick batch 100/ lần + ưu tiên `status_observed_at NULLS FIRST`; tests dọn namespace sau run (backlog 1133 ns cũ từng stall Tick). Tests: INT-ENV-08 (apply order), INT-ENV-09 (aggregation all-children-ready).
- **Centralize state machines (plans/26, phản hồi review)**: `pkgs/store/{namespace,loadtest,script,dataset}_lifecycle.go` — status constants + typed wrappers + bảng caller-owner; mọi call-site migrated (loadtest 10, namespace 5, dataset 3, script 2); **lint-transitions guard** trong CI chặn raw `store.Transition` ngoài `pkgs/store/` — thêm/sửa trạng thái chỉ review 1 file.

### plans/34 — AIO serve image + Dex compose + integration tests (2026-11-11)

- **Earthfile rewrite** (pattern `sources/opsense-rs/Earthfile`): `+build` 2 binary (fast path `bin/` host, fallback go build); `+serve` image AIO openresty-fat (nginx + supervisor + controller **unix socket** `unix:/var/run/axum` + mock-service :8090 + alloy); `+mock-service` image riêng (data-plane); `+integration-images` build local no-push.
- **Controller unix socket (TDD)**: `ADDR=unix:/var/run/axum` → `net.Listen("unix")` + rm stale socket + chmod 666; TCP giữ nguyên. nginx `conf/nginx` KHÔNG đổi.
- **docker-compose**: postgres + valkey (noeviction, session lua) + dex v2.41.1 (conf/dex/config.dev.yaml, issuer localhost:5556/dex) + loadtest-serve (SOPS mount, healthcheck /health).
- **Integration test** pattern opsense-rs: `tests/integration/` (tag `integration`) — dex login flow Go port, health smoke qua nginx, oauth flow qua gateway `/login`+`/callback`; skip khi không có stack, fail hard khi `LTP_INTEGRATION=1`. `scripts/mint-dex-token.py` host CLI. Makefile `test-integration`/`test-integration-down`/`up`/`down`.
- Chi tiết: `docs/packaging-deployment.md`, `plans/34-aio-serve-image.md`.

### plans/36 — Deploy lên Kubernetes (kind) + full test in-cluster (2026-11-11)

- **Manifest**: `deploy/k8s/` (base: ltp-system, mock-system, postgres StatefulSet, valkey, dex, mock-service, serve AIO Deployment/Service/SA/RBAC/SOPS secrets; overlays: `kind/`, khung `prod/`).
- **Secrets 2 tầng**: SOPS + age (`deploy/k8s/base/serve/secrets.enc.yaml`, commit được) mount qua ConfigMap + `SOPS_AGE_KEY_CONTENT` Secret → `scripts/release.sh` giải mã boot; `pkgs/secrets` field-level AES-256-GCM (`LTP_MASTER_KEY`) cho giá trị trong DB.
- **CLI**: `loadtest-controller sops encrypt|decrypt` (hex blob, pattern `opsense-rs sops.rs`) — sinh/verify `token_encrypted` khi seed SQL.
- **Test**: CI job `Full Deploy E2E (in-cluster)` — Earthly build → kind load → kustomize apply → `tests/e2e-full-deploy.sh DEPLOY_MODE=in-cluster` (bearer token từ `scripts/mint-dex-token.py`) → `tests/integration` qua port-forward.
- Chi tiết: `docs/k8s-deployment.md`, `plans/36-k8s-manifests.md`.
