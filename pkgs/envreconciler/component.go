package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Component là một node trong cây apply theo mô hình Composite. Mỗi node render
// các object của chính nó (pure, không I/O), biết các con qua Children(). Apply
// là một DFS dùng chung (applyComponent trong tree.go), không phải phương thức
// trên mỗi node — giữ nguyên thứ tự bên ngoài→trong đã khóa.
//
// ServiceComponent là composite (chứa các sub-resource); NetworkPolicy,
// Database, Ingress, MockBinding là leaf; EnvironmentComponent là root composite.
type Component interface {
	Kind() string
	Name() string
	// Render trả về các object của chính node này (không bao gồm con). Pure,
	// có thể gọi lặp lại mà không side effect — dùng cho test render, prune
	// và Watcher expected-state.
	Render(env *Env) []*unstructured.Unstructured
	Children() []Component
	SpecHash() string
	Labels() map[string]string
}
