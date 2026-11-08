package envreconciler

import (
	"context"
	"fmt"
	"strconv"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

// Options configures the reconciler loop.
type Options struct {
	// Interval between level-triggered ticks (default 5s).
	Interval time.Duration
}

// Reconciler owns the desired → K8s direction. Given one Env it applies every
// rendered object in order and prunes managed objects that are no longer
// desired. It never writes status back to PG — see pkgs/statuswatcher.
type Reconciler struct {
	client   k8s.Client
	interval time.Duration
	// RenderFunc renders the desired objects (default Render). mockcontrol
	// overrides it to inject MOCK_* env into SUT deployments.
	RenderFunc func(env *Env) []*unstructured.Unstructured
	// BindingsFunc renders extra binding objects (mock binding ConfigMaps +
	// ExternalName Services, apply step 5); nil = none.
	BindingsFunc func(env *Env) []*unstructured.Unstructured
}

// New wires a Reconciler over any k8s.Client (real or Fake).
func New(c k8s.Client, opts Options) *Reconciler {
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}
	return &Reconciler{client: c, interval: opts.Interval}
}

// Attach configures the mock-wiring hooks (called by mockcontrol).
func (r *Reconciler) Attach(render, bindings func(env *Env) []*unstructured.Unstructured) {
	r.RenderFunc, r.BindingsFunc = render, bindings
}

func (r *Reconciler) render(env *Env) []*unstructured.Unstructured {
	if r.RenderFunc != nil {
		return r.RenderFunc(env)
	}
	return Render(env)
}

// Interval returns the configured tick interval.
func (r *Reconciler) Interval() time.Duration { return r.interval }

// ApplyEnv applies every rendered object in order (SSA), stamping the
// namespace's desired_generation on each object for the watcher's generation
// fencing (WATCH-03). Failure at any step surfaces immediately — the caller
// records it; the next tick re-applies (level-triggered, at-least-once).
func (r *Reconciler) ApplyEnv(ctx context.Context, env *Env) error {
	objs := r.render(env)
	if r.BindingsFunc != nil {
		objs = append(objs, r.BindingsFunc(env)...)
	}
	for _, obj := range objs {
		if env.Generation > 0 {
			ann := obj.GetAnnotations()
			if ann == nil {
				ann = map[string]string{}
			}
			ann[k8s.AnnotationDesiredGeneration] = strconv.FormatInt(env.Generation, 10)
			obj.SetAnnotations(ann)
		}
		if err := r.client.Apply(ctx, obj); err != nil {
			return fmt.Errorf("envreconciler: apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return r.prune(ctx, env, objs)
}

// prune deletes managed objects in the env namespace whose kind+name are no
// longer part of the desired set (docs/k8s-orchestration.md §4.1 prune).
func (r *Reconciler) prune(ctx context.Context, env *Env, desired []*unstructured.Unstructured) error {
	envNS := envNamespaceName(env.NamespaceID)
	want := map[string]bool{}
	for _, o := range desired {
		if o.GetKind() == "Namespace" {
			continue
		}
		want[o.GetKind()+"/"+o.GetName()] = true
	}
	kinds := []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Version: "v1", Kind: "Service"},
		{Version: "v1", Kind: "ConfigMap"},
		{Group: networkingv1.GroupName, Version: "v1", Kind: "Ingress"},
		{Group: networkingv1.GroupName, Version: "v1", Kind: "NetworkPolicy"},
		{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"},
	}
	for _, gvk := range kinds {
		objs, err := r.client.List(ctx, gvk, envNS, labels.Set{k8s.LabelManagedBy: k8s.ManagedByValue})
		if err != nil {
			return fmt.Errorf("envreconciler: list %s for prune: %w", gvk.Kind, err)
		}
		for _, o := range objs {
			if want[gvk.Kind+"/"+o.GetName()] {
				continue
			}
			if err := r.client.Delete(ctx, gvk, envNS, o.GetName()); err != nil && err != k8s.ErrNotFound {
				return fmt.Errorf("envreconciler: prune %s/%s: %w", gvk.Kind, o.GetName(), err)
			}
		}
	}
	return nil
}

// DeleteEnv removes the whole environment namespace (and thereby everything
// in it). Idempotent: a missing namespace is success (DELETING→DELETED).
func (r *Reconciler) DeleteEnv(ctx context.Context, namespaceID string) error {
	nsGVK := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	err := r.client.Delete(ctx, nsGVK, "", envNamespaceName(namespaceID))
	if err == k8s.ErrNotFound {
		return nil
	}
	return err
}

// EnvLoader fetches the envs with pending work (APPLYING/DATA_LOADING for
// apply, DELETING for teardown). DB access stays behind this interface so the
// reconciler itself is trivially testable.
type EnvLoader func(ctx context.Context) ([]*Env, error)

// Tick runs one level-triggered pass over the loader's envs. Start calls it
// on a timer; tests call it directly.
func (r *Reconciler) Tick(ctx context.Context, load EnvLoader) error {
	envs, err := load(ctx)
	if err != nil {
		return fmt.Errorf("envreconciler: load envs: %w", err)
	}
	for _, env := range envs {
		if env.Status == "DELETING" {
			if err := r.DeleteEnv(ctx, env.NamespaceID); err != nil {
				return err
			}
			continue
		}
		if err := r.ApplyEnv(ctx, env); err != nil {
			return err
		}
	}
	return nil
}

// Start runs the tick loop until ctx is cancelled.
func (r *Reconciler) Start(ctx context.Context, load EnvLoader) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		if err := r.Tick(ctx, load); err != nil {
			// Log-and-continue: level-triggered convergence means the next
			// tick retries. A dead reconciler cannot damage the cluster.
			_ = err
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
