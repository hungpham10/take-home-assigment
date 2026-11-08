package k8s_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var deployGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

func deploy(name string, lbls map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(deployGVK)
	obj.SetNamespace("lt-env-ns1")
	obj.SetName(name)
	obj.SetLabels(lbls)
	obj.SetGeneration(1)
	return obj
}

func TestFakeApplyGet(t *testing.T) {
	c := k8s.NewFake()
	ctx := context.Background()

	if err := c.Apply(ctx, deploy("svc-a", map[string]string{"loadtest.io/namespace-id": "ns1"})); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := c.Get(ctx, deployGVK, "lt-env-ns1", "svc-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GetName() != "svc-a" || got.GetNamespace() != "lt-env-ns1" {
		t.Fatalf("unexpected object %s/%s", got.GetNamespace(), got.GetName())
	}
}

// SSA semantics in the fake: a second Apply by the same field manager merges
// (last-writer-wins for the fields it sets), it must not error AlreadyExists.
func TestFakeApplyIsIdempotent(t *testing.T) {
	c := k8s.NewFake()
	ctx := context.Background()
	if err := c.Apply(ctx, deploy("svc-a", nil)); err != nil {
		t.Fatalf("apply 1: %v", err)
	}
	obj := deploy("svc-a", map[string]string{"loadtest.io/spec-hash": "abc"})
	if err := c.Apply(ctx, obj); err != nil {
		t.Fatalf("apply 2: %v", err)
	}
	got, _ := c.Get(ctx, deployGVK, "lt-env-ns1", "svc-a")
	if got.GetLabels()["loadtest.io/spec-hash"] != "abc" {
		t.Fatalf("second apply did not merge labels: %v", got.GetLabels())
	}
	if got.GetGeneration() != 1 {
		t.Fatalf("generation mutated: %d", got.GetGeneration())
	}
}

func TestFakeDeleteList(t *testing.T) {
	c := k8s.NewFake()
	ctx := context.Background()
	_ = c.Apply(ctx, deploy("svc-a", map[string]string{"loadtest.io/managed-by": "loadtest-controller"}))
	_ = c.Apply(ctx, deploy("svc-b", map[string]string{"loadtest.io/managed-by": "other"}))
	_ = c.Apply(ctx, deploy("svc-c", nil))

	got, err := c.List(ctx, deployGVK, "lt-env-ns1",
		labels.Set{"loadtest.io/managed-by": "loadtest-controller"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].GetName() != "svc-a" {
		t.Fatalf("label selector matched %d objects, want [svc-a]", len(got))
	}

	if err := c.Delete(ctx, deployGVK, "lt-env-ns1", "svc-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Get(ctx, deployGVK, "lt-env-ns1", "svc-a"); !errors.Is(err, k8s.ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}
	if err := c.Delete(ctx, deployGVK, "lt-env-ns1", "svc-a"); !errors.Is(err, k8s.ErrNotFound) {
		t.Fatalf("delete missing: %v, want ErrNotFound", err)
	}
}

func TestFakeFaultInjection(t *testing.T) {
	c := k8s.NewFake()
	ctx := context.Background()

	c.Faults.ApplyErr = errors.New("transient")
	if err := c.Apply(ctx, deploy("svc-a", nil)); err == nil {
		t.Fatal("want injected apply error")
	}
	c.Faults.ApplyErr = nil

	c.Faults.GetErr = k8s.ErrNotFound
	if _, err := c.Get(ctx, deployGVK, "lt-env-ns1", "svc-a"); !errors.Is(err, k8s.ErrNotFound) {
		t.Fatalf("want injected not-found, got %v", err)
	}
}

func TestFakeIsolationByNamespace(t *testing.T) {
	c := k8s.NewFake()
	ctx := context.Background()
	_ = c.Apply(ctx, deploy("svc-a", nil))
	if _, err := c.Get(ctx, deployGVK, "other-ns", "svc-a"); !errors.Is(err, k8s.ErrNotFound) {
		t.Fatalf("cross-namespace leak: %v", err)
	}
}

// The dynamic constructor builds lazily; connection/discovery errors must
// surface on the first call, not panic.
func TestNewDynamicClientUnreachable(t *testing.T) {
	c, err := k8s.NewDynamicClient("http://127.0.0.1:1", "loadtest-controller")
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	_, err = c.Get(context.Background(), deployGVK, "ns", "x")
	if err == nil {
		t.Fatal("want discovery/connection error on first call")
	}
}

func TestKindToVersionKind(t *testing.T) {
	gvk := corev1.SchemeGroupVersion.WithKind("Service")
	if gvk.Kind != "Service" || gvk.Version != "v1" {
		t.Fatalf("unexpected gvk %v", gvk)
	}
	_ = metav1.ObjectMeta{}
}
