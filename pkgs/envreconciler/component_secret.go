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

// Kind returns the Kubernetes kind of the component object.
func (s SecretComponent) Kind() string { return "Secret" }

// Name returns the object name.
func (s SecretComponent) Name() string { return s.svc.Name + "-secret" }

// Labels returns the ownership labels stamped on the object.
func (s SecretComponent) Labels() map[string]string {
	return withComponent(baseLabels(s.env), s.svc.Name)
}

// SpecHash returns the spec hash annotation value.
func (s SecretComponent) SpecHash() string { return s.svc.SpecHash }

// Children returns nil — SecretComponent is a leaf.
func (s SecretComponent) Children() []Component { return nil }

// Render renders the Secret manifest.
func (s SecretComponent) Render(env *Env) []*unstructured.Unstructured {
	if len(s.svc.Secrets) == 0 {
		return nil
	}
	sec := toUnstructured(secret(s.envNS, env, s.svc), "", "v1", "Secret")
	annotate(sec, s.svc.SpecHash)
	return []*unstructured.Unstructured{sec}
}
