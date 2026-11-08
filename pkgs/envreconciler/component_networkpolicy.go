package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// NetworkPolicyComponent là leaf: NetworkPolicy default-deny (áp ngay sau
// Namespace). SpecHash là hash của PolicySpec (dùng cho prune drift detection).
type NetworkPolicyComponent struct {
	envNS   string
	env     *Env
	specHash string
}

func (n NetworkPolicyComponent) Kind() string { return "NetworkPolicy" }
func (n NetworkPolicyComponent) Name() string { return "default-deny" }
func (n NetworkPolicyComponent) Labels() map[string]string {
	return baseLabels(n.env)
}
func (n NetworkPolicyComponent) SpecHash() string { return n.specHash }
func (n NetworkPolicyComponent) Children() []Component { return nil }

func (n NetworkPolicyComponent) Render(env *Env) []*unstructured.Unstructured {
	np := toUnstructured(defaultDenyNP(n.envNS, env), "networking.k8s.io", "v1", "NetworkPolicy")
	annotate(np, n.specHash)
	return []*unstructured.Unstructured{np}
}
