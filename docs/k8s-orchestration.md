# K8s Orchestration — từ desired state (Postgres) đến tài nguyên thật

Tài liệu này định nghĩa **cách** platform biến desired state của một Environment (namespace) trong PostgreSQL thành tài nguyên Kubernetes thật: render, apply, observe readiness, prune, và teardown. Nó bổ sung (không thay thế) các contract:

- [`namespace-management-api.md`](namespace-management-api.md) — desired spec, lifecycle status machine, readiness payload.
- [`architecture.md`](architecture.md) §3.3 (Env Reconciler sequence), §4 (K8s orchestrator/adapter).
- [`k6-operator-go-client.md`](k6-operator-go-client.md) — chạy load test qua `TestRun` CR (cùng adapter, cùng pattern dynamic client).
- [`harness-testing.md`](harness-testing.md) §2.3/§4 — yêu cầu fake K8s client + fault injection cho `RUN-*`/`STATE-*`.

**Phạm vi MVP:** controller render manifest trong Go + Server-Side Apply; **không dùng Helm** cho SUT (ADR-004, §2). Ingress dùng `networking.k8s.io/v1` (nginx class). Phần implement (`pkgs/k8s` adapter + Env Reconciler) là plan 20; plan này chỉ khoá thiết kế.

## 1. Scope & model

Mapping nền tảng (locked trong `namespace-management-api.md`): **1 Environment ⇄ 1 K8s Namespace**, tên deterministic:

```text
lt-env-<namespace_id>
```

Mọi resource controller tạo trong namespace đó (và chính Namespace object) mang label ownership:

| Label | Giá trị | Vai trò |
|---|---|---|
| `loadtest.io/managed-by` | `loadtest-controller` | Đánh dấu resource do controller sở hữu (SSA field manager trùng tên) |
| `loadtest.io/tenant-id` | tenant slug/id | Isolation, audit, reporting |
| `loadtest.io/namespace-id` | `ns-...` | Enqueue reconcile theo Environment; prune scope |
| `loadtest.io/component-id` | `svc-...`, `db-...`, `ing-...` | Map resource ↔ component row trong Postgres |
| `loadtest.io/component-kind` | `service`, `database`, `ingress`, `mock`, `network-policy` | Prune + readiness aggregation |

Quy tắc ownership/prune: mọi resource có đủ bộ label trên là **managed**; khi một component bị xoá khỏi desired state (hoặc Environment bị xoá), controller delete theo label selector thay vì theo tên riêng lẻ. Xoá Environment = xoá K8s Namespace (cascade 1:1, không cần finalizer per-resource cho teardown toàn bộ).

## 2. ADR-004 — Render manifest trong Go + SSA, không Helm cho SUT

**Quyết định:** controller render thẳng `Deployment` + `Service` (+ `ServiceAccount`) từ spec `deploy {image, replicas, port, env, health}` bằng typed structs / `unstructured`, apply bằng **Server-Side Apply** với `fieldManager=loadtest-controller`.

**Lý do:**
- Spec `deploy` đã có cấu trúc chặt (image/replicas/port/env/health); không cần template engine.
- SSA cho drift detection tự nhiên (managed fields), apply idempotent — khớp mô hình level-triggered/at-least-once trong `harness-testing.md` §3.
- Prune theo label selector, không phụ thuộc release state.
- Test được với fake client + fault injection (harness §4: injected errors, "already exists", stale reads); Helm Go SDK kéo release state (Secret trong namespace), khó diff và khó test.
- Đơn giản hoá kiến trúc khi vượt nhu cầu MVP.

**Helm (future, §9):** chỉ khi người dùng **mang chart của họ** (`deploy.chart_ref`); khi đó thêm `deploy.mode: manifest|helm`, mặc định `manifest`. Không nằm trong MVP.

## 3. Mapping component → K8s resource

