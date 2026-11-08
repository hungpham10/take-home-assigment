// Package k8s — the narrow boundary through which the controller touches the
// Kubernetes API (docs/k8s-orchestration.md §5). Everything above this package
// (env reconciler, status watcher) programs against Client so that unit tests
// can swap in Fake with fault injection, matching the harness hooks
// (docs/harness-testing.md §4: injected errors, AlreadyExists, stale reads).
//
// The real implementation uses the client-go dynamic client with unstructured
// objects and Server-Side Apply (field manager loadtest-controller) — the same
// pattern docs/k6-operator-go-client.md recommends for TestRun. CNPG Cluster
// CRs go through the same dynamic path (GVR postgresql.cnpg.io/v1, clusters) —
// no operator Go types, no heavy dependency.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// ErrNotFound is returned by Get/Delete when the object does not exist. The
// adapter normalizes apierrors.IsNotFound into this sentinel so callers do not
// need client-go error types.
var ErrNotFound = errors.New("k8s: object not found")

// FieldManager is the SSA field manager ownership identity of this controller
// (docs/k8s-orchestration.md §4.1 — all applies share one manager so prune
// semantics stay predictable).
const FieldManager = "loadtest-controller"

// Well-known labels (docs/k8s-orchestration.md §1). The env reconciler stamps
// them on every object it applies; the status watcher filters on them.
const (
	LabelManagedBy     = "loadtest.io/managed-by"
	LabelTenantID      = "loadtest.io/tenant-id"
	LabelNamespaceID   = "loadtest.io/namespace-id"
	LabelComponentID   = "loadtest.io/component-id"
	AnnotationSpecHash = "loadtest.io/spec-hash"
	// AnnotationDesiredGeneration carries the namespace's desired_generation
	// the reconciler applied (watcher generation fencing, WATCH-03).
	AnnotationDesiredGeneration = "loadtest.io/desired-generation"
)

// ManagedByValue is the label value every controller-owned object carries.
const ManagedByValue = "loadtest-controller"

// Client is the only K8s surface the controller may use.
type Client interface {
	// Apply server-side-applies obj (field manager FieldManager, force).
	Apply(ctx context.Context, obj *unstructured.Unstructured) error
	Get(ctx context.Context, gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error)
	Delete(ctx context.Context, gvk schema.GroupVersionKind, ns, name string) error
	List(ctx context.Context, gvk schema.GroupVersionKind, ns string, sel labels.Set) ([]*unstructured.Unstructured, error)
	Watch(ctx context.Context, gvk schema.GroupVersionKind, ns string) (watch.Interface, error)
}

// GVRFor maps a GroupVersionKind to the resource plural name via a RESTMapper.
// Kept package-level so Fake-free callers (and tests) share one mapping table.
func GVRFor(mapper meta.RESTMapper, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return mapper.ResourceFor(gvk.GroupVersion().WithResource(gvk.Kind + "s"))
}

// DynamicClient is the real Client backed by discovery + dynamic client-go.
type DynamicClient struct {
	dyn    dynamic.Interface
	disc   discovery.DiscoveryInterface
	mapper meta.RESTMapper
	mu     sync.Mutex
}

// NewDynamicClient builds the client from an API server URL (in-cluster
// config takes precedence when running inside a pod via rest.InClusterConfig
// at the call site).
func NewDynamicClient(serverURL string, _ string) (*DynamicClient, error) {
	cfg := &rest.Config{Host: serverURL}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: discovery client: %w", err)
	}
	return &DynamicClient{dyn: dyn, disc: disc}, nil
}

// mapper lazily builds the RESTMapper (group resources change when CRDs land).
func (c *DynamicClient) mapperFor() (meta.RESTMapper, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mapper != nil {
		return c.mapper, nil
	}
	grp, err := restmapper.GetAPIGroupResources(c.disc)
	if err != nil {
		return nil, fmt.Errorf("k8s: discovery: %w", err)
	}
	c.mapper = restmapper.NewDiscoveryRESTMapper(grp)
	return c.mapper, nil
}

func (c *DynamicClient) resource(gvk schema.GroupVersionKind, ns string) (dynamic.ResourceInterface, error) {
	mapper, err := c.mapperFor()
	if err != nil {
		return nil, err
	}
	gvr, err := GVRFor(mapper, gvk)
	if err != nil {
		return nil, fmt.Errorf("k8s: map %s: %w", gvk, err)
	}
	if ns == "" {
		return c.dyn.Resource(gvr), nil
	}
	return c.dyn.Resource(gvr).Namespace(ns), nil
}

