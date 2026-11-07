package namespace

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// captureWriter records status + body so idempotency can store the response.
type captureWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *captureWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.buf.Write(b)
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) bodyString() string { return c.buf.String() }

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func strPtr(s string) *string { return &s }

func jsonBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// trimEtagNormal normalizes "W/\"3\"" to "\"3\"".
func trimEtagNormal(s string) string { return strings.TrimPrefix(s, "W/") }
