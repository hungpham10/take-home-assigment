# Plan 29 — E2E L3/L4 probes: verify in-flight, before run terminal

## Mục tiêu

Sửa `tests/e2e-full-deploy.sh` step 13 (L3/L4 NetworkPolicy probes) để không race với việc loadtest reconciler xoá run-scoped NetworkPolicies ngay khi run sang `COMPLETED`.

## Nguyên nhân gốc (đã xác nhận trên CI run 42770172357)

- Step 11 `loadtest COMPLETED ✅` ở `06:46:27`.
- Reconciler (`pkgs/loadtest/reconciler.go` `finish()`) chạy mỗi **2s** (`cmds/loadtest-controller/main.go:80`) và xoá run-scoped NetworkPolicies + script ConfigMap khi run terminal (dòng 104-111).
- Step 13 chạy **sau** step 11 ⇒ khoảng 06:46:29 `loadtest-run-egress-<LT_ID>` đã bị xoá ⇒ chỉ còn `default-deny` ⇒ `runner→echo:80 denied` ⇒ E2E fail.

**Không phải** bug render policy (step 10 đã xác nhận policy đúng target/port), và **không phải** Calico leftover (chẩn đoán trước sai — local fail cùng nguyên nhân).

## Thay đổi

### 1. `tests/e2e-full-deploy.sh`

- **Di chuyển block 13 (L3/L4 probes) lên trước block 11 (wait completion)**: tạo probe pod ngay sau step 10 (policy đã được assert tồn tại), chạy 13a/13b/13c trong lúc run còn sống, policy chưa bị xoá.
- **Tăng `duration` 20s → 45s** (dòng 289): để có headroom cho probe pod start ~5s + 3 phép wget, tránh flake khi CI chậm. Run vẫn kết thúc sớm (k6 20s workload) nhưng cửa sổ kiểm chứng rộng hơn.
- **Thêm assert sau step 11**: trong ~15s, các run-scoped NetworkPolicies (`loadtest-run-egress-<id>`, `loadtest-run-ingress-<id>-echo`, `loadtest-run-api-<id>`) phải đã bị xoá → kiểm chứng hygiene §3.3 (đúng thiết kế, không phải flaky).

### 2. `docs/network-policy.md` §3.3 (đã làm)

Thêm câu: "Enforcement verification must happen while the run is alive... Running L3/L4 probes after completion asserts 'no policy exists' (weak), not 'policy scoped correctly' (strong)."

### 3. TDD

Đây là thay đổi script E2E (test-side), không có prod code thay đổi → không viết unit test mới. Quy tắc "failing test first" áp prod code; đây là reorder/assert trong shell script.

## Acceptance criteria

- [ ] `go build ./...` ✅ (không có prod code thay đổi, nhưng chạy để chắc)
- [ ] `go test ./...` ✅
- [ ] `golangci-lint run ./...` ✅ 0 issues
- [ ] E2E full-deploy local xanh: step 10 → step 13 probes in-flight → step 11 COMPLETED → assert policies đã bị xoá
- [ ] Push branch, mở PR, squash-merge khi CI green
- [ ] CodeGraph review: không trùng code, không thiếu edge case

## Không làm trong plan này

- Thay đổi prod code (`pkgs/`) — reconciler xoá policy sau finish là đúng thiết kế (§3.3).
- Thêm env flag test-only để giữ policy sau finish.
- Sửa k6-operator (`sources/` read-only).
