package runner

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestLocalLifecycleWithRealProcess(t *testing.T) {
	l := NewLocal("k6-nonexistent") // Start would fail on binary; drive procs directly
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Skipf("cannot start sleep process: %v", err)
	}
	t.Cleanup(func() { _ = sleep.Process.Kill() })
	l.procs.add("ref1", &proc{cmd: sleep, script: ""})

	st, err := l.Status(context.Background(), "ref1")
	if err != nil || st.State != "RUNNING" {
		t.Fatalf("status = (%+v, %v), want RUNNING", st, err)
	}

	// Kill → Status polls until the reaper observes the exit.
	_ = sleep.Process.Kill()
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, _ = l.Status(context.Background(), "ref1")
		if st.State != "RUNNING" || time.Now().After(deadline) {
			break
		}
	}
	if st.State != "STOPPED" && st.State != "FAILED" {
		t.Errorf("status after kill = %s, want STOPPED|FAILED", st.State)
	}
}

func TestLocalStopIdempotent(t *testing.T) {
	l := NewLocal("k6-nonexistent")
	if err := l.Stop(context.Background(), "missing-ref"); err != nil {
		t.Errorf("stop of unknown ref must be nil, got %v", err)
	}
}

func TestLocalProvisionUniqueRefs(t *testing.T) {
	l := NewLocal("")
	ctx := context.Background()
	r1, _ := l.Provision(ctx, Spec{})
	time.Sleep(2 * time.Millisecond)
	r2, _ := l.Provision(ctx, Spec{})
	if r1 == r2 {
		t.Errorf("refs must be unique: %s", r1)
	}
}
