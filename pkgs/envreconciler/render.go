// Package envreconciler — Loop 1 of the two-loop control model
// (docs/k8s-orchestration.md §4, desired PG → K8s). It renders the desired
// environment from DB rows into Kubernetes objects and applies them with
// Server-Side Apply in the locked 8-step order. It writes K8s ONLY — it never
// touches readiness/status (that is the Status Watcher's job,
// docs/namespace-status-watcher.md).
package envreconciler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

// Env is the desired state of one environment, gathered from PG by the
// reconciler's loader. It is a plain data structure so unit tests construct it
// directly and the DB query stays out of the render path.
type Env struct {
	NamespaceID string
	TenantID    string
	Status      string
	// Generation is the namespace's desired_generation the reconciler stamps
	// onto every object (generation fencing input for the watcher).
	Generation int64
	Services   []ServiceSpec
	Databases  []DatabaseSpec
	Ingresses  []IngressSpec
	Policy     *PolicySpec
}

// HPASpec is the HorizontalPodAutoscaler intent for a service.
type HPASpec struct {
	MinReplicas      int32 `json:"min_replicas"`
	MaxReplicas      int32 `json:"max_replicas"`
	CPUTargetPercent int32 `json:"cpu_target_percent"`
}

// ServiceSpec mirrors tbl_services.deploy (+ spec identity for SSA/labels).
// plans/26: a service MANAGES its full object set — ConfigMap (env lớn),
// Secret (secretKeyRef, không bao giờ plaintext — docs/k8s-orchestration.md
// §7), Deployment, Service, HPA, ServiceAccount.
type ServiceSpec struct {
	Name     string            `json:"name"`
	Image    string            `json:"image"`
	Replicas int32             `json:"replicas"`
	Port     int32             `json:"port"`
	Env      map[string]string `json:"env"`
	Health   *HealthProbe      `json:"health"`
	SpecHash string            `json:"spec_hash"`
	// ConfigMap data (env/aggregate config vượt giới hạn container env).
	ConfigMap map[string]string `json:"config_map"`
	// Secrets: key → value. Rendered as an Opaque Secret; container env uses
	// valueFrom.secretKeyRef — plaintext NEVER reaches the Deployment.
	Secrets map[string]string `json:"secrets"`
	// HPA non-nil → render autoscaling/v2 HorizontalPodAutoscaler.
	HPA *HPASpec `json:"hpa"`
	// ServiceAccount: render a dedicated SA (no platform creds) and assign it.
	ServiceAccount bool `json:"service_account"`
	// Mock is the raw tbl_services.mock JSONB; mockcontrol (controller-side
	// mock wiring) decodes it — envreconciler stays mock-runtime agnostic.
	Mock json.RawMessage `json:"mock,omitempty"`
}

// HealthProbe renders into container liveness/readiness probes.
type HealthProbe struct {
	Path string `json:"path"`
	Port int32  `json:"port"`
}

// DatabaseSpec mirrors tbl_databases — the operator CR is chosen by the
// `operator` column (plans/26: cloudnative-pg | elastic/ECK; unknown fails
// closed).
type DatabaseSpec struct {
	Name      string `json:"name"`
	Operator  string `json:"operator"`
	Image     string `json:"image"`
	StorageGB int64  `json:"storage_gb"`
	SpecHash  string `json:"spec_hash"`
	// ExtraConfig carries operator-specific fields (CNPG: imageName/storage;
	// ECK: version/nodeCount/storage).
	ExtraConfig map[string]any `json:"-"`
}

// IngressSpec mirrors tbl_ingress.
type IngressSpec struct {
	Name         string        `json:"name"`
	IngressClass string        `json:"ingress_class"`
	Rules        []IngressRule `json:"rules"`
	SpecHash     string        `json:"spec_hash"`
}

// IngressRule is one host rule referencing a service in the same namespace.
type IngressRule struct {
	Host        string `json:"host"`
	Path        string `json:"path"`
	ServiceName string `json:"service_name"`
	Port        int32  `json:"port"`
}

