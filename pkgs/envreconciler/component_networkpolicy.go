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

// Kind returns the Kubernetes kind of the component object.
func (n NetworkPolicyComponent) Kind() string { return "NetworkPolicy" }

// Name returns the object name.
func (n NetworkPolicyComponent) Name() string { return "default-deny" }

// Labels returns the ownership labels stamped on the object.
func (n NetworkPolicyComponent) Labels() map[string]string {
	return baseLabels(n.env)
}

// SpecHash returns the spec hash annotation value.
func (n NetworkPolicyComponent) SpecHash() string { return n.specHash }

// Children returns nil — NetworkPolicyComponent is a leaf.
func (n NetworkPolicyComponent) Children() []Component { return nil }

// Render renders the NetworkPolicy manifest.
func (n NetworkPolicyComponent) Render(env *Env) []*unstructured.Unstructured {
	np := toUnstructured(defaultDenyNP(n.envNS, env), "networking.k8s.io", "v1", "NetworkPolicy")
	annotate(np, n.specHash)
	return []*unstructured.Unstructured{np}
}
