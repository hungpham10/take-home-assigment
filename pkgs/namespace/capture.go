package namespace

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(r.Body)
}

func readCloser(b []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(b))
}

func marshalJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
