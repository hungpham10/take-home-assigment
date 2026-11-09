package main

import (
	"fmt"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/runner"
)

// newRunner picks the execution backend from RUNNER_MODE (config):
// k8s -> k6-operator TestRun client; local -> host k6 binary.
// k8s mode is fail-closed: it requires a non-nil k8s.Client, otherwise
// newRunner errors so the controller does not silently fall back to local
// execution in a cluster deployment.
func newRunner(mode, k6Bin string, k8sClient k8s.Client) (runner.Runner, error) {
	switch mode {
	case "k8s":
		if k8sClient == nil {
			return nil, fmt.Errorf("RUNNER_MODE=k8s requires a k8s client (K8S_ENABLED must be true)")
		}
		return runner.NewK8sTestRunRunner(k8sClient), nil
	case "local":
		return runner.NewLocal(k6Bin), nil
	default:
		return nil, fmt.Errorf("unknown RUNNER_MODE %q: must be local or k8s", mode)
	}
}