// Apply server-side-applies obj with FieldManager and Force.
func (c *DynamicClient) Apply(ctx context.Context, obj *unstructured.Unstructured) error {
	ns := obj.GetNamespace()
	ri, err := c.resource(obj.GroupVersionKind(), ns)
	if err != nil {
		return err
	}
	_, err = ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
		FieldManager: FieldManager, Force: true,
	})
	return normalize(err)
}

// Get returns the object by gvk/ns/name (ErrNotFound when missing).
func (c *DynamicClient) Get(ctx context.Context, gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error) {
	ri, err := c.resource(gvk, ns)
	if err != nil {
		return nil, err
	}
	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	return obj, normalize(err)
}

// Delete removes the object; missing objects normalize to ErrNotFound.
func (c *DynamicClient) Delete(ctx context.Context, gvk schema.GroupVersionKind, ns, name string) error {
	ri, err := c.resource(gvk, ns)
	if err != nil {
		return err
	}
	var graceZero int64
	opts := metav1.DeleteOptions{GracePeriodSeconds: &graceZero}
	return normalize(ri.Delete(ctx, name, opts))
}

// List returns objects of the kind in ns matching the label set.
func (c *DynamicClient) List(ctx context.Context, gvk schema.GroupVersionKind, ns string, sel labels.Set) ([]*unstructured.Unstructured, error) {
	ri, err := c.resource(gvk, ns)
	if err != nil {
		return nil, err
	}
	opts := metav1.ListOptions{LabelSelector: sel.AsSelector().String()}
	list, err := ri.List(ctx, opts)
	if err != nil {
		return nil, normalize(err)
	}
	out := make([]*unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out, nil
}

// Watch streams events for the kind filtered on managed-by.
func (c *DynamicClient) Watch(ctx context.Context, gvk schema.GroupVersionKind, ns string) (watch.Interface, error) {
	ri, err := c.resource(gvk, ns)
	if err != nil {
		return nil, err
	}
	opts := metav1.ListOptions{
		LabelSelector: labels.Set{LabelManagedBy: ManagedByValue}.AsSelector().String(),
	}
	return ri.Watch(ctx, opts)
}

func normalize(err error) error {
	if err == nil {
		return nil
	}
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

var _ Client = (*DynamicClient)(nil)

// Fake is the in-memory Client for unit tests and the harness fault-injection
// hooks (RUN-NET-01 transient errors, RUN-IDEM-01 AlreadyExists, stale reads).
// Apply is idempotent (merge-by-name, last writer wins for the fields it sets)
// and emits Added/Modified events on every registered Watch.
type Fake struct {
	mu       sync.Mutex
	objects  map[string]map[string]*unstructured.Unstructured // gvk-key -> ns/name -> obj
	watchers []*fakeWatcher
	Faults   Faults
}

// Faults injects errors per operation; nil means "succeed".
type Faults struct {
	ApplyErr  error
	GetErr    error
	ListErr   error
	DeleteErr error
	WatchErr  error
	// ApplyDelay sleeps before applying (timeout/retry tests).
	ApplyDelay time.Duration
}

// NewFake returns an empty Fake client.
func NewFake() *Fake {
	return &Fake{objects: map[string]map[string]*unstructured.Unstructured{}}
}

func fakeKey(gvk schema.GroupVersionKind) string {
	return gvk.String()
}

// Apply merges obj into the stored copy (SSA-like), stamping generation 1
// on first write and emitting Added/Modified to matching watchers.
func (f *Fake) Apply(ctx context.Context, obj *unstructured.Unstructured) error {
	if f.Faults.ApplyDelay > 0 {
		select {
		case <-time.After(f.Faults.ApplyDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.Faults.ApplyErr != nil {
		return f.Faults.ApplyErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fakeKey(obj.GroupVersionKind())
	bucket, ok := f.objects[key]
	if !ok {
		bucket = map[string]*unstructured.Unstructured{}
		f.objects[key] = bucket
	}
	id := obj.GetNamespace() + "/" + obj.GetName()
	if prev, exists := bucket[id]; exists {
		// SSA merge: last writer wins per field set; generation is server-managed.
		merged := prev.DeepCopy()
		merged.SetLabels(mergeLabels(prev.GetLabels(), obj.GetLabels()))
		merged.SetAnnotations(mergeLabels(prev.GetAnnotations(), obj.GetAnnotations()))
		// SSA field-manager ownership covers spec; the API server keeps
		// subresource status from the previous write unless the apply sets it
		// (client-go ApplyStatus) — mirror that: new status replaces, absent
		// status keeps the old one.
		if st, has := obj.UnstructuredContent()["status"]; has {
			merged.UnstructuredContent()["status"] = st
		}
		merged.UnstructuredContent()["spec"] = obj.UnstructuredContent()["spec"]
		bucket[id] = merged
		f.emitLocked(watch.Modified, merged)
		return nil
	}
	stored := obj.DeepCopy()
	if stored.GetGeneration() == 0 {
		stored.SetGeneration(1)
	}
	bucket[id] = stored
	f.emitLocked(watch.Added, stored)
	return nil
}

// Get returns a deep copy or ErrNotFound.
func (f *Fake) Get(_ context.Context, gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error) {
	if f.Faults.GetErr != nil {
		return nil, f.Faults.GetErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if obj, ok := f.objects[fakeKey(gvk)][ns+"/"+name]; ok {
		return obj.DeepCopy(), nil
	}
	return nil, ErrNotFound
}

// Delete removes the object and emits Deleted; ErrNotFound when missing.
func (f *Fake) Delete(_ context.Context, gvk schema.GroupVersionKind, ns, name string) error {
	if f.Faults.DeleteErr != nil {
		return f.Faults.DeleteErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket := f.objects[fakeKey(gvk)]
	id := ns + "/" + name
	obj, ok := bucket[id]
	if !ok {
		return ErrNotFound
	}
	delete(bucket, id)
	f.emitLocked(watch.Deleted, obj)
	return nil
}

// List returns deep copies matching ns and label selector.
func (f *Fake) List(_ context.Context, gvk schema.GroupVersionKind, ns string, sel labels.Set) ([]*unstructured.Unstructured, error) {
	if f.Faults.ListErr != nil {
		return nil, f.Faults.ListErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*unstructured.Unstructured
	selector := labels.Set(nil).AsSelector()
	if sel != nil {
		selector = sel.AsSelector()
	}
	for _, obj := range f.objects[fakeKey(gvk)] {
		if obj.GetNamespace() != ns {
			continue
		}
		if !selector.Matches(labels.Set(obj.GetLabels())) {
			continue
		}
		out = append(out, obj.DeepCopy())
	}
	return out, nil
}

// Watch registers a watcher and replays current state as Added events
// (startup relist); the watcher stops when ctx is cancelled.
func (f *Fake) Watch(ctx context.Context, gvk schema.GroupVersionKind, ns string) (watch.Interface, error) {
	if f.Faults.WatchErr != nil {
		return nil, f.Faults.WatchErr
	}
	f.mu.Lock()
	fw := newFakeWatcher()
	fw.adopt(ctx)
	fw.key, fw.namespace = fakeKey(gvk), ns
	f.watchers = append(f.watchers, fw)
	// Startup relist: replay current state as Added events (namespace-status-watcher.md §3).
	for id, obj := range f.objects[fakeKey(gvk)] {
		if ns != "" && splitNS(id) != ns {
			continue
		}
		fw.ch <- watch.Event{Type: watch.Added, Object: obj.DeepCopy()}
	}
	f.mu.Unlock()
	return fw, nil
}

func splitNS(id string) string {
	for i := 0; i < len(id); i++ {
		if id[i] == '/' {
			return id[:i]
		}
	}
	return ""
}

func mergeLabels(prev, next map[string]string) map[string]string {
	if prev == nil && next == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range prev {
		out[k] = v
	}
	for k, v := range next {
		out[k] = v
	}
	return out
}

// emitLocked fans an event out to every matching watcher. Caller holds f.mu.
func (f *Fake) emitLocked(t watch.EventType, obj *unstructured.Unstructured) {
	for _, w := range f.watchers {
		if w.key != fakeKey(obj.GroupVersionKind()) {
			continue
		}
		if w.namespace != "" && w.namespace != obj.GetNamespace() {
			continue
		}
		w.ch <- watch.Event{Type: t, Object: obj.DeepCopy()}
	}
}

var _ Client = (*Fake)(nil)
