//go:build harness_k8s

// Package harness_k8s holds the cluster-capable K8s/NetworkPolicy harness
// (plan 16 — "K8s/NetworkPolicy harness (cluster-capable, phased)"). It only
// compiles under `-tags harness_k8s` and is run manually against a cluster
// with a CNI that enforces NetworkPolicies, before any production claim about
// runner egress isolation.
package harness_k8s

import (
	"context"
	"testing"
)

// TestRunScopedEgressIsolation verifies (per plans/16-harness-testing-integrity.md):
//
// TODO(egress):
//  1. Default-deny egress: apply a default-deny NetworkPolicy to the
//     namespace of a running load test; assert the runner pod can NOT reach
//     anything outside the declared targets.
//  2. Allowed targets only: assert egress succeeds ONLY to
//     (a) the run's declared targets[] (service/mock endpoints resolved from
//     tbl_dependencies → tbl_network_policies.spec), and
//     (b) platform dependencies (DNS, mock-service, script staging) —
//     everything else (metadata API, other tenants' namespaces, node
//     endpoints) must be blocked.
//  3. No K8s/platform creds in the runner pod: exec into the runner pod and
//     assert the serviceaccount token is NOT mounted (automountServiceAccount
//     Token=false) and no LTP_/SOPS/AWS-style platform env vars or secrets
//     are present in the pod spec or filesystem.
//
// Run manually: go test -tags harness_k8s ./tests/harness/k8s/ -v
// (requires KUBECONFIG + cluster with CNI NetworkPolicy support).
func TestRunScopedEgressIsolation(t *testing.T) {
	t.Skip("requires cluster with CNI NetworkPolicy support — run manually per plans/16")

	_ = context.Background() // placeholder for the cluster-backed implementation
}
