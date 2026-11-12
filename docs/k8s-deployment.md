# Deploying loadtest-platform to Kubernetes (kind / multi-cluster later)

Two-layer security model:

| Layer | Mechanism | What it protects | Where it is stored |
|------|--------|-----------|------------|
| 1 | **SOPS + age** (file-level) | All sensitive env values (DB DSN, OIDC secrets, tenant secrets, `LTP_MASTER_KEY`) | `deploy/k8s/base/serve/secrets.enc.yaml` — safe to commit |
| 2 | **AES-256-GCM field-level** (`pkgs/secrets`) | Sensitive values in the DB (abt_ plaintext, shared secrets later on) | DB column `BYTEA`, master key from `LTP_MASTER_KEY` (separated from the SOPS env) |

The private age key **never goes into the repo/image**; only the public key lives in `.sops.yaml`.

## 1. Generate an age key (one time only, on the operator machine)

```bash
mkdir -p ~/.config/sops/age
age-keygen -o ~/.config/sops/age/keys.txt
chmod 600 ~/.config/sops/age/keys.txt
```

Get the **public key** (the line starting with `# public key:`):

```bash
sed -n 's/^# public key: //p' ~/.config/sops/age/keys.txt
```

> ⚠️ Do not use `head -1` — the file starts with a `# created: …` line; `head -1` returns that comment and yields the wrong recipient.

Add the public key to `.sops.yaml` (modelled on `env/secrets*.yaml`):

```yaml
creation_rules:
  - path_regex: deploy/k8s/.*\.enc\.(yaml|yml|json)$
    age: age1ql3z7hjy54pw3hyww5ayy…   # <-- your real public key
  - path_regex: env/secrets\.[a-z]+\.yaml$
    age: age1ql3z7hjy54pw3hyww5ayy…
```

## 2. Create the encrypted secrets file (safe to commit)

Write the plaintext (do NOT commit):

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

Generate a new `LTP_MASTER_KEY` (32 bytes hex = 64 characters):

```bash
openssl rand -hex 32
```

> ⚠️ `LTP_MASTER_KEY` must be exactly 32 bytes after hex-decoding (64 hex characters). `pkgs/secrets.LoadMasterKey()` rejects a key of the wrong length, and the token reveal feature is silently disabled (no crash).

Verify that the encrypted file decrypts to the correct `KEY=VALUE` pairs:

```bash
sops -d --output-type dotenv deploy/k8s/base/serve/secrets.enc.yaml
```

## 3. Create the K8s Secret holding `SOPS_AGE_KEY_CONTENT` (do not commit)

```bash
kubectl -n ltp-system create secret generic sops-age-key \
  --from-literal=SOPS_AGE_KEY_CONTENT="$(cat ~/.config/sops/age/keys.txt)"
```

The contents of `keys.txt` (including the `# created:` and `# public key:` lines) must be pasted as the **entire block** — a missing line makes `sops -d` fail.

## 4. Deploy to kind

```bash
# 0. kind cluster (first time)
kind create cluster --name ltp --wait 120s
kind export kubeconfig --name ltp

# 1. dependencies: CNPG + k6-operator + Calico (reuse the existing script)
#    tests/e2e-full-deploy.sh does this automatically when DEPLOY_MODE=in-cluster

# 2. build images + load them into kind
earthly +integration-images          # → loadtest-platform-serve:local, mock-service:local
kind load docker-image loadtest-platform-serve:local loadtest-platform-mock-service:local --name ltp

# 3. apply manifests
kubectl apply -k deploy/k8s/overlays/kind
kubectl -n ltp-system rollout status deploy/serve --timeout=180s
kubectl -n ltp-system rollout status deploy/dex --timeout=180s
kubectl -n ltp-system rollout status sts/postgres --timeout=180s

# 4. port-forward (one per terminal)
kubectl -n ltp-system port-forward svc/serve 8080:8080 &
kubectl -n ltp-system port-forward svc/dex 5556:5556 &
```

## 5. Get a bearer token from Dex (without opening a browser)

```bash
export LTP_DEX_ISSUER=http://localhost:5556/dex
export LTP_DEX_CLIENT_ID=ltp-test
export LTP_DEX_CLIENT_SECRET=ltp-dev-shared-secret-32-bytes-min!!
export LTP_DEX_USER=dev-user@example.com
export LTP_DEX_PASSWORD=password

TOKEN=$(python3 scripts/mint-dex-token.py)
curl -H "Host: localhost" -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/loadtest/v1/namespaces
```

`scripts/mint-dex-token.py` drives the Dex HTML login and outputs an `id_token` (JWT RS256) — the same mechanism as `tests/integration/dex.go` (ported from `sources/opsense-rs/scripts/mint-dex-token.py`).

### Note: the header must be **one** curl argv

```bash
# WRONG — the shell splits "Bearer $TOKEN" into 2 args ⇒ curl reads `-H Authorization:`
# (drops the header) and sends an HTTP/1.1 request without Host ⇒ nginx returns 400
H="-H Authorization: Bearer $TOKEN -H Host: localhost"; curl $H ...

# RIGHT — use a curl config file (a single word, no word-splitting)
printf 'header = "Authorization: Bearer %s"\nheader = "Host: localhost"\n' "$TOKEN" > /tmp/curl.conf
curl -K /tmp/curl.conf ...
```

