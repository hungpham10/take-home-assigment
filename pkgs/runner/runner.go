// Package runner abstracts the load-test execution backend. Two
// implementations share the interface:
//
//   - Local: spawns the k6 binary on the host (MVP default, RUNNER_MODE=local)
//   - K8s:   creates k6-operator `TestRun` CRs via the Go K8s API
//     (RUNNER_MODE=k8s, wired in a follow-up — see docs/k6-operator-go-client.md)
package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Spec is one executable load test.
type Spec struct {
	ID          string
	ScriptPath  string // staged k6 script (local) / content ref (k8s)
	Script      []byte // script content
	VUs         int    // virtual users
	Duration    string // e.g. "10m"
	TargetsJSON []byte // targets[] for egress checks
	Env         map[string]string
}

// Status is the runner's view of a running test.
type Status struct {
	State string // PROVISIONING | RUNNING | COMPLETED | FAILED | STOPPED
	Msg   string
	// Missing is true when the ref is unknown to the runner (e.g. after a
	// controller restart lost the in-process table). The reconciler treats
	// Missing as "runner lost the run" → FAILED, not a normal STOPPED.
	Missing bool
}

// Runner executes load tests.
type Runner interface {
	// Provision prepares the execution (returns a backend ref).
	Provision(ctx context.Context, s Spec) (ref string, err error)
	// Start launches the run; non-blocking.
	Start(ctx context.Context, ref string, s Spec) error
	// Stop terminates a running test.
	Stop(ctx context.Context, ref string) error
	// Status polls the backend state.
	Status(ctx context.Context, ref string) (Status, error)
}

// Local runs k6 as an OS process (per test). Suitable for MVP/dev only —
// K8s mode isolates runners in environment namespaces (ADR-002).
type Local struct {
	Bin   string // k6 binary (default "k6")
	procs *procTable
}

// NewLocal returns a Local runner using the given k6 binary ("" = "k6").
func NewLocal(bin string) *Local {
	if bin == "" {
		bin = "k6"
	}
	return &Local{Bin: bin, procs: newProcTable()}
}

// Mode reports the backend mode for tbl_load_tests.runner_type.
func (l *Local) Mode() string { return "local" }

// Provision prepares a local run ref.
func (l *Local) Provision(_ context.Context, _ Spec) (string, error) {
	return fmt.Sprintf("local-%d", time.Now().UnixNano()), nil
}

// Start launches the k6 process.
func (l *Local) Start(ctx context.Context, ref string, s Spec) error {
	tmp, err := os.CreateTemp("", "ltp-run-*.js")
	if err != nil {
		return fmt.Errorf("runner: temp script: %w", err)
	}
	if _, err := tmp.Write(s.Script); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("runner: write script: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, l.Bin, "run",
		"--vus", fmt.Sprint(maxInt(s.VUs, 1)),
		"--duration", s.Duration,
		"--quiet", tmp.Name())
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("runner: k6 start: %w", err)
	}
	l.procs.add(ref, &proc{cmd: cmd, script: tmp.Name()})
	return nil
}

// Stop terminates the k6 process (idempotent).
func (l *Local) Stop(_ context.Context, ref string) error {
	p, ok := l.procs.get(ref)
	if !ok {
		return nil // already gone — idempotent stop
	}
	_ = p.cmd.Process.Kill() // executor interface kept for tests via procs table
	l.procs.remove(ref)
	return nil
}

// Status polls the process state with a short grace window.
func (l *Local) Status(_ context.Context, ref string) (Status, error) {
	p, ok := l.procs.get(ref)
	if !ok {
		return Status{State: "STOPPED", Msg: "no such run", Missing: true}, nil
	}
	// Short grace window: a just-exited process should be observed on the
	// same poll (reconcilers poll at seconds, so 200ms is invisible). reap
	// is idempotent — concurrent polls never double-Wait the process.
	exited := make(chan struct{})
	go func() {
		_, _ = p.reap() // waitErr read below after exited closes
		close(exited)
	}()
	select {
	case <-exited:
		l.procs.remove(ref)
		_ = os.Remove(p.script)
		if p.waitErr != nil {
			return Status{State: "FAILED", Msg: p.waitErr.Error()}, nil
		}
		return Status{State: "COMPLETED"}, nil
	case <-time.After(200 * time.Millisecond):
		return Status{State: "RUNNING"}, nil
	}
}
