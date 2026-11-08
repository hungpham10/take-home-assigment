# Plan 25 — Codebase layering PH1 (architecture.md §5.4 Stage 1)

## Goal
Thực thi §5 "Codebase Framework & Layering": Stage 1 — tách readiness projection/gate khỏi `pkgs/namespace` (package lớn nhất, 577 dòng lifecycle.go); sửa 2 §5.3 violations qua doc-driven amendment (B → D pure helpers; E → httpx); Stage 2/3 trigger chưa bắn — giữ baseline.

## Dependencies
- Plans/21/22 (readiness projection K8S mode — readiness code đã đủ lớn để tách).
- Audit import graph 2026-11-08: A↔A clean; statuswatcher→mockcontrol + transitive mockruntime; authz/idempotency→httpx.

## Deliverables
1. `pkgs/readiness` mới (thuần: store + serviceopenapi): readinessReport/componentBlock types, Gates() (từ readinessGates), ProjectionReport(), BlockNameOf, datasetForTenant, datasetEngineMismatch, ingressRulesOf. `pkgs/namespace` giữ handlers + gọi readiness.*.
2. Docs: §5.2.1 blocks (+readiness), §5.3 (B → D pure helpers; E → F, httpx), §5.4 Stage 1 ✅; plans/README; mvp-status.
3. Tests: unit move theo; harness/API tests giữ nguyên (composition không đổi).

## Out of scope
Stage 2 (split loadtest — chờ K8s runner/quota), Stage 3 (repo interfaces toàn diện — chờ second query consumer), hexagonal/store-per-aggregate.

## Verification
go test -race + lint xanh (pure move); import graph: namespace→readiness→(store, serviceopenapi), không cycle; e2e-smoke PASS; CI 5/5.
