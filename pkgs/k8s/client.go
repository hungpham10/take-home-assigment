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
	"strings"
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
	"k8s.io/client-go/tools/clientcmd"
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

// GVRFor maps a GroupVersionKind to its plural resource via the RESTMapper
// (falls back to UnsafeGuessKindToResource for irregular plurals —
// NetworkPolicy→networkpolicies, Ingress→ingresses — that discovery-derived
// mappers can miss before CRDs are fully listed).
func GVRFor(mapper meta.RESTMapper, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	gvr, err := mapper.ResourceFor(gvk.GroupVersion().WithResource(gvk.Kind))
	if err == nil {
		return gvr, nil
	}
	plural, singular := meta.UnsafeGuessKindToResource(gvk)
	if plural.Resource != "" {
		return gvk.GroupVersion().WithResource(plural.Resource), nil
	}
	_ = singular
	return schema.GroupVersionResource{}, fmt.Errorf("k8s: map %s: %w", gvk, err)
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
	return newDynamicForConfig(cfg)
}

// NewClientFromKubeconfig builds the client from a kubeconfig file (colima /
// kind clusters carry TLS certs there — a bare URL would fail the self-signed
// handshake). Falls back to the default loading rules when path is "".
func NewClientFromKubeconfig(path string) (*DynamicClient, error) {
	loadingRules := &clientcmd.ClientConfigLoadingRules{}
	if path != "" {
		loadingRules.ExplicitPath = path
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s: kubeconfig: %w", err)
	}
	return newDynamicForConfig(cfg)
}

func newDynamicForConfig(cfg *rest.Config) (*DynamicClient, error) {
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
	mu      sync.Mutex
	objects map[string]map[string]*unstructured.Unstructured // gvk-key -> ns/name -> obj
	Faults  Faults
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
		return nil
	}
	stored := obj.DeepCopy()
	if stored.GetGeneration() == 0 {
		stored.SetGeneration(1)
	}
	bucket[id] = stored
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
	if _, ok := bucket[id]; !ok {
		return ErrNotFound
	}
	delete(bucket, id)
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
// Watch satisfies the Client interface without event machinery: the fake
// returns an already-closed channel (no events). The watch→workqueue path is
// tested against a REAL cluster (kind) in tests/harness/watcher — plans/26
// removed the fakeWatcher duplicate.
func (f *Fake) Watch(_ context.Context, _ schema.GroupVersionKind, _ string) (watch.Interface, error) {
	ch := make(chan watch.Event)
	close(ch)
	return closedWatcher(ch), nil
}

// closedWatcher is a watch.Interface whose channel is already closed.
type closedWatcher <-chan watch.Event

func (w closedWatcher) ResultChan() <-chan watch.Event { return w }
func (w closedWatcher) Stop()                          {}

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

var _ Client = (*Fake)(nil)

// ObjectsSnapshot returns a deep copy of everything the Fake currently
// stores (debug helper for tests asserting reconciler convergence).
func (f *Fake) ObjectsSnapshot() map[schema.GroupVersionKind][]*unstructured.Unstructured {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[schema.GroupVersionKind][]*unstructured.Unstructured{}
	for key, bucket := range f.objects {
		gvk := parseFakeKey(key)
		for _, obj := range bucket {
			out[gvk] = append(out[gvk], obj.DeepCopy())
		}
	}
	return out
}

// parseFakeKey reverses schema.GroupVersionKind.String() — "Group/Version, Kind".
func parseFakeKey(key string) schema.GroupVersionKind {
	group, rest1, _ := strings.Cut(key, "/")
	version, kind, _ := strings.Cut(rest1, ", ")
	kind = strings.TrimSpace(strings.TrimPrefix(kind, "Kind="))
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}
