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

// Kind returns the Kubernetes kind of the component object.
func (h HPAComponent) Kind() string { return "HorizontalPodAutoscaler" }

// Name returns the object name.
func (h HPAComponent) Name() string { return h.svc.Name }

// Labels returns the ownership labels stamped on the object.
func (h HPAComponent) Labels() map[string]string {
	return withComponent(baseLabels(h.env), h.svc.Name)
}

// SpecHash returns the spec hash annotation value.
func (h HPAComponent) SpecHash() string { return h.svc.SpecHash }

// Children returns nil — HPAComponent is a leaf.
func (h HPAComponent) Children() []Component { return nil }

// Render renders the HorizontalPodAutoscaler manifest.
func (h HPAComponent) Render(env *Env) []*unstructured.Unstructured {
	if h.svc.HPA == nil {
		return nil
	}
	hpaObj := toUnstructured(hpa(h.envNS, env, h.svc), "autoscaling", "v2", "HorizontalPodAutoscaler")
	annotate(hpaObj, h.svc.SpecHash)
	return []*unstructured.Unstructured{hpaObj}
}
