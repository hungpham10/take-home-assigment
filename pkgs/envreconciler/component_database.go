package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DatabaseComponent là leaf, đa hình theo operator (cloudnative-pg → Cluster,
// elastic → Elasticsearch). Render object CR của database.
type DatabaseComponent struct {
	envNS string
	env   *Env
	db    DatabaseSpec
}

// Kind returns the Kubernetes kind of the component object.
func (d DatabaseComponent) Kind() string {
	if d.db.Operator == "elastic" {
		return "Elasticsearch"
	}
	return "Cluster"
}

// Name returns the object name.
func (d DatabaseComponent) Name() string { return d.db.Name }

// Labels returns the ownership labels stamped on the object.
func (d DatabaseComponent) Labels() map[string]string {
	return baseLabels(d.env)
}

// SpecHash returns the spec hash annotation value.
func (d DatabaseComponent) SpecHash() string { return d.db.SpecHash }

// Children returns nil — DatabaseComponent is a leaf.
func (d DatabaseComponent) Children() []Component { return nil }

// Render renders the database operator CR manifest.
func (d DatabaseComponent) Render(env *Env) []*unstructured.Unstructured {
	u, err := renderDatabaseCR(d.envNS, env, d.db)
	if err != nil {
		// ValidateDatabaseOperators đã fail closed trước khi ApplyTree chạy.
		return nil
	}
	annotate(u, d.db.SpecHash)
	return []*unstructured.Unstructured{u}
}
