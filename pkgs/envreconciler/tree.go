package envreconciler

import (
	"context"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hungpham/loadtest-platform/pkgs/k8s"
)

// BuildComponentTree dựng cây Component từ desired Env. Apply order
// (bên ngoài → bên trong, khóa theo INT-ENV-07):
//
//	Environment (Namespace)
//	├── NetworkPolicy (default-deny)
//	├── Service policies (per-service ingress allow)
//	├── Service egress policies (per-service egress allow, declared deps only)
//	├── Database policies (per-database ingress allow, bound services only)
//	├── Database operator CRs (CNPG Cluster | ECK Elasticsearch)
//	├── Service (composite): ConfigMap → Secret → ServiceAccount → Deployment
//	│                          → Service → HPA
//	├── Ingress
//	└── MockBinding
//
// Policies apply before workloads so there is no unenforced window.
// Render stays pure (no I/O); desired_generation is stamped at apply time
// via applyComponent (WATCH-03 generation fencing).
func BuildComponentTree(env *Env) (Component, error) {
	envNS := envNamespaceName(env.NamespaceID)

	var children []Component

	// 2. NetworkPolicy (default-deny) — ngay sau namespace.
	children = append(children, NetworkPolicyComponent{
		envNS:    envNS,
		env:      env,
		specHash: HashSpec(env.Policy),
	})

	// 3. Per-service ingress policies (allow same-namespace → service port).
	for _, svc := range env.Services {
		children = append(children, ServicePolicyComponent{
			envNS: envNS,
			env:   env,
			svc:   svc,
		})
	}

	// 4. Per-service egress policies (allow service → declared deps only).
	for _, svc := range env.Services {
		children = append(children, ServiceEgressPolicyComponent{
			envNS: envNS,
			env:   env,
			svc:   svc,
		})
	}

	// 5. Per-database ingress policies (allow bound services → engine port).
	for _, db := range env.Databases {
		children = append(children, DatabasePolicyComponent{
			envNS: envNS,
			env:   env,
			db:    db,
		})
	}

	// 5. Database operator CRs (CNPG | ECK).
	for _, db := range env.Databases {
		children = append(children, DatabaseComponent{
			envNS: envNS,
			env:   env,
			db:    db,
		})
	}

	// 6. Service components (composite).
	for _, svc := range env.Services {
		children = append(children, ServiceComponent{
			envNS: envNS,
			env:   env,
			svc:   svc,
		})
	}

	// 7. Ingress (tham chiếu service đã có).
	for _, ing := range env.Ingresses {
		children = append(children, IngressComponent{
			envNS:    envNS,
			env:      env,
			ing:      ing,
			specHash: ing.SpecHash,
		})
	}

	// Mock binding như một child bình thường (bỏ hook BindingsFunc riêng).
	children = append(children, MockBindingComponent{
		envNS: envNS,
		env:   env,
	})

	return EnvironmentComponent{
		envNS:    envNS,
		env:      env,
		children: children,
	}, nil
}

// RenderAll thu gom tất cả object của một component và các cháu (dùng cho
// prune + Watcher expected-state). Render() của mỗi node chỉ trả về object
// của chính nó; RenderAll đi sâu qua Children().
func RenderAll(comp Component, env *Env) []*unstructured.Unstructured {
	var objs []*unstructured.Unstructured
	objs = append(objs, comp.Render(env)...)
	for _, child := range comp.Children() {
		objs = append(objs, RenderAll(child, env)...)
	}
	return objs
}

// ApplyTree validate, BuildComponentTree, rồi apply toàn bộ cây ngoài→trong qua
// một DFS duy nhất. Tên hàm này được giữ (không dùng applyDFS) để thân với
// code cũ đang gọi ApplyTree.
func ApplyTree(ctx context.Context, c k8s.Client, env *Env) error {
	if err := ValidateDatabaseOperators(env); err != nil {
		return err
	}
	tree, err := BuildComponentTree(env)
	if err != nil {
		return err
	}
	return applyComponent(ctx, c, env, tree)
}

// applyComponent áp object của chính node (từ Render), sau đó đệ quy vào các
// con. Thứ tự tự nhiên đảm bảo ngoài→trông: cha xong mới đến con.
func applyComponent(ctx context.Context, c k8s.Client, env *Env, comp Component) error {
	for _, obj := range comp.Render(env) {
		stampObject(obj, env)
		if err := c.Apply(ctx, obj); err != nil {
			return fmt.Errorf("envreconciler: %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	for _, child := range comp.Children() {
		if err := applyComponent(ctx, c, env, child); err != nil {
			return err
		}
	}
	return nil
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
