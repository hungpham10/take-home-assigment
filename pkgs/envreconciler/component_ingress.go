package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// IngressComponent là leaf: Ingress tham chiếu service đã có (áp cuối cùng).
type IngressComponent struct {
	envNS   string
	env     *Env
	ing     IngressSpec
	specHash string
}

// Kind returns the Kubernetes kind of the component object.
func (i IngressComponent) Kind() string { return "Ingress" }

// Name returns the object name.
func (i IngressComponent) Name() string { return i.ing.Name }

// Labels returns the ownership labels stamped on the object.
func (i IngressComponent) Labels() map[string]string {
	return baseLabels(i.env)
}

// SpecHash returns the spec hash annotation value.
func (i IngressComponent) SpecHash() string { return i.specHash }

// Children returns nil — IngressComponent is a leaf.
func (i IngressComponent) Children() []Component { return nil }

// Render renders the Ingress manifest.
func (i IngressComponent) Render(env *Env) []*unstructured.Unstructured {
	u := toUnstructured(ingress(i.envNS, env, i.ing), "networking.k8s.io", "v1", "Ingress")
	annotate(u, i.specHash)
	return []*unstructured.Unstructured{u}
}
