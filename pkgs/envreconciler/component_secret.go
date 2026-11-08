package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// SecretComponent render <name>-secret (Opaque, values chỉ nằm trong Secret).
type SecretComponent struct {
	envNS string
	env   *Env
	svc   ServiceSpec
}

func (s SecretComponent) Kind() string { return "Secret" }
func (s SecretComponent) Name() string { return s.svc.Name + "-secret" }
func (s SecretComponent) Labels() map[string]string {
	return withComponent(baseLabels(s.env), s.svc.Name)
}
func (s SecretComponent) SpecHash() string { return s.svc.SpecHash }
func (s SecretComponent) Children() []Component { return nil }

func (s SecretComponent) Render(env *Env) []*unstructured.Unstructured {
	if len(s.svc.Secrets) == 0 {
		return nil
	}
	sec := toUnstructured(secret(s.envNS, env, s.svc), "", "v1", "Secret")
	annotate(sec, s.svc.SpecHash)
	return []*unstructured.Unstructured{sec}
}
