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

func (c ConfigMapComponent) Kind() string { return "ConfigMap" }
func (c ConfigMapComponent) Name() string { return c.svc.Name + "-config" }
func (c ConfigMapComponent) Labels() map[string]string {
	return withComponent(baseLabels(c.env), c.svc.Name)
}
func (c ConfigMapComponent) SpecHash() string { return c.svc.SpecHash }
func (c ConfigMapComponent) Children() []Component { return nil }

func (c ConfigMapComponent) Render(env *Env) []*unstructured.Unstructured {
	if len(c.svc.ConfigMap) == 0 {
		return nil
	}
	cm := toUnstructured(configMap(c.envNS, env, c.svc), "", "v1", "ConfigMap")
	annotate(cm, c.svc.SpecHash)
	return []*unstructured.Unstructured{cm}
}
