package runner

import (
	"context"
	"errors"
	"strings"
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

// objName returns the K8s object name rendered for a run: "run-" + ref. The
// "run-" prefix keeps metadata.name DNS-1035 compliant even when ref (the
// LT_ID UUID) starts with a digit. Tests use this mirror of
// pkgs/k8s.RunObjectName so object names in test fixtures match production.
func objName(ref string) string {
	return "run-" + ref
}

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
	// Script ConfigMap is run--script (DNS-1035), not -script.
	cm, err := r.Client.Get(ctx, cmGVK(), "lt-env-ns1", objName(ref)+"-script")
	if err != nil {
		t.Fatalf("script ConfigMap %s not created: %v", objName(ref)+"-script", err)
	}
	content, _, _ := unstructured.NestedString(cm.Object, "data", "test.js")
	if content == "" {
		t.Fatal("ConfigMap data.test.js empty")
	}
	lbls := cm.GetLabels()
	if lbls["loadtest.io/run-id"] != "run-123" {
		t.Fatalf("ConfigMap run-id label = %q", lbls)
	}
	if _, err := r.Client.Get(ctx, cmGVK(), "lt-env-ns1", ref+"-script"); err != k8s.ErrNotFound {
		t.Fatalf("unprefixed ConfigMap %q should not exist (err=%v)", ref+"-script", err)
	}
}

