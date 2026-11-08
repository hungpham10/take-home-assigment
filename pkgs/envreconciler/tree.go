package envreconciler

// Hierarchical apply (plans/26 bổ sung): apply đi từ NGOÀI vào TRONG theo bao
// đóng — Namespace áp trước, rồi gọi tuần tự Apply() của các con
// (NetworkPolicy → database operator CR → per-service ConfigMap → Secret →
// ServiceAccount → Deployment → Service → HPA → Ingress). Mỗi node là một
// Applier — interface cụ thể với Apply() riêng; namespace gọi apply xuống con
// qua Children().

import (
	"context"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

// Applier là một node trong cây apply: mỗi object (hoặc nhóm object của một
// component) tự apply chính nó qua k8s.Client, rồi gọi Apply() của con.
type Applier interface {
	Kind() string
	Name() string
	// Apply áp object của node này (SAU khi cha đã áp — thứ tự ngoài→trong
	// được đảm bảo bởi cha gọi con sau khi self-apply).
	Apply(ctx context.Context, c k8s.Client) error
	Children() []Applier
}

// objectApplier áp đúng MỘT unstructured object.
type objectApplier struct {
	obj *unstructured.Unstructured
}

// stampObject ghi desired-generation vào annotation cho watcher fencing
// (WATCH-03) — thay cho stamping của flat-loop cũ.
func stampObject(obj *unstructured.Unstructured, env *Env) {
	if env.Generation > 0 {
		ann := obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[k8s.AnnotationDesiredGeneration] = strconv.FormatInt(env.Generation, 10)
		obj.SetAnnotations(ann)
	}
}

func (a objectApplier) Kind() string { return a.obj.GetKind() }
func (a objectApplier) Name() string { return a.obj.GetName() }
func (a objectApplier) Apply(ctx context.Context, c k8s.Client) error {
	return c.Apply(ctx, a.obj)
}
func (a objectApplier) Children() []Applier { return nil }

// groupApplier gom nhiều object của một component (service: CM→Secret→SA→
// Deployment→Service→HPA) — apply tuần tự trong group.
type groupApplier struct {
	kind     string
	name     string
	sequence []Applier
}

func (g groupApplier) Kind() string { return g.kind }
func (g groupApplier) Name() string { return g.name }
func (g groupApplier) Apply(ctx context.Context, c k8s.Client) error {
	for _, a := range g.sequence {
		if err := a.Apply(ctx, c); err != nil {
			return fmt.Errorf("envreconciler: %s/%s: %w", a.Kind(), a.Name(), err)
		}
	}
	return nil
}
func (g groupApplier) Children() []Applier { return g.sequence }

// namespaceApplier là ROOT: áp Namespace object (bao ngoài) trước, rồi gọi
// tuần tự Apply() của mọi con.
type namespaceApplier struct {
	self     objectApplier
	children []Applier
}

func (n namespaceApplier) Kind() string { return n.self.Kind() }
func (n namespaceApplier) Name() string { return n.self.Name() }
func (n namespaceApplier) Apply(ctx context.Context, c k8s.Client) error {
	if err := n.self.Apply(ctx, c); err != nil {
		return fmt.Errorf("envreconciler: namespace %s: %w", n.self.Name(), err)
	}
	for _, child := range n.children {
		if err := child.Apply(ctx, c); err != nil {
			return err
		}
	}
	return nil
}
func (n namespaceApplier) Children() []Applier { return n.children }

// BuildApplyTree dựng cây apply từ desired Env (thay thế flat Render+Apply —
// Render giữ lại cho prune và cho test shape).
func BuildApplyTree(env *Env) (Applier, error) {
	envNS := envNamespaceName(env.NamespaceID)

	var children []Applier
	// 2. NetworkPolicy (default-deny) — ngay sau namespace.
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
			"ingress":     []any{map[string]any{}},
			"egress":      renderEgress(env),
		},
	}, "networking.k8s.io", "v1", "NetworkPolicy")
	annotate(np, HashSpec(env.Policy))
	stampObject(np, env)
	children = append(children, objectApplier{obj: np})

	// 3. Database operator CRs (CNPG | ECK — registry, fail closed upstream).
	for _, db := range env.Databases {
		u, err := renderDatabaseCR(envNS, env, db)
		if err != nil {
			return nil, err
		}
		annotate(u, db.SpecHash)
		stampObject(u, env)
		children = append(children, objectApplier{obj: u})
	}

	// 6. Service group: ConfigMap → Secret → ServiceAccount → Deployment →
	// Service → HPA (trong group áp theo dependency, ngoài→trong).
	for _, svc := range env.Services {
		var seq []Applier
		add := func(obj map[string]any, group, version, kind string) {
			u := toUnstructured(obj, group, version, kind)
			annotate(u, svc.SpecHash)
			stampObject(u, env)
			seq = append(seq, objectApplier{obj: u})
		}
		if len(svc.ConfigMap) > 0 {
			add(configMap(envNS, env, svc), "", "v1", "ConfigMap")
		}
		if len(svc.Secrets) > 0 {
			add(secret(envNS, env, svc), "", "v1", "Secret")
		}
		if svc.ServiceAccount {
			add(serviceAccount(envNS, env, svc), "", "v1", "ServiceAccount")
		}
		add(deployment(envNS, env, svc), "apps", "v1", "Deployment")
		add(service(envNS, env, svc), "", "v1", "Service")
		if svc.HPA != nil {
			add(hpa(envNS, env, svc), "autoscaling", "v2", "HorizontalPodAutoscaler")
		}
		children = append(children, groupApplier{kind: "ServiceGroup", name: svc.Name, sequence: seq})
	}

	// 7. Ingress (tham chiếu service đã có).
	for _, ing := range env.Ingresses {
		u := toUnstructured(ingress(envNS, env, ing), "networking.k8s.io", "v1", "Ingress")
		annotate(u, ing.SpecHash)
		stampObject(u, env)
		children = append(children, objectApplier{obj: u})
	}

	self := objectApplier{obj: toUnstructured(map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": envNS, "labels": baseLabels(env)},
	}, "", "v1", "Namespace")}
	return namespaceApplier{self: self, children: children}, nil
}

// ApplyTree apply toàn bộ cây ngoài→trong.
func ApplyTree(ctx context.Context, c k8s.Client, env *Env) error {
	if err := ValidateDatabaseOperators(env); err != nil {
		return err
	}
	tree, err := BuildApplyTree(env)
	if err != nil {
		return err
	}
	return tree.Apply(ctx, c)
}
