package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// EnvironmentComponent là ROOT của cây apply. Render object Namespace (bao
// ngoài) trước, sau đó delegate sang các con theo thứ tự khóa.
type EnvironmentComponent struct {
	envNS   string
	env     *Env
	children []Component
}

func (e EnvironmentComponent) Kind() string { return "Namespace" }
func (e EnvironmentComponent) Name() string { return e.envNS }
func (e EnvironmentComponent) Labels() map[string]string {
	return baseLabels(e.env)
}
func (e EnvironmentComponent) SpecHash() string { return "" }
func (e EnvironmentComponent) Children() []Component { return e.children }

func (e EnvironmentComponent) Render(env *Env) []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		toUnstructured(map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]any{"name": e.envNS, "labels": baseLabels(env)},
		}, "", "v1", "Namespace"),
	}
}
