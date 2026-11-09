package runner

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

func newK8sRunner(t *testing.T) *K8sTestRunRunner {
	t.Helper()
	return NewK8sTestRunRunner(k8s.NewFake())
}

const testNS = "lt-env-ns1"

func testRunGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "k6.io", Version: "v1alpha1", Kind: "TestRun"}
}

func cmGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
}

func TestK8sTestRunRunnerMode(t *testing.T) {
	r := newK8sRunner(t)
	if r.Mode() != "k8s" {
		t.Fatalf("mode = %q, want k8s", r.Mode())
	}
}

func TestK8sTestRunRunnerProvisionCreatesScriptConfigMap(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, err := r.Provision(ctx, Spec{
		ID:        "run-123",
		Namespace: testNS,
		Script:    []byte(`export default function(){ http.get("http://echo:80/"); }`),
		VUs:       10,
		Duration:  "30s",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if ref != "run-123" {
		t.Fatalf("ref = %q, want run-123", ref)
	}
	cm, err := r.Client.Get(ctx, cmGVK(), "lt-env-ns1", "run-123-script")
	if err != nil {
		t.Fatalf("script ConfigMap not created: %v", err)
	}
	content, _, _ := unstructured.NestedString(cm.Object, "data", "test.js")
	if content == "" {
		t.Fatal("ConfigMap data.test.js empty")
	}
	lbls := cm.GetLabels()
	if lbls["loadtest.io/run-id"] != "run-123" {
		t.Fatalf("ConfigMap run-id label = %q", lbls)
	}
}

func TestK8sTestRunRunnerStartCreatesTestRun(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-123", Namespace: testNS, VUs: 10, Duration: "30s"})
	if err := r.Start(ctx, ref, Spec{ID: ref, Namespace: testNS, Duration: "30s"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tr, err := r.Client.Get(ctx, testRunGVK(), "lt-env-ns1", "run-123")
	if err != nil {
		t.Fatalf("TestRun not created: %v", err)
	}
	lbls := tr.GetLabels()
	if lbls["loadtest.io/run-id"] != "run-123" {
		t.Fatalf("TestRun run-id label = %q", lbls)
	}
	if lbls["app"] != "k6" {
		t.Fatalf("TestRun app label = %q", lbls)
	}
	if lbls["k6_cr"] != "run-123" {
		t.Fatalf("TestRun k6_cr label = %q", lbls)
	}
	// run-id must be stamped on runner/initializer/starter metadata so
	// k6-operator propagates it to the pods the run policies select.
	for _, comp := range []string{"runner", "initializer", "starter"} {
		l, _, _ := unstructured.NestedMap(tr.Object, "spec", comp, "metadata", "labels")
		if l["loadtest.io/run-id"] != "run-123" {
			t.Fatalf("TestRun spec.%s.metadata.labels run-id = %v, want run-123", comp, l)
		}
	}
	if l, _, _ := unstructured.NestedMap(tr.Object, "spec", "runner", "metadata", "labels"); l["runner"] != "true" {
		t.Fatalf("TestRun spec.runner.metadata.labels runner = %v, want true", l)
	}
	script, _, _ := unstructured.NestedMap(tr.Object, "spec", "script")
	cm, _, _ := unstructured.NestedString(script, "configMap", "name")
	if cm != "run-123-script" {
		t.Fatalf("TestRun script.configMap.name = %q, want run-123-script", cm)
	}
	args, _, _ := unstructured.NestedSlice(tr.Object, "spec", "args")
	foundDuration := false
	for _, a := range args {
		if a.(string) == "30s" {
			foundDuration = true
		}
	}
	if !foundDuration {
		t.Fatalf("TestRun spec.args missing duration: %v", args)
	}
}

func TestK8sTestRunRunnerStatusMapsStages(t *testing.T) {
	stageCases := []struct {
		stage string
		want  string
	}{
		{"initialization", "PROVISIONING"},
		{"initialized", "PROVISIONING"},
		{"created", "PROVISIONING"},
		{"started", "RUNNING"},
		{"finished", "COMPLETED"},
		{"stopped", "STOPPED"},
		{"error", "FAILED"},
		{"", "PROVISIONING"}, // unknown → still provisioning, not terminal
	}
	for _, c := range stageCases {
		t.Run(c.stage, func(t *testing.T) {
			r := newK8sRunner(t)
			ctx := context.Background()
			ref, _ := r.Provision(ctx, Spec{ID: "run-x", Namespace: testNS})
			// Plant a TestRun with the given stage.
			tr := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "k6.io/v1alpha1",
					"kind":       "TestRun",
					"metadata":   map[string]any{"name": ref, "namespace": "lt-env-ns1"},
					"status": map[string]any{
						"stage": c.stage,
					},
				},
			}
			if err := r.Client.Apply(ctx, tr); err != nil {
				t.Fatalf("plant TestRun: %v", err)
			}
			st, err := r.Status(ctx, ref)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if st.State != c.want {
				t.Fatalf("stage %q → state %q, want %q", c.stage, st.State, c.want)
			}
		})
	}
}

// TestK8sTestRunRunnerStatusFallsBackToRunnerPod — k6-operator may not advance
// TestRun.status.stage past "created" even after spawning runner pods. When a
// runner pod labeled loadtest.io/run-id=ref is Running, Status must return
// RUNNING so the reconciler can advance PROVISIONING→RUNNING.
func TestK8sTestRunRunnerStatusFallsBackToRunnerPod(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-y", Namespace: testNS})

	// Plant a TestRun stuck at stage "created".
	tr := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k6.io/v1alpha1",
			"kind":       "TestRun",
			"metadata":   map[string]any{"name": ref, "namespace": testNS},
			"status": map[string]any{
				"stage": "created",
			},
		},
	}
	if err := r.Client.Apply(ctx, tr); err != nil {
		t.Fatalf("plant TestRun: %v", err)
	}

	// Plant a runner pod labeled loadtest.io/run-id=ref AND runner=true,
	// phase Running.
	pod := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":      ref + "-runner",
				"namespace": testNS,
				"labels": map[string]any{
					"loadtest.io/run-id": ref,
					"runner":             "true",
				},
			},
			"status": map[string]any{
				"phase": "Running",
			},
		},
	}
	if err := r.Client.Apply(ctx, pod); err != nil {
		t.Fatalf("plant Pod: %v", err)
	}

	st, err := r.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != "RUNNING" {
		t.Fatalf("stage=created + runner pod Running → state %q, want RUNNING", st.State)
	}
}

