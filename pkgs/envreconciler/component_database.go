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

func (d DatabaseComponent) Kind() string {
	if d.db.Operator == "elastic" {
		return "Elasticsearch"
	}
	return "Cluster"
}
func (d DatabaseComponent) Name() string { return d.db.Name }
func (d DatabaseComponent) Labels() map[string]string {
	return baseLabels(d.env)
}
func (d DatabaseComponent) SpecHash() string { return d.db.SpecHash }
func (d DatabaseComponent) Children() []Component { return nil }

func (d DatabaseComponent) Render(env *Env) []*unstructured.Unstructured {
	u, err := renderDatabaseCR(d.envNS, env, d.db)
	if err != nil {
		// ValidateDatabaseOperators đã fail closed trước khi ApplyTree chạy.
		return nil
	}
	annotate(u, d.db.SpecHash)
	return []*unstructured.Unstructured{u}
}
