package serviceopenapi

import (
	"encoding/json"
	"testing"
)

const validSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "Widgets", "version": "1.0.0"},
  "paths": {
    "/widgets": {
      "get": {"operationId": "listWidgets", "responses": {"200": {"description": "ok"}}},
      "post": {"responses": {"201": {"description": "created"}}}
    },
    "/widgets/{id}": {
      "get": {"operationId": "getWidget", "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}], "responses": {"200": {"description": "ok"}}}
    }
  }
}`

func TestValidateAndIndex(t *testing.T) {
	s, err := ValidateAndIndex([]byte(validSpec))
	if err != nil {
		t.Fatalf("ValidateAndIndex: %v", err)
	}
	if s.OpCount != 3 || s.Title != "Widgets" {
		t.Errorf("summary = %+v", s)
	}
	byID := map[string]Target{}
	for _, tg := range s.Targets {
		byID[tg.ID] = tg
	}
	// operationId preferred
	if byID["listWidgets"].Path != "/widgets" || byID["listWidgets"].Method != "GET" {
		t.Errorf("listWidgets = %+v", byID["listWidgets"])
	}
	// fallback method+path when no operationId
	if byID["POST+/widgets"].Path != "/widgets" || byID["POST+/widgets"].OperationID != "" {
		t.Errorf("POST fallback = %+v", byID["POST+/widgets"])
	}
}

func TestValidateAndIndexErrors(t *testing.T) {
	if _, err := ValidateAndIndex([]byte(`{not json`)); err == nil {
		t.Error("malformed JSON must fail")
	}
	// semantically invalid: path parameter undeclared
	bad := `{"openapi":"3.0.0","info":{"title":"x","version":"1"},
	         "paths":{"/items/{id}":{"get":{"responses":{"200":{"description":"ok"}}}}}}`
	if _, err := ValidateAndIndex([]byte(bad)); err == nil {
		t.Error("undeclared path parameter must fail validation")
	}
}

func TestSpecHashStable(t *testing.T) {
	h1 := SpecHash([]byte(validSpec))
	h2 := SpecHash([]byte(validSpec))
	if h1 != h2 {
		t.Error("hash must be stable")
	}
	if h1 == SpecHash([]byte(validSpec+" ")) {
		t.Error("hash must differ on content change")
	}
}

// TestNormalizeYAMLToJSON — tbl_services.openapi_spec is JSONB, but OpenAPI
// documents are commonly YAML. Validation accepts YAML (kin-openapi), so
// storage must normalize it or the insert fails with SQLSTATE 22P02 (500).
func TestNormalizeYAMLToJSON(t *testing.T) {
	yaml := []byte(`openapi: 3.0.0
info:
  title: Sample
  version: 1.0.0
paths:
  /todos:
    get:
      operationId: listTodos
      responses:
        "200":
          description: ok
`)
	out, err := Normalize(yaml)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("normalized document is not JSON: %s", out)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.0.0" {
		t.Errorf("openapi = %v", doc["openapi"])
	}
	paths, _ := doc["paths"].(map[string]any)
	if _, ok := paths["/todos"]; !ok {
		t.Errorf("paths lost /todos: %v", doc["paths"])
	}
	// The normalized document must still validate (round-trip).
	if _, err := ValidateAndIndex(out); err != nil {
		t.Errorf("normalized document does not validate: %v", err)
	}
}

// TestNormalizeJSONIsStable — JSON input is passed through untouched so the
// spec hash does not change for existing services.
func TestNormalizeJSONIsStable(t *testing.T) {
	in := []byte(`{"openapi":"3.0.0","info":{"title":"T","version":"1"},"paths":{}}`)
	out, err := Normalize(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(in) {
		t.Errorf("JSON input was rewritten:\n in: %s\nout: %s", in, out)
	}
}

func TestNormalizeInvalid(t *testing.T) {
	if _, err := Normalize([]byte("openapi: [unclosed")); err == nil {
		t.Error("expected an error for an unparseable document")
	}
}
