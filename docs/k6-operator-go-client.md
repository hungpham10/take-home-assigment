# Shipping k6 load tests via k6-operator using Go (no `kubectl`)

This note documents how to interact with **k6-operator** from Go code to submit/load-test runs, without shelling out to `kubectl`. It is based on `sources/k6-operator` (reference, read-only).

## How k6-operator works
k6-operator is a Kubernetes operator that runs distributed [k6](https://grafana.com/docs/k6/latest/) tests.

- **CRDs**: `TestRun` and `PrivateLoadZone`.
- **API group/version**: `k6.io/v1alpha1` (`api/v1alpha1/groupversion_info.go`: `Group=k6.io`, `Version=v1alpha1`, registers `TestRun`/`TestRunList`).
- **Controller**: `TestRunReconciler` (`internal/controller/testrun_controller.go`) reconciles `TestRun` resources and creates the k6 runner/initializer/starter Pods.

“Ship a load test to k6” (via k6-operator) means **create a `TestRun` custom resource** (and usually provide the script source) in the cluster where k6-operator and its CRDs are installed.

## `TestRun` essentials
From `api/v1alpha1/testrun_types.go` (`TestRunSpec`):

```go
Script      K6Script  // where the k6 script is located
Parallelism int32
Arguments   string    // space-separated argv string (legacy/simple)
Args        []string  // exact argv elements; non-empty overrides Arguments
// + runner/starter/initializer Pod config, ports, quiet/paused, cleanup, etc.
```

### Script sources (`spec.script`)
`TestRun.spec.script` accepts **one** of: `configMap`, `volumeClaim`, `localFile` (see `api/v1alpha1/testrun_types.go`). **k6-operator has no native S3/HTTP script source** — remote scripts (e.g., S3) must be resolved/validated/staged by the platform.

| Field | Meaning | Good for |
|---|---|---|
| `configMap: { name, file }` | Script stored as a key in a ConfigMap (same namespace). | **Dynamic scripts shipped from Go** (create run-scoped ConfigMap, then TestRun). **ConfigMap data ~1 MiB limit**. |
| `volumeClaim: { name, file, readOnly }` | Script on a PVC, mounted at `/test`. `file` can include a path. | Pre-staged scripts/artifacts; **large S3/object-ref scripts after controlled staging**. |
| `localFile` | Script path already present in the runner image. | Vetted baked images (not arbitrary user remote code). |

**Remote scripts (S3/approved object ref):** resolve approved refs only (checksum, scoped credentials, no arbitrary URL execution). **Validate before execution** with an isolated **Kubernetes Job** (least privilege, time/resource/network limits; e.g., `k6 inspect`/archive/guardrail checks), reconcile status via **K8s watcher/informer** (level-triggered) into projection/notification state (`PENDING_VALIDATION/VALIDATING/VALIDATED/INVALID/VALIDATION_FAILED`). Stage validated script to `configMap` (small) or `volumeClaim` (large), then create `TestRun`. See `docs/loadtest-manager-api.md` §5.

Sample (`config/samples/k6_v1alpha1_testrun.yaml`):
```yaml
apiVersion: k6.io/v1alpha1
kind: TestRun
metadata:
  name: testrun-sample
spec:
  parallelism: 4
  script:
    configMap:
      name: k6-test
      file: test.js
```

### Status / lifecycle
`status.stage` (`Stage` enum): `initialization`, `initialized`, `created`, `started`, `stopped`, `finished`, `error`. Also check `status.conditions`, and `status.testRunId` when applicable.

## Go approach: call the Kubernetes API directly (no `kubectl`)
Prerequisites:
- k6-operator + CRDs installed in the target cluster.
- Go process can reach the Kubernetes API (`clientcmd`/kubeconfig out-of-cluster, or `rest.InClusterConfig()` in-cluster).
- RBAC allows the identity to manage `configmaps` (if used) and `testruns.k6.io` (create/get/list/watch/patch/delete as needed) in the target namespace.

### Option 1 (recommended, minimal coupling): dynamic client + `unstructured`
No dependency on k6-operator Go types; you only need the GVR.

```go
package loadtest

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var TestRunGVR = schema.GroupVersionResource{
	Group: "k6.io", Version: "v1alpha1", Resource: "testruns",
}

func ShipTestRun(ctx context.Context, cfg *rest.Config, ns, name, script string, parallelism int64) error {
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}

	_, err = core.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "run-" + runID + "-script", Namespace: ns, Labels: labels}, // run-scoped unique DNS-1035 name ("run-" prefix); consider OwnerReference for cleanup on failure
		Data:       map[string]string{"test.js": script}, // ConfigMap data value limit ~1 MiB; enforce script size guardrails
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	tr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "k6.io/v1alpha1",
		"kind":       "TestRun",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"parallelism": parallelism,
			"script": map[string]any{
				"configMap": map[string]any{"name": "run-" + runID + "-script", "file": "test.js"},
			},
			// "args": []any{"--tag", "env=staging"}, // takes precedence over "arguments"
		},
	}}
	_, err = dyn.Resource(TestRunGVR).Namespace(ns).Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		// TODO: cleanup script ConfigMap (or use owner references / deterministic reconciliation)
		return err
	}
	return nil
}
```

Notes:
- Use `int64` for numeric fields in `unstructured`.
- To observe progress, `Get`/`Watch` the same GVR and read `status.stage` / `status.conditions`.
- Handle “already exists” (idempotency) based on your session semantics (e.g., unique name per run, or `Create` vs `Apply`/patch).

### Option 2 (typed, type-safe): k6-operator API types + `controller-runtime` client
Use when you accept depending on the operator module and want compile-time types.

```go
import (
	k6v1alpha1 "github.com/grafana/k6-operator/api/v1alpha1" // pin to a released version; do NOT modify sources/
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

scheme := runtime.NewScheme()
_ = clientgoscheme.AddToScheme(scheme)
_ = k6v1alpha1.AddToScheme(scheme) // registers k6.io/v1alpha1 TestRun/PrivateLoadZone

cl, err := crclient.New(cfg, crclient.Options{Scheme: scheme})
// ...
tr := &k6v1alpha1.TestRun{
	ObjectMeta: metav1.ObjectMeta{Name: "testrun-sample", Namespace: ns},
	Spec: k6v1alpha1.TestRunSpec{
		Parallelism: 2,
		Script: k6v1alpha1.K6Script{
			ConfigMap: k6v1alpha1.K6Configmap{Name: "k6-test", File: "test.js"},
		},
		// Args: []string{"--tag", "env=staging"},
	},
}
err = cl.Create(ctx, tr)
```

Caveat: `sources/k6-operator` in this repo is **reference-only** (read-only). If this project consumes it as a library, depend on a **released/pinned module version** in the project’s own `go.mod` (workspace currently has no root `go.mod`) rather than editing `sources/`.

## Minimal RBAC sketch
Grant the caller’s ServiceAccount (namespace-scoped) permissions like:
- `core/v1` `configmaps`: `get, list, watch, create, update, patch, delete` (if shipping scripts via ConfigMap)
- `k6.io/v1alpha1` `testruns`: `get, list, watch, create, update, patch, delete`

Adjust to least privilege and your lifecycle (e.g., read-only watch if another component creates runs).

## Implications for this project (MVP / multiple sessions)
- A “test session” maps naturally to **one or more `TestRun` objects** with unique names/labels (and optionally per-session ConfigMaps). Isolation is provided by Kubernetes namespacing/naming + operator-managed Pods.
- For a GA service with many sessions, prefer **unique resource names per run** (e.g., include session/run ID), set labels for lookup, watch `status.stage`, and implement cleanup (`delete` TestRun/ConfigMap) per retention policy.
- MVP direction (per `AGENTS.MD`): expose a **Go + go-chi** API (with Swagger/OpenAPI) that accepts a load-test request and creates the ConfigMap/`TestRun` via the Kubernetes API as above; follow `plans/` → `docs/` → TDD implementation workflow.


## Operational notes
- **Pin k6-operator version/API:** examples use `k6.io/v1alpha1` `TestRun`; verify against the deployed operator version and update manifests/types accordingly.
- **ConfigMap size:** Kubernetes ConfigMap values are limited (~1 MiB). Large scripts should use approved object storage/ref or volume-backed mechanisms with guardrails.
- **Orphans:** create script source and `TestRun` with deterministic names/labels and owner references or compensating cleanup; reconciler must handle partial failures.
- **Runner isolation:** run TestRun pods with least privilege (no K8s/platform credentials), scoped namespace, and NetworkPolicy allowing only resolved load-test targets + required allowances.
