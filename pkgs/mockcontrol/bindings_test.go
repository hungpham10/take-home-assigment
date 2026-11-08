package mockcontrol_test

import (
	"encoding/json"
	"testing"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/mockcontrol"
	"github.com/hungpham/loadtest-platform/pkgs/mockruntime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mockEnv(t *testing.T) *envreconciler.Env {
	return &envreconciler.Env{
		NamespaceID: "11111111-2222-3333-4444-555555555555", TenantID: "t-1", Generation: 3,
		Services: []envreconciler.ServiceSpec{{
			Name: "checkout", Image: "img:1", Replicas: 1, Port: 8080,
			Mock: mustJSON(t, mockcontrol.MockDef{
				Enabled: true, Generation: 3,
				Routes: []mockruntime.Route{{
					Method: "GET", Path: "/widgets",
					Variants: []mockruntime.Variant{{Weight: 1, Status: 200}},
				}},
			}),
		}, {
			Name: "plain", Image: "img:2", Replicas: 1, Port: 8080, // no mock
		}},
	}
}

func find(objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	return nil
}

func TestRenderBindings(t *testing.T) {
	env := mockEnv(t)
	objs := mockcontrol.RenderBindings(env)

	// One binding ConfigMap for the mock-enabled service, none for the other.
	cm := find(objs, "ConfigMap", "mock-binding-checkout")
	if cm == nil {
		t.Fatal("binding ConfigMap for checkout missing")
	}
	data, found, _ := unstructured.NestedMap(cm.Object, "data")
	if !found {
		t.Fatal("configmap without data")
	}
	snapJSON, _ := data["snapshot.json"].(string)
	if snapJSON == "" {
		t.Fatalf("snapshot.json missing: %v", data)
	}
	snap, err := mockcontrol.DecodeSnapshot([]byte(snapJSON))
	if err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Generation != 3 || snap.SchemaVersion != mockruntime.SchemaVersion {
		t.Fatalf("snapshot header: %+v", snap)
	}
	if len(snap.Routes) != 1 {
		t.Fatalf("routes: %+v", snap.Routes)
	}
	if find(objs, "ConfigMap", "mock-binding-plain") != nil {
		t.Fatal("binding rendered for non-mock service")
	}

	// ExternalName Service pointing SUT at the shared mock runtime.
	svc := find(objs, "Service", "mock-checkout")
	if svc == nil {
		t.Fatal("ExternalName service missing")
	}
	ext, found, _ := unstructured.NestedString(svc.Object, "spec", "externalName")
	if !found || ext == "" {
		t.Fatalf("externalName: %v", svc.Object["spec"])
	}
}

func TestMockURLEnvInjection(t *testing.T) {
	env := mockEnv(t)
	// The envreconciler renders SUT deployments; mockcontrol injects
	// MOCK_<COMPONENT>_URL into the container env.
	objs := mockcontrol.InjectMockEnv(env)
	dep := find(objs, "Deployment", "checkout")
	if dep == nil {
		t.Fatal("deployment missing")
	}
	containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		t.Fatal("no containers")
	}
	c0 := containers[0].(map[string]any)
	envs, _, _ := unstructured.NestedSlice(c0, "env")
	found := false
	for _, e := range envs {
		m := e.(map[string]any)
		if m["name"] == "MOCK_CHECKOUT_URL" && m["value"] != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("MOCK_CHECKOUT_URL not injected: %v", envs)
	}
}

func TestSnapshotSchemaValidation(t *testing.T) {
	if _, err := mockcontrol.DecodeSnapshot([]byte(`{"schema_version":"v9"}`)); err == nil {
		t.Fatal("bad schema_version accepted")
	}
	var lbls = map[string]string{}
	if err := mockcontrol.ValidateLabels(map[string]string{
		k8s.LabelManagedBy: k8s.ManagedByValue,
	}); err != nil {
		t.Fatal(err)
	}
	_ = lbls
}
