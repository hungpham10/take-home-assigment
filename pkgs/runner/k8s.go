package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

var (
	// TestRunGVR is the GroupVersionResource for k6-operator TestRun CRs
	// (k6.io/v1alpha1). See docs/k6-operator-go-client.md.
	TestRunGVR = schema.GroupVersionResource{
		Group:    "k6.io",
		Version:  "v1alpha1",
		Resource: "testruns",
	}

	// testRunKind is the GroupVersionKind for TestRun objects rendered as
	// unstructured (the dynamic client works on GVKs).
	testRunKind = schema.GroupVersionKind{
		Group:   "k6.io",
		Version: "v1alpha1",
		Kind:    "TestRun",
	}

	// podGVK is used by Status() to fall back to runner-pod readiness when
	// k6-operator does not advance TestRun.status.stage past "created" (seen
	// in CI: stage stays "created" while the runner pod is already Running).
	podGVK = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
)

// toUnstructured wraps a rendered manifest map with its GVK. The JSON
// round-trip normalizes Go-native maps (e.g. map[string]string labels) into
// the JSON-compatible shapes apimachinery's DeepCopyJSONValue requires —
// otherwise the Fake client panics on DeepCopy.
func toUnstructured(obj map[string]any, group, version, kind string) *unstructured.Unstructured {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(fmt.Sprintf("k8s runner: render marshal: %v", err))
	}
	var normalized map[string]any
	if err := json.Unmarshal(b, &normalized); err != nil {
		panic(fmt.Sprintf("k8s runner: render unmarshal: %v", err))
	}
	u := &unstructured.Unstructured{Object: normalized}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	return u
}

// K8sTestRunRunner executes load tests by creating k6-operator TestRun custom
// resources in the environment namespace (RUNNER_MODE=k8s). Script content is
// staged into a run-scoped ConfigMap (<run-id>-script) referenced by
// spec.script.configMap — k6-operator has no native S3/HTTP script source
// (docs/k6-operator-go-client.md §2.6).
//
// Each run targets a different environment namespace
// (`lt-env-<namespace_id>`, carried in Spec.Namespace), so the runner resolves
// the namespace per call and remembers ref → namespace so Status/Stop can find
// the object later. Runner pods run inside that namespace and are therefore
// subject to its NetworkPolicy: default-deny plus the run-scoped policies
// (docs/network-policy.md §3). The runner ServiceAccount carries no
// platform/K8s credentials (least privilege).
type K8sTestRunRunner struct {
	Client k8s.Client
	mu     sync.Mutex
	// nsByRef maps a run ref to the environment namespace its TestRun lives in.
	nsByRef map[string]string
}

// NewK8sTestRunRunner builds a k8s runner backed by client.
func NewK8sTestRunRunner(client k8s.Client) *K8sTestRunRunner {
	return &K8sTestRunRunner{Client: client, nsByRef: map[string]string{}}
}

// Mode reports the backend mode for tbl_load_tests.runner_type.
func (r *K8sTestRunRunner) Mode() string { return "k8s" }

func (r *K8sTestRunRunner) nsFor(ref string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nsByRef[ref]
}

func (r *K8sTestRunRunner) setNs(ref, ns string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nsByRef[ref] = ns
}

// Provision stages the script into a run-scoped ConfigMap and returns the run
// ref (the run id). The ConfigMap is the script source for the TestRun created
// in Start; owner references / cleanup keep orphans from accumulating.
func (r *K8sTestRunRunner) Provision(_ context.Context, spec Spec) (string, error) {
	if spec.ID == "" {
		return "", fmt.Errorf("k8s runner: Provision requires spec.ID")
	}
	if spec.Namespace == "" {
		return "", fmt.Errorf("k8s runner: Provision requires spec.Namespace")
	}
	cm := toUnstructured(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      spec.ID + "-script",
			"namespace": spec.Namespace,
			"labels": map[string]any{
				"loadtest.io/run-id":     spec.ID,
				"loadtest.io/managed-by": "loadtest-controller",
			},
		},
		"data": map[string]any{
			"test.js": string(spec.Script),
		},
	}, "", "v1", "ConfigMap")
	if err := r.Client.Apply(context.Background(), cm); err != nil {
		return "", fmt.Errorf("k8s runner: create script configmap: %w", err)
	}
	r.setNs(spec.ID, spec.Namespace)
	return spec.ID, nil
}