```text
Environment ns-1a2b → K8s Namespace lt-env-ns-1a2b (+ ResourceQuota/LimitRange nếu cấu hình quota)
│
├─ service svc-order (deploy)
│    → Deployment + ClusterIP Service + ServiceAccount riêng (no platform creds)
│    (+ ConfigMap env khi env size vượt giới hạn container env)
│    ready = Deployment Available (condition) AND observedGeneration == metadata.generation
│
├─ database db-orders (operator = cloudnative-pg)
│    → CNPG Cluster CR (postgresql.cnpg.io/v1, tên `db-<database_id>`)
│    → Secret do operator sinh (`<cluster>-app`): controller CHỈ đọc TÊN secret,
│       không đọc value; inject secretKeyRef vào Deployment SUT theo database_bindings
│    → data_source: K8s Job (pg_restore) chạy SAU khi Cluster condition Ready,
│       activeDeadlineSeconds từ restore_policy.timeout
│    ready = Cluster condition Ready AND restore Job Complete (nếu có) AND verify pass
│
├─ ingress ing-order
│    → networking.k8s.io/v1 Ingress, ingressClassName từ spec (nginx),
│      host từ host_template, rules → service_id+port (validate đã ở API layer)
│
├─ mock (service.mock / database.mock)
│    → mock-service runtime là deployment dùng chung NGOÀI env namespace (data-plane riêng)
│    → trong env namespace: Service ExternalName (hoặc selector Service) cho giao thức
│      nội bộ, HOẶC route qua Ingress cho giao thức cần host/path — xem §8
│    → binding snapshot: ConfigMap `mock-binding-<component_id>` (generation/observed_generation)
│    → NetworkPolicy cho phép egress SUT → mock-service
│    ready = observed_generation == generation của snapshot
│
└─ NetworkPolicy
     → 1 default-deny (ingress+egress) + 1 policy per service render từ dependencies[]
       + platform allowances (DNS, mock-service, ingress-nginx → SUT)
     → apply TRƯỚC workload (tránh cửa sổ không bị chặn)
```

Chi tiết từng loại:

### 3.1 Service
- Render từ `deploy`: image (không cho `:latest` trong prod profile), `replicas`, containerPort = `port`, `env` (map trực tiếp; value từ secret dùng `secretKeyRef`, không bao giờ render plaintext vào manifest), probes từ `health` (liveness/readiness/startup nếu có).
- `Service` ClusterIP trỏ `service_id` port; DNS nội bộ `<service_id>.<ns>.svc.cluster.local`.
- `ServiceAccount` riêng per service, không mount platform credentials, `automountServiceAccountToken: false`.

### 3.2 Database (cloudnative-pg)
- CR `Cluster` với spec render từ `database.config` (instances, storage, version, bootstrap). Controller không tự chạy Postgres — chỉ tạo CR và đọc status của operator.
- Secret credentials do operator tạo: controller **chỉ ghi nhận tên** (pattern `<cluster>-app`) và inject `secretKeyRef` vào các Deployment có `database_bindings[].database_id` trỏ đến. Value không bao giờ đi qua controller DB/API.
- Restore: khi `data_source.dataset_id` configured → sau Cluster Ready, tạo Job restore (một container chạy `pg_restore`/copy từ dataset location, mount creds qua secret), `activeDeadlineSeconds = restore_policy.timeout` (khớp gate DATA_LOADING trong `namespace-management-api.md` §3). Job fail/timeout → `DEGRADED`/`INVALID` với `blocking_reasons`.

### 3.3 Ingress
- `networking.k8s.io/v1 Ingress`, `ingressClassName` lấy từ spec (mặc định nginx). `host_template` render deterministic (vd `{{service_id}}.<env-slug>.lt.internal`); TLS block từ `tls` flag.
- DNS trong cluster loadtest: wildcard DNS trỏ về ingress-nginx của cluster là cấu hình hạ tầng (ngoài phạm vi controller); controller chỉ tạo Ingress object.

### 3.4 Mock
Xem §8. Điểm chính: mock **không có public ingress**; SUT gọi mock qua Service nội bộ (ExternalName/selector) hoặc qua Ingress nội bộ theo protocol; snapshot binding được publish dưới dạng ConfigMap có `generation`, mock-service báo `observed_generation` (khớp `architecture.md` §4 và harness `MOCK-VER-01`).

### 3.5 NetworkPolicy
- Render từ dependency graph đã resolve ở API layer (`GET .../network-policies`): mỗi service được phép egress tới `dependencies[]` và `database_bindings[]`; mọi traffic khác bị deny.
- Bộ: 1 `default-deny-all` cho namespace + 1 policy per service (labels theo §1) + platform allowances (kube-dns, mock-service, ingress-nginx backend).
- **Runner policy:** cho load-test run, render thêm 2 policy per run (egress runner → target + ingress target ← runner) chọn bởi label `loadtest.io/run-id`; spec đầy đủ tại [`docs/network-policy.md`](network-policy.md) §3. K8s runner (`RUNNER_MODE=k8s`) tạo `TestRun` (k6.io/v1alpha1) qua Go Kubernetes API (`docs/k6-operator-go-client.md`), không dùng `kubectl`.
- **Chi tiết DB / k6-operator / nguyên hạn của NetworkPolicy:** [`docs/network-policy.md`](network-policy.md) §4–§6.
- **Thứ tự:** NetworkPolicy apply trước workload (bước 2 trong §4) để không có cửa sổ traffic không bị chặn.

