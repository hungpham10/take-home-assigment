package adminapi

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// newToken returns the random secret part of an abt_ token (prefix stays
// outside so the DB prefix column = first 8 chars of the secret).
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("adminapi: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
