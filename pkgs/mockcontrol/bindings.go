// Package mockcontrol — controller-side mock wiring (plans/21 PR D,
// docs/k8s-orchestration.md §8). It renders the snapshot binding ConfigMap
// (`mock-binding-<component_id>`, carrying schema_version/generation), the
// ExternalName Service pointing SUT traffic at the shared mock runtime, and
// injects `MOCK_<COMPONENT>_URL` into SUT container env. The mock runtime
// itself lives in pkgs/mockruntime/cmds/mock-service and never writes PG.
package mockcontrol

import (
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/mockruntime"
)

// MockDef is the mock section of a component spec (tbl_services.mock /
// tbl_databases.mock, docs/mock-service-api.md §2).
type MockDef struct {
	Enabled    bool                `json:"enabled"`
	Generation int64               `json:"generation"`
	PoolClass  string              `json:"pool_class"` // general | perf-isolated
	Routes     []mockruntime.Route `json:"routes"`
}

// DecodeMockDef parses the raw component mock JSONB (nil/empty → nil).
func DecodeMockDef(raw json.RawMessage) *MockDef {
	if len(raw) == 0 {
		return nil
	}
	var m MockDef
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return &m
}

// MockServiceDNS is the cluster-internal DNS name of the shared mock runtime
// (env namespaces reach it via ExternalName — no public ingress, MOCK-VER-01).
func MockServiceDNS() string { return "mock-service.mock-system.svc.cluster.local" }

// MockServiceURL is the HTTP base for env injection.
func MockServiceURL() string { return "http://" + MockServiceDNS() }

// envNSName mirrors envreconciler's namespace mapping without an import cycle.
func envNSName(namespaceID string) string { return envreconciler.EnvNamespaceName(namespaceID) }

// DecodeSnapshot validates the binding payload header.
func DecodeSnapshot(b []byte) (*mockruntime.Snapshot, error) {
	var s mockruntime.Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("mockcontrol: decode snapshot: %w", err)
	}
	if s.SchemaVersion != mockruntime.SchemaVersion {
		return nil, fmt.Errorf("mockcontrol: schema_version %q (want %s)", s.SchemaVersion, mockruntime.SchemaVersion)
	}
	return &s, nil
}

// ValidateLabels guards the ownership label convention on mock objects.
func ValidateLabels(labels map[string]string) error {
	if labels[k8s.LabelManagedBy] != k8s.ManagedByValue {
		return errors.New("mockcontrol: object missing managed-by label")
	}
	return nil
}

// RenderBindings produces, per mock-enabled component:
//   - ConfigMap mock-binding-<component_id> with snapshot.json (versioned
//     desired config the mock runtime mounts and the watcher keys on)
//   - Service mock-<component_id> (ExternalName) for cluster-internal access
func RenderBindings(env *envreconciler.Env) []*unstructured.Unstructured {
	var objs []*unstructured.Unstructured
	envNS := envNSName(env.NamespaceID)
	for _, svc := range env.Services {
		mock := DecodeMockDef(svc.Mock)
		if mock == nil || !mock.Enabled {
			continue
		}
		snap := &mockruntime.Snapshot{
			SchemaVersion: mockruntime.SchemaVersion,
			NamespaceID:   env.NamespaceID,
			Generation:    mock.Generation,
			Routes:        mock.Routes,
		}
		snapJSON, err := json.Marshal(snap)
		if err != nil {
			continue // snapshot must be marshalable by construction
		}
		labels := map[string]string{
			k8s.LabelManagedBy:   k8s.ManagedByValue,
			k8s.LabelTenantID:    env.TenantID,
			k8s.LabelNamespaceID: env.NamespaceID,
			k8s.LabelComponentID: svc.Name,
		}
		bindingName := "mock-binding-" + svc.Name
		objs = append(objs, toUnstructured(map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":        bindingName,
				"namespace":   envNS,
				"labels":      labels,
				"annotations": map[string]string{k8s.AnnotationSpecHash: fmt.Sprintf("mock-gen-%d", mock.Generation)},
			},
			"data": map[string]any{
				"snapshot.json": string(snapJSON),
				"pool_class":    poolClass(mock.PoolClass),
			},
		}, "", "v1", "ConfigMap"))
		objs = append(objs, toUnstructured(map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]any{
				"name":      "mock-" + svc.Name,
				"namespace": envNS,
				"labels":    labels,
			},
			"spec": map[string]any{
				"type":         "ExternalName",
				"externalName": MockServiceDNS(),
			},
		}, "", "v1", "Service"))
	}
	return objs
}

// InjectMockEnv returns the reconciler's deployment objects with
// MOCK_<COMPONENT>_URL added for mock-enabled components.
func InjectMockEnv(env *envreconciler.Env) []*unstructured.Unstructured {
	objs := envreconciler.Render(env)
	mockNames := map[string]bool{}
	for _, svc := range env.Services {
		if mock := DecodeMockDef(svc.Mock); mock != nil && mock.Enabled {
			mockNames[svc.Name] = true
		}
	}
	if len(mockNames) == 0 {
		return objs
	}
	for _, obj := range objs {
		if obj.GetKind() != "Deployment" || !mockNames[obj.GetName()] {
			continue
		}
		injectContainerEnv(obj, mockEnvVarName(obj.GetName()), MockServiceURL())
	}
	return objs
}

// injectContainerEnv appends one env var to the deployment's first container.
func injectContainerEnv(dep *unstructured.Unstructured, name, value string) {
	containers, found, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	if !found || len(containers) == 0 {
		return
	}
	c0, ok := containers[0].(map[string]any)
	if !ok {
		return
	}
	envList, _, _ := unstructured.NestedSlice(c0, "env")
	envList = append(envList, map[string]any{"name": name, "value": value})
	c0["env"] = envList
	containers[0] = c0
	_ = unstructured.SetNestedSlice(dep.Object, containers, "spec", "template", "spec", "containers")
}

// MockEnvVarName builds MOCK_<COMPONENT>_URL (upper snake).
func mockEnvVarName(component string) string {
	out := "MOCK_"
	for _, r := range component {
		if r >= 'a' && r <= 'z' {
			r -= 32
		}
		if r == '-' || r == '.' {
			r = '_'
		}
		out += string(r)
	}
	return out + "_URL"
}

func poolClass(c string) string {
	if c == "" {
		return "general"
	}
	return c
}

func toUnstructured(obj map[string]any, group, version, kind string) *unstructured.Unstructured {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(fmt.Sprintf("mockcontrol: marshal manifest: %v", err))
	}
	var normalized map[string]any
	_ = json.Unmarshal(b, &normalized)
	u := &unstructured.Unstructured{Object: normalized}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	return u
}

// Attach wires the mock hooks onto the reconciler: SUT env injection +
// binding objects (apply step 5, docs/k8s-orchestration.md §4.1).
func Attach(rc *envreconciler.Reconciler) {
	rc.Attach(InjectMockEnv, RenderBindings)
}
