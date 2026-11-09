package envreconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

// k6OperatorNamespace is the namespace k6-operator controller pods run in.
// StartJobs probes http://<runner-svc>:6565/v1/status from this namespace, so
// the run-scoped "api" NetworkPolicy must allow ingress from it. Override via
// K6_OPERATOR_NAMESPACE when k6-operator is installed elsewhere.
var k6OperatorNamespace = func() string {
	if v := os.Getenv("K6_OPERATOR_NAMESPACE"); v != "" {
		return v
	}
	return "k6-operator-system"
}()

// RunNetworkPolicies renders the run-scoped NetworkPolicies for a load-test
// run (docs/network-policy.md §3):
//
//   - egress: runner → resolved targets (pod selector + port), plus DNS
//   - ingress: target ← runner (required because default-deny denies all
//     ingress), per target, port-scoped
//   - api: operator initializer/starter → runner on 6565 (k6-operator needs it
//     to mark the TestRun started; see sources/k6-operator/internal/controller/service_ready.go)
//
// Runner and operator pods are selected by label loadtest.io/run-id=<run-id>,
// which the platform stamps on the TestRun (spec.runner/initializer.metadata.labels)
// so k6-operator propagates it to the pods it spawns.
//
// Labels use component-kind=run-policy (NOT network-policy) so the Env
// Reconciler prune and the Watcher expected-set evaluation skip them. They
// are created before the TestRun and deleted when the run reaches a terminal
// status (docs/network-policy.md §3.3).
func RunNetworkPolicies(envNS string, env *Env, runID string, targets []map[string]any) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured

	// Collect unique (service_name, port) pairs from targets. service_name is
	// the pod label (loadtest.io/component-id=<service name>) rendered onto
	// service pods; service_id is the server-generated "svc-<rand>" id and is
	// NOT a pod label. A target without service_name is a contract violation —
	// fail-closed: return an error rather than rendering a policy that selects
	// by a non-pod-label id (which would match no pods).
	type targetKey struct {
		service string
		port    int32
	}
	seen := map[targetKey]bool{}
	var egressRules []map[string]any
	for _, t := range targets {
		name, _ := t["service_name"].(string)
		if name == "" {
			return nil, fmt.Errorf("run policy: target missing service_name (selecting by service_id would match no pods)")
		}
		port := toPortInt32(t["port"])
		key := targetKey{service: name, port: port}
		if seen[key] {
			continue
		}
		seen[key] = true

		to := []map[string]any{{
			"podSelector": map[string]any{
				"matchLabels": map[string]string{k8s.LabelComponentID: name},
			},
		}}
		rule := map[string]any{"to": to}
		if port > 0 {
			rule["ports"] = []map[string]any{{"protocol": "TCP", "port": int64(port)}}
		}
		egressRules = append(egressRules, rule)
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
	}, egressRules...)

	runnerNP := toUnstructured(map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      fmt.Sprintf("loadtest-run-egress-%s", runID),
			"namespace": envNS,
			"labels": map[string]string{
				k8s.LabelManagedBy:           k8s.ManagedByValue,
				k8s.LabelNamespaceID:         env.NamespaceID,
				k8s.LabelTenantID:            env.TenantID,
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

	// 2. Target ingress: one policy per target, target ← runner, port-scoped.
	// Without this, default-deny ingress blocks runner → SUT traffic even
	// though egress is allowed. Each policy selects its own target pods by
	// component-id (service name) so a runner can only reach targets declared
	// for this run (docs/network-policy.md §3.2).
	ingressSeen := map[targetKey]bool{}
	for _, t := range targets {
		name, _ := t["service_name"].(string)
		if name == "" {
			return nil, fmt.Errorf("run policy: target missing service_name (selecting by service_id would match no pods)")
		}
		port := toPortInt32(t["port"])
		key := targetKey{service: name, port: port}
		if ingressSeen[key] {
			continue
		}
		ingressSeen[key] = true

		ingressRule := []any{map[string]any{
			"from": []any{map[string]any{
				"podSelector": map[string]any{
					"matchLabels": map[string]string{"loadtest.io/run-id": runID},
				},
			}},
		}}
		if port > 0 {
			ingressRule = []any{map[string]any{
				"from": []any{map[string]any{
					"podSelector": map[string]any{
						"matchLabels": map[string]string{"loadtest.io/run-id": runID},
					},
				}},
				"ports": []map[string]any{{"protocol": "TCP", "port": int64(port)}},
			}}
		}

		targetNP := toUnstructured(map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "NetworkPolicy",
			"metadata": map[string]any{
				"name":      fmt.Sprintf("loadtest-run-ingress-%s-%s", runID, name),
				"namespace": envNS,
				"labels": map[string]string{
					k8s.LabelManagedBy:           k8s.ManagedByValue,
					k8s.LabelNamespaceID:         env.NamespaceID,
					k8s.LabelTenantID:            env.TenantID,
					"loadtest.io/component-kind": "run-policy",
					"loadtest.io/run-id":         runID,
					k8s.LabelComponentID:         name,
				},
			},
			"spec": map[string]any{
				"podSelector": map[string]any{
					"matchLabels": map[string]string{k8s.LabelComponentID: name},
				},
				"policyTypes": []string{"Ingress"},
				"ingress":     ingressRule,
			},
		}, "networking.k8s.io", "v1", "NetworkPolicy")
		objs = append(objs, targetNP)
	}

	// 3. Runner API ingress (6565): k6-operator initializer/starter pods must
	// reach the runner API on 6565 to mark the TestRun started. Without this,
	// default-deny ingress blocks them and the run never leaves initialization.
	apiNP := toUnstructured(map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      fmt.Sprintf("loadtest-run-api-%s", runID),
			"namespace": envNS,
			"labels": map[string]string{
				k8s.LabelManagedBy:           k8s.ManagedByValue,
				k8s.LabelNamespaceID:         env.NamespaceID,
				k8s.LabelTenantID:            env.TenantID,
				"loadtest.io/component-kind": "run-policy",
				"loadtest.io/run-id":         runID,
			},
		},
		"spec": map[string]any{
			"podSelector": map[string]any{
				"matchLabels": map[string]string{
					"loadtest.io/run-id": runID,
					"runner":             "true", // only runner pods expose the API
				},
			},
			"policyTypes": []string{"Ingress"},
			"ingress": []any{
				map[string]any{
					"from": []any{map[string]any{
						"podSelector": map[string]any{
							"matchLabels": map[string]string{"loadtest.io/run-id": runID},
						},
					}},
					"ports": []map[string]any{{"protocol": "TCP", "port": int64(6565)}},
				},
				// k6-operator StartJobs probes http://<runner-svc>:6565/v1/status
				// from its controller pod in k6-operator-system. default-deny
				// blocks cross-namespace ingress, so allow it explicitly.
				map[string]any{
					"from": []any{map[string]any{
						"namespaceSelector": map[string]any{
							"matchLabels": map[string]string{"kubernetes.io/metadata.name": k6OperatorNamespace},
						},
					}},
					"ports": []map[string]any{{"protocol": "TCP", "port": int64(6565)}},
				},
			},
		},
	}, "networking.k8s.io", "v1", "NetworkPolicy")
	objs = append(objs, apiNP)

	return objs, nil
}

