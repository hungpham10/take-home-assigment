package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ServiceComponent là wrapper composite: quản lý đầy đủ objects của một service
// (ConfigMap → Secret → ServiceAccount → Deployment → Service → HPA).
// Không có object "self" (không có ServiceGroup K8s) — Render delegate sang
// các con; apply qua DFS trong applyComponent (tree.go).
type ServiceComponent struct {
	envNS string
	env   *Env
	svc   ServiceSpec
}

// Kind returns the Kubernetes kind of the component object.
func (s ServiceComponent) Kind() string { return "ServiceGroup" }

// Name returns the object name.
func (s ServiceComponent) Name() string { return s.svc.Name }

// Labels returns the ownership labels stamped on the object.
func (s ServiceComponent) Labels() map[string]string {
	return withComponent(baseLabels(s.env), s.svc.Name)
}

// SpecHash returns the spec hash annotation value.
func (s ServiceComponent) SpecHash() string { return s.svc.SpecHash }

// Children returns the service's sub-component children.
func (s ServiceComponent) Children() []Component {
	return []Component{
		ConfigMapComponent(s),
		SecretComponent(s),
		ServiceAccountComponent(s),
		DeploymentComponent(s),
		K8sServiceComponent(s),
		HPAComponent(s),
	}
}

// Render returns nil — ServiceComponent has no self object, only children.
func (s ServiceComponent) Render(env *Env) []*unstructured.Unstructured {
	// ServiceComponent không có object "self"; tất cả objects nằm ở các con.
	return nil
}
