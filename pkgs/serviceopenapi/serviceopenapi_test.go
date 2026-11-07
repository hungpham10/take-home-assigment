package serviceopenapi

import "testing"

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
	if SpecHash([]byte(validSpec)) != SpecHash([]byte(validSpec)) {
		t.Error("hash must be stable")
	}
	if SpecHash([]byte(validSpec)) == SpecHash([]byte(validSpec+" ")) {
		t.Error("hash must differ on content change")
	}
}
