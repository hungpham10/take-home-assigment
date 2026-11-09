package envreconciler

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

// RunNetworkPolicies renders the two run-scoped NetworkPolicies for a load-test
// run (docs/network-policy.md §3):
//
//   - egress: runner → resolved targets (pod selector + port), plus DNS/mock
//   - ingress: target ← runner (required because default-deny denies all
//     ingress)
//
// Labels use component-kind=run-policy (NOT network-policy) so the Env
// Reconciler prune and the Watcher expected-set evaluation skip them. They
// are created before the TestRun and deleted by the reaper on run cleanup.
func RunNetworkPolicies(envNS string, env *Env, runID string, targets []map[string]any) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured

	// Collect unique (service_id, port) pairs from targets.
	type targetKey struct {
		serviceID string
		port      int32
	}
	seen := map[targetKey]bool{}
	var rules []map[string]any
	for _, t := range targets {
		sid, _ := t["service_id"].(string)
		if sid == "" {
			continue
		}
		port := int32(0)
		if p, ok := t["port"].(float64); ok {
			port = int32(p)
		}
		key := targetKey{serviceID: sid, port: port}
		if seen[key] {
			continue
		}
		seen[key] = true

		to := []map[string]any{{
			"podSelector": map[string]any{
				"matchLabels": map[string]string{k8s.LabelComponentID: sid},
			},
		}}
		rule := map[string]any{"to": to}
		if port > 0 {
			rule["ports"] = []map[string]any{{"protocol": "TCP", "port": int64(port)}}
		}
		rules = append(rules, rule)
	}

	// 1. Runner egress: runner → targets + DNS.
	// DNS is always allowed so k6 can resolve service names.
	egress := append([]map[string]any{
		{
			"to": []map[string]any{{
				"namespaceSelector": map[string]any{
					"matchLabels": map[string]string{"kubernetes.io/metadata.name": "kube-system"},
				},
			}},
			"ports": []map[string]any{
				{"protocol": "UDP", "port": int64(53)},
				{"protocol": "TCP", "port": int64(53)},
			},
		},
	}, rules...)

	runnerNP := toUnstructured(map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      fmt.Sprintf("loadtest-run-egress-%s", runID),
			"namespace": envNS,
			"labels": map[string]string{
				k8s.LabelManagedBy:   k8s.ManagedByValue,
				k8s.LabelNamespaceID: env.NamespaceID,
				k8s.LabelTenantID:    env.TenantID,
				"loadtest.io/component-kind": "run-policy",
				"loadtest.io/run-id":         runID,
			},
		},
		"spec": map[string]any{
			"podSelector": map[string]any{
				"matchLabels": map[string]string{"loadtest.io/run-id": runID},
			},
			"policyTypes": []string{"Egress"},
			"egress":      egress,
		},
	}, "networking.k8s.io", "v1", "NetworkPolicy")
	objs = append(objs, runnerNP)

	// 2. Target ingress: target ← runner.
	// Without this, default-deny ingress blocks runner → SUT traffic even
	// though egress is allowed.
	// Build per-target ingress rules grouped by (service_id, port).
	ingressRules := []any{}
	seenIngress := map[targetKey]bool{}
	for _, t := range targets {
		sid, _ := t["service_id"].(string)
		if sid == "" {
			continue
		}
		port := int32(0)
		if p, ok := t["port"].(float64); ok {
			port = int32(p)
		}
		key := targetKey{serviceID: sid, port: port}
		if seenIngress[key] {
			continue
		}
		seenIngress[key] = true

		rule := map[string]any{
			"from": []any{map[string]any{
				"podSelector": map[string]any{
					"matchLabels": map[string]string{"loadtest.io/run-id": runID},
				},
			}},
		}
		if port > 0 {
			rule["ports"] = []map[string]any{{"protocol": "TCP", "port": int64(port)}}
		}
		ingressRules = append(ingressRules, rule)
	}

	targetNP := toUnstructured(map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      fmt.Sprintf("loadtest-run-ingress-%s", runID),
			"namespace": envNS,
			"labels": map[string]string{
				k8s.LabelManagedBy:   k8s.ManagedByValue,
				k8s.LabelNamespaceID: env.NamespaceID,
				k8s.LabelTenantID:    env.TenantID,
				"loadtest.io/component-kind": "run-policy",
				"loadtest.io/run-id":         runID,
			},
		},
		"spec": map[string]any{
			"podSelector": map[string]any{}, // matches all pods in ns (targets are identified by ingress rule ports)
			"policyTypes": []string{"Ingress"},
			"ingress":     ingressRules,
		},
	}, "networking.k8s.io", "v1", "NetworkPolicy")
	objs = append(objs, targetNP)

	return objs, nil
}
