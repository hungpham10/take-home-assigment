package statuswatcher

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/workqueue"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

// Queue is the dedup/rate-limited workqueue of namespace keys (docs/
// namespace-status-watcher.md §3: events only enqueue key=namespace_id; the
// worker recomputes everything level-triggered — event payloads are never
// trusted). Implemented over client-go's workqueue so Add/Enqueue coalesce
// and Pop blocks until work exists.
type Queue struct {
	q workqueue.TypedRateLimitingInterface[any]
}

// NewQueue returns a dedup + rate-limited queue (default item exponential
// rate limiter backs off repeated failures).
func NewQueue() *Queue {
	return &Queue{q: workqueue.NewTypedRateLimitingQueue[any](workqueue.DefaultTypedControllerRateLimiter[any]())}
}

// KeyOf extracts the enqueue key from a watched object: the namespace-id
// label. Objects without the label are not watcher-managed → "" (ignored by
// Enqueue).
func KeyOf(obj interface{ GetLabels() map[string]string }) string {
	return obj.GetLabels()[k8s.LabelNamespaceID]
}

// Enqueue adds a namespace key (empty keys are ignored).
func (q *Queue) Enqueue(namespaceID string) {
	if namespaceID == "" {
		return
	}
	q.q.Add(namespaceID)
}

// AddAfter re-queues a key after a delay (reconnects, retry backoff).
func (q *Queue) AddAfter(namespaceID string, d time.Duration) {
	if namespaceID == "" {
		return
	}
	q.q.AddAfter(namespaceID, d)
}

// Pop blocks until an item is available or the queue shuts down (Start
// shuts it down on ctx cancel — Pop itself does not select on ctx because
// client-go's workqueue Get blocks; shutdown unblocks it).
func (q *Queue) Pop(_ context.Context) (string, bool) {
	// Get blocks until an item exists or the queue shuts down (Start shuts
	// it down on ctx cancel — the second return value is shutdown, not ok).
	key, shutdown := q.q.Get()
	if shutdown {
		return "", false
	}
	q.q.Done(key)
	return keyToString(key), true
}

// Len reports pending (deduped) keys.
func (q *Queue) Len() int { return q.q.Len() }

func keyToString(k any) string {
	s, _ := k.(string)
	return s
}

// watchGVKs are the resource kinds whose events trigger status recomputes.
var watchGVKs = []gvkSpec{
	{group: "apps", version: "v1", kind: "Deployment"},
	{group: "postgresql.cnpg.io", version: "v1", kind: "Cluster"},
	{group: "networking.k8s.io", version: "v1", kind: "Ingress"},
	{group: "networking.k8s.io", version: "v1", kind: "NetworkPolicy"},
	{version: "v1", kind: "ConfigMap"},
	{version: "v1", kind: "Service"},
}

type gvkSpec struct {
	group, version, kind string
}

func (g gvkSpec) toGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: g.group, Version: g.version, Kind: g.kind}
}

// runWatches consumes k8s.Client.Watch for every GVK, feeding the queue.
// Each consumer reconnects with backoff when its watch channel closes.
func (w *Watcher) runWatches(ctx context.Context, q *Queue) {
	for _, spec := range watchGVKs {
		go w.consumeWatch(ctx, spec.toGVK(), q)
	}
}

// consumeWatch loops one GVK: watch → events → enqueue namespace-id; on
// channel close (network drop) reconnect after a short backoff. Cancelled
// context stops the loop.
func (w *Watcher) consumeWatch(ctx context.Context, gvk schema.GroupVersionKind, q *Queue) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		wi, err := w.client.Watch(ctx, gvk, "")
		if err != nil {
			logIfNotCancelled(ctx, "watch", gvk, err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = minDuration(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		for ev := range wi.ResultChan() {
			if ctx.Err() != nil {
				wi.Stop()
				return
			}
			obj, ok := ev.Object.(interface{ GetLabels() map[string]string })
			if !ok {
				continue
			}
			if obj.GetLabels()[k8s.LabelManagedBy] != k8s.ManagedByValue {
				continue // not managed by this controller
			}
			if key := KeyOf(obj); key != "" {
				q.Enqueue(key)
			}
		}
		// Channel closed — reconnect (startup relist replays via resync).
	}
}

func logIfNotCancelled(ctx context.Context, what string, gvk schema.GroupVersionKind, err error) {
	if ctx.Err() != nil || strings.Contains(err.Error(), "context canceled") {
		return
	}
	slog.Warn("statuswatcher: watch failed", "what", what, "gvk", gvk.Kind, "err", err)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