## 4. Env Reconciler design

Level-triggered, at-least-once, idempotent (khớp `harness-testing.md` §3 và `STATE-MEM-01`).

### 4.1 Thứ tự apply trong một lần reconcile

1. **Namespace** `lt-env-<namespace_id>` + labels + ResourceQuota/LimitRange (nếu cấu hình).
2. **NetworkPolicy** (default-deny + per-service + platform allowances).
3. **Database CRs** (CNPG Cluster) → chờ condition Ready.
4. **Restore Job** (nếu `data_source`) → tạo Job; transition `DATA_LOADING` khi Job `Complete` do **Status Watcher** ghi.
5. **Mock bindings/Services** (ConfigMap snapshot + ExternalName/Ingress route — §8).
6. **Service workloads** (Deployment cần secret DB đã tồn tại từ bước 3).
7. **Ingress**.
8. **Dừng lại — không ghi readiness.** Reconciler chỉ ghi K8s. Việc thu thập trạng thái, aggregate và ghi `READY`/`DEGRADED`/`INVALID` + `blocking_reasons` vào PG là việc của **Status Watcher** (xem `namespace-status-watcher.md`).

Mỗi bước là idempotent: apply lại cùng spec → no-op (SSA); resource "already exists" với cùng owner là success. Bước nào chưa đủ điều kiện (vd Cluster chưa Ready) → reconcile kết thúc, chờ event tiếp theo (watch) hoặc requeue có backoff.

**Observed → PG is a separate loop:** the Env Reconciler never writes readiness into Postgres. Observing K8s state and computing `tbl_component_status` / namespace transitions is the Status Watcher's job: [`docs/namespace-status-watcher.md`](namespace-status-watcher.md). The two docs share the label conventions (§1) and `spec_hash` annotations.

### 4.2 Cơ chế

- **Phát hiện thay đổi:** `spec_hash` (hash JSONB desired spec của Environment) lưu trong annotation trên Namespace / trong state row; không khớp → apply lại từ bước 1. Per-component dùng `component-id` label + hash annotation riêng để chỉ apply lại component đổi.
- **Watch:** informer trên Deployment, Service, Ingress, NetworkPolicy, CNPG `Cluster`, Job, k6 `TestRun`; mỗi event resolve `namespace-id` từ label → enqueue key `namespace_id` vào workqueue (dedupe). Kèm requeue định kỳ (resync) làm safety net.
- **Leader election:** một instance controller reconcile tại một thời điểm (khớp bảng deployment units trong `architecture.md` §4); các instance khác standby.
- **Prune:** sau apply, list managed resources theo label selector trong namespace; resource có `component-id` không còn trong desired → delete. Không prune resource không có label managed.
- **Crash safety:** controller restart đọc desired state từ Postgres (không in-process state), phát hiện drift bằng `spec_hash`/observed state — khớp `RUN-CRASH-01`, `STATE-MEM-01`.
- **Single-step mode:** hook chạy 1 vòng reconcile cho harness test deterministic (khớp harness §4).

## 5. Adapter `pkgs/k8s`

Interface hẹp, là ranh giới duy nhất controller chạm K8s API — để fake client + fault injection (harness §4: injected errors, delays, "already exists", stale reads):

```go
type Client interface {
    Apply(ctx context.Context, obj *unstructured.Unstructured) error // SSA, fieldManager loadtest-controller
    Get(ctx context.Context, gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error)
    Delete(ctx context.Context, gvk schema.GroupVersionKind, ns, name string) error
    List(ctx context.Context, gvk schema.GroupVersionKind, ns string, labels labels.Set) ([]*unstructured.Unstructured, error)
    Watch(ctx context.Context, gvk schema.GroupVersionKind, ns string) (watch.Interface, error)
}
```

- Implement thật: client-go **dynamic client + unstructured** — nhất quán với Option 1 đã khuyến nghị trong `k6-operator-go-client.md` cho `TestRun`.
- CNPG `Cluster` xử lý qua cùng dynamic client (GVR `postgresql.cnpg.io/v1`, `clusters`) — không import Go types của operator, tránh dependency nặng.
- Fake client trong `pkgs/testutil` (hoặc `pkgs/k8s/fake`) mô phỏng: lỗi transient (để test retry/backoff `RUN-NET-01`), `AlreadyExists` (`RUN-IDEM-01`), stale reads, watch event feed.

