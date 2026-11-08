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

func (s ServiceComponent) Kind() string { return "ServiceGroup" }
func (s ServiceComponent) Name() string { return s.svc.Name }
func (s ServiceComponent) Labels() map[string]string {
	return withComponent(baseLabels(s.env), s.svc.Name)
}
func (s ServiceComponent) SpecHash() string { return s.svc.SpecHash }
func (s ServiceComponent) Children() []Component {
	return []Component{
		ConfigMapComponent{envNS: s.envNS, env: s.env, svc: s.svc},
		SecretComponent{envNS: s.envNS, env: s.env, svc: s.svc},
		ServiceAccountComponent{envNS: s.envNS, env: s.env, svc: s.svc},
		DeploymentComponent{envNS: s.envNS, env: s.env, svc: s.svc},
		K8sServiceComponent{envNS: s.envNS, env: s.env, svc: s.svc},
		HPAComponent{envNS: s.envNS, env: s.env, svc: s.svc},
	}
}

func (s ServiceComponent) Render(env *Env) []*unstructured.Unstructured {
	// ServiceComponent không có object "self"; tất cả objects nằm ở các con.
	return nil
}
