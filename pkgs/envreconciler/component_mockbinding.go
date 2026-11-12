package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// mockBindingRenderFunc is injected by mockcontrol (block D) to render mock
// binding objects. Nil by default (no mock bindings). Set via SetMockBindingRenderFunc.
var mockBindingRenderFunc func(env *Env) []*unstructured.Unstructured

// SetMockBindingRenderFunc allows mockcontrol to inject binding render into
// MockBindingComponent without an import cycle (mockcontrol → envreconciler,
// never the reverse).
func SetMockBindingRenderFunc(fn func(env *Env) []*unstructured.Unstructured) {
	mockBindingRenderFunc = fn
}

// MockBindingComponent là leaf: mock binding objects (ConfigMap + ExternalName
// Service). Render delegated sang mockBindingRenderFunc (được mockcontrol set
// qua SetMockBindingRenderFunc); nil = không có mock binding nào.
type MockBindingComponent struct {
	envNS string
	env   *Env
}

// Kind returns the Kubernetes kind of the component object.
func (m MockBindingComponent) Kind() string { return "MockBinding" }

// Name returns the object name.
func (m MockBindingComponent) Name() string { return m.envNS }

// Labels returns the ownership labels stamped on the object.
func (m MockBindingComponent) Labels() map[string]string {
	return baseLabels(m.env)
}

// SpecHash returns the spec hash annotation value (empty for mock binding).
func (m MockBindingComponent) SpecHash() string { return "" }

// Children returns nil — MockBindingComponent is a leaf.
func (m MockBindingComponent) Children() []Component { return nil }

// Render renders mock binding objects via the injected render function.
func (m MockBindingComponent) Render(env *Env) []*unstructured.Unstructured {
	if mockBindingRenderFunc != nil {
		return mockBindingRenderFunc(env)
	}
	return nil
}