## 6. RBAC (ServiceAccount `loadtest-controller`)

| Resource (group) | Verbs | Ghi chú |
|---|---|---|
| `namespaces` (core) | get, list, watch, create, delete | 1:1 với Environment |
| `deployments`, `services`, `serviceaccounts`, `configmaps` (apps/core) | get, list, watch, create, update, patch, delete | workload + env config |
| `networkpolicies` (networking.k8s.io) | get, list, watch, create, update, patch, delete | default-deny + per-service |
| `ingresses` (networking.k8s.io) | get, list, watch, create, update, patch, delete | nginx class |
| `jobs` (batch) | get, list, watch, create, delete | restore/verify Jobs |
| `clusters` (postgresql.cnpg.io) | get, list, watch, create, update, patch, delete | CR của operator |
| `testruns` (k6.io) | get, list, watch, create, delete | theo `k6-operator-go-client.md` |
| `secrets` (core) | **get** (tên secret DB do operator sinh — không list toàn namespace nếu có thể), không create/update từ API payload | value không qua controller DB/API |
| `events`, `pods`, `pod/logs` | get, list, watch | chẩn đoán blocking_reasons |

Runner pods / SUT ServiceAccount: **không** có quyền nào trên control-plane resources (least privilege, khớp sketch RBAC trong `k6-operator-go-client.md`).

## 7. Readiness per component → payload API

Map điều kiện K8s vào readiness payload trong `namespace-management-api.md` §3 (`components{...}` + `ready_for_loadtest` + `blocking_reasons[]`). Bảng dưới là điều kiện thô; **ai tính / ghi vào đâu** (evaluators, generation fencing, hysteresis, aggregation) được định nghĩa trong [`docs/namespace-status-watcher.md`](namespace-status-watcher.md) §4–§5:

| Component | Điều kiện ready | blocking_reasons ví dụ |
|---|---|---|
| services | Deployment `Available=True` && `observedGeneration == generation` && Service endpoints sẵn sàng | `image_pull_backoff`, `crash_loop`, `replicas_unavailable` |
| databases | CNPG `Cluster` condition `Ready=True` | `cluster_not_ready`, `operator_missing` |
| data_load | Restore Job `Complete=True` && verify pass; timeout → fail | `restore_job_failed`, `restore_timeout` |
| ingress | Ingress accepted (condition nếu controller báo) && service rule exists | `ingress_class_missing`, `backend_unreachable` |
| dependencies | resolved graph valid (đã validate ở API) | `unknown_dependency` |
| network_policy | tất cả policy applied (SSA ok) | `policy_apply_failed` |
| mocks | snapshot `observed_generation == generation` | `mock_not_reconciled` |

`READY` chỉ khi mọi group cần thiết ready (khớp state machine `APPLYING/DATA_LOADING → READY`; fail → `INVALID`/`DEGRADED`).

## 8. Mock wiring chi tiết

Mock runtime (`cmds/mock-service`, phase sau theo `mock-service-api.md`) là **deployment dùng chung ngoài env namespace**. Trong env namespace chỉ có wiring:

- **Đường nội bộ (HTTP/gRPC thường):** Service `ExternalName` tên deterministic (vd `mock-<component_id>`) trỏ DNS mock-service; SUT gọi qua tên này. Với protocol cần selector/endpoint riêng (vd gRPC streaming), dùng Service selector tới mock-service endpoint thay ExternalName.
- **Đường host/path (mock cần được gọi như host riêng — vd mock trả response phụ thuộc Host header):** route qua **Ingress nội bộ** với rule host riêng → service mock; SUT gọi qua host đó. Đây là lý do chọn "ExternalName hoặc Ingress tuỳ protocol".
- **Inject vào SUT:** controller set env cho Deployment SUT (vd `MOCK_<COMPONENT>_URL`) trỏ tới DNS đã chọn — SUT không cần hardcode. Trong cả hai đường, giá trị inject là DNS/host ổn định trong cluster.
- **Snapshot binding:** ConfigMap `mock-binding-<component_id>` chứa snapshot metadata (`schema_version`, `generation`, spec mock); mock-service đọc và report `observed_generation` — reconcile mock xong khi hai giá trị khớp (`MOCK-VER-01`).
- **NetworkPolicy:** egress SUT → mock-service được allow trong policy per service (bước 2 §4.1); mock không bao giờ có public ingress.

