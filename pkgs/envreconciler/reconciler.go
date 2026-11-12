package envreconciler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

// DeleteStatusSink persists the delete terminal transition (DELETING → DELETED)
// when the reaper observes the K8s namespace is gone (plans/48). Implemented by
// the controller over store.NSToDeleted so the transition goes through
// fn_namespace_transition (single-writer rule).
type DeleteStatusSink interface {
	MarkDeleted(ctx context.Context, namespaceID, reason string) error
}

// Options configures the reconciler loop.
type Options struct {
	// Interval between level-triggered ticks (default 5s).
	Interval time.Duration
	// IsLeader gates writes when HA leader election is enabled. Nil = always
	// leader (single-pod / non-HA mode, backward compatible).
	IsLeader func() bool
	// GenerationLoader reads desired_generation from PG for the prune
	// generation check (§2.2.4 backstop). Nil = skip check.
	GenerationLoader func(ctx context.Context, namespaceID string) (int64, error)
	// DeleteSink completes DELETING → DELETED when the K8s namespace is gone
	// (delete reaper, plans/48). Nil = legacy delete-only behavior (no DB write).
	DeleteSink DeleteStatusSink
	// DeletionDeadline is how long a namespace may sit Terminating before the
	// reaper escalates with a WARN (stuck finalizer = operator intervention;
	// the reaper never removes finalizers). 0 = 10m, negative = disabled.
	DeletionDeadline time.Duration
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
	// IsLeader gates writes when HA leader election is enabled.
	isLeader func() bool
	// GenerationLoader reads desired_generation from PG for prune backstop.
	genLoader func(ctx context.Context, namespaceID string) (int64, error)
	// Delete reaper (plans/48): sink + deadline + warn-once bookkeeping.
	deleteSink       DeleteStatusSink
	deletionDeadline time.Duration
	stuckWarned      map[string]bool
}

// New wires a Reconciler over any k8s.Client (real or Fake).
func New(c k8s.Client, opts Options) *Reconciler {
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}
	deadline := 10 * time.Minute
	if opts.DeletionDeadline > 0 {
		deadline = opts.DeletionDeadline
	}
	if opts.DeletionDeadline < 0 {
		deadline = 0 // disabled
	}
	return &Reconciler{
		client:           c,
		interval:         opts.Interval,
		isLeader:         opts.IsLeader,
		genLoader:        opts.GenerationLoader,
		deleteSink:       opts.DeleteSink,
		deletionDeadline: deadline,
		stuckWarned:      map[string]bool{},
	}
}

// Attach configures the render override hook (called by mockcontrol).
func (r *Reconciler) Attach(render func(env *Env) []*unstructured.Unstructured) {
	r.RenderFunc = render
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
	// Fail closed BEFORE applying anything when a database operator is
	// unsupported (plans/26: cloudnative-pg | elastic only).
	if err := ValidateDatabaseOperators(env); err != nil {
		return err
	}
	// Hierarchical apply: Component tree (BuildComponentTree), áp DFS ngoài→trong.
	// Mock binding giờ là một Component trong cây (MockBindingComponent), không
	// còn hook BindingsFunc riêng.
	if err := ApplyTree(ctx, r.client, env); err != nil {
		return err
	}
	desired := r.render(env)
	return r.prune(ctx, env, desired)
}

