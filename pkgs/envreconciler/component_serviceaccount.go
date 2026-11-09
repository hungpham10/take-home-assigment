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

// Kind returns the Kubernetes kind of the component object.
func (s ServiceAccountComponent) Kind() string { return "ServiceAccount" }

// Name returns the object name.
func (s ServiceAccountComponent) Name() string { return s.svc.Name }

// Labels returns the ownership labels stamped on the object.
func (s ServiceAccountComponent) Labels() map[string]string {
	return withComponent(baseLabels(s.env), s.svc.Name)
}

// SpecHash returns the spec hash annotation value.
func (s ServiceAccountComponent) SpecHash() string { return s.svc.SpecHash }

// Children returns nil — ServiceAccountComponent is a leaf.
func (s ServiceAccountComponent) Children() []Component { return nil }

// Render renders the ServiceAccount manifest.
func (s ServiceAccountComponent) Render(env *Env) []*unstructured.Unstructured {
	if !s.svc.ServiceAccount {
		return nil
	}
	sa := toUnstructured(serviceAccount(s.envNS, env, s.svc), "", "v1", "ServiceAccount")
	annotate(sa, s.svc.SpecHash)
	return []*unstructured.Unstructured{sa}
}
