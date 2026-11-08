package statuswatcher_test

import (
	"context"
	"testing"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/statuswatcher"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var queueDeployGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

// eventObject builds an unstructured carrying the watcher's ownership labels.
func eventObject(namespaceID string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(queueDeployGVK)
	obj.SetNamespace("lt-env-x")
	obj.SetName("d")
	lbls := map[string]string{k8s.LabelManagedBy: k8s.ManagedByValue}
	if namespaceID != "" {
		lbls[k8s.LabelNamespaceID] = namespaceID
	}
	obj.SetLabels(lbls)
	return obj
}

// WATCH-01/02 primitives at the event-source level: labeled events enqueue
// exactly one deduped key; duplicate events collapse; unlabeled ones ignored.
func TestQueueDispatchesNamespaceKeys(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), nsID: "11111111-2222-3333-4444-555555555555", envNS: ""}

	wq := statuswatcher.NewQueue()
	wq.Enqueue(statuswatcher.KeyOf(eventObject(w.nsID))) // labeled → ns id
	wq.Enqueue(statuswatcher.KeyOf(eventObject(w.nsID))) // duplicate → dedup
	wq.Enqueue(statuswatcher.KeyOf(eventObject("")))     // unlabeled → ignored

	keys := map[string]int{}
	for wq.Len() > 0 {
		key, ok := wq.Pop(context.Background())
		if !ok {
			break
		}
		keys[key]++
	}
	if keys[w.nsID] != 1 {
		t.Fatalf("keys %v, want exactly one for %s (dedup + unlabeled ignored)", keys, w.nsID)
	}
}

// Queue.AddAfter re-queues a key after a delay (rate-limiting primitive used
// for watch reconnects and retry backoff).
func TestQueueAddAfter(t *testing.T) {
	wq := statuswatcher.NewQueue()
	wq.AddAfter("ns-1", 20*time.Millisecond)
	deadline := time.Now().Add(1 * time.Second)
	for wq.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	key, ok := wq.Pop(context.Background())
	if !ok || key != "ns-1" {
		t.Fatalf("pop %q ok=%v, want ns-1", key, ok)
	}
}

// WATCH-01: even with NO events, the resync loop converges the namespace to
// the correct status (level-triggered safety net).
func TestResyncConvergesWithoutEvents(t *testing.T) {
	w := &world{db: gormDB(t), client: k8s.NewFake(), envNS: ""}
	w.nsID = seedNamespace(t, w.db, "APPLYING")
	w.envNS = envreconciler.EnvNamespaceName(w.nsID)
	applyEnv(t, w, 1)
	markDeploymentReady(t, w)

	cfg := statuswatcher.DefaultConfig()
	cfg.Resync = 10 * time.Millisecond
	watcher := newWatcher(w, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		_ = w.db.Raw(`SELECT status FROM tbl_namespaces WHERE id = ?`, w.nsID).Scan(&status)
		if status == "READY" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("resync loop did not converge to READY (WATCH-01)")
}
