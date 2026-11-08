package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// K8sServiceComponent render Service (tránh trùng tên với ServiceComponent).
type K8sServiceComponent struct {
	envNS string
	env   *Env
	svc   ServiceSpec
}

func (s K8sServiceComponent) Kind() string { return "Service" }
func (s K8sServiceComponent) Name() string { return s.svc.Name }
func (s K8sServiceComponent) Labels() map[string]string {
	return withComponent(baseLabels(s.env), s.svc.Name)
}
func (s K8sServiceComponent) SpecHash() string { return s.svc.SpecHash }
func (s K8sServiceComponent) Children() []Component { return nil }

func (s K8sServiceComponent) Render(env *Env) []*unstructured.Unstructured {
	sv := toUnstructured(service(s.envNS, env, s.svc), "", "v1", "Service")
	annotate(sv, s.svc.SpecHash)
	return []*unstructured.Unstructured{sv}
}
