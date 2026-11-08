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

func (m MockBindingComponent) Kind() string { return "MockBinding" }
func (m MockBindingComponent) Name() string { return m.envNS }
func (m MockBindingComponent) Labels() map[string]string {
	return baseLabels(m.env)
}
func (m MockBindingComponent) SpecHash() string { return "" }
func (m MockBindingComponent) Children() []Component { return nil }

func (m MockBindingComponent) Render(env *Env) []*unstructured.Unstructured {
	if mockBindingRenderFunc != nil {
		return mockBindingRenderFunc(env)
	}
	return nil
}