// prune deletes managed objects in the env namespace whose kind+name are no
// longer part of the desired set (docs/k8s-orchestration.md §4.1 prune).
func (r *Reconciler) prune(ctx context.Context, env *Env, desired []*unstructured.Unstructured) error {
	// Generation check before prune (§2.2.4 backstop): re-read desired_generation
	// from PG; if it changed since we snapshotted env, skip prune to avoid
	// prune thrash under overlapping writers.
	if r.genLoader != nil {
		cur, err := r.genLoader(ctx, env.NamespaceID)
		if err != nil {
			return fmt.Errorf("envreconciler: load generation before prune: %w", err)
		}
		if cur != env.Generation {
			return nil
		}
	}
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
		{Group: "elasticsearch.k8s.elastic.co", Version: "v1", Kind: "Elasticsearch"},
		{Version: "v1", Kind: "ConfigMap"},
		{Version: "v1", Kind: "Secret"},
		{Version: "v1", Kind: "ServiceAccount"},
		{Group: "autoscaling", Version: "v2", Kind: "HorizontalPodAutoscaler"},
	}
	for _, gvk := range kinds {
		objs, err := r.client.List(ctx, gvk, envNS, labels.Set{k8s.LabelManagedBy: k8s.ManagedByValue})
		if err != nil {
			if err == k8s.ErrNotFound || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || meta.IsNoMatchError(err) {
				// API resource kind absent, forbidden, or not matchable on this
				// cluster (e.g. optional CRD not installed / no RBAC) — nothing
				// of that kind can be pruned safely.
				slog.Warn("envreconciler: skip prune for kind", "kind", gvk.Kind, "namespace", envNS, "err", err)
				continue
			}
			return fmt.Errorf("envreconciler: list %s for prune: %w", gvk.Kind, err)
		}
		for _, o := range objs {
			if want[gvk.Kind+"/"+o.GetName()] {
				continue
			}
			// k6-operator owns the runner Job/Service/<run>-service-<i> and
			// stamps them with loadtest.io/run-id; they are not part of the
			// env desired set and must not be pruned as "drift" (docs/
			// network-policy.md §10, plan 28 latent-bug item).
			if _, ok := o.GetLabels()["loadtest.io/run-id"]; ok {
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

// reapDeleting is the delete reaper (plans/48): level-triggered completion of
// DELETING → DELETED without trusting any event. GET the K8s namespace —
// NotFound (real state deleted, DB desynced) → MarkDeleted; Terminating →
// wait (WARN once past DeletionDeadline — stuck finalizer = operator action);
// Active → issue the delete (idempotent, retried next tick on error).
func (r *Reconciler) reapDeleting(ctx context.Context, env *Env) error {
	if r.isLeader != nil && !r.isLeader() {
		// Standby: no K8s delete, no status write — leader owns both.
		return nil
	}
	nsGVK := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	obj, err := r.client.Get(ctx, nsGVK, "", envNamespaceName(env.NamespaceID))
	if err == k8s.ErrNotFound || apierrors.IsNotFound(err) {
		// Real state deleted; DB row desynced (crash after K8s delete, lost
		// event, manual kubectl delete). Converge DB → DELETED this tick.
		if r.deleteSink != nil {
			return r.deleteSink.MarkDeleted(ctx, env.NamespaceID, "k8s namespace gone")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("envreconciler: get namespace for reaper: %w", err)
	}
	if obj.GetDeletionTimestamp() != nil {
		// Terminating: finalizers are draining. Only escalate past the deadline.
		if r.deletionDeadline > 0 && !env.DeletingSince.IsZero() &&
			time.Since(env.DeletingSince) >= r.deletionDeadline {
			if !r.stuckWarned[env.NamespaceID] {
				r.stuckWarned[env.NamespaceID] = true
				slog.Warn("envreconciler: namespace deletion deadline exceeded — stuck finalizer, operator intervention required",
					"namespace", env.NamespaceID, "deleting_since", env.DeletingSince, "deadline", r.deletionDeadline)
			}
		}
		return nil
	}
	return r.DeleteEnv(ctx, env.NamespaceID)
}

// Tick runs one level-triggered pass over the loader's envs. Start calls it
// on a timer; tests call it directly.
func (r *Reconciler) Tick(ctx context.Context, load EnvLoader) error {
	envs, err := load(ctx)
	if err != nil {
		return fmt.Errorf("envreconciler: load envs: %w", err)
	}
	for _, env := range envs {
		if env.Status == "DELETING" {
			if err := r.reapDeleting(ctx, env); err != nil {
				return err
			}
			continue
		}
		if r.isLeader != nil && !r.isLeader() {
			// Standby: không áp, không prune, để leader xử lý.
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
			// tick retries. Không nuốt lỗi — apply lỗi phải thấy được trong
			// log (bắt được bug tree apply dừng giữa chừng trên kind).
			slog.Warn("envreconciler: tick", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
