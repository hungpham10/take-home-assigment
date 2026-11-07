# Plan 17 — Service Management API đầy đủ (docs/service-management-api.md)

## Mục tiêu
Implement đầy đủ contract `docs/service-management-api.md`: service definition đầy đủ (deploy/mock/database_bindings/dependencies), paths theo **service_id** (không phải name), `/openapi` + `/apis` endpoints.

## Dependencies
- Plans 11 (service OpenAPI basic + kin-openapi), plans 16 (Mount pattern + testutil).

## CodeGraph baseline (2026-11-07 — bằng chứng gap)
- `(namespace.Deps).create` node 576 — flow chỉ: PrincipalOf → WriteCodeError → WithTimeout → JSONBytes → isUniqueViolation → audit → writeWithETag. **Không có createService/createDatabase/createIngress.**
- `createRequest` node 575: chỉ `name, description, ttl_expires_at` (docs yêu cầu slug/visibility/apply/spec).
- Kết luận khớp nhận định review: namespace POST chỉ tạo metadata; components phải tạo qua route riêng và mới chỉ có service basic.

## Deliverables
1. **Migration `0010_service_runtime_metadata.sql`** (chi tiết ở plans/18 §migration chung 0010): `tbl_services` thêm `service_id TEXT` (`svc-`+rand, unique per namespace), `description`, `deploy JSONB`, `mock JSONB`, `database_bindings JSONB`, `dependencies JSONB`.
2. **API** (paths theo service_id):
   - `POST /namespaces/{ns}/services` (Idempotency-Key) — full definition; openapi validate → 422; mock operations phải match operationId trong parsed summary → 422; database_bindings/dependencies refs phải tồn tại → 422
   - `GET/PUT/PATCH/DELETE .../services/{service_id}` — PUT full replace (revalidate spec), PATCH partial; DELETE bị reference → 409
   - `GET .../services/{service_id}/openapi` — spec content (content_type yaml/json)
   - `GET .../services/{service_id}/apis` — targets index từ parsed_summary
3. **Definition shape** (docs §5): `{service_id, name, description, deploy:{image,replicas,port,health_path,env[]}, openapi:{format,version,source,content_type,content}, mock:{enabled,mode,pool_class,defaults,operations[],capacity_request}, database_bindings[], dependencies[]}`
4. Backward-compat: routes theo name xoá (update e2e-smoke).

## Verification
- TDD qua testutil.NewServer; 422 matrix (spec sai, mock op không match, ref không tồn tại); 409 delete-referenced.
- CodeGraph re-index: `mountServices` flow có validateMock/validateBindings.
