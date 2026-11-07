package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteErrorEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusConflict, "INVALID_TRANSITION", "nope")
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	var body struct {
		Error Error `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "INVALID_TRANSITION" {
		t.Errorf("code = %q", body.Error.Code)
	}
}

func TestWriteCodeErrorMapping(t *testing.T) {
	cases := map[string]int{
		"NAMESPACE_NOT_READY":  409,
		"SCRIPT_NOT_VALIDATED": 409,
		"QUOTA_EXCEEDED":       429,
		"VALIDATION_FAILED":    422,
		"OBJECT_NOT_FOUND":     404,
		"UNKNOWN_CODE":         500,
	}
	for code, want := range cases {
		rec := httptest.NewRecorder()
		WriteCodeError(rec, code, "m")
		if rec.Code != want {
			t.Errorf("code %s: status = %d, want %d", code, rec.Code, want)
		}
	}
}
