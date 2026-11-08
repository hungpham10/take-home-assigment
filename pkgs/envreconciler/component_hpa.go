package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// HPAComponent render HorizontalPodAutoscaler (optional).
type HPAComponent struct {
	envNS string
	env   *Env
	svc   ServiceSpec
}

func (h HPAComponent) Kind() string { return "HorizontalPodAutoscaler" }
func (h HPAComponent) Name() string { return h.svc.Name }
func (h HPAComponent) Labels() map[string]string {
	return withComponent(baseLabels(h.env), h.svc.Name)
}
func (h HPAComponent) SpecHash() string { return h.svc.SpecHash }
func (h HPAComponent) Children() []Component { return nil }

func (h HPAComponent) Render(env *Env) []*unstructured.Unstructured {
	if h.svc.HPA == nil {
		return nil
	}
	hpaObj := toUnstructured(hpa(h.envNS, env, h.svc), "autoscaling", "v2", "HorizontalPodAutoscaler")
	annotate(hpaObj, h.svc.SpecHash)
	return []*unstructured.Unstructured{hpaObj}
}