### Tenant/user identity in bearer mode

The gateway takes its identity from the nginx side (`conf/nginx/vhost/04-api.conf`):

| Header | Source | Schema constraint |
|---|---|---|
| `X-Tenant-Id` | `auth-config` → `auth.id` | **=`tbl_tenants.id`** (`tbl_namespaces.tenant_id` is a FK) |
| `X-User-Id` | JWT `sub` | UUID or external subject — see below |

`tbl_namespaces.owner_user_id REFERENCES tbl_users(id)` is a **UUID**, while Dex's `sub` is
**always** opaque: `base64url(protobuf{userID, connectorID})` (Dex v2.41
`server/oauth2.go:genSubject`, cannot be disabled). Therefore:

- The in-cluster ConfigMap sets `staticPasswords[].userID: 11111111-1111-4111-8111-111111111111`
  to keep `tbl_users.id` fixed, and the E2E reads `sub` from the freshly minted token and then seeds
  `tbl_user_external_identities(issuer, subject)` pointing at that user.
- The controller resolves `X-User-Id` (plan 37, `pkgs/authz/identity.go`): a UUID → used as-is;
  otherwise it looks up `tbl_user_external_identities` within the tenant; not found → `401`.

Quick check:

```bash
kubectl -n ltp-system exec statefulset/postgres -- \
  psql -U ltp -d ltp -tAc "SELECT u.id, u.email, uei.subject
                             FROM tbl_users u
                             LEFT JOIN tbl_user_external_identities uei ON uei.user_id = u.id"
```

### DNS resolver for Lua (JWKS, OIDC discovery)

nginx uses cosockets (`lua-resty-http`) to call JWKS/OIDC, so it needs the `resolver` to match the
environment. `conf/nginx/http.conf` + `www.conf` use the placeholder `%%NGINX_RESOLVER%%`,
and `scripts/release.sh` replaces it from the env (default `127.0.0.11` for Docker Compose):

```yaml
# deploy/k8s/base/serve/deployment.yaml
- name: NGINX_RESOLVER
  value: kube-dns.kube-system.svc.cluster.local
```

Forgetting this step ⇒ the Lua fetch of `http://dex:5556/dex/keys` fails ⇒ every bearer request returns 401
(`Invalid Token`) even though JWKS is still reachable from outside.

> ⚠️ **Use the FQDN in the URL, not the short name.** nginx's `resolver` (cosocket)
> does **not** apply the search domain/ndots from `/etc/resolv.conf`; a query for `dex` returns
> `SERVFAIL` ⇒ `Invalid Token … failed fetching jwks: dex could not be resolved
> (2: Server failure)`. Inside the cluster use
> `http://dex.ltp-system.svc.cluster.local:5556/dex` (issuer + jwks_uri +
> `oidc_jwks_url` in the auth profile). With Docker Compose the short name `dex` still works because
> the embedded DNS resolves it directly.

## 6. CLI `sops` subcommand (encrypt/decrypt sensitive values)

```bash
export LTP_MASTER_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef

# Encrypt plaintext → hex blob (paste straight into the SQL seed)
./bin/loadtest-controller sops encrypt "client-secret-abc"
# → \x3f9a…

# Decrypt it back to verify the seed (accepts \x / 0x / bare hex)
./bin/loadtest-controller sops decrypt '\x3f9a…'
# → client-secret-abc
```

Dispatch happens **before** `config.FromEnv()`, so no `DATABASE_DSN`/DB is needed when running `sops`. The master key is read from `LTP_MASTER_KEY` (SOPS env — if unset, `Encrypt`/`Decrypt` return a clear error instead of panicking).

## 7. Teardown

```bash
kubectl delete -k deploy/k8s/overlays/kind
kind delete cluster --name ltp
```

## 8. CI

The `Full Deploy E2E (in-cluster)` job in `.github/workflows/ci.yml`:
1. `checkout` + `setup-go`
2. install `kind` v0.24.0 + `kubectl` v1.31.0
3. create the kind cluster (`ltp-full-deploy`, retry x3)
4. `earthly +integration-images` → `kind load docker-image` (serve + mock-service)
5. `DEPLOY_MODE=in-cluster ./tests/e2e-full-deploy.sh`
6. dump cluster state (pods/events/CNPG/TestRun/controller logs) on failure

Estimate: 15–20 minutes (earthly cold build 6–10m + bootstrap + lifecycle). Timeout 45m.

## Debugging tips

- View controller logs: `kubectl -n ltp-system logs deploy/serve -c serve` (supervisord collects stdout)
- View TestRun: `kubectl -n lt-env-<ns> get testrun -o wide`
- View the CNPG cluster: `kubectl -n lt-env-<ns> get cluster`
- View NetworkPolicy events: `kubectl -n lt-env-<ns> get networkpolicy`
- Dump everything on failure: `kubectl get ns,deploy,svc,pods,testruns,clusters -A` + `kubectl events`
