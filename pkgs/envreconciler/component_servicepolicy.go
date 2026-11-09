package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

// ServicePolicyComponent render the per-service NetworkPolicy (ingress side):
// allow traffic into the service pod from the same env namespace on the
// service port. Egress is governed by default-deny's declared-dependency
// allow-list, so a service can only reach targets it declared.
//
// NOTE: strict per-service egress (source service → target service) is now
// rendered per-service from svc.Dependencies / svc.DatabaseBindings
// (component_service_egress_policy.go), so each service can only reach its
// own declared targets. See docs/network-policy.md §2.
type ServicePolicyComponent struct {
	envNS string
	env   *Env
	svc   ServiceSpec
}

// Kind returns the Kubernetes kind of the component object.
func (s ServicePolicyComponent) Kind() string { return "NetworkPolicy" }

// Name returns the object name.
func (s ServicePolicyComponent) Name() string { return s.svc.Name + "-policy" }

// Labels returns the ownership labels stamped on the object.
func (s ServicePolicyComponent) Labels() map[string]string {
	return withComponent(baseLabels(s.env), s.svc.Name)
}

// SpecHash returns the spec hash annotation value.
func (s ServicePolicyComponent) SpecHash() string { return s.svc.SpecHash }

// Children returns nil — ServicePolicyComponent is a leaf.
func (s ServicePolicyComponent) Children() []Component { return nil }

// Render renders a NetworkPolicy that allows ingress to the service pod from
// same-namespace pods on svc.Port.
func (s ServicePolicyComponent) Render(_ *Env) []*unstructured.Unstructured {
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
			"policyTypes": []string{"Ingress"},
			"ingress": []any{
				map[string]any{
					"from":  []any{map[string]any{"podSelector": map[string]any{}}},
					"ports": []map[string]any{{"protocol": "TCP", "port": int64(s.svc.Port)}},
				},
			},
		},
	}, "networking.k8s.io", "v1", "NetworkPolicy")
	annotate(u, s.svc.SpecHash)
	return []*unstructured.Unstructured{u}
}
