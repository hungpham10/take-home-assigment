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

func (i IngressComponent) Kind() string { return "Ingress" }
func (i IngressComponent) Name() string { return i.ing.Name }
func (i IngressComponent) Labels() map[string]string {
	return baseLabels(i.env)
}
func (i IngressComponent) SpecHash() string { return i.specHash }
func (i IngressComponent) Children() []Component { return nil }

func (i IngressComponent) Render(env *Env) []*unstructured.Unstructured {
	u := toUnstructured(ingress(i.envNS, env, i.ing), "networking.k8s.io", "v1", "Ingress")
	annotate(u, i.specHash)
	return []*unstructured.Unstructured{u}
}