func TestK8sTestRunRunnerProvisionStartWithNumericRef(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	// Digit-leading UUID, exactly like the CI failure.
	ref := "3c1cb1c9-bc06-4d88-ad0f-d6a5cadb4b5f"

	gotRef, err := r.Provision(ctx, Spec{
		ID:        ref,
		Namespace: testNS,
		Script:    []byte(`export default function(){ http.get("http://echo:80/"); }`),
		VUs:       10,
		Duration:  "30s",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if gotRef != ref {
		t.Fatalf("Provision ref = %q, want %q", gotRef, ref)
	}

	// Script ConfigMap must be run--script, not -script (starts
	// with a digit -> DNS-1035 invalid).
	if _, err := r.Client.Get(ctx, cmGVK(), testNS, objName(ref)+"-script"); err != nil {
		t.Fatalf("script ConfigMap %s not created: %v", objName(ref)+"-script", err)
	}
	if _, err := r.Client.Get(ctx, cmGVK(), testNS, ref+"-script"); err != k8s.ErrNotFound {
		t.Fatalf("unprefixed ConfigMap %q should not exist", ref+"-script")
	}

	if err := r.Start(ctx, ref, Spec{ID: ref, Namespace: testNS, Duration: "30s"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	tr, err := r.Client.Get(ctx, testRunGVK(), testNS, objName(ref))
	if err != nil {
		t.Fatalf("TestRun %s not created: %v", objName(ref), err)
	}
	if lbls := tr.GetLabels(); lbls["loadtest.io/run-id"] != ref {
		t.Fatalf("TestRun run-id label = %q, want raw UUID %q", lbls["loadtest.io/run-id"], ref)
	}
	cm, _, _ := unstructured.NestedMap(tr.Object, "spec", "script")
	cmName, _, _ := unstructured.NestedString(cm, "configMap", "name")
	if cmName != objName(ref)+"-script" {
		t.Fatalf("TestRun script.configMap.name = %q, want %s-script", cmName, objName(ref))
	}
	// Get by raw UUID must NOT find the TestRun (name is run-).
	if _, err := r.Client.Get(ctx, testRunGVK(), testNS, ref); err != k8s.ErrNotFound {
		t.Fatalf("TestRun %s should not exist (name is %s), err=%v", ref, objName(ref), err)
	}
}

func TestK8sTestRunRunnerStartCreatesTestRun(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-123", Namespace: testNS, VUs: 10, Duration: "30s"})
	if err := r.Start(ctx, ref, Spec{ID: ref, Namespace: testNS, Duration: "30s"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tr, err := r.Client.Get(ctx, testRunGVK(), "lt-env-ns1", objName(ref))
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
	// run-id must be stamped on runner/starter metadata so k6-operator
	// propagates it to the pods the run policies select. (initializer carries
	// disabled:true and is asserted separately.)
	for _, comp := range []string{"runner", "starter"} {
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
	if cm != objName(ref)+"-script" {
		t.Fatalf("TestRun script.configMap.name = %q, want %s-script", cm, objName(ref))
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
			// Plant a TestRun with the given stage (object name is run-).
			tr := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "k6.io/v1alpha1",
					"kind":       "TestRun",
					"metadata":   map[string]any{"name": objName(ref), "namespace": "lt-env-ns1"},
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

	// Plant a TestRun stuck at stage "created" (object name is run-).
	tr := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k6.io/v1alpha1",
			"kind":       "TestRun",
			"metadata":   map[string]any{"name": objName(ref), "namespace": testNS},
			"status": map[string]any{
				"stage": "created",
			},
		},
	}
	if err := r.Client.Apply(ctx, tr); err != nil {
		t.Fatalf("plant TestRun: %v", err)
	}

	// Plant a runner pod labeled loadtest.io/run-id=ref AND runner=true,
	// phase Running. (ref starts with a letter so the pod name is valid.)
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

	// Plant a TestRun stuck at stage "created" (object name is run-).
	tr := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k6.io/v1alpha1",
			"kind":       "TestRun",
			"metadata":   map[string]any{"name": objName(ref), "namespace": testNS},
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
			"metadata":   map[string]any{"name": objName(ref), "namespace": "lt-env-ns1"},
		},
	}); err != nil {
		t.Fatalf("plant: %v", err)
	}
	if err := r.Stop(ctx, ref); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := r.Client.Get(ctx, testRunGVK(), "lt-env-ns1", objName(ref)); err != k8s.ErrNotFound {
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
		"metadata":   map[string]any{"name": objName(ref), "namespace": "lt-env-ns1", "labels": map[string]any{"loadtest.io/run-id": ref}},
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

// plantJob plants a Job named run--1 with the given condition type/status.
// Job name mirrors the Status() computation (k8s.RunObjectName(ref) + "-1").
func plantJob(t *testing.T, r *K8sTestRunRunner, ref, condType, condStatus string) {
	t.Helper()
	ctx := context.Background()
	cond := map[string]any{
		"type":   condType,
		"status": condStatus,
	}
	jobName := objName(ref) + "-1"
	job := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "batch/v1",
			"kind":       "Job",
			"metadata": map[string]any{
				"name":      jobName,
				"namespace": testNS,
				"labels": map[string]any{
					"job-name": jobName,
				},
			},
			"status": map[string]any{
				"conditions": []any{cond},
			},
		},
	}
	if err := r.Client.Apply(ctx, job); err != nil {
		t.Fatalf("plant Job: %v", err)
	}
}

// TestK8sTestRunRunnerStatusJobCompleteIsCompleted — stage stuck at
// "initialized" (operator never advanced it) but Job "-1" is Complete must
// still surface COMPLETED to the reconciler.
func TestK8sTestRunRunnerStatusJobCompleteIsCompleted(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-jc", Namespace: testNS})

	tr := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k6.io/v1alpha1",
			"kind":       "TestRun",
			"metadata":   map[string]any{"name": objName(ref), "namespace": testNS},
			"status": map[string]any{
				"stage": "initialized", // operator stalled here
			},
		},
	}
	if err := r.Client.Apply(ctx, tr); err != nil {
		t.Fatalf("plant TestRun: %v", err)
	}
	plantJob(t, r, ref, "Complete", "True")

	st, err := r.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != "COMPLETED" {
		t.Fatalf("stage=initialized + Job Complete -> state %q, want COMPLETED", st.State)
	}
}

// TestK8sTestRunRunnerStatusJobFailedIsFailed — stage stuck but Job "-1"
// Failed must surface FAILED.
func TestK8sTestRunRunnerStatusJobFailedIsFailed(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-jf", Namespace: testNS})

	tr := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k6.io/v1alpha1",
			"kind":       "TestRun",
			"metadata":   map[string]any{"name": objName(ref), "namespace": testNS},
			"status": map[string]any{
				"stage": "initialized",
			},
		},
	}
	if err := r.Client.Apply(ctx, tr); err != nil {
		t.Fatalf("plant TestRun: %v", err)
	}
	plantJob(t, r, ref, "Failed", "True")

	st, err := r.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != "FAILED" {
		t.Fatalf("stage=initialized + Job Failed -> state %q, want FAILED", st.State)
	}
}