// toPortInt32 coerces a port value stored as int/int32/float64/json.Number
// (JSON unmarshals numbers into float64) to int32, returning 0 when absent or
// non-numeric. A zero port means "all ports" in the rendered rule.
func toPortInt32(v any) int32 {
	switch n := v.(type) {
	case nil:
		return 0
	case int:
		return int32(n)
	case int32:
		return n
	case int64:
		return int32(n)
	case float64:
		return int32(n)
	case json.Number:
		if i, err := strconv.ParseInt(string(n), 10, 32); err == nil {
			return int32(i)
		}
		return 0
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return int32(i)
		}
		return 0
	default:
		return 0
	}
}

// DeleteRunPolicies removes all run-scoped NetworkPolicies (egress, ingress per
// target, api 6565) and the run-scoped script ConfigMap for a run that reached
// a terminal status (docs/network-policy.md §3.3). Idempotent: missing objects
// are ignored. Called by the loadtest reconciler on run finish/timeout.
func DeleteRunPolicies(ctx context.Context, client k8s.Client, envNS, runID string) error {
	npGVK := schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}
	// List all run-policy NetworkPolicies for this run and delete them.
	list, err := client.List(ctx, npGVK, envNS, labels.Set{
		"loadtest.io/component-kind": "run-policy",
		"loadtest.io/run-id":         runID,
	})
	if err != nil {
		return fmt.Errorf("delete run policies: list networkpolicies: %w", err)
	}
	for _, np := range list {
		if err := client.Delete(ctx, npGVK, envNS, np.GetName()); err != nil && err != k8s.ErrNotFound {
			return fmt.Errorf("delete run policies: delete %s: %w", np.GetName(), err)
		}
	}
	// Delete the run-scoped script ConfigMap.
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}
	if err := client.Delete(ctx, cmGVK, envNS, runID+"-script"); err != nil && err != k8s.ErrNotFound {
		return fmt.Errorf("delete run policies: delete configmap %s-script: %w", runID, err)
	}
	return nil
}