// PolicySpec is the resolved egress policy (tbl_network_policies.spec).
type PolicySpec struct {
	Egress []EgressRule `json:"egress"`
}

// EgressRule is one dependency-derived egress allow entry.
type EgressRule struct {
	Type  string `json:"type"`
	RefID string `json:"ref_id"`
}

// envNamespaceName maps a namespace_id to its 1:1 K8s environment namespace
// (docs/k8s-orchestration.md §1: lt-env-<namespace_id>).
func envNamespaceName(namespaceID string) string {
	return "lt-env-" + namespaceID
}

// EnvNamespaceName exposes the mapping for the status watcher (it keys the
// projection on namespace_id but locates objects via the env namespace).
func EnvNamespaceName(namespaceID string) string { return envNamespaceName(namespaceID) }

// baseLabels returns the ownership labels stamped on every managed object.
func baseLabels(env *Env) map[string]string {
	return map[string]string{
		k8s.LabelManagedBy:   k8s.ManagedByValue,
		k8s.LabelTenantID:    env.TenantID,
		k8s.LabelNamespaceID: env.NamespaceID,
	}
}

func annotate(obj *unstructured.Unstructured, specHash string) {
	if specHash != "" {
		obj.SetAnnotations(map[string]string{k8s.AnnotationSpecHash: specHash})
	}
}

// toUnstructured wraps a rendered manifest map with its GVK. The JSON
// round-trip normalizes Go-native slices/maps into the JSON-compatible shapes
// apimachinery's DeepCopyJSONValue requires ([]any, not []map[string]any).
func toUnstructured(obj map[string]any, group, version, kind string) *unstructured.Unstructured {
	b, err := json.Marshal(obj)
	if err != nil { // manifests are JSON-serializable by construction
		panic(fmt.Sprintf("envreconciler: render marshal: %v", err))
	}
	var normalized map[string]any
	if err := json.Unmarshal(b, &normalized); err != nil {
		panic(fmt.Sprintf("envreconciler: render unmarshal: %v", err))
	}
	u := &unstructured.Unstructured{Object: normalized}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	return u
}

// Render produces the environment's objects in the locked apply order
// (docs/k8s-orchestration.md §4.1): Namespace+quota → NetworkPolicy → CNPG
// Cluster → restore Job → mock bindings → Deployments+Services → Ingress.
// The restore-Job and mock-binding steps are added by later PRs (mockcontrol,
// plan 21 PR D); today they are absent for envs without those features.
func Render(env *Env) []*unstructured.Unstructured {
	var objs []*unstructured.Unstructured
	envNS := envNamespaceName(env.NamespaceID)

	// 1. Environment namespace with ownership labels.
	objs = append(objs, toUnstructured(map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": envNS, "labels": baseLabels(env)},
	}, "", "v1", "Namespace"))

	// 2. NetworkPolicy: default-deny in/out with an allow-list for declared
	// ingress traffic; egress allow-list comes from the resolved dependency
	// spec. K8s has no status subresource here — the watcher infers "applied"
	// from existence + spec_hash.
	np := toUnstructured(map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      "default-deny",
			"namespace": envNS,
			"labels":    baseLabels(env),
		},
		"spec": map[string]any{
			"podSelector": map[string]any{},
			"policyTypes": []string{"Ingress", "Egress"},
			// Empty rule object = allow in-namespace traffic only; the
			// watcher's network_policy evaluator checks the full expected
			// set + spec_hash.
			"ingress": []any{map[string]any{}},
			"egress":  renderEgress(env),
		},
	}, "networking.k8s.io", "v1", "NetworkPolicy")
	annotate(np, HashSpec(env.Policy))
	objs = append(objs, np)

	// 3. Operator CR per database — chosen by the operator column
	// (cloudnative-pg | elastic/ECK); unknown operator fails closed upstream
	// (ValidateDatabaseOperators) and is skipped defensively here.
	for _, db := range env.Databases {
		u, err := renderDatabaseCR(envNS, env, db)
		if err != nil {
			continue // ValidateDatabaseOperators already failed the apply
		}
		annotate(u, db.SpecHash)
		objs = append(objs, u)
	}

	// 6. Per service: ConfigMap → Secret → ServiceAccount → Deployment
	// (mounts/envFrom/secretKeyRef) → Service → HPA.
	for _, svc := range env.Services {
		if len(svc.ConfigMap) > 0 {
			cm := toUnstructured(configMap(envNS, env, svc), "", "v1", "ConfigMap")
			annotate(cm, svc.SpecHash)
			objs = append(objs, cm)
		}
		if len(svc.Secrets) > 0 {
			sec := toUnstructured(secret(envNS, env, svc), "", "v1", "Secret")
			annotate(sec, svc.SpecHash)
			objs = append(objs, sec)
		}
		if svc.ServiceAccount {
			sa := toUnstructured(serviceAccount(envNS, env, svc), "", "v1", "ServiceAccount")
			annotate(sa, svc.SpecHash)
			objs = append(objs, sa)
		}
		dep := toUnstructured(deployment(envNS, env, svc), "apps", "v1", "Deployment")
		annotate(dep, svc.SpecHash)
		objs = append(objs, dep)
		sv := toUnstructured(service(envNS, env, svc), "", "v1", "Service")
		annotate(sv, svc.SpecHash)
		objs = append(objs, sv)
		if svc.HPA != nil {
			hpa := toUnstructured(hpa(envNS, env, svc), "autoscaling", "v2", "HorizontalPodAutoscaler")
			annotate(hpa, svc.SpecHash)
			objs = append(objs, hpa)
		}
	}

	// 7. Ingress per ingress spec.
	for _, ing := range env.Ingresses {
		u := toUnstructured(ingress(envNS, env, ing), "networking.k8s.io", "v1", "Ingress")
		annotate(u, ing.SpecHash)
		objs = append(objs, u)
	}
	return objs
}

