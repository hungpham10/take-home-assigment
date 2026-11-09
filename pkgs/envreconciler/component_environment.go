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

// Kind returns the Kubernetes kind of the component object.
func (e EnvironmentComponent) Kind() string { return "Namespace" }

// Name returns the object name.
func (e EnvironmentComponent) Name() string { return e.envNS }

// Labels returns the ownership labels stamped on the object.
func (e EnvironmentComponent) Labels() map[string]string {
	return baseLabels(e.env)
}

// SpecHash returns the spec hash annotation value (empty for root).
func (e EnvironmentComponent) SpecHash() string { return "" }

// Children returns the component's child components.
func (e EnvironmentComponent) Children() []Component { return e.children }

// Render renders the Namespace manifest.
func (e EnvironmentComponent) Render(env *Env) []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		toUnstructured(map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]any{"name": e.envNS, "labels": baseLabels(env)},
		}, "", "v1", "Namespace"),
	}
}
