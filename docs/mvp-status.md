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
                             ├─ pkgs/runner         Runner interface: Local (k6 binary) | K8s (cắm sau)
                             ├─ pkgs/secrets        SOPS env resolver + AES-256-GCM field-level
                             └─ pkgs/store          GORM models + migration runner + Transition (Raw duy nhất)
                                    ▼
                            PostgreSQL (9 migrations, tbl_schema_migrations tracked)
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
| POST | `/namespaces/{id}/apply` | Idempotency-Key; →DATA_LOADING→READY; resolve deps → network_policies |
| GET | `/namespaces/{id}/readiness` | checklist gates (services, policies, mock_capacity hook) |
| POST | `/namespaces/{id}/scripts` | ≤1MiB; async validate → VALIDATED/INVALID |
| POST | `/namespaces/{id}/scripts/{sid}/validate` | revalidate |
| GET | `/namespaces/{id}/scripts/{sid}/validations` | attempts + errors JSONB |
| POST | `/namespaces/{id}/loadtests` | gates: NAMESPACE_NOT_READY/SCRIPT_NOT_VALIDATED → 409; targets rỗng → 422 |
| POST | `/namespaces/{id}/loadtests/{lt}/stop` | →STOPPING (reaper/reconciler hoàn tất) |
| GET | `/namespaces/{id}/loadtests/{lt}/events` | transition trail |

Health: `/health`, `/ready` (fail-closed DB), `/metrics` (prometheus).

## 3. DB schema (9 migrations — nguồn chân lý `sql/`)

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
| E2E | `scripts/e2e-smoke.sh` — seed → ns → service → validate → apply → script → loadtest → events (**PASS đã verify**) | local: `docker compose up -d` + chạy script |
| Perf | `tests/perf/control-plane.js` (40rps reads p95<100ms), `admin.js` (80rps introspect) | k6 run — **chưa chạy gate lần nào** (k6 binary chưa cài) |

Tổng: 17 test files, 12/12 packages pass với `-race`.

## 6. Chạy cục bộ

```bash
docker compose up -d --build          # postgres + controller
./scripts/e2e-smoke.sh                # e2e pass/fail
earthly +controller-image             # build image (pattern opsense-rs)
```

CI (GitHub Actions): lint (golangci-lint v2) + test (`-race`, postgres service) + build + sql-migrations — **4/4 xanh trên main**.

## 7. Gap / việc còn lại

1. **K8s runner thật** — `runner.Runner` interface sẵn, `newRunner()` vẫn luôn trả Local; cần client tạo `k6.io/v1alpha1 TestRun` (sample: `docs/k6-operator-go-client.md`) + integration test với kind cluster
2. **k6 perf gate chưa chạy** — cần cài k6 và chạy `tests/perf/*` ít nhất 1 lần, ghi số liệu vào README
3. **mock-service** — phase sau (biên riêng, không dùng pkgs/controller); endpoint mock-status/mock-pools còn stub
4. **Outbox worker** — bảng + ghi outbox đã có, chưa có dispatcher loop
5. **Quota gate 429** — `tbl_reservation_ledger` có schema, chưa wire vào loadtest create
6. **CodeGraph final review + bump version** (`scripts/bump.sh`) — theo AGENTS.md trước khi coi là release
7. Plans 16 (harness-testing-integrity) — do user tạo, chưa implement

## 8. Điểm nên review kỹ khi code review

1. `pkgs/store/transitions.go` — điểm Raw duy nhất; mọi status change phải đi qua đây
2. `pkgs/store/models.go` — `BeforeCreate` UUID (GORM insert '' đè DEFAULT nếu thiếu); json tags
3. `pkgs/authz/selforadmin.go` + `pkgs/adminapi/handlers.go` — trust boundary, constant-time compare, secret handling
4. `pkgs/loadtest/reconciler.go` — transition-first (không UPDATE status trực tiếp), idempotent reap
5. `pkgs/namespace/handlers.go` — idempotency replay (captureWriter), ETag, 404-vs-403
6. `pkgs/secrets/crypto.go` — format nonce(12)||ciphertext, master key lifecycle
7. `sql/0002` — transition function template (dùng `%I` dynamic SQL — review injection surface)
