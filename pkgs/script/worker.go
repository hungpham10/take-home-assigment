package script

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Queue is a bounded in-process validation queue with a fixed worker pool.
// MVP-local; the K8s variant (k6 inspect Job) plugs behind the same interface
// in plan 13.
type Queue struct {
	ch      chan string
	workers int
}

// NewQueue starts n workers consuming script IDs from a bounded channel.
func NewQueue(ctx context.Context, db *gorm.DB, scriptsDir string, workers, buffer int) *Queue {
	q := &Queue{ch: make(chan string, buffer), workers: workers}
	for i := 0; i < workers; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-q.ch:
					if err := ValidateNow(ctx, db, scriptsDir, id); err != nil {
						slog.Error("script validation failed", "script", id, "err", err)
					}
				}
			}
		}()
	}
	return q
}

// Submit enqueues a script ID; drops (with a log) when full — the client can
// re-trigger validation via POST /{id}/validate.
func (q *Queue) Submit(scriptID string) {
	select {
	case q.ch <- scriptID:
	default:
		slog.Warn("validation queue full, dropping", "script", scriptID)
	}
}

// --- small fs helpers (kept local: no package sprawl for 3 one-liners) ---

func scriptsJoin(dir, ref string) string {
	clean := filepath.Clean(ref)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return ""
	}
	return filepath.Join(dir, clean)
}

func writeFileTemp(content []byte) (string, error) {
	f, err := os.CreateTemp("", "ltp-script-*.js")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(content); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func removeFile(path string) error { return os.Remove(path) }

var _ = store.Script{} // models referenced via store package
