# Triển khai loadtest-platform lên Kubernetes (kind / multi-cluster sau)

Mô hình 2 tầng bảo mật:

| Tầng | Cơ chế | Bảo vệ gì | Vị trí lưu |
|------|--------|-----------|------------|
| 1 | **SOPS + age** (file-level) | Toàn bộ env nhạy cảm (DB DSN, OIDC secrets, tenant secrets, `LTP_MASTER_KEY`) | `deploy/k8s/base/serve/secrets.enc.yaml` — commit được |
| 2 | **AES-256-GCM field-level** (`pkgs/secrets`) | Giá trị nhạy cảm trong DB (abt_ plaintext, shared secret sau này) | DB cột `BYTEA`, master key từ `LTP_MASTER_KEY` (tách từ SOPS env) |

Private age key **không bao giờ vào repo/image**; chỉ public key trong `.sops.yaml`.

## 1. Sinh age key (một lần duy nhất, trên máy vận hành)

```bash
mkdir -p ~/.config/sops/age
age-keygen -o ~/.config/sops/age/keys.txt
chmod 600 ~/.config/sops/age/keys.txt
```

Lấy **public key** (dòng bắt đầu `# public key:`):

```bash
sed -n 's/^# public key: //p' ~/.config/sops/age/keys.txt
```

> ⚠️ Đừng `head -1` — file bắt đầu bằng dòng `# created: …`; `head -1` trả comment tạo ra recipient sai.

Thêm public key vào `.sops.yaml` (model theo `env/secrets*.yaml`):

```yaml
creation_rules:
  - path_regex: deploy/k8s/.*\.enc\.(yaml|yml|json)$
    age: age1ql3z7hjy54pw3hyww5ayy…   # <-- public key thật của bạn
  - path_regex: env/secrets\.[a-z]+\.yaml$
    age: age1ql3z7hjy54pw3hyww5ayy…
```

## 2. Tạo file secrets mã hoá (commit được)

Viết plaintext (KHÔNG commit):

```bash
cat > /tmp/secrets.dev.yaml <<'EOF'
DATABASE_DSN: postgres://ltp:ltp@postgres.ltp-system.svc.cluster.local:5432/ltp?sslmode=disable
REDIS_HOST: valkey
REDIS_PORT: "6379"
OIDC_ISSUER: http://dex:5556/dex
OIDC_CLIENT_ID: ltp-test
OIDC_CLIENT_SECRET: ltp-dev-shared-secret-32-bytes-min!!
OIDC_SESSION_SECRET: global-dev-session-secret-32-bytes!
LTP_MASTER_KEY: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
LTP_TENANT_ACME_OIDC_CLIENT_SECRET: change-me-dev
LTP_TENANT_ACME_SESSION_SECRET: dev-session-secret-32-bytes-min!!
LTP_TENANT_ACME_JWT_SECRET: dev-jwt-secret-32-bytes-min!!
EOF

sops -e /tmp/secrets.dev.yaml > deploy/k8s/base/serve/secrets.enc.yaml
rm /tmp/secrets.dev.yaml
```

Sinh `LTP_MASTER_KEY` mới (32 byte hex = 64 ký tự):

```bash
openssl rand -hex 32
```

> ⚠️ `LTP_MASTER_KEY` phải đúng 32 byte sau khi hex-decode (64 ký tự hex). `pkgs/secrets.LoadMasterKey()` sẽ từ chối key sai độ dài và tính năng reveal token sẽ im lặng tắt (không crash).

Kiểm tra file đã mã hoá giải mã được đúng `KEY=VALUE`:

```bash
sops -d --output-type dotenv deploy/k8s/base/serve/secrets.enc.yaml
```

## 3. Tạo K8s Secret chứa `SOPS_AGE_KEY_CONTENT` (không commit)

```bash
kubectl -n ltp-system create secret generic sops-age-key \
  --from-literal=SOPS_AGE_KEY_CONTENT="$(cat ~/.config/sops/age/keys.txt)"
```

Nội dung file `keys.txt` (bao gồm dòng `# created:` và `# public key:`) phải paste **nguyên block** — thiếu một dòng thì `sops -d` fail.

