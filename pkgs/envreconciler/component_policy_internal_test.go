package envreconciler

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// defaultDenySpecEgressCIDRs returns the ipBlock CIDRs rendered in the
// default-deny egress (the kube-apiserver rule).
func defaultDenySpecEgressCIDRs(env *Env) []string {
	spec := defaultDenySpec(env)
	egress, _ := spec["egress"].([]map[string]any)
	var cidrs []string
	for _, r := range egress {
		to, _ := r["to"].([]map[string]any)
		for _, t := range to {
			if ipBlock, ok := t["ipBlock"].(map[string]any); ok {
				if cidr, ok := ipBlock["cidr"].(string); ok {
					cidrs = append(cidrs, cidr)
				}
			}
		}
	}
	return cidrs
}

func defaultDenySpecEgressMockRule(env *Env) (map[string]any, bool) {
	spec := defaultDenySpec(env)
	egress, _ := spec["egress"].([]map[string]any)
	for _, r := range egress {
		to, _ := r["to"].([]map[string]any)
		for _, t := range to {
			nsSel, _ := t["namespaceSelector"].(map[string]any)
			matchLabels, _ := nsSel["matchLabels"].(map[string]string)
			if matchLabels["kubernetes.io/metadata.name"] == env.netConfig().MockServiceNamespace {
				ports, _ := r["ports"].([]map[string]any)
				return map[string]any{"ports": ports}, true
			}
		}
	}
	return nil, false
}

func TestDefaultDenyDefaultsNodeCIDR(t *testing.T) {
	env := &Env{}
	cidrs := defaultDenySpecEgressCIDRs(env)
	if len(cidrs) != 1 || cidrs[0] != "172.18.0.0/16" {
		t.Fatalf("default node CIDRs = %v, want [172.18.0.0/16]", cidrs)
	}
}

func TestDefaultDenyUsesConfiguredNodeCIDRs(t *testing.T) {
	env := &Env{Net: PlatformNet{K8SNodeCIDRs: []string{"10.0.0.0/8", "192.168.0.0/24"}}}
	cidrs := defaultDenySpecEgressCIDRs(env)
	if len(cidrs) != 2 || cidrs[0] != "10.0.0.0/8" || cidrs[1] != "192.168.0.0/24" {
		t.Fatalf("configured node CIDRs = %v, want [10.0.0.0/8 192.168.0.0/24]", cidrs)
	}
}

func TestDefaultDenyAllowsMockServiceEgress(t *testing.T) {
	env := &Env{Net: PlatformNet{MockServiceNamespace: "mock-prod", MockServicePorts: []int32{8080, 8090}}}
	rule, found := defaultDenySpecEgressMockRule(env)
	if !found {
		t.Fatal("default-deny missing mock-service egress rule")
	}
	ports, _ := rule["ports"].([]map[string]any)
	if len(ports) != 2 {
		t.Fatalf("mock-service ports = %v, want 2 rules", ports)
	}
	// Verify each port present.
	seen := map[int32]bool{}
	for _, p := range ports {
		pv, _ := p["port"].(int64)
		seen[int32(pv)] = true
	}
	if !seen[8080] || !seen[8090] {
		t.Fatalf("mock-service ports missing 8080/8090: %v", ports)
	}
}

func TestRunAPI6565UsesConfiguredK6OperatorNamespace(t *testing.T) {
	env := &Env{Net: PlatformNet{K6OperatorNamespace: "k6-operator-prod"}}
	targets := []map[string]any{
		{"service_id": "svc-echo", "service_name": "echo", "port": int32(80)},
	}
	objs, err := RunNetworkPolicies("lt-env-ns", env, "run-1", targets)
	if err != nil {
		t.Fatalf("RunNetworkPolicies: %v", err)
	}
	var api *unstructured.Unstructured
	for _, o := range objs {
		if o.GetName() == "loadtest-run-api-run-1" {
			api = o
		}
	}
	if api == nil {
		t.Fatal("run api (6565) ingress policy not rendered")
	}
	rules, _, _ := unstructured.NestedSlice(api.Object, "spec", "ingress")
	foundNS := false
	for _, r := range rules {
		rm := r.(map[string]any)
		from, _, _ := unstructured.NestedSlice(rm, "from")
		for _, f := range from {
			fm := f.(map[string]any)
			nsSel, found, _ := unstructured.NestedMap(fm, "namespaceSelector", "matchLabels")
			if found && nsSel["kubernetes.io/metadata.name"] == "k6-operator-prod" {
				ports, _, _ := unstructured.NestedSlice(rm, "ports")
				if len(ports) != 1 {
					t.Fatalf("k6-operator ns rule should have 1 port, got %d", len(ports))
				}
				pm := ports[0].(map[string]any)
				pv, _, _ := unstructured.NestedFloat64(pm, "port")
				if int32(pv) != 6565 {
					t.Fatalf("k6-operator ns port = %v, want 6565", pv)
				}
				foundNS = true
			}
		}
	}
	if !foundNS {
		t.Fatalf("run api policy missing ingress from k6-operator-prod: %v", rules)
	}
	// Ensure the default namespace is NOT present.
	for _, r := range rules {
		rm := r.(map[string]any)
		from, _, _ := unstructured.NestedSlice(rm, "from")
		for _, f := range from {
			fm := f.(map[string]any)
			nsSel, found, _ := unstructured.NestedMap(fm, "namespaceSelector", "matchLabels")
			if found && nsSel["kubernetes.io/metadata.name"] == "k6-operator-system" {
				t.Fatal("run api policy still allows default k6-operator-system")
			}
		}
	}
}


func defaultDenySpecAPIServerPorts(env *Env) []int64 {
	spec := defaultDenySpec(env)
	egress, _ := spec["egress"].([]map[string]any)
	for _, r := range egress {
		to, _ := r["to"].([]map[string]any)
		hasIPBlock := false
		for _, t := range to {
			if _, ok := t["ipBlock"].(map[string]any); ok {
				hasIPBlock = true
			}
		}
		if !hasIPBlock {
			continue
		}
		// apiserver rule targets node CIDR ipBlocks (not namespace selectors)
		ports, _ := r["ports"].([]map[string]any)
		if len(ports) == 0 {
			continue
		}
		out := make([]int64, 0, len(ports))
		for _, p := range ports {
			pv, _ := p["port"].(int64)
			out = append(out, pv)
		}
		// distinguish from other possible ipBlock rules by presence of common apiserver ports
		for _, pv := range out {
			if pv == 443 || pv == 6443 {
				return out
			}
		}
	}
	return nil
}

func TestDefaultDenyDefaultsAPIServerPorts(t *testing.T) {
	env := &Env{}
	ports := defaultDenySpecAPIServerPorts(env)
	if len(ports) != 3 || ports[0] != 443 || ports[1] != 6443 || ports[2] != 60898 {
		t.Fatalf("default apiserver ports = %v, want [443 6443 60898]", ports)
	}
}

func TestDefaultDenyUsesConfiguredAPIServerPorts(t *testing.T) {
	env := &Env{Net: PlatformNet{K8SAPIServerPorts: []int32{6443, 8443}}}
	ports := defaultDenySpecAPIServerPorts(env)
	if len(ports) != 2 || ports[0] != 6443 || ports[1] != 8443 {
		t.Fatalf("configured apiserver ports = %v, want [6443 8443]", ports)
	}
}
