package script

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
)

func jsonReader(v any) *bytes.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

var _ = httptest.NewRecorder
