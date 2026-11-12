package envreconciler_test

import (
	"context"
	"sync"

	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func testEnv() *envreconciler.Env {
	return &envreconciler.Env{
		NamespaceID: "11111111-2222-3333-4444-555555555555",
		TenantID:    "t-1",
		Status:      "APPLYING",
		Services: []envreconciler.ServiceSpec{{
			Name: "checkout", Image: "ghcr.io/acme/checkout:v1", Replicas: 2, Port: 8080,
			Env:      map[string]string{"FEATURE": "on"},
			Health:   &envreconciler.HealthProbe{Path: "/healthz", Port: 8080},
			SpecHash: "hash-checkout",
		}},
		Databases: []envreconciler.DatabaseSpec{{
			Name: "orders-db", Image: "postgres:16", StorageGB: 5, SpecHash: "hash-db",
		}},
		Ingresses: []envreconciler.IngressSpec{{
			Name: "main", IngressClass: "nginx", SpecHash: "hash-ing",
			Rules: []envreconciler.IngressRule{{Host: "shop.example.com", Path: "/", ServiceName: "checkout", Port: 8080}},
		}},
		Policy: &envreconciler.PolicySpec{
			Egress: []envreconciler.EgressRule{{Type: "service", RefID: "checkout"}},
		},
	}
}

func TestINT_ENV_01_RenderOrderAndLabels(t *testing.T) {
	objs := envreconciler.Render(testEnv())
	if len(objs) == 0 {
		t.Fatal("no objects rendered")
	}
	wantOrder := []string{"Namespace", "NetworkPolicy", "NetworkPolicy", "NetworkPolicy", "NetworkPolicy", "Cluster", "Deployment", "Service", "Ingress"}
	var gotOrder []string
	for _, o := range objs {
		gotOrder = append(gotOrder, o.GetKind())
	}
	if len(gotOrder) < len(wantOrder) {
		t.Fatalf("rendered kinds %v, want prefix %v", gotOrder, wantOrder)
	}
	for i, kind := range wantOrder {
		if gotOrder[i] != kind {
			t.Fatalf("step %d: got %s, want %s (full order %v)", i, gotOrder[i], kind, gotOrder)
		}
	}
	envNS := "lt-env-11111111-2222-3333-4444-555555555555"
	for _, o := range objs {
		if o.GetKind() == "Namespace" {
			if o.GetName() != envNS {
				t.Fatalf("env namespace name %s, want %s", o.GetName(), envNS)
			}
			continue
		}
		if o.GetNamespace() != envNS {
			t.Fatalf("%s/%s namespaced object in ns %s", o.GetKind(), o.GetName(), o.GetNamespace())
		}
		lbls := o.GetLabels()
		if lbls[k8s.LabelManagedBy] != k8s.ManagedByValue ||
			lbls[k8s.LabelNamespaceID] != testEnv().NamespaceID ||
			lbls[k8s.LabelTenantID] != "t-1" {
			t.Fatalf("%s/%s missing ownership labels: %v", o.GetKind(), o.GetName(), lbls)
		}
		if o.GetAnnotations()[k8s.AnnotationSpecHash] == "" {
			t.Fatalf("%s/%s missing spec_hash annotation", o.GetKind(), o.GetName())
		}
	}
}

func TestINT_ENV_02_RenderServiceShape(t *testing.T) {
	objs := envreconciler.Render(testEnv())
	var dep *unstructured.Unstructured
	for _, o := range objs {
		if o.GetKind() == "Deployment" && o.GetName() == "checkout" {
			dep = o
		}
	}
	if dep == nil {
		t.Fatal("checkout Deployment not rendered")
	}
	spec, _, _ := unstructured.NestedMap(dep.Object, "spec", "template", "spec")
	containers, _, _ := unstructured.NestedSlice(spec, "containers")
	c0 := containers[0].(map[string]any)
	if c0["image"] != "ghcr.io/acme/checkout:v1" {
		t.Fatalf("container image %v", c0["image"])
	}
	envMap := map[string]any{}
	for _, e := range c0["env"].([]any) {
		envMap[e.(map[string]any)["name"].(string)] = e.(map[string]any)["value"]
	}
	if envMap["FEATURE"] != "on" {
		t.Fatalf("env not injected: %v", envMap)
	}
	// replicas + labels the watcher keys on
	reps, _, _ := unstructured.NestedNumberAsFloat64(dep.Object, "spec", "replicas")
	if reps != 2 {
		t.Fatalf("replicas %v", reps)
	}
	if dep.GetLabels()[k8s.LabelComponentID] != "checkout" {
		t.Fatalf("component label %v", dep.GetLabels())
	}
}

func TestINT_ENV_03_RenderNetworkPolicyDefaultDeny(t *testing.T) {
	objs := envreconciler.Render(testEnv())
	np := objs[1]
	podSel, _, _ := unstructured.NestedMap(np.Object, "spec", "podSelector")
	if len(podSel) != 0 {
		t.Fatalf("default-deny podSelector not empty: %v", podSel)
	}
	ingressTypes, found, _ := unstructured.NestedSlice(np.Object, "spec", "ingress")
	if !found || len(ingressTypes) == 0 {
		t.Fatal("default-deny must set empty ingress rule list")
	}
	if np.GetAnnotations()[k8s.AnnotationSpecHash] == "" {
		t.Fatal("policy missing spec_hash")
	}
}

func TestINT_ENV_04_AppliesAndPrunes(t *testing.T) {
	c := k8s.NewFake()
	rc := envreconciler.New(c, envreconciler.Options{})
	ctx := context.Background()

	if err := rc.ApplyEnv(ctx, testEnv()); err != nil {
		t.Fatalf("apply env: %v", err)
	}
	envNS := "lt-env-11111111-2222-3333-4444-555555555555"
	dGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	got, err := c.List(ctx, dGVK, envNS, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("deployments in env ns: %d err=%v", len(got), err)
	}

	// Idempotent: applying again must not duplicate.
	if err := rc.ApplyEnv(ctx, testEnv()); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	got, _ = c.List(ctx, dGVK, envNS, nil)
	if len(got) != 1 {
		t.Fatalf("duplicate deployments after re-apply: %d", len(got))
	}

	// Prune: a managed deployment no longer desired must be removed.
	stray := &unstructured.Unstructured{}
	stray.SetGroupVersionKind(dGVK)
	stray.SetNamespace(envNS)
	stray.SetName("removed-svc")
	stray.SetLabels(map[string]string{k8s.LabelManagedBy: k8s.ManagedByValue})
	if err := c.Apply(ctx, stray); err != nil {
		t.Fatalf("seed stray: %v", err)
	}
	if err := rc.ApplyEnv(ctx, testEnv()); err != nil {
		t.Fatalf("apply with stray: %v", err)
	}
	got, _ = c.List(ctx, dGVK, envNS, nil)
	for _, o := range got {
		if o.GetName() == "removed-svc" {
			t.Fatal("stray managed deployment not pruned")
		}
	}
}

func TestINT_ENV_05_DeleteEnv(t *testing.T) {
	c := k8s.NewFake()
	rc := envreconciler.New(c, envreconciler.Options{})
	ctx := context.Background()
	if err := rc.ApplyEnv(ctx, testEnv()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	envNS := "lt-env-11111111-2222-3333-4444-555555555555"
	if err := rc.DeleteEnv(ctx, testEnv().NamespaceID); err != nil {
		t.Fatalf("delete env: %v", err)
	}
	nsGVK := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	if _, err := c.Get(ctx, nsGVK, "", envNS); !errors.Is(err, k8s.ErrNotFound) {
		t.Fatalf("env namespace still exists: %v", err)
	}
	// Second delete is idempotent.
	if err := rc.DeleteEnv(ctx, testEnv().NamespaceID); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

func TestINT_ENV_06_FaultPropagates(t *testing.T) {
	c := k8s.NewFake()
	c.Faults.ApplyErr = errors.New("api server down")
	rc := envreconciler.New(c, envreconciler.Options{})
	if err := rc.ApplyEnv(context.Background(), testEnv()); err == nil {
		t.Fatal("want apply fault to surface")
	}
}

func TestINT_ENV_07_TickDelegates(t *testing.T) {
	c := k8s.NewFake()
	rc := envreconciler.New(c, envreconciler.Options{Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Tick with an empty loader converges to no-op without error.
	if err := rc.Tick(ctx, func(context.Context) ([]*envreconciler.Env, error) { return nil, nil }); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if err := rc.Tick(ctx, func(context.Context) ([]*envreconciler.Env, error) {
		return []*envreconciler.Env{testEnv()}, nil
	}); err != nil {
		t.Fatalf("tick with env: %v", err)
	}
	// Every managed object in the fake carries the managed-by label.
	for _, objs := range c.ObjectsSnapshot() {
		for _, o := range objs {
			ls := labels.Set(o.GetLabels())
			if ls[k8s.LabelManagedBy] != k8s.ManagedByValue {
				t.Fatalf("%s/%s unmanaged: %v", o.GetKind(), o.GetName(), o.GetLabels())
			}
		}
	}
}

// --- plans/26: service quản lý ĐẦY ĐỦ objects — ConfigMap (env lớn), Secret
// (secretKeyRef, không bao giờ plaintext), HPA, ServiceAccount; database CR
// theo operator (CNPG | ECK); unknown operator fail closed.
func fullServiceEnv() *envreconciler.Env {
	return &envreconciler.Env{
		NamespaceID: "11111111-2222-3333-4444-555555555555", TenantID: "t-1", Status: "APPLYING",
		Services: []envreconciler.ServiceSpec{{
			Name: "echo", Image: "ealen/echo-server:latest", Replicas: 1, Port: 8080, SpecHash: "h-echo",
			Env:            map[string]string{"LOG_LEVEL": "info"},
			ConfigMap:      map[string]string{"app.yaml": "listen: 8080"},
			Secrets:        map[string]string{"API_KEY": "supersecret"},
			HPA:            &envreconciler.HPASpec{MinReplicas: 1, MaxReplicas: 5, CPUTargetPercent: 80},
			ServiceAccount: true,
		}},
		Databases: []envreconciler.DatabaseSpec{{
			Name: "demo-db", Operator: "cloudnative-pg", Image: "postgres:16", StorageGB: 1, SpecHash: "h-db",
		}},
	}
}

func find2(objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	return nil
}

func TestRenderConfigMapSecretHPASA(t *testing.T) {
	objs := envreconciler.Render(fullServiceEnv())

	// ConfigMap <name>-config
	cm := find2(objs, "ConfigMap", "echo-config")
	if cm == nil {
		t.Fatal("ConfigMap echo-config missing")
	}
	data, _, _ := unstructured.NestedMap(cm.Object, "data")
	if data["app.yaml"] != "listen: 8080" {
		t.Fatalf("configmap data %v", data)
	}

	// Secret <name>-secret (Opaque) — value CHỈ nằm trong Secret, không trong Deployment
	sec := find2(objs, "Secret", "echo-secret")
	if sec == nil {
		t.Fatal("Secret echo-secret missing")
	}
	if sec.Object["type"] != "Opaque" {
		t.Fatalf("secret type %v", sec.Object["type"])
	}
	secData, _, _ := unstructured.NestedMap(sec.Object, "stringData")
	if secData["API_KEY"] != "supersecret" {
		t.Fatalf("secret data %v", secData)
	}

	// HPA autoscaling/v2 trỏ Deployment echo
	hpa := find2(objs, "HorizontalPodAutoscaler", "echo")
	if hpa == nil {
		t.Fatal("HPA echo missing")
	}
	if hpa.GetAPIVersion() != "autoscaling/v2" {
		t.Fatalf("hpa apiVersion %q", hpa.GetAPIVersion())
	}
	refName, _, _ := unstructured.NestedString(hpa.Object, "spec", "scaleTargetRef", "name")
	if refName != "echo" {
		t.Fatalf("hpa target %q", refName)
	}
	maxRepF, _, _ := unstructured.NestedNumberAsFloat64(hpa.Object, "spec", "maxReplicas")
	maxRep := int64(maxRepF)
	if maxRep != 5 {
		t.Fatalf("hpa maxReplicas %v", maxRep)
	}

	// ServiceAccount echo + Deployment reference
	if find2(objs, "ServiceAccount", "echo") == nil {
		t.Fatal("ServiceAccount echo missing")
	}
	dep := find2(objs, "Deployment", "echo")
	if dep == nil {
		t.Fatal("Deployment echo missing")
	}
	saName, found, _ := unstructured.NestedString(dep.Object, "spec", "template", "spec", "serviceAccountName")
	if !found || saName != "echo" {
		t.Fatalf("deployment serviceAccountName %q found=%v", saName, found)
	}

	// envFrom ConfigMap + env secretKeyRef trong container
	containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	c0 := containers[0].(map[string]any)
	envFrom, _, _ := unstructured.NestedSlice(c0, "envFrom")
	if len(envFrom) == 0 {
		t.Fatal("container envFrom (ConfigMap) missing")
	}
	foundSecretRef := false
	for _, e := range c0["env"].([]any) {
		m := e.(map[string]any)
		if m["name"] == "API_KEY" {
			if vr, ok := m["valueFrom"].(map[string]any); ok {
				skr := vr["secretKeyRef"].(map[string]any)
				if skr["name"] == "echo-secret" && skr["key"] == "API_KEY" {
					foundSecretRef = true
				}
			}
			if m["value"] == "supersecret" {
				t.Fatal("plaintext secret rendered into Deployment (violation docs §7)")
			}
		}
	}
	if !foundSecretRef {
		t.Fatal("secretKeyRef env missing")
	}
}

func TestRenderECKCluster(t *testing.T) {
	env := fullServiceEnv()
	env.Databases = []envreconciler.DatabaseSpec{{
		Name: "search-db", Operator: "elastic", Image: "docker.elastic.co/elasticsearch/elasticsearch:8.15.0",
		StorageGB: 2, SpecHash: "h-eck",
	}}
	objs := envreconciler.Render(env)
	es := find2(objs, "Elasticsearch", "search-db")
	if es == nil {
		t.Fatal("ECK Elasticsearch CR missing")
	}
	if es.GetAPIVersion() != "elasticsearch.k8s.elastic.co/v1" {
		t.Fatalf("apiVersion %q", es.GetAPIVersion())
	}
	version, _, _ := unstructured.NestedString(es.Object, "spec", "version")
	if version == "" {
		t.Fatal("spec.version missing")
	}
}

func TestRenderUnknownOperatorFailsClosed(t *testing.T) {
	env := fullServiceEnv()
	env.Databases = []envreconciler.DatabaseSpec{{Name: "weird", Operator: "mongodb-operator"}}
	if err := envreconciler.ValidateDatabaseOperators(env); err == nil {
		t.Fatal("unknown operator accepted — want fail closed")
	}
}

type recordingClient struct {
	k8s.Client
	mu    sync.Mutex
	order []string
}

func (r *recordingClient) Apply(ctx context.Context, obj *unstructured.Unstructured) error {
	r.mu.Lock()
	r.order = append(r.order, obj.GetKind()+"/"+obj.GetName())
	r.mu.Unlock()
	return r.Client.Apply(ctx, obj)
}

func (r *recordingClient) appliedOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.order...)
}

func indexOf(order []string, key string) int {
	for i, k := range order {
		if k == key {
			return i
		}
	}
	return -1
}

// TestINT_ENV_08_HierarchicalApplyOrder — namespace áp TRƯỚC tất cả con;
// trong mỗi service: ConfigMap/Secret/ServiceAccount trước Deployment.
func TestINT_ENV_08_HierarchicalApplyOrder(t *testing.T) {
	rc := &recordingClient{Client: k8s.NewFake()}
	env := fullServiceEnv()
	env.Ingresses = []envreconciler.IngressSpec{{
		Name: "main", IngressClass: "nginx", SpecHash: "h-ing",
		Rules: []envreconciler.IngressRule{{Host: "x.example.com", Path: "/", ServiceName: "echo", Port: 8080}},
	}}
	if err := envreconciler.ApplyTree(context.Background(), rc, env); err != nil {
		t.Fatalf("apply tree: %v", err)
	}
	order := rc.appliedOrder()

	nsIdx := indexOf(order, "Namespace/lt-env-11111111-2222-3333-4444-555555555555")
	if nsIdx == -1 {
		t.Fatalf("namespace not applied: %v", order)
	}
	for _, key := range []string{
		"NetworkPolicy/default-deny",
		"Cluster/demo-db",
		"ConfigMap/echo-config",
		"Secret/echo-secret",
		"Deployment/echo",
		"Service/echo",
		"HorizontalPodAutoscaler/echo",
		"Ingress/main",
	} {
		i := indexOf(order, key)
		if i == -1 {
			t.Fatalf("%s not applied: %v", key, order)
		}
		if i < nsIdx {
			t.Fatalf("%s applied BEFORE namespace (order %v)", key, order)
		}
	}
	if indexOf(order, "ConfigMap/echo-config") > indexOf(order, "Deployment/echo") {
		t.Fatalf("ConfigMap after Deployment (order %v)", order)
	}
}

// --- plan 48: namespace delete reaper (DELETING → DELETED auto-completion) ---

type recordingSink struct {
	deleted []string
}

func (s *recordingSink) MarkDeleted(_ context.Context, namespaceID, reason string) error {
	s.deleted = append(s.deleted, namespaceID+"|"+reason)
	return nil
}

func nsGVK() schema.GroupVersionKind { return schema.GroupVersionKind{Version: "v1", Kind: "Namespace"} }

func seedNamespace(ctx context.Context, t *testing.T, c *k8s.Fake, name string, terminating bool) {
	t.Helper()
	ns := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": name,
		},
	}}
	if terminating {
		ns.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	}
	if err := c.Apply(ctx, ns); err != nil {
		t.Fatalf("seed namespace %s: %v", name, err)
	}
}