// TestK8sTestRunRunnerStatusIgnoresInitializerOnlyPod — an initializer pod
// (Completed, no runner=true label) must not be treated as a running runner.
// With stage "created" and only an initializer pod present, Status returns
// PROVISIONING, not RUNNING.
func TestK8sTestRunRunnerStatusIgnoresInitializerOnlyPod(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-z", Namespace: testNS})

	// Plant a TestRun stuck at stage "created".
	tr := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k6.io/v1alpha1",
			"kind":       "TestRun",
			"metadata":   map[string]any{"name": ref, "namespace": testNS},
			"status": map[string]any{
				"stage": "created",
			},
		},
	}
	if err := r.Client.Apply(ctx, tr); err != nil {
		t.Fatalf("plant TestRun: %v", err)
	}

	// Plant an initializer pod: Completed, labeled run-id but NOT runner=true.
	pod := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":      ref + "-initializer",
				"namespace": testNS,
				"labels": map[string]any{
					"loadtest.io/run-id": ref,
				},
			},
			"status": map[string]any{
				"phase": "Completed",
			},
		},
	}
	if err := r.Client.Apply(ctx, pod); err != nil {
		t.Fatalf("plant Pod: %v", err)
	}

	st, err := r.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != "PROVISIONING" {
		t.Fatalf("stage=created + only initializer pod → state %q, want PROVISIONING", st.State)
	}
}

func TestK8sTestRunRunnerStatusMissingIsNotTerminal(t *testing.T) {
	r := newK8sRunner(t)
	st, err := r.Status(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("Status missing ref: %v", err)
	}
	if !st.Missing {
		t.Fatalf("Missing = %v, want true", st.Missing)
	}
}

func TestK8sTestRunRunnerStopDeletesTestRun(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-stop", Namespace: testNS})
	if err := r.Client.Apply(ctx, &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k6.io/v1alpha1",
			"kind":       "TestRun",
			"metadata":   map[string]any{"name": ref, "namespace": "lt-env-ns1"},
		},
	}); err != nil {
		t.Fatalf("plant: %v", err)
	}
	if err := r.Stop(ctx, ref); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := r.Client.Get(ctx, testRunGVK(), "lt-env-ns1", ref); err != k8s.ErrNotFound {
		t.Fatalf("TestRun still present after Stop: err=%v", err)
	}
	// Idempotent: stopping again is fine.
	if err := r.Stop(ctx, ref); err != nil {
		t.Fatalf("Stop idempotent: %v", err)
	}
}

func TestK8sTestRunRunnerListByRunID(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-list", Namespace: testNS})
	_ = r.Client.Apply(ctx, toUnstructured(map[string]any{
		"apiVersion": "k6.io/v1alpha1",
		"kind":       "TestRun",
		"metadata":   map[string]any{"name": ref, "namespace": "lt-env-ns1", "labels": map[string]any{"loadtest.io/run-id": ref}},
	}, "k6.io", "v1alpha1", "TestRun"))
	list, err := r.Client.List(ctx, testRunGVK(), "lt-env-ns1", labels.Set{"loadtest.io/run-id": ref})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List by run-id = %d, want 1", len(list))
	}
}

func TestK8sTestRunRunnerStopMissingIsNoop(t *testing.T) {
	r := newK8sRunner(t)
	if err := r.Stop(context.Background(), "does-not-exist"); err != nil {
		t.Fatalf("Stop of missing TestRun must be nil, got %v", err)
	}
}
