package main

import "github.com/hungpham/loadtest-platform/pkgs/runner"

// newRunner picks the execution backend from RUNNER_MODE (config):
// k8s → k6-operator TestRun client (wired in a follow-up), local → host k6.
// newRunner picks the execution backend from RUNNER_MODE (config):
// k8s → k6-operator TestRun client (wired in a follow-up), local → host k6.
func newRunner(mode, k6Bin string) runner.Runner {
	_ = mode // k8s client wiring lands with the k8s integration plan
	return runner.NewLocal(k6Bin)
}