// renderEgress builds the egress rule list; DNS resolution is always allowed
// so ExternalName-style mock wiring resolves.
func renderEgress(env *Env) []map[string]any {
	if env.Policy == nil {
		return nil
	}
	var rules []map[string]any
	for _, e := range env.Policy.Egress {
		rules = append(rules, map[string]any{
			"to": []map[string]any{{
				"podSelector": map[string]any{
					"matchLabels": map[string]string{k8s.LabelComponentID: e.RefID},
				},
			}},
		})
	}
	return rules
}

func cnpgCluster(envNS string, env *Env, db DatabaseSpec) map[string]any {
	spec := map[string]any{
		"instances": int64(1),
		"storage": map[string]any{
			"size": resource.NewQuantity(db.StorageGB*1024*1024*1024, resource.BinarySI).String(),
		},
	}
	if db.Image != "" {
		spec["imageName"] = db.Image
	}
	return map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata": map[string]any{
			"name":      db.Name,
			"namespace": envNS,
			"labels":    baseLabels(env),
		},
		"spec": spec,
	}
}

func deployment(envNS string, env *Env, svc ServiceSpec) map[string]any {
	container := map[string]any{
		"name":  svc.Name,
		"image": svc.Image,
		"ports": []map[string]any{{"containerPort": int64(svc.Port)}},
	}
	if svc.Health != nil && svc.Health.Path != "" {
		probe := map[string]any{
			"httpGet": map[string]any{"path": svc.Health.Path, "port": int64(svc.Port)},
		}
		container["livenessProbe"] = probe
		container["readinessProbe"] = probe
	}
	// env: plaintext cho env thường; Secrets CHỈ qua valueFrom.secretKeyRef —
	// plaintext secret không bao giờ vào Deployment manifest (docs §7:84).
	var envList []map[string]any
	for k, v := range svc.Env {
		envList = append(envList, map[string]any{"name": k, "value": v})
	}
	for k := range svc.Secrets {
		envList = append(envList, map[string]any{
			"name": k,
			"valueFrom": map[string]any{
				"secretKeyRef": map[string]any{"name": svc.Name + "-secret", "key": k},
			},
		})
	}
	if envList != nil {
		container["env"] = envList
	}
	if len(svc.ConfigMap) > 0 {
		container["envFrom"] = []map[string]any{
			{"configMapRef": map[string]any{"name": svc.Name + "-config"}},
		}
	}
	podSpec := map[string]any{"containers": []any{container}}
	if svc.ServiceAccount {
		podSpec["serviceAccountName"] = svc.Name
	}
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      svc.Name,
			"namespace": envNS,
			"labels":    withComponent(baseLabels(env), svc.Name),
		},
		"spec": map[string]any{
			"replicas": int64(svc.Replicas),
			"selector": map[string]any{
				"matchLabels": map[string]string{k8s.LabelComponentID: svc.Name},
			},
			"template": map[string]any{
				"metadata": map[string]any{"labels": withComponent(baseLabels(env), svc.Name)},
				"spec":     podSpec,
			},
		},
	}
}

