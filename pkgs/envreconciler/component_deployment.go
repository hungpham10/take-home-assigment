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

func (d DeploymentComponent) Kind() string { return "Deployment" }
func (d DeploymentComponent) Name() string { return d.svc.Name }
func (d DeploymentComponent) Labels() map[string]string {
	return withComponent(baseLabels(d.env), d.svc.Name)
}
func (d DeploymentComponent) SpecHash() string { return d.svc.SpecHash }
func (d DeploymentComponent) Children() []Component { return nil }

func (d DeploymentComponent) Render(env *Env) []*unstructured.Unstructured {
	dep := toUnstructured(deployment(d.envNS, env, d.svc), "apps", "v1", "Deployment")
	annotate(dep, d.svc.SpecHash)
	return []*unstructured.Unstructured{dep}
}
