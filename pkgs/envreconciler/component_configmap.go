package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ConfigMapComponent render <name>-config.
type ConfigMapComponent struct {
	envNS string
	env   *Env
	svc   ServiceSpec
}

// Kind returns the Kubernetes kind of the component object.
func (c ConfigMapComponent) Kind() string { return "ConfigMap" }

// Name returns the object name.
func (c ConfigMapComponent) Name() string { return c.svc.Name + "-config" }

// Labels returns the ownership labels stamped on the object.
func (c ConfigMapComponent) Labels() map[string]string {
	return withComponent(baseLabels(c.env), c.svc.Name)
}

// SpecHash returns the spec hash annotation value.
func (c ConfigMapComponent) SpecHash() string { return c.svc.SpecHash }

// Children returns nil — ConfigMapComponent is a leaf.
func (c ConfigMapComponent) Children() []Component { return nil }

// Render renders the ConfigMap manifest.
func (c ConfigMapComponent) Render(env *Env) []*unstructured.Unstructured {
	if len(c.svc.ConfigMap) == 0 {
		return nil
	}
	cm := toUnstructured(configMap(c.envNS, env, c.svc), "", "v1", "ConfigMap")
	annotate(cm, c.svc.SpecHash)
	return []*unstructured.Unstructured{cm}
}