// TestK8sTestRunRunnerStartRendersPausedFalse — paused must be "false" so k6
// runs without the starter/Service 6565 handshake.
func TestK8sTestRunRunnerStartRendersPausedFalse(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-pf", Namespace: testNS})
	if err := r.Start(ctx, ref, Spec{ID: ref, Namespace: testNS}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tr, err := r.Client.Get(ctx, testRunGVK(), testNS, objName(ref))
	if err != nil {
		t.Fatalf("TestRun not created: %v", err)
	}
	paused, _, _ := unstructured.NestedString(tr.Object, "spec", "paused")
	if paused != "false" {
		t.Fatalf("spec.paused = %q, want \"false\"", paused)
	}
}

// TestK8sTestRunRunnerStartRendersInitializerDisabled — initializer must be
// disabled so the platform skips the k6-inspect chagne.
func TestK8sTestRunRunnerStartRendersInitializerDisabled(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-id", Namespace: testNS})
	if err := r.Start(ctx, ref, Spec{ID: ref, Namespace: testNS}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tr, err := r.Client.Get(ctx, testRunGVK(), testNS, objName(ref))
	if err != nil {
		t.Fatalf("TestRun not created: %v", err)
	}
	disabled, found, _ := unstructured.NestedBool(tr.Object, "spec", "initializer", "disabled")
	if !found || !disabled {
		t.Fatalf("spec.initializer.disabled = %v (found=%v), want true", disabled, found)
	}
}

// TestK8sTestRunRunnerStartRendersRunnerProbes — explicit readiness/liveness
// probes must be rendered (not the operator's 1s default).
func TestK8sTestRunRunnerStartRendersRunnerProbes(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-pr", Namespace: testNS})
	if err := r.Start(ctx, ref, Spec{ID: ref, Namespace: testNS}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tr, err := r.Client.Get(ctx, testRunGVK(), testNS, objName(ref))
	if err != nil {
		t.Fatalf("TestRun not created: %v", err)
	}
	for _, probe := range []string{"readinessProbe", "livenessProbe"} {
		p, found, _ := unstructured.NestedMap(tr.Object, "spec", "runner", probe)
		if !found {
			t.Fatalf("spec.runner.%s not rendered", probe)
		}
		path, _, _ := unstructured.NestedString(p, "httpGet", "path")
		portAny, portFound, _ := unstructured.NestedFieldNoCopy(p, "httpGet", "port")
		if !portFound {
			t.Fatalf("spec.runner.%s httpGet.port missing", probe)
		}
		var port int64
		switch v := portAny.(type) {
		case float64:
			port = int64(v)
		case int64:
			port = v
		default:
			t.Fatalf("spec.runner.%s httpGet.port type %T, want numeric", probe, portAny)
		}
		if path != "/v1/status" || port != 6565 {
			t.Fatalf("spec.runner.%s httpGet = path=%q port=%d, want /v1/status 6565", probe, path, port)
		}
	}
}

// plantRunnerPod plants the k6-operator runner pod (labels Status() uses) so
// the results collector can find it by run id.
func plantRunnerPod(t *testing.T, r *K8sTestRunRunner, ref, name string) {
	t.Helper()
	pod := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":      name,
				"namespace": testNS,
				"labels": map[string]any{
					"loadtest.io/run-id": ref,
					"runner":             "true",
				},
			},
			"status": map[string]any{"phase": "Succeeded"},
		},
	}
	if err := r.Client.Apply(context.Background(), pod); err != nil {
		t.Fatalf("plant pod: %v", err)
	}
}

