package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ServiceAccountComponent render dedicated ServiceAccount.
type ServiceAccountComponent struct {
	envNS string
	env   *Env
	svc   ServiceSpec
}

func (s ServiceAccountComponent) Kind() string { return "ServiceAccount" }
func (s ServiceAccountComponent) Name() string { return s.svc.Name }
func (s ServiceAccountComponent) Labels() map[string]string {
	return withComponent(baseLabels(s.env), s.svc.Name)
}
func (s ServiceAccountComponent) SpecHash() string { return s.svc.SpecHash }
func (s ServiceAccountComponent) Children() []Component { return nil }

func (s ServiceAccountComponent) Render(env *Env) []*unstructured.Unstructured {
	if !s.svc.ServiceAccount {
		return nil
	}
	sa := toUnstructured(serviceAccount(s.envNS, env, s.svc), "", "v1", "ServiceAccount")
	annotate(sa, s.svc.SpecHash)
	return []*unstructured.Unstructured{sa}
}
