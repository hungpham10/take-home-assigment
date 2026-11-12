package namespace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hungpham/loadtest-platform/pkgs/store"
)

func TestCheckIfMatch(t *testing.T) {
	cases := []struct {
		name   string
		ifM    string
		wantOK bool
		wantSt int
	}{
		{"no header takes latest", "", true, 200},
		{"matching etag", `"3"`, true, 200},
		{"weak etag", `W/"3"`, true, 200},
		{"stale etag", `"2"`, false, 412},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPatch, "/", nil)
			if tc.ifM != "" {
				req.Header.Set("If-Match", tc.ifM)
			}
			rec := httptest.NewRecorder()
			ok := checkIfMatch(rec, req, 3)
			if ok != tc.wantOK || rec.Code != tc.wantSt {
				t.Errorf("checkIfMatch = (%v, %d), want (%v, %d)", ok, rec.Code, tc.wantOK, tc.wantSt)
			}
		})
	}
}

func TestETagFormat(t *testing.T) {
	if got := etagFor(7); got != `"7"` {
		t.Errorf("etagFor(7) = %q", got)
	}
}

// TestValidateDatasetMetadataUnit covers the dataset request-shape validation
// (plan 18): S3-only location, PII enum, engine + restore_format enums.
func TestValidateDatasetMetadataUnit(t *testing.T) {
	valid := datasetRequest{
		Name:              "snap",
		Engine:            "postgres",
		Location:          json.RawMessage(`{"provider":"s3","bucket":"b","key":"k.dump"}`),
		PIIClassification: "masked",
		RestoreFormat:     "pg_dump_custom",
	}
	if err := validateDatasetMetadata(valid); err != nil {
		t.Fatalf("valid rejected: %v", err)
	}
	for name, mutate := range map[string]func(*datasetRequest){
		"missing name":         func(r *datasetRequest) { r.Name = "" },
		"engine mysql":         func(r *datasetRequest) { r.Engine = "mysql" },
		"pii out of enum":      func(r *datasetRequest) { r.PIIClassification = "public" },
		"location provider gs": func(r *datasetRequest) { r.Location = json.RawMessage(`{"provider":"gs","bucket":"b","key":"k"}`) },
		"location missing key": func(r *datasetRequest) { r.Location = json.RawMessage(`{"provider":"s3","bucket":"b"}`) },
		"location absent":      func(r *datasetRequest) { r.Location = nil },
		"restore_format tar":   func(r *datasetRequest) { r.RestoreFormat = "tar" },
	} {
		req := valid
		mutate(&req)
		if err := validateDatasetMetadata(req); err == nil {
			t.Errorf("%s: expected 422 detail, got nil", name)
		}
	}
}

// TestValidatePIIMetadataUnit covers the PII metadata self-check placeholder.
func TestValidatePIIMetadataUnit(t *testing.T) {
	pg := "postgres"
	ds := store.Dataset{PIIClassification: "synthetic", Engine: &pg}
	if reasons := validatePIIMetadata(ds); len(reasons) != 0 {
		t.Errorf("valid dataset rejected: %v", reasons)
	}
	bad := store.Dataset{PIIClassification: "raw", Engine: &pg}
	if reasons := validatePIIMetadata(bad); len(reasons) != 1 {
		t.Errorf("invalid pii_classification: %v", reasons)
	}
	mysql := "mysql"
	if reasons := validatePIIMetadata(store.Dataset{PIIClassification: "masked", Engine: &mysql}); len(reasons) != 1 {
		t.Errorf("mysql dataset: %v", reasons)
	}
}
