# Plan 18 — Namespace create-with-spec + Database/Datasets/Ingress/Dependencies (docs/namespace-management-api.md + docs/database-management-api.md)

## Mục tiêu
1. **Namespace create-with-spec** (docs/namespace-management-api.md create): `POST /namespaces` nhận `{name, slug, visibility, apply, spec:{services[], databases[], ingress[], dependency_policy}}` — tạo atomic toàn bộ components.
2. Database CRUD, Datasets API, Ingress CRUD, Dependencies/NetworkPolicy views, mock stubs, restore flow MVP.

## Dependencies
- Plans 17 (service definition), plans 16 (Mount/testutil), migration 0010 dùng chung.

## Migration `0010_namespace_spec.sql` (dùng chung với plans/17)
- `tbl_namespaces`: +`slug TEXT` (unique), `visibility TEXT DEFAULT 'tenant'`
- `tbl_services` / `tbl_databases` / `tbl_ingress`: +opaque `*_id TEXT` unique per ns + runtime metadata JSONB (deploy/mock/bindings/dependencies; operator/version/config/init_script_ref/data_source/restore_policy/ephemeral/mock/secret_ref; class/hosts)
- `tbl_datasets`: +`owner_user_id` (immutable), `checksum JSONB` (REQUIRED sha256), `engine`, `engine_version`, `location JSONB`, `restore_format`, `schema_version`, `retention_until`

## Deliverables
### 1. Namespace create-with-spec (`pkgs/namespace/handlers.go`)
- `createRequest` đầy đủ docs field (name, slug, description, visibility, ttl, apply, spec{services[],databases[],ingress[],dependency_policy})
- 1 transaction: ns DRAFT + services (validate openapi + service_id=`svc-`+rand) + databases + ingress (validate service refs) — **invalid component → 422 + rollback toàn bộ** (atomic)
- `apply=true`: tiếp validate→apply→READY ngay (dataset → DATA_LOADING trước)
- 201 stored; Idempotency-Key replay; backward-compat spec optional

### 2. Databases CRUD `/namespaces/{ns}/databases[/{database_id}]`
- POST (Idempotency-Key), GET list/detail, PUT/PATCH, DELETE → 409 khi bị service bindings
- Validate: engine=postgres, data_source.dataset_id tồn tại+AVAILABLE khi set, mock profile → 422

### 3. Datasets `/datasets`
- POST (202 IMPORTING, owner-only, Idempotency-Key), GET list/detail (no raw download), PATCH (retention/policy), DELETE (409 khi namespace reference)
- POST /{id}/validate (202): checksum sha256 + PII patterns (email, VN phone) → INVALID khi fail

### 4. Ingress CRUD `/namespaces/{ns}/ingress[/{ingress_id}]`
- hosts rules → service_id+port phải tồn tại → 422

### 5. Views + mock stubs
- `GET /namespaces/{ns}/dependencies` (resolve services.dependencies + bindings), `GET .../network-policies` (effective spec)
- `GET .../mock-status`, `POST .../mocks/reconcile` (202), `GET /mock-pools` — projection từ mock metadata (stub rõ ràng, mock-service phase sau)

### 6. Readiness rework + restore MVP
- Checklist docs shape: `components:{services,databases,data_load,ingress,dependencies,network_policy,mocks}` + `blocking_reasons[]` — thay hardcode mock_capacity
- Restore MVP: apply có data_source → DATA_LOADING → stub worker (validate dataset checksum/status/engine) → READY; fault → DEGRADED/INVALID. K8s pg_restore Job thật: phase sau

### 7. Harness mở rộng
- API-AUTHZ-02/03 thêm case cross-owner cho databases/datasets/ingress

## Verification
- TDD qua testutil.NewServer; atomic-rollback test (spec sai → không còn row nào)
- e2e-smoke: create-with-spec + database + dataset cơ bản
- CodeGraph re-index: flow `create` có createService/createDatabase/createIngress
- go test -race + Postgres, golangci-lint 0, CI 4/4