## 9. Open items / future

- `deploy.mode: helm` cho chart do người dùng cung cấp (ADR-004 extension).
- Gateway API (HTTPRoute) thay thế/ song song Ingress.
- Drift report UI (so observed managed fields vs desired).
- TTL tự động xoá Environment/Namespace (hiện reaper chạy cho run; env TTL đang open item trong `architecture.md` §3.3).
- ResourceQuota/LimitRange profile per tenant.

## 10. High Availability (HA) — Leader Election & Sharding

**Current MVP limitation:** The `loadtest-controller` runs as a **single replica**. Both the Env Reconciler (Loop 1) and Status Watcher (Loop 2) are simple goroutine loops with **no coordination** between instances.

### Problems with Multiple Replicas (No HA)

| Component | Issue |
|-----------|-------|
| **Env Reconciler** | - Concurrent SSA applies with same field manager (`loadtest-controller`) → field ownership conflicts<br>- Prune thrash: Pod A applies, Pod B prunes (different desired set snapshot)<br>- Double work: All pods apply same manifests<br>- `DeleteEnv` races: Multiple pods delete same K8s namespace |
| **Status Watcher** | - Concurrent `tbl_component_status` upserts → lost updates<br>- Concurrent `fn_namespace_transition` calls → transition conflicts<br>- Multiple watchers watching same K8s resources → duplicate events |

### Required for HA (Post-MVP)

#### Option A: Per-Namespace Leader Election (Recommended)

Use K8s `coordination.k8s.io/v1` **Lease** per namespace:

```go
// One Lease per namespace: loadtest-reconciler-<namespace_id>
// One Lease per namespace: loadtest-watcher-<namespace_id>

lock := &resourcelock.LeaseLock{
    LeaseMeta: metav1.ObjectMeta{
        Name:      "loadtest-reconciler-" + namespaceID,
        Namespace: "loadtest-system",
    },
    Client: client.CoreV1(),
    LockConfig: resourcelock.ResourceLockConfig{Identity: podName},
}

election, _ := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
    Lock:            lock,
    LeaseDuration:   15 * time.Second,
    RenewDeadline:   10 * time.Second,
    RetryPeriod:     2 * time.Second,
    Callbacks: leaderelection.LeaderCallbacks{
        OnStartedLeading: func(ctx context.Context) { runReconcileLoop(ctx) },
        OnStoppedLeading: func() { /* resign */ },
    },
})
election.Run(ctx)
```

**Properties:**
- Only leader reconciles that namespace
- Automatic failover on pod death (lease expires)
- No sharding complexity

#### Option B: Consistent-Hash Sharding

Each pod handles a subset of namespaces:

```go
func shouldReconcile(namespaceID string, replicaCount, replicaIndex int) bool {
    h := fnv.New32a()
    h.Write([]byte(namespaceID))
    return int(h.Sum32())%replicaCount == replicaIndex
}
```

**Properties:**
- All pods active (no standby)
- Need stable `replicaCount` (Deployment spec) + `replicaIndex` (pod ordinal via `statefulset` or downward API)
- On pod churn, some namespaces briefly un-reconciled until rebalance

### Shared SSA Field Manager

If multiple pods apply, **field manager must be unique per pod** OR use leader election:

```go
// Option 1: Unique field manager per pod (with leader election)
fieldManager := "loadtest-controller-" + podName

// Option 2: Same field manager ONLY if leader election ensures single writer
// Current: const FieldManager = "loadtest-controller" (single writer assumed)
```

### Watcher HA

Same pattern — per-namespace Lease for watcher:
- Only one watcher writes `tbl_component_status` + calls `fn_namespace_transition` per namespace
- Watch events still delivered to all pods, but only leader processes

### Implementation Locations

| Component | File | Change Needed |
|-----------|------|---------------|
| Env Reconciler | `pkgs/envreconciler/reconciler.go` | Add `LeaderElection` config to `Start()` |
| Status Watcher | `pkgs/statuswatcher/watcher.go` | Add `LeaderElection` config to `Start()` |
| Orchestration wiring | `cmds/loadtest-controller/orchestration.go` | Initialize election locks per namespace |
| Deployment | `conf/k8s/deployment.yaml` | Single replica for MVP; HA = add Lease RBAC + config |

