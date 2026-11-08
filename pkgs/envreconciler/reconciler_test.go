package envreconciler_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
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

func TestRenderOrderAndLabels(t *testing.T) {
	objs := envreconciler.Render(testEnv())
	if len(objs) == 0 {
		t.Fatal("no objects rendered")
	}
	wantOrder := []string{"Namespace", "NetworkPolicy", "Cluster", "Deployment", "Service", "Ingress"}
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

func TestRenderServiceShape(t *testing.T) {
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

func TestRenderNetworkPolicyDefaultDeny(t *testing.T) {
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

func TestReconcilerAppliesAndPrunes(t *testing.T) {
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

func TestReconcilerDeleteEnv(t *testing.T) {
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

func TestReconcilerFaultPropagates(t *testing.T) {
	c := k8s.NewFake()
	c.Faults.ApplyErr = errors.New("api server down")
	rc := envreconciler.New(c, envreconciler.Options{})
	if err := rc.ApplyEnv(context.Background(), testEnv()); err == nil {
		t.Fatal("want apply fault to surface")
	}
}

func TestReconcilerTickDelegates(t *testing.T) {
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