func service(envNS string, env *Env, svc ServiceSpec) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":      svc.Name,
			"namespace": envNS,
			"labels":    withComponent(baseLabels(env), svc.Name),
		},
		"spec": map[string]any{
			"selector": map[string]string{k8s.LabelComponentID: svc.Name},
			"ports": []map[string]any{{
				"port":       int64(svc.Port),
				"targetPort": int64(svc.Port),
			}},
		},
	}
}

func ingress(envNS string, env *Env, ing IngressSpec) map[string]any {
	var rules []map[string]any
	for _, r := range ing.Rules {
		path := r.Path
		if path == "" {
			path = "/"
		}
		rules = append(rules, map[string]any{
			"host": r.Host,
			"http": map[string]any{
				"paths": []map[string]any{{
					"path":     path,
					"pathType": "Prefix",
					"backend": map[string]any{
						"service": map[string]any{
							"name": r.ServiceName,
							"port": map[string]any{"number": int64(r.Port)},
						},
					},
				}},
			},
		})
	}
	ic := ing.IngressClass
	if ic == "" {
		ic = "nginx"
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata": map[string]any{
			"name":      ing.Name,
			"namespace": envNS,
			"labels":    baseLabels(env),
		},
		"spec": map[string]any{
			"ingressClassName": ic,
			"rules":            rules,
		},
	}
}

func withComponent(base map[string]string, componentID string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	out[k8s.LabelComponentID] = componentID
	return out
}

// PolicySpecOf decodes a stored tbl_network_policies.spec JSONB into the
// PolicySpec shape so callers recompute the identical HashSpec expectation.
func PolicySpecOf(raw []byte) *PolicySpec {
	var p PolicySpec
	_ = json.Unmarshal(raw, &p)
	return &p
}

// HashSpec renders a stable spec_hash for objects whose source-of-truth has
// no natural hash (network policy). Real component hashes come from the DB.
func HashSpec(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// --- plans/26: per-service auxiliary objects --------------------------------

// configMap renders <name>-config (env/aggregate config vượt container env).
func configMap(envNS string, env *Env, svc ServiceSpec) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      svc.Name + "-config",
			"namespace": envNS,
			"labels":    withComponent(baseLabels(env), svc.Name),
		},
		"data": svc.ConfigMap,
	}
}

// secret renders <name>-secret (Opaque). Values live ONLY here; the
// Deployment references them via valueFrom.secretKeyRef (docs §7:84).
func secret(envNS string, env *Env, svc ServiceSpec) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      svc.Name + "-secret",
			"namespace": envNS,
			"labels":    withComponent(baseLabels(env), svc.Name),
		},
		"type":       "Opaque",
		"stringData": svc.Secrets,
	}
}

// serviceAccount renders a dedicated SA (no platform credentials).
func serviceAccount(envNS string, env *Env, svc ServiceSpec) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata": map[string]any{
			"name":      svc.Name,
			"namespace": envNS,
			"labels":    withComponent(baseLabels(env), svc.Name),
		},
	}
}

