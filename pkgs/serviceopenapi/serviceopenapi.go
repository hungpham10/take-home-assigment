// Package serviceopenapi parses/validates OpenAPI service specs and extracts
// the load-test targets[] index (operationId preferred, method+path fallback).
package serviceopenapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Target is one indexed operation — the atomic unit a load test may hit.
type Target struct {
	OperationID string `json:"operation_id,omitempty"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	// ID is the canonical reference: operationId when present, else METHOD+PATH.
	ID string `json:"id"`
}

// Summary is stored in tbl_services.parsed_summary (JSONB).
type Summary struct {
	Targets []Target `json:"targets"`
	Title   string   `json:"title"`
	Version string   `json:"version"`
	OpCount int      `json:"op_count"`
}

// ValidateAndIndex parses and validates an OpenAPI 3 spec, returning the
// parsed summary. All errors carry the reason (returned as 422 detail).
func ValidateAndIndex(spec []byte) (*Summary, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid OpenAPI document: %w", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		return nil, fmt.Errorf("OpenAPI validation failed: %w", err)
	}

	s := &Summary{Title: doc.Info.Title, Version: doc.Info.Version}
	seen := map[string]bool{}
	for path, pathItem := range doc.Paths.Map() {
		for method, op := range pathItem.Operations() {
			id := op.OperationID
			if id == "" {
				id = strings.ToUpper(method) + "+" + path
			}
			if seen[id] {
				return nil, fmt.Errorf("duplicate target id %q", id)
			}
			seen[id] = true
			s.Targets = append(s.Targets, Target{
				OperationID: op.OperationID,
				Method:      strings.ToUpper(method),
				Path:        path,
				ID:          id,
			})
		}
	}
	sort.Slice(s.Targets, func(i, j int) bool {
		return s.Targets[i].ID < s.Targets[j].ID
	})
	s.OpCount = len(s.Targets)
	return s, nil
}

// SpecHash is a stable content hash for revalidation checks.
func SpecHash(spec []byte) string {
	const hexDigits = "0123456789abcdef"
	var h uint64 = 14695981039346656037 // FNV-1a
	for _, b := range spec {
		h ^= uint64(b)
		h *= 1099511628211
	}
	var out [16]byte
	for i := 0; i < 16; i++ {
		out[i] = hexDigits[(h>>(uint(i%8)*8))&0xf]
	}
	return string(out[:])
}

// Marshal serializes the summary for JSONB storage.
func (s *Summary) Marshal() []byte {
	b, _ := json.Marshal(s)
	return b
}
