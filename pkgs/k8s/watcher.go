package k8s

import (
	"context"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

// fakeWatcher is a watch.Interface fed by Fake and closed when its context is
// cancelled (unblocks the fan-out by draining the channel).
type fakeWatcher struct {
	ctx       context.Context
	cancel    context.CancelFunc
	key       string
	namespace string
	ch        chan watch.Event
	once      sync.Once
}

func newFakeWatcher() *fakeWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	fw := &fakeWatcher{ctx: ctx, cancel: cancel, ch: make(chan watch.Event, 64)}
	go func() {
		<-ctx.Done()
		fw.once.Do(func() { close(fw.ch) })
	}()
	return fw
}

func (fw *fakeWatcher) ResultChan() <-chan watch.Event { return fw.ch }

// adopt ties the watcher to the caller's context: cancelling it stops the
// watcher exactly as if Stop had been called.
func (fw *fakeWatcher) adopt(ctx context.Context) {
	if ctx == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			fw.Stop()
		case <-fw.ctx.Done():
		}
	}()
}

func (fw *fakeWatcher) Stop() { fw.cancel() }

var _ watch.Interface = (*fakeWatcher)(nil)

// parseFakeKey reverses schema.GroupVersionKind.String() — "Group/Version, Kind".
func parseFakeKey(key string) schema.GroupVersionKind {
	group, rest1, _ := strings.Cut(key, "/")
	version, kind, _ := strings.Cut(rest1, ", ")
	kind = strings.TrimSpace(strings.TrimPrefix(kind, "Kind="))
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

// ObjectsSnapshot returns a deep copy of everything the Fake currently stores
// (debug helper for tests asserting reconciler convergence).
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