func deletingEnv() *envreconciler.Env {
	env := testEnv()
	env.Status = "DELETING"
	return env
}

func TestINT_ENV_08_ReapDeleting_NotFoundMarksDeleted(t *testing.T) {
	c := k8s.NewFake()
	sink := &recordingSink{}
	rc := envreconciler.New(c, envreconciler.Options{DeleteSink: sink})
	ctx := context.Background()
	// Namespace already gone (event lost, controller crash after K8s delete):
	// the reaper must converge DB → DELETED without any event.
	err := rc.Tick(ctx, func(context.Context) ([]*envreconciler.Env, error) {
		return []*envreconciler.Env{deletingEnv()}, nil
	})
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(sink.deleted) != 1 || !strings.HasPrefix(sink.deleted[0], testEnv().NamespaceID) {
		t.Fatalf("MarkDeleted calls: %v", sink.deleted)
	}
}

func TestINT_ENV_09_ReapDeleting_ActiveDeletesNoTransition(t *testing.T) {
	c := k8s.NewFake()
	sink := &recordingSink{}
	rc := envreconciler.New(c, envreconciler.Options{DeleteSink: sink})
	ctx := context.Background()
	seedNamespace(ctx, t, c, "lt-env-11111111-2222-3333-4444-555555555555", false)
	if err := rc.Tick(ctx, func(context.Context) ([]*envreconciler.Env, error) {
		return []*envreconciler.Env{deletingEnv()}, nil
	}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, err := c.Get(ctx, nsGVK(), "", "lt-env-11111111-2222-3333-4444-555555555555"); !errors.Is(err, k8s.ErrNotFound) {
		t.Fatalf("namespace not deleted: %v", err)
	}
	if len(sink.deleted) != 0 {
		t.Fatalf("MarkDeleted must not fire until namespace is gone: %v", sink.deleted)
	}
}

func TestINT_ENV_10_ReapDeleting_TerminatingWithinDeadlineWaits(t *testing.T) {
	c := k8s.NewFake()
	sink := &recordingSink{}
	rc := envreconciler.New(c, envreconciler.Options{DeleteSink: sink, DeletionDeadline: 10 * time.Minute})
	ctx := context.Background()
	seedNamespace(ctx, t, c, "lt-env-11111111-2222-3333-4444-555555555555", true)
	env := deletingEnv()
	env.DeletingSince = time.Now().Add(-time.Minute) // well inside deadline
	if err := rc.Tick(ctx, func(context.Context) ([]*envreconciler.Env, error) {
		return []*envreconciler.Env{env}, nil
	}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(sink.deleted) != 0 {
		t.Fatalf("terminating within deadline must stay DELETING: %v", sink.deleted)
	}
}

func TestINT_ENV_11_ReapDeleting_TerminatingPastDeadlineWarnsOnce(t *testing.T) {
	c := k8s.NewFake()
	sink := &recordingSink{}
	rc := envreconciler.New(c, envreconciler.Options{DeleteSink: sink, DeletionDeadline: 10 * time.Minute})
	ctx := context.Background()
	seedNamespace(ctx, t, c, "lt-env-11111111-2222-3333-4444-555555555555", true)
	env := deletingEnv()
	env.DeletingSince = time.Now().Add(-11 * time.Minute) // past deadline
	tick := func() error {
		return rc.Tick(ctx, func(context.Context) ([]*envreconciler.Env, error) {
			return []*envreconciler.Env{env}, nil
		})
	}
	if err := tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if err := tick(); err != nil {
		t.Fatalf("second tick (stuck, retried level-triggered): %v", err)
	}
	if len(sink.deleted) != 0 {
		t.Fatalf("stuck finalizer never transitions to DELETED: %v", sink.deleted)
	}
}

func TestINT_ENV_12_ReapDeleting_DeleteErrorSurfaces(t *testing.T) {
	c := k8s.NewFake()
	c.Faults.DeleteErr = errors.New("api server down")
	sink := &recordingSink{}
	rc := envreconciler.New(c, envreconciler.Options{DeleteSink: sink})
	ctx := context.Background()
	seedNamespace(ctx, t, c, "lt-env-11111111-2222-3333-4444-555555555555", false)
	err := rc.Tick(ctx, func(context.Context) ([]*envreconciler.Env, error) {
		return []*envreconciler.Env{deletingEnv()}, nil
	})
	if err == nil {
		t.Fatal("delete fault must surface so the next tick retries")
	}
	if len(sink.deleted) != 0 {
		t.Fatalf("no DB transition when delete failed: %v", sink.deleted)
	}
}

func TestINT_ENV_13_ReapDeleting_NilSinkLegacy(t *testing.T) {
	c := k8s.NewFake()
	rc := envreconciler.New(c, envreconciler.Options{})
	ctx := context.Background()
	seedNamespace(ctx, t, c, "lt-env-11111111-2222-3333-4444-555555555555", false)
	if err := rc.Tick(ctx, func(context.Context) ([]*envreconciler.Env, error) {
		return []*envreconciler.Env{deletingEnv()}, nil
	}); err != nil {
		t.Fatalf("tick (nil sink = legacy delete-only): %v", err)
	}
	if _, err := c.Get(ctx, nsGVK(), "", "lt-env-11111111-2222-3333-4444-555555555555"); !errors.Is(err, k8s.ErrNotFound) {
		t.Fatalf("namespace not deleted: %v", err)
	}
}

func TestINT_ENV_14_ReapDeleting_StandbyNoop(t *testing.T) {
	c := k8s.NewFake()
	sink := &recordingSink{}
	rc := envreconciler.New(c, envreconciler.Options{DeleteSink: sink, IsLeader: func() bool { return false }})
	ctx := context.Background()
	seedNamespace(ctx, t, c, "lt-env-11111111-2222-3333-4444-555555555555", false)
	if err := rc.Tick(ctx, func(context.Context) ([]*envreconciler.Env, error) {
		return []*envreconciler.Env{deletingEnv()}, nil
	}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, err := c.Get(ctx, nsGVK(), "", "lt-env-11111111-2222-3333-4444-555555555555"); err != nil {
		t.Fatalf("standby must not delete: %v", err)
	}
	if len(sink.deleted) != 0 {
		t.Fatalf("standby must not write status: %v", sink.deleted)
	}
}