// Start creates the TestRun CR referencing the staged ConfigMap. Non-blocking:
// the operator's controller runs the k6 pods; Status polls progress.
func (r *K8sTestRunRunner) Start(_ context.Context, ref string, spec Spec) error {
	if ref == "" {
		return fmt.Errorf("k8s runner: Start requires a ref")
	}
	if spec.Namespace == "" {
		return fmt.Errorf("k8s runner: Start requires spec.Namespace")
	}
	args := []string{
		"--vus", strconv.Itoa(maxInt(spec.VUs, 1)),
		"--duration", strOfNonEmpty(spec.Duration, "1m"),
		"--quiet",
	}
	tr := toUnstructured(map[string]any{
		"apiVersion": "k6.io/v1alpha1",
		"kind":       "TestRun",
		"metadata": map[string]any{
			"name":      ref,
			"namespace": spec.Namespace,
			"labels": map[string]any{
				"loadtest.io/run-id":     ref,
				"app":                    "k6",
				"k6_cr":                  ref,
				"loadtest.io/managed-by": "loadtest-controller",
			},
		},
		"spec": map[string]any{
			"parallelism": int64(1),
			"script": map[string]any{
				"configMap": map[string]any{
					"name": ref + "-script",
					"file": "test.js",
				},
			},
			"args": args,
			// Stamp run-id on runner/initializer/starter so k6-operator
			// propagates it to the pods they spawn. The run-scoped
			// NetworkPolicies select pods by loadtest.io/run-id, so without
			// these labels the policies match nothing and the run stalls.
			"runner": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{
						"loadtest.io/run-id":     ref,
						"app":                    "k6",
						"k6_cr":                  ref,
						"runner":                 "true",
						"loadtest.io/managed-by": "loadtest-controller",
					},
				},
			},
			"initializer": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{
						"loadtest.io/run-id":     ref,
						"app":                    "k6",
						"k6_cr":                  ref,
						"loadtest.io/managed-by": "loadtest-controller",
					},
				},
			},
			"starter": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{
						"loadtest.io/run-id":     ref,
						"app":                    "k6",
						"k6_cr":                  ref,
						"loadtest.io/managed-by": "loadtest-controller",
					},
				},
			},
		},
	}, "k6.io", "v1alpha1", "TestRun")
	if err := r.Client.Apply(context.Background(), tr); err != nil {
		return fmt.Errorf("k8s runner: create TestRun: %w", err)
	}
	return nil
}

// Status reads the TestRun and maps status.stage to the shared RunStatus:
//
//	initialization / initialized → PROVISIONING
//	started → RUNNING
//	finished → COMPLETED
//	stopped → STOPPED
//	error → FAILED
//	missing → STOPPED with Missing=true (reconciler treats as runner-lost)
//
// Fallback: k6-operator may not advance TestRun.status.stage past "created"
// even after spawning runner pods. When stage is "created"/"initialization"/
// "initialized"/"" and a pod labeled loadtest.io/run-id=<ref> is Running, treat
// the run as RUNNING so the reconciler can advance PROVISIONING→RUNNING.
func (r *K8sTestRunRunner) Status(_ context.Context, ref string) (Status, error) {
	tr, err := r.Client.Get(context.Background(), testRunKind, r.nsFor(ref), ref)
	if err != nil {
		if err == k8s.ErrNotFound {
			return Status{State: "STOPPED", Msg: "testrun not found", Missing: true}, nil
		}
		return Status{}, err
	}
	stage, _, _ := unstructured.NestedString(tr.Object, "status", "stage")
	switch stage {
	case "started":
		return Status{State: "RUNNING"}, nil
	case "finished":
		return Status{State: "COMPLETED"}, nil
	case "stopped":
		return Status{State: "STOPPED"}, nil
	case "error":
		return Status{State: "FAILED", Msg: "testrun stage error"}, nil
	}
	// stage is "created"/"initialization"/"initialized"/"" — fall back to
	// runner-pod readiness.
	pods, err := r.Client.List(context.Background(), podGVK, r.nsFor(ref),
		labels.Set{"loadtest.io/run-id": ref})
	if err != nil {
		return Status{}, err
	}
	for _, p := range pods {
		phase, _, _ := unstructured.NestedString(p.Object, "status", "phase")
		if phase == "Running" {
			return Status{State: "RUNNING"}, nil
		}
	}
	return Status{State: "PROVISIONING"}, nil
}

// Stop deletes the TestRun CR. Idempotent: deleting an already-gone run is
// success. The operator cleans up the runner/initializer/starter pods.
func (r *K8sTestRunRunner) Stop(_ context.Context, ref string) error {
	if err := r.Client.Delete(context.Background(), testRunKind, r.nsFor(ref), ref); err != nil && err != k8s.ErrNotFound {
		return fmt.Errorf("k8s runner: delete TestRun: %w", err)
	}
	return nil
}

// ListByRunID returns TestRuns carrying the run-id label, used by the
// reconciler to discover runs after a controller restart.
func (r *K8sTestRunRunner) ListByRunID(_ context.Context, runID string) ([]string, error) {
	list, err := r.Client.List(context.Background(), testRunKind, r.nsFor(runID),
		labels.Set{"loadtest.io/run-id": runID})
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(list))
	for _, tr := range list {
		refs = append(refs, tr.GetName())
	}
	return refs, nil
}

// strOfNonEmpty returns s when non-empty, else def.
func strOfNonEmpty(s, def string) string {
	if s != "" {
		return s
	}
	return def
}
