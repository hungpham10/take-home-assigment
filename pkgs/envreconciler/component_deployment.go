package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DeploymentComponent render Deployment (mounts/envFrom/secretKeyRef).
type DeploymentComponent struct {
	envNS string
	env   *Env
	svc   ServiceSpec
}

// Kind returns the Kubernetes kind of the component object.
func (d DeploymentComponent) Kind() string { return "Deployment" }

// Name returns the object name.
func (d DeploymentComponent) Name() string { return d.svc.Name }

// Labels returns the ownership labels stamped on the object.
func (d DeploymentComponent) Labels() map[string]string {
	return withComponent(baseLabels(d.env), d.svc.Name)
}

// SpecHash returns the spec hash annotation value.
func (d DeploymentComponent) SpecHash() string { return d.svc.SpecHash }

// Children returns nil — DeploymentComponent is a leaf.
func (d DeploymentComponent) Children() []Component { return nil }

// Render renders the Deployment manifest.
func (d DeploymentComponent) Render(env *Env) []*unstructured.Unstructured {
	dep := toUnstructured(deployment(d.envNS, env, d.svc), "apps", "v1", "Deployment")
	annotate(dep, d.svc.SpecHash)
	return []*unstructured.Unstructured{dep}
}
