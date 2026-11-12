package envreconciler

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

// DatabasePolicyComponent render the per-database NetworkPolicy (ingress
// side): allow traffic into the database pod from the same-namespace pods of
// **bound services only** on the engine port (postgres 5432, ECK 9200). Egress
// from a service to the DB is governed by that service's per-service egress
// policy (component_service_egress_policy.go).
//
// A service is bound when it declared database_bindings referencing this DB.
// Without a binding, the service cannot reach the DB (L4 deny).
//
// Intra-cluster replication and operator→cluster traffic are operator-managed
// and intentionally out of scope here.
type DatabasePolicyComponent struct {
	envNS string
	env   *Env
	db    DatabaseSpec
}

// NewDatabasePolicyComponent builds a DatabasePolicyComponent.
func NewDatabasePolicyComponent(envNS string, env *Env, db DatabaseSpec) DatabasePolicyComponent {
	return DatabasePolicyComponent{envNS: envNS, env: env, db: db}
}

// Kind returns the Kubernetes kind of the component object.
func (d DatabasePolicyComponent) Kind() string { return "NetworkPolicy" }

// Name returns the object name.
func (d DatabasePolicyComponent) Name() string { return d.db.Name + "-policy" }

// Labels returns the ownership labels stamped on the object.
func (d DatabasePolicyComponent) Labels() map[string]string {
	return withComponent(baseLabels(d.env), d.db.Name)
}

// SpecHash returns the spec hash annotation value.
func (d DatabasePolicyComponent) SpecHash() string { return d.db.SpecHash }

// Children returns nil — DatabasePolicyComponent is a leaf.
func (d DatabasePolicyComponent) Children() []Component { return nil }

// dbPort returns the engine listen port.
func dbPort(db DatabaseSpec) int32 {
	if db.Operator == "elastic" {
		return 9200
	}
	return 5432
}

// Render renders a NetworkPolicy that allows ingress to the database pod from
// bound services' pods only, on the engine port. If no services are bound,
// the ingress rule list is empty (deny-all).
func (d DatabasePolicyComponent) Render(_ *Env) []*unstructured.Unstructured {
	ingressRules := []any{}
	for _, bound := range d.db.BoundServices {
		ingressRules = append(ingressRules, map[string]any{
			"from": []any{map[string]any{
				"podSelector": map[string]any{
					"matchLabels": map[string]string{k8s.LabelComponentID: bound},
				},
			}},
			"ports": []map[string]any{{"protocol": "TCP", "port": int64(dbPort(d.db))}},
		})
	}

	u := toUnstructured(map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      d.Name(),
			"namespace": d.envNS,
			"labels":    d.Labels(),
		},
		"spec": map[string]any{
			"podSelector": map[string]any{
				"matchLabels": map[string]string{k8s.LabelComponentID: d.db.Name},
			},
			"policyTypes": []string{"Ingress"},
			"ingress":     ingressRules,
		},
	}, "networking.k8s.io", "v1", "NetworkPolicy")
	annotate(u, d.db.SpecHash)
	return []*unstructured.Unstructured{u}
}