// TestK8sStatusAttachesPodLogSummary — terminal status must carry the pod log
// tail, which is the only place k6's summary exists in K8s mode (plans/45 §1).
func TestK8sStatusAttachesPodLogSummary(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-log", Namespace: testNS})

	tr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "k6.io/v1alpha1",
		"kind":       "TestRun",
		"metadata":   map[string]any{"name": objName(ref), "namespace": testNS},
		"status":     map[string]any{"stage": "finished"},
	}}
	if err := r.Client.Apply(ctx, tr); err != nil {
		t.Fatal(err)
	}
	plantRunnerPod(t, r, ref, objName(ref)+"-1-abc12")
	r.Client.(*k8s.Fake).Logs[testNS+"/"+objName(ref)+"-1-abc12"] =
		[]byte("k6 output\n█ TOTAL RESULTS\n  http_reqs: 10\n")

	st, err := r.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != "COMPLETED" {
		t.Fatalf("state = %q", st.State)
	}
	if !strings.Contains(string(st.Summary), "TOTAL RESULTS") {
		t.Errorf("Summary = %q", st.Summary)
	}
	if st.SummaryErr != "" {
		t.Errorf("SummaryErr = %q", st.SummaryErr)
	}
}

// TestK8sStatusSummaryMissingPodKeepsTerminalState — a GC'd pod must not turn
// a completed run into an error; it records SummaryErr instead.
func TestK8sStatusSummaryMissingPodKeepsTerminalState(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-nopod", Namespace: testNS})
	tr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "k6.io/v1alpha1",
		"kind":       "TestRun",
		"metadata":   map[string]any{"name": objName(ref), "namespace": testNS},
		"status":     map[string]any{"stage": "finished"},
	}}
	if err := r.Client.Apply(ctx, tr); err != nil {
		t.Fatal(err)
	}
	st, err := r.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status must not fail when the pod is gone: %v", err)
	}
	if st.State != "COMPLETED" {
		t.Errorf("state = %q, want COMPLETED", st.State)
	}
	if st.SummaryErr == "" {
		t.Error("SummaryErr is empty with no runner pod")
	}
	if len(st.Summary) != 0 {
		t.Errorf("Summary = %q", st.Summary)
	}
}

// TestK8sStatusSummaryPodLogFailure — an injected log-read failure is reported,
// never swallowed and never fatal.
func TestK8sStatusSummaryPodLogFailure(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-logerr", Namespace: testNS})
	tr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "k6.io/v1alpha1",
		"kind":       "TestRun",
		"metadata":   map[string]any{"name": objName(ref), "namespace": testNS},
		"status":     map[string]any{"stage": "finished"},
	}}
	if err := r.Client.Apply(ctx, tr); err != nil {
		t.Fatal(err)
	}
	plantRunnerPod(t, r, ref, objName(ref)+"-1-abc12")
	fake := r.Client.(*k8s.Fake)
	fake.Faults.PodLogsErr = errors.New("forbidden")

	st, err := r.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != "COMPLETED" || st.SummaryErr == "" {
		t.Errorf("status = %+v, want COMPLETED with SummaryErr", st)
	}
}

// TestK8sStatusNoSummaryWhileRunning — the log is read once, at the end.
func TestK8sStatusNoSummaryWhileRunning(t *testing.T) {
	r := newK8sRunner(t)
	ctx := context.Background()
	ref, _ := r.Provision(ctx, Spec{ID: "run-running", Namespace: testNS})
	tr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "k6.io/v1alpha1",
		"kind":       "TestRun",
		"metadata":   map[string]any{"name": objName(ref), "namespace": testNS},
		"status":     map[string]any{"stage": "started"},
	}}
	if err := r.Client.Apply(ctx, tr); err != nil {
		t.Fatal(err)
	}
	plantRunnerPod(t, r, ref, objName(ref)+"-1-abc12")
	r.Client.(*k8s.Fake).Logs[testNS+"/"+objName(ref)+"-1-abc12"] = []byte("partial")

	st, err := r.Status(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "RUNNING" {
		t.Errorf("state = %q", st.State)
	}
	if len(st.Summary) != 0 || st.SummaryErr != "" {
		t.Errorf("running status carried a summary: %+v", st)
	}
}
