package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

// ServiceEgressPolicyComponent renders one per-service egress NetworkPolicy:
// allow this service's pods to reach only the targets it declared
// (dependencies + database_bindings), port-scoped. Everything else is denied
// by default-deny. This replaces the namespace-wide dependency allow-list so
// "service A → DB allowed, other-service → DB denied" is enforceable.
//
// Direction: egress only (service → dependency pod selector + ports).
//
// Labels are the standard network-policy set (component-kind=network-policy)
// so the Watcher expected-set evaluates them; name is <service>-egress.
type ServiceEgressPolicyComponent struct {
	envNS string
	env   *Env
	svc   ServiceSpec
}

// NewServiceEgressPolicyComponent builds a ServiceEgressPolicyComponent.
func NewServiceEgressPolicyComponent(envNS string, env *Env, svc ServiceSpec) ServiceEgressPolicyComponent {
	return ServiceEgressPolicyComponent{envNS: envNS, env: env, svc: svc}
}

// Kind returns the Kubernetes kind of the object.
func (s ServiceEgressPolicyComponent) Kind() string { return "NetworkPolicy" }

// Name returns the object name.
func (s ServiceEgressPolicyComponent) Name() string { return s.svc.Name + "-egress" }

// Labels returns the ownership labels stamped on the object.
func (s ServiceEgressPolicyComponent) Labels() map[string]string {
	return withComponent(baseLabels(s.env), s.svc.Name)
}

// SpecHash returns the spec hash annotation value.
func (s ServiceEgressPolicyComponent) SpecHash() string { return s.svc.SpecHash }

// Children returns nil — ServiceEgressPolicyComponent is a leaf.
func (s ServiceEgressPolicyComponent) Children() []Component { return nil }

// Render renders one egress rule per declared dependency/database binding,
// keyed by the target's pod label (loadtest.io/component-id=<target name>).
func (s ServiceEgressPolicyComponent) Render(_ *Env) []*unstructured.Unstructured {
	rules := []any{}
	for _, dep := range s.svc.Dependencies {
		if dep.RefID == "" {
			continue
		}
		rule := map[string]any{
			"to": []any{map[string]any{
				"podSelector": map[string]any{
					"matchLabels": map[string]string{k8s.LabelComponentID: dep.RefID},
				},
			}},
		}
		if len(dep.Ports) > 0 {
			var ports []map[string]any
			for _, p := range dep.Ports {
				ports = append(ports, map[string]any{"protocol": "TCP", "port": int64(p)})
			}
			rule["ports"] = ports
		}
		rules = append(rules, rule)
	}

	u := toUnstructured(map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      s.Name(),
			"namespace": s.envNS,
			"labels":    s.Labels(),
		},
		"spec": map[string]any{
			"podSelector": map[string]any{
				"matchLabels": map[string]string{k8s.LabelComponentID: s.svc.Name},
			},
			"policyTypes": []string{"Egress"},
			"egress":      rules,
		},
	}, "networking.k8s.io", "v1", "NetworkPolicy")
	annotate(u, s.svc.SpecHash)
	return []*unstructured.Unstructured{u}
}