**Platform networking env vars** (read by `pkgs/config.FromEnv`, injected into every `envreconciler.Env`): `K6_OPERATOR_NAMESPACE` (default `k6-operator-system`), `K8S_NODE_CIDRS` (default `172.18.0.0/16` — production must set), `MOCK_SERVICE_NAMESPACE` (default `mock-system`), `MOCK_SERVICE_PORTS` (default `80,8090`). See [`docs/network-policy.md` §8](network-policy.md) for the rendered NetworkPolicy effect.

### RBAC for Leases

```yaml
# Additional rules for loadtest-controller ServiceAccount
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  resourceNames: ["loadtest-reconciler-*", "loadtest-watcher-*"]
```

### MVP Decision

| Environment | Reconciler Replicas | Watcher Replicas | Mechanism |
|-------------|---------------------|------------------|-----------|
| **MVP (current)** | 1 | 1 | Single instance, no HA |
| **Post-MVP HA** | N (with leader election) | N (with leader election) | Per-namespace Lease |

This is documented as **Open Item #10** in `architecture.md` and tracked for post-MVP implementation.

## 11. Future Refactor: Composite Component Pattern for ApplyTree

**Current implementation** (`pkgs/envreconciler/tree.go`) uses a custom `Applier` interface with three concrete types (`objectApplier`, `groupApplier`, `namespaceApplier`). The `BuildApplyTree()` function is a monolithic builder that hardcodes the apply order and component types.

**Proposed refactor** — **Composite Pattern** with a `Component` interface:

```go
type Component interface {
    Kind() string
    Name() string
    Render(env *Env) []*unstructured.Unstructured
    Apply(ctx context.Context, client k8s.Client, env *Env) error
    Children() []Component
    SpecHash() string
    DesiredGeneration() int64
    Labels() map[string]string
}
```

### Component Hierarchy

```
EnvironmentComponent (root composite)
├── NetworkPolicyComponent (leaf)
├── DatabaseComponent (leaf, polymorphic: CNPG/ECK)
├── ServiceComponent (composite) ← "svc-order"
│   ├── ConfigMapComponent (leaf) ← "svc-order-config"
│   ├── SecretComponent (leaf) ← "svc-order-secret"
│   ├── ServiceAccountComponent (leaf) ← "svc-order"
│   ├── DeploymentComponent (leaf) ← "svc-order"
│   ├── K8sServiceComponent (leaf) ← "svc-order"
│   └── HPAComponent (leaf, optional) ← "svc-order"
├── IngressComponent (leaf)
└── MockBindingComponent (leaf)
```

### Benefits

| Aspect | Current | Composite Pattern |
|--------|---------|-------------------|
| **New component type** | Modify `BuildApplyTree()` + `tree.go` | Add factory to registry |
| **Service sub-resources** | Hardcoded sequence in `BuildApplyTree` | Natural children of `ServiceComponent` |
| **Mock bindings** | Separate `BindingsFunc` hook | Just another component in tree |
| **Render vs Apply** | Coupled in builder | Separated: `Render()` pure, `Apply()` default |
| **Testability** | Need full tree + fake client | Test `Render()` in isolation |
| **Extensibility** | Low | High (plugin-style registry) |

### Migration Plan

1. Add `component.go` with `Component` interface + `LeafComponent`/`CompositeComponent` base types
2. Extract each component's render logic from `render.go` into dedicated files:
   - `component_networkpolicy.go`
   - `component_database.go` (CNPG/ECK polymorphic)
   - `component_service.go` (composite with sub-components)
   - `component_ingress.go`
   - `component_mockbinding.go`
   - `component_environment.go` (root)
3. Create `registry.go` for component factories
4. Replace `BuildApplyTree()` with `BuildComponentTree(env *Env) Component`
5. Replace `ApplyTree()` with simple DFS `applyDFS()`
6. Delete `Applier` implementations and `tree.go`

### Example: Adding ServiceMeshComponent

```go
// New file: component_servicemesh.go
func NewServiceMeshComponent(env *Env, spec ServiceMeshSpec) Component {
    return &LeafComponent{
        kind: "ServiceMeshPolicy",
        name: spec.Name,
        renderFn: func(env *Env) []*unstructured.Unstructured {
            return []*unstructured.Unstructured{renderServiceMesh(env, spec)}
        },
        specHash: spec.SpecHash,
        desiredGeneration: env.Generation,
        labels: withComponent(baseLabels(env), spec.Name),
    }
}

// Register in init()
func init() { registry["servicemesh"] = NewServiceMeshComponent }
```

**Zero changes to apply logic** — just works.

### Tracking

This is a post-MVP refactor tracked as **Open Item** in `architecture.md`.
