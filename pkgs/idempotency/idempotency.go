// Package idempotency provides the shared mandatory-Idempotency-Key
// middleware and response-replay middleware backed by tbl_idempotency_keys
// (docs contract: POST /namespaces, /apply, /loadtests, /stop, /scripts).
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// RequireKey enforces the mandatory Idempotency-Key header.
func RequireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == "" {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "Idempotency-Key header is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Replay wraps next with response replay for (tenant, endpoint, key):
// identical request hash → replay stored response (Idempotency-Replayed);
// different payload → 422; in-flight duplicate → 409.
func Replay(endpoint string, db *gorm.DB, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := authz.PrincipalOf(r)
		key := r.Header.Get("Idempotency-Key")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			httpx.WriteCodeError(w, "BAD_REQUEST", "unreadable request body")
			return
		}
		sum := sha256.Sum256(body)
		reqHash := hex.EncodeToString(sum[:])

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var stored store.IdempotencyKey
		err = db.WithContext(ctx).
			Where("tenant_id = ? AND endpoint = ? AND key = ?", p.TenantID, endpoint, key).
			First(&stored).Error
		switch {
		case err == nil && stored.RequestHash != reqHash:
			httpx.WriteCodeError(w, "VALIDATION_FAILED",
				"Idempotency-Key reused with different request payload")
			return
		case err == nil && stored.ResponseStatus != nil && stored.ResponseBody != nil:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotency-Replayed", "true")
			w.WriteHeader(*stored.ResponseStatus)
			_, _ = w.Write([]byte(*stored.ResponseBody))
			return
		case err == nil:
			// In-flight duplicate: first request still processing.
			httpx.WriteCodeError(w, "INVALID_TRANSITION", "request with this Idempotency-Key is in progress")
			return
		}

		rec := &captureWriter{ResponseWriter: w}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next(rec, r)

		if rec.status >= 200 && rec.status < 500 {
			_ = db.WithContext(ctx).Create(&store.IdempotencyKey{
				TenantID:       p.TenantID,
				Endpoint:       endpoint,
				Key:            key,
				RequestHash:    reqHash,
				ResponseStatus: &rec.status,
				ResponseBody:   store.StrPtr(rec.bodyString()),
			}).Error
		}
	}
}

// captureWriter records status + body for replay storage.
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
