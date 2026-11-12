package runner

import (
	"os/exec"
	"sync"
)

// proc tracks one k6 process; waitOnce guarantees cmd.Wait() is called
// exactly once (os/exec Wait is not concurrency-safe) and caches the result
// for subsequent Status polls.
type proc struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	script  string
	waitErr error
	waited  bool
}

// reap waits (once) for the process to exit and returns (exited, waitErr).
func (p *proc) reap() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.waited {
		p.waitErr = p.cmd.Wait()
		p.waited = true
	}
	return p.waited, p.waitErr
}

type procTable struct {
	mu    sync.Mutex
	procs map[string]*proc
}

func newProcTable() *procTable {
	return &procTable{procs: map[string]*proc{}}
}

func (t *procTable) add(ref string, p *proc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.procs[ref] = p
}

func (t *procTable) get(ref string) (*proc, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.procs[ref]
	return p, ok
}

func (t *procTable) remove(ref string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.procs, ref)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
