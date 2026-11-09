package envreconciler_test

import (
	"strings"
	"testing"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func testEnvPolicy() *envreconciler.Env {
	return &envreconciler.Env{
		NamespaceID: "11111111-2222-3333-4444-555555555555",
		TenantID:    "t-1",
		Services: []envreconciler.ServiceSpec{{
			Name: "echo", Port: 80,
		}, {
			Name: "other-svc", Port: 8080,
		}},
		Databases: []envreconciler.DatabaseSpec{{
			Name: "demo-db", Operator: "cloudnative-pg",
		}},
	}
}

func npEgressRules(np *unstructured.Unstructured) []any {
	rules, found, _ := unstructured.NestedSlice(np.Object, "spec", "egress")
	if !found {
		return nil
	}
	return rules
}

func npIngressRules(np *unstructured.Unstructured) []any {
	rules, found, _ := unstructured.NestedSlice(np.Object, "spec", "ingress")
	if !found {
		return nil
	}
	return rules
}

func egressToPort(rules []any, port int32) bool {
	for _, r := range rules {
		rm := r.(map[string]any)
		ports, _, _ := unstructured.NestedSlice(rm, "ports")
		for _, p := range ports {
			pm := p.(map[string]any)
			pv, ok, _ := unstructured.NestedFloat64(pm, "port")
			if ok && int32(pv) == port {
				return true
			}
		}
	}
	return false
}

func TestRunEgressPortsScoped(t *testing.T) {
	// Regression: collectTargets stores port as int32; RunNetworkPolicies must
	// not drop it (the float64 type-assert bug silently made port=0 → all ports).
	targets := []map[string]any{
		{"service_id": "svc-echo", "service_name": "echo", "port": int32(80)},
		{"service_id": "svc-other", "service_name": "other-svc", "port": int32(8080)},
	}
	objs, err := envreconciler.RunNetworkPolicies("lt-env-ns", &envreconciler.Env{}, "run-1", targets)
	if err != nil {
		t.Fatalf("RunNetworkPolicies: %v", err)
	}
	var egress *unstructured.Unstructured
	for _, o := range objs {
		if o.GetName() == "loadtest-run-egress-run-1" {
			egress = o
		}
	}
	if egress == nil {
		t.Fatal("run egress policy not rendered")
	}
	if !egressToPort(npEgressRules(egress), 80) {
		t.Fatalf("run egress missing port 80 rule: %v", npEgressRules(egress))
	}
	if !egressToPort(npEgressRules(egress), 8080) {
		t.Fatalf("run egress missing port 8080 rule: %v", npEgressRules(egress))
	}
	// DNS always present.
	if !egressToPort(npEgressRules(egress), 53) {
		t.Fatalf("run egress missing DNS port 53: %v", npEgressRules(egress))
	}
}

func TestRunEgressSelectsByServiceName(t *testing.T) {
	targets := []map[string]any{
		{"service_id": "svc-123", "service_name": "echo", "port": int32(80)},
	}
	objs, _ := envreconciler.RunNetworkPolicies("lt-env-ns", &envreconciler.Env{}, "run-1", targets)
	var egress *unstructured.Unstructured
	for _, o := range objs {
		if o.GetName() == "loadtest-run-egress-run-1" {
			egress = o
		}
	}
	// First (non-DNS) rule targets the service by component-id = service NAME.
	rules := npEgressRules(egress)
	found := false
	for _, r := range rules {
		rm := r.(map[string]any)
		to, _, _ := unstructured.NestedSlice(rm, "to")
		if len(to) == 0 {
			continue
		}
		ps, _, _ := unstructured.NestedMap(to[0].(map[string]any), "podSelector", "matchLabels")
		if ps[k8s.LabelComponentID] == "echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("run egress does not select by component-id=echo: %v", rules)
	}
	// Must NOT select by service_id (svc-123).
	for _, r := range rules {
		rm := r.(map[string]any)
		to, _, _ := unstructured.NestedSlice(rm, "to")
		if len(to) == 0 {
			continue
		}
		ps, _, _ := unstructured.NestedMap(to[0].(map[string]any), "podSelector", "matchLabels")
		if ps[k8s.LabelComponentID] == "svc-123" {
			t.Fatalf("run egress selects by service_id svc-123 instead of name: %v", rules)
		}
	}
}

func TestRunIngressPerTargetAnd6565(t *testing.T) {
	targets := []map[string]any{
		{"service_id": "svc-echo", "service_name": "echo", "port": int32(80)},
	}
	objs, _ := envreconciler.RunNetworkPolicies("lt-env-ns", &envreconciler.Env{}, "run-1", targets)
	var ingress *unstructured.Unstructured
	for _, o := range objs {
		if o.GetName() == "loadtest-run-ingress-run-1-echo" {
			ingress = o
		}
	}
	if ingress == nil {
		t.Fatal("run ingress policy for echo not rendered")
	}
	// podSelector selects the target pods by component-id = service name.
	ps, _, _ := unstructured.NestedMap(ingress.Object, "spec", "podSelector", "matchLabels")
	if ps[k8s.LabelComponentID] != "echo" {
		t.Fatalf("run ingress podSelector = %v, want component-id=echo", ps)
	}
	// Port-scoped: 80.
	rules := npIngressRules(ingress)
	if !egressToPort(rules, 80) {
		t.Fatalf("run ingress missing port 80 rule: %v", rules)
	}
	// From runner (run-id).
	fromRunner := false
	for _, r := range rules {
		rm := r.(map[string]any)
		from, _, _ := unstructured.NestedSlice(rm, "from")
		for _, f := range from {
			fm := f.(map[string]any)
			ps2, _, _ := unstructured.NestedMap(fm, "podSelector", "matchLabels")
			if ps2["loadtest.io/run-id"] == "run-1" {
				fromRunner = true
			}
		}
	}
	if !fromRunner {
		t.Fatalf("run ingress does not allow from runner run-id=run-1: %v", rules)
	}
	// 6565 API ingress policy exists.
	apiFound := false
	for _, o := range objs {
		if o.GetName() == "loadtest-run-api-run-1" {
			apiFound = true
			aps, _, _ := unstructured.NestedMap(o.Object, "spec", "podSelector", "matchLabels")
			if aps["runner"] != "true" || aps["loadtest.io/run-id"] != "run-1" {
				t.Fatalf("api policy podSelector = %v, want runner=true + run-id=run-1", aps)
			}
			arules := npIngressRules(o)
			if !egressToPort(arules, 6565) {
				t.Fatalf("api ingress missing port 6565: %v", arules)
			}
		}
	}
	if !apiFound {
		t.Fatal("run api (6565) ingress policy not rendered")
	}
}

// TestRunAPI6565AllowsK6OperatorNamespace — k6-operator StartJobs probes
// http://<runner-svc>:6565/v1/status from its controller pod in
// k6-operator-system. default-deny blocks cross-namespace ingress, so the api
// NetworkPolicy must allow ingress from that namespace on 6565 (in addition to
// from runner pods in the same run).
func TestRunAPI6565AllowsK6OperatorNamespace(t *testing.T) {
	targets := []map[string]any{
		{"service_id": "svc-echo", "service_name": "echo", "port": int32(80)},
	}
	objs, _ := envreconciler.RunNetworkPolicies("lt-env-ns", &envreconciler.Env{}, "run-1", targets)
	var api *unstructured.Unstructured
	for _, o := range objs {
		if o.GetName() == "loadtest-run-api-run-1" {
			api = o
		}
	}
	if api == nil {
		t.Fatal("run api (6565) ingress policy not rendered")
	}
	rules := npIngressRules(api)
	if len(rules) != 2 {
		t.Fatalf("api ingress should have 2 rules (runner + k6-operator ns), got %d: %v", len(rules), rules)
	}
	// Rule 2: namespaceSelector kubernetes.io/metadata.name=k6-operator-system.
	foundNS := false
	for _, r := range rules {
		rm := r.(map[string]any)
		from, _, _ := unstructured.NestedSlice(rm, "from")
		for _, f := range from {
			fm := f.(map[string]any)
			nsSel, found, _ := unstructured.NestedMap(fm, "namespaceSelector", "matchLabels")
			if found && nsSel["kubernetes.io/metadata.name"] == "k6-operator-system" {
				foundNS = true
				ports, _, _ := unstructured.NestedSlice(rm, "ports")
				if len(ports) != 1 {
					t.Fatalf("k6-operator ns rule should have 1 port, got %d", len(ports))
				}
				pm := ports[0].(map[string]any)
				proto, _, _ := unstructured.NestedString(pm, "protocol")
				pv, ok, _ := unstructured.NestedFloat64(pm, "port")
				if proto != "TCP" || !ok || int32(pv) != 6565 {
					t.Fatalf("k6-operator ns rule port = proto=%s port=%v ok=%v, want TCP/6565", proto, pv, ok)
				}
			}
		}
	}
	if !foundNS {
		t.Fatalf("api ingress missing namespaceSelector rule for k6-operator-system: %v", rules)
	}
}

// TestRunNetworkPoliciesFailsWithoutServiceName — fail-closed: a target with
// only service_id (no service_name) must not render a policy selecting by the
// non-pod-label id (which would match no pods and silently break enforcement).
func TestRunNetworkPoliciesFailsWithoutServiceName(t *testing.T) {
	targets := []map[string]any{
		{"service_id": "svc-123", "port": int32(80)},
	}
	_, err := envreconciler.RunNetworkPolicies("lt-env-ns", &envreconciler.Env{}, "run-1", targets)
	if err == nil {
		t.Fatal("expected error for target without service_name")
	}
	if !strings.Contains(err.Error(), "service_name") {
		t.Fatalf("error should mention service_name, got: %v", err)
	}
}

func TestServiceEgressPolicyDeclaredDepsOnly(t *testing.T) {
	env := testEnvPolicy()
	// echo depends on other-svc (port 8080); no DB binding.
	svc := envreconciler.ServiceSpec{
		Name:     "echo",
		Port:     80,
		SpecHash: "hash-echo",
		Dependencies: []envreconciler.EgressRule{{
			Type:  "service",
			RefID: "other-svc",
			Ports: []int32{8080},
		}},
	}
	np := (envreconciler.NewServiceEgressPolicyComponent("lt-env-ns", env, svc)).Render(env)
	if len(np) != 1 || np[0].GetKind() != "NetworkPolicy" {
		t.Fatalf("expected 1 NetworkPolicy, got %d", len(np))
	}
	u := np[0]
	if u.GetName() != "echo-egress" {
		t.Fatalf("name = %q, want echo-egress", u.GetName())
	}
	// podSelector = echo's own pods.
	ps, _, _ := unstructured.NestedMap(u.Object, "spec", "podSelector", "matchLabels")
	if ps[k8s.LabelComponentID] != "echo" {
		t.Fatalf("egress podSelector = %v, want component-id=echo", ps)
	}
	// egress rule: to other-svc:8080.
	rules, found, _ := unstructured.NestedSlice(u.Object, "spec", "egress")
	if !found || len(rules) != 1 {
		t.Fatalf("expected 1 egress rule, got %v", rules)
	}
	to, _, _ := unstructured.NestedSlice(rules[0].(map[string]any), "to")
	if len(to) != 1 {
		t.Fatalf("expected 1 'to' entry, got %v", to)
	}
	sel, _, _ := unstructured.NestedMap(to[0].(map[string]any), "podSelector", "matchLabels")
	if sel[k8s.LabelComponentID] != "other-svc" {
		t.Fatalf("egress target = %v, want component-id=other-svc", sel)
	}
	ports, _, _ := unstructured.NestedSlice(rules[0].(map[string]any), "ports")
	if len(ports) != 1 {
		t.Fatalf("expected 1 port, got %v", ports)
	}
	pv, _, _ := unstructured.NestedFloat64(ports[0].(map[string]any), "port")
	if int32(pv) != 8080 {
		t.Fatalf("port = %v, want 8080", pv)
	}
}

func TestServiceEgressPolicyNoDepsEmpty(t *testing.T) {
	svc := envreconciler.ServiceSpec{Name: "standalone", Port: 80, SpecHash: "h"}
	np := (envreconciler.NewServiceEgressPolicyComponent("lt-env-ns", testEnvPolicy(), svc)).Render(testEnvPolicy())
	if len(np) != 1 {
		t.Fatalf("expected 1 object, got %d", len(np))
	}
	egress, found, _ := unstructured.NestedSlice(np[0].Object, "spec", "egress")
	if !found || len(egress) != 0 {
		t.Fatalf("expected empty egress (no deps), got %v", egress)
	}
}

func TestDatabasePolicyBoundServicesOnly(t *testing.T) {
	env := testEnvPolicy()
	// demo-db bound to echo only → ingress allows echo pods on 5432.
	db := envreconciler.DatabaseSpec{
		Name:          "demo-db",
		Operator:      "cloudnative-pg",
		BoundServices: []string{"echo"},
		SpecHash:      "hash-db",
	}
	np := (envreconciler.NewDatabasePolicyComponent("lt-env-ns", env, db)).Render(env)
	u := np[0]
	ingress, found, _ := unstructured.NestedSlice(u.Object, "spec", "ingress")
	if !found || len(ingress) != 1 {
		t.Fatalf("expected 1 ingress rule (bound echo), got %v", ingress)
	}
	from, _, _ := unstructured.NestedSlice(ingress[0].(map[string]any), "from")
	sel, _, _ := unstructured.NestedMap(from[0].(map[string]any), "podSelector", "matchLabels")
	if sel[k8s.LabelComponentID] != "echo" {
		t.Fatalf("DB ingress from = %v, want component-id=echo", sel)
	}
	ports, _, _ := unstructured.NestedSlice(ingress[0].(map[string]any), "ports")
	pv, _, _ := unstructured.NestedFloat64(ports[0].(map[string]any), "port")
	if int32(pv) != 5432 {
		t.Fatalf("DB port = %v, want 5432", pv)
	}
}

func TestDatabasePolicyNoBindingsDenyAll(t *testing.T) {
	db := envreconciler.DatabaseSpec{Name: "demo-db", Operator: "cloudnative-pg"}
	np := (envreconciler.NewDatabasePolicyComponent("lt-env-ns", testEnvPolicy(), db)).Render(testEnvPolicy())
	ingress, found, _ := unstructured.NestedSlice(np[0].Object, "spec", "ingress")
	if !found || len(ingress) != 0 {
		t.Fatalf("expected empty ingress (no bindings), got %v", ingress)
	}
}
