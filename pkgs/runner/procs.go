package runner

import (
	"bytes"
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
	// out receives the child's stdout+stderr (see Local.Start). Written by
	// os/exec's copy goroutines while the process runs; only read after reap()
	// has returned, which is when those goroutines are done (that is what makes
	// output() race-free).
	out *bytes.Buffer
}

// output returns a copy of the captured tail (never while the process runs).
func (p *proc) output() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.out == nil {
		return nil
	}
	b := make([]byte, len(p.out.Bytes()))
	copy(b, p.out.Bytes())
	return b
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