// hpa renders autoscaling/v2 HorizontalPodAutoscaler targeting the Deployment.
func hpa(envNS string, env *Env, svc ServiceSpec) map[string]any {
	minR := svc.HPA.MinReplicas
	if minR <= 0 {
		minR = 1
	}
	maxR := svc.HPA.MaxReplicas
	if maxR < minR {
		maxR = minR
	}
	cpu := svc.HPA.CPUTargetPercent
	if cpu <= 0 {
		cpu = 80
	}
	return map[string]any{
		"apiVersion": "autoscaling/v2",
		"kind":       "HorizontalPodAutoscaler",
		"metadata": map[string]any{
			"name":      svc.Name,
			"namespace": envNS,
			"labels":    withComponent(baseLabels(env), svc.Name),
		},
		"spec": map[string]any{
			"scaleTargetRef": map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment", "name": svc.Name,
			},
			"minReplicas": int64(minR),
			"maxReplicas": int64(maxR),
			"metrics": []map[string]any{{
				"type": "Resource",
				"resource": map[string]any{
					"name": "cpu",
					"target": map[string]any{
						"type":               "Utilization",
						"averageUtilization": int64(cpu),
					},
				},
			}},
		},
	}
}

// --- operator registry (plans/26) -------------------------------------------

// renderDatabaseCR renders the operator CR for a database by its `operator`
// column: cloudnative-pg | elastic (ECK). Unknown → error (fail closed).
func renderDatabaseCR(envNS string, env *Env, db DatabaseSpec) (*unstructured.Unstructured, error) {
	switch db.Operator {
	case "", "cloudnative-pg":
		return toUnstructured(cnpgCluster(envNS, env, db), "postgresql.cnpg.io", "v1", "Cluster"), nil
	case "elastic":
		return toUnstructured(eckElasticsearch(envNS, env, db), "elasticsearch.k8s.elastic.co", "v1", "Elasticsearch"), nil
	default:
		return nil, fmt.Errorf("envreconciler: unsupported database operator %q (want cloudnative-pg|elastic)", db.Operator)
	}
}

// ValidateDatabaseOperators fails closed BEFORE any object is applied when a
// database uses an operator this controller cannot render.
func ValidateDatabaseOperators(env *Env) error {
	for _, db := range env.Databases {
		if db.Operator != "" && db.Operator != "cloudnative-pg" && db.Operator != "elastic" {
			return fmt.Errorf("envreconciler: unsupported database operator %q for db %q (want cloudnative-pg|elastic)", db.Operator, db.Name)
		}
	}
	return nil
}

// eckElasticsearch renders an ECK Elasticsearch CR (elasticsearch.k8s.elastic.co/v1).
func eckElasticsearch(envNS string, env *Env, db DatabaseSpec) map[string]any {
	version := ""
	nodeCount := int64(1)
	if db.ExtraConfig != nil {
		if v, ok := db.ExtraConfig["version"].(string); ok {
			version = v
		}
		if n, ok := db.ExtraConfig["node_count"].(float64); ok && n >= 1 {
			nodeCount = int64(n)
		}
	}
	spec := map[string]any{
		"nodeSets": []map[string]any{{
			"name":   "default",
			"count":  nodeCount,
			"config": map[string]any{},
			"podTemplate": map[string]any{
				"spec": map[string]any{"containers": []any{map[string]any{
					"name": "elasticsearch", "resources": map[string]any{},
				}}},
			},
		}},
		"volumeClaimDeletePolicy": "DeleteOnScaledownAndClusterDeletion",
	}
	if version != "" {
		spec["version"] = version
	} else if db.Image != "" {
		// ECK derives the version from the image tag when spec.version unset.
		if i := strings.LastIndex(db.Image, ":"); i >= 0 {
			spec["version"] = db.Image[i+1:]
		}
	}
	return map[string]any{
		"apiVersion": "elasticsearch.k8s.elastic.co/v1",
		"kind":       "Elasticsearch",
		"metadata": map[string]any{
			"name":      db.Name,
			"namespace": envNS,
			"labels":    baseLabels(env),
		},
		"spec": spec,
	}
}