## 4. Deploy lên kind

```bash
# 0. kind cluster (lần đầu)
kind create cluster --name ltp --wait 120s
kind export kubeconfig --name ltp

# 1. dependencies: CNPG + k6-operator + Calico (tái dùng script có sẵn)
#    tests/e2e-full-deploy.sh tự làm khi DEPLOY_MODE=in-cluster

# 2. build images + nạp vào kind
earthly +integration-images          # → loadtest-platform-serve:local, mock-service:local
kind load docker-image loadtest-platform-serve:local loadtest-platform-mock-service:local --name ltp

# 3. apply manifest
kubectl apply -k deploy/k8s/overlays/kind
kubectl -n ltp-system rollout status deploy/serve --timeout=180s
kubectl -n ltp-system rollout status deploy/dex --timeout=180s
kubectl -n ltp-system rollout status sts/postgres --timeout=180s

# 4. port-forward (mỗi terminal một)
kubectl -n ltp-system port-forward svc/serve 8080:8080 &
kubectl -n ltp-system port-forward svc/dex 5556:5556 &
```

## 5. Lấy bearer token từ Dex (không mở browser)

```bash
export LTP_DEX_ISSUER=http://localhost:5556/dex
export LTP_DEX_CLIENT_ID=ltp-test
export LTP_DEX_CLIENT_SECRET=ltp-dev-shared-secret-32-bytes-min!!
export LTP_DEX_USER=dev-user@example.com
export LTP_DEX_PASSWORD=password

TOKEN=$(python3 scripts/mint-dex-token.py)
curl -H "Host: localhost" -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/loadtest/v1/namespaces
```

`scripts/mint-dex-token.py` drive HTML login của Dex và xuất `id_token` (JWT RS256) — đúng cơ chế `tests/integration/dex.go` (port từ `sources/opsense-rs/scripts/mint-dex-token.py`).

## 6. CLI `sops` subcommand (encrypt/decrypt giá trị nhạy cảm)

```bash
export LTP_MASTER_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef

# Encrypt plaintext → hex blob (paste thẳng vào SQL seed)
./bin/loadtest-controller sops encrypt "client-secret-abc"
# → \x3f9a…

# Decrypt ngược lại để verify seed (nhận \x / 0x / hex trần)
./bin/loadtest-controller sops decrypt '\x3f9a…'
# → client-secret-abc
```

Dispatch xảy ra **trước** `config.FromEnv()`, nên không cần `DATABASE_DSN`/DB khi chạy `sops`. Master key đọc từ `LTP_MASTER_KEY` (SOPS env — nếu unset, `Encrypt`/`Decrypt` báo lỗi rõ ràng thay vì panic).

## 7. Teardown

```bash
kubectl delete -k deploy/k8s/overlays/kind
kind delete cluster --name ltp
```

## 8. CI

Job `Full Deploy E2E (in-cluster)` trong `.github/workflows/ci.yml`:
1. `checkout` + `setup-go`
2. cài `kind` v0.24.0 + `kubectl` v1.31.0
3. tạo kind cluster (`ltp-full-deploy`, retry x3)
4. `earthly +integration-images` → `kind load docker-image` (serve + mock-service)
5. `DEPLOY_MODE=in-cluster ./tests/e2e-full-deploy.sh`
6. dump cluster state (pods/events/CNPG/TestRun/controller logs) khi fail

Estimate: 15–20 phút (earthly cold build 6–10m + bootstrap + lifecycle). Timeout 45m.

## Mẹo debug

- Xem log controller: `kubectl -n ltp-system logs deploy/serve -c serve` (supervisord gom stdout)
- Xem TestRun: `kubectl -n lt-env-<ns> get testrun -o wide`
- Xem CNPG cluster: `kubectl -n lt-env-<ns> get cluster`
- Xem event NetworkPolicy: `kubectl -n lt-env-<ns> get networkpolicy`
- Dump toàn bộ khi fail: `kubectl get ns,deploy,svc,pods,testruns,clusters -A` + `kubectl events`
