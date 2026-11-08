package namespace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/serviceopenapi"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// --- create-with-spec request shape (docs/namespace-management-api.md §create) ----

type openapiSpec struct {
	Format      string `json:"format"`
	Version     string `json:"version"`
	Source      string `json:"source"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"`
}

type serviceSpec struct {
	ServiceID string `json:"service_id"`
	// Spec is the LEGACY raw-OpenAPI field ({"name","spec"} shape); it is
	// normalized onto OpenAPI.Content by normalizeOpenAPI before validation.
	Spec             json.RawMessage `json:"spec"`
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	Deploy           json.RawMessage `json:"deploy"`
	Mock             json.RawMessage `json:"mock"`
	DatabaseBindings json.RawMessage `json:"database_bindings"`
	Dependencies     json.RawMessage `json:"dependencies"`
	OpenAPI          *openapiSpec    `json:"openapi"`
}

type databaseSpec struct {
	DatabaseID    string          `json:"database_id"`
	Name          string          `json:"name"`
	Engine        string          `json:"engine"`
	Operator      string          `json:"operator"`
	Version       string          `json:"version"`
	Config        json.RawMessage `json:"config"`
	InitScriptRef string          `json:"init_script_ref"`
	DataSource    json.RawMessage `json:"data_source"`
	RestorePolicy json.RawMessage `json:"restore_policy"`
	Ephemeral     json.RawMessage `json:"ephemeral"`
	Mock          json.RawMessage `json:"mock"`
	SecretRef     string          `json:"secret_ref"`
}

type ingressSpec struct {
	IngressID string          `json:"ingress_id"`
	Name      string          `json:"name"`
	Class     string          `json:"class"`
	Hosts     json.RawMessage `json:"hosts"`
}

type dependencyPolicy struct {
	Mode     string         `json:"mode"`
	Defaults map[string]any `json:"defaults"`
}

type namespaceSpec struct {
	Services         []serviceSpec     `json:"services"`
	Databases        []databaseSpec    `json:"databases"`
	Ingress          []ingressSpec     `json:"ingress"`
	DependencyPolicy *dependencyPolicy `json:"dependency_policy"`
}

// specErr is a spec-component validation failure inside the create flow.
type specErr struct{ msg string }

func (e specErr) Error() string { return e.msg }

// randomID returns 8 hex chars from crypto/rand (component id suffix).
func randomID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// validateSpecComponents runs all spec checks that do not need the DB
// (openapi parse, engine defaults, ingress references). Returns 422 detail.
func validateSpecComponents(spec *namespaceSpec) error {
	svcRefs := map[string]bool{}
	summaries := make([]*serviceopenapi.Summary, len(spec.Services))
	for i, s := range spec.Services {
		if s.Name == "" {
			return specErr{"spec.services[].name is required"}
		}
		if s.OpenAPI == nil || s.OpenAPI.Content == "" {
			return specErr{"spec.services[" + s.Name + "].openapi.content is required"}
		}
		summary, err := serviceopenapi.ValidateAndIndex([]byte(s.OpenAPI.Content))
		if err != nil {
			return specErr{"spec.services[" + s.Name + "] openapi invalid: " + err.Error()}
		}
		summaries[i] = summary
		// Host rules / dependencies may reference the service by name or by
		// client-supplied service_id (server generates "svc-"+rand when empty).
		svcRefs[s.Name] = true
		if s.ServiceID != "" {
			svcRefs[s.ServiceID] = true
		}
	}
	// Full-definition validation (mock ops, database_bindings shape,
	// dependencies refs) — same rules as POST /services (plans/17).
	for i, s := range spec.Services {
		if err := validateServiceDefinition(nil, "", s, summaries[i], svcRefs); err != nil {
			return specErr{"spec.services[" + s.Name + "] " + err.Error()}
		}
	}
	for _, db := range spec.Databases {
		if db.Name == "" {
			return specErr{"spec.databases[].name is required"}
		}
		if db.Engine != "" && db.Engine != "postgres" {
			return specErr{"spec.databases[" + db.Name + "].engine must be postgres"}
		}
	}
	for _, ing := range spec.Ingress {
		if ing.Name == "" {
			return specErr{"spec.ingress[].name is required"}
		}
		if len(ing.Hosts) == 0 {
			continue
		}
		var rules []struct {
			ServiceID string `json:"service_id"`
		}
		if err := json.Unmarshal(ing.Hosts, &rules); err != nil {
			return specErr{"spec.ingress[" + ing.Name + "].hosts must be an array of rules"}
		}
		for _, rule := range rules {
			if !svcRefs[rule.ServiceID] {
				return specErr{"spec.ingress[" + ing.Name + "] rule references unknown service " + rule.ServiceID}
			}
		}
	}
	// Dependency graph phải là DAG — vòng deps (A→B→A) chết readiness vì
	// không service nào bao giờ "resolved" (harness-testing.md INT-NS-33).
	if cycle := detectDependencyCycle(spec.Services); cycle != "" {
		return specErr{"spec.services dependency cycle: " + cycle}
	}
	return nil
}

// detectDependencyCycle returns "a -> b -> a" describing the first cycle in
// the services' dependency graph ("" when acyclic).
func detectDependencyCycle(services []serviceSpec) string {
	edges := map[string][]string{}
	for _, s := range services {
		var deps []dependencyRef
		if json.Unmarshal(s.Dependencies, &deps) != nil {
			continue // malformed deps are caught by validateServiceDefinition
		}
		for _, d := range deps {
			if d.ServiceID == "" {
				continue
			}
			edges[s.Name] = append(edges[s.Name], d.ServiceID)
		}
	}
	const (
		white = 0 // unvisited
		gray  = 1 // in progress (current DFS path)
		black = 2 // done
	)
	state := map[string]int{}
	path := []string{}
	var cycle string
	var dfs func(node string)
	dfs = func(node string) {
		if cycle != "" || state[node] == black {
			return
		}
		if state[node] == gray {
			start := 0
			for i, p := range path {
				if p == node {
					start = i
				}
			}
			cycle = strings.Join(append(append([]string{}, path[start:]...), node), " -> ")
			return
		}
		state[node] = gray
		path = append(path, node)
		for _, next := range edges[node] {
			dfs(next)
			if cycle != "" {
				return
			}
		}
		path = path[:len(path)-1]
		state[node] = black
	}
	for _, s := range services {
		dfs(s.Name)
		if cycle != "" {
			return cycle
		}
	}
	return ""
}

// datasetAvailable checks (inside the tx) that data_source.dataset_id refers
// to an AVAILABLE dataset in the caller's tenant.
func datasetAvailable(tx *gorm.DB, tenantID string, ds json.RawMessage) error {
	if len(ds) == 0 {
		return nil
	}
	var ref struct {
		DatasetID string `json:"dataset_id"`
	}
	if err := json.Unmarshal(ds, &ref); err != nil || ref.DatasetID == "" {
		return specErr{"data_source.dataset_id is required when data_source is set"}
	}
	var count int64
	if err := tx.Model(&store.Dataset{}).
		Where("id = ? AND tenant_id = ? AND status = ?", ref.DatasetID, tenantID, "AVAILABLE").
		Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return specErr{"data_source.dataset_id " + ref.DatasetID + " not found or not AVAILABLE"}
	}
	return nil
}

// createWithSpec creates the namespace and all components in ONE transaction;
// any error rolls back every row. Returns the created rows + generated ids.
func (d Deps) createWithSpec(ctx context.Context, p authz.Principal, req createRequest) (
	store.Namespace, []string, []string, []string, error,
) {
	var (
		ns       store.Namespace
		svcIDs   []string
		dbIDs    []string
		ingIDs   []string
		specHash string
		summary  []byte
	)
	txErr := d.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Unique slug → 422 (pre-check inside the tx so the message is
		// specific; uq_namespaces_slug is the backstop).
		if req.Slug != "" {
			var slugCount int64
			if err := tx.Model(&store.Namespace{}).Where("slug = ?", req.Slug).
				Count(&slugCount).Error; err != nil {
				return err
			}
			if slugCount > 0 {
				return specErr{"slug already exists"}
			}
		}
		ns = store.Namespace{
			TenantID:      p.TenantID,
			OwnerUserID:   p.UserID,
			Name:          req.Name,
			Slug:          strPtr(req.Slug),
			Visibility:    strPtr(req.Visibility),
			Description:   req.Description,
			Status:        "DRAFT",
			Version:       1,
			TTLExpiresAt:  req.TTL,
			CleanupPolicy: store.JSONBytes(map[string]any{}),
		}
		if err := tx.Create(&ns).Error; err != nil {
			if isUniqueViolation(err) {
				return specErr{"namespace name already exists in tenant"}
			}
			return err
		}

		for _, s := range req.Spec.Services {
			svcID, randErr := randomID()
			if randErr != nil {
				return randErr
			}
			sum, sumErr := serviceopenapi.ValidateAndIndex([]byte(s.OpenAPI.Content))
			if sumErr != nil {
				return specErr{"spec.services[" + s.Name + "] openapi invalid: " + sumErr.Error()}
			}
			summary = sum.Marshal()
			specHash = serviceopenapi.SpecHash([]byte(s.OpenAPI.Content))
			svc := store.Service{
				NamespaceID:      ns.ID,
				ServiceID:        strPtr("svc-" + svcID),
				Name:             s.Name,
				Description:      s.Description,
				Deploy:           rawOr(s.Deploy, "{}"),
				Mock:             rawOr(s.Mock, "{}"),
				DatabaseBindings: rawOr(s.DatabaseBindings, "[]"),
				Dependencies:     rawOr(s.Dependencies, "[]"),
				OpenAPISpec:      []byte(s.OpenAPI.Content),
				SpecHash:         strPtr(specHash),
				ParsedSummary:    summary,
				Version:          1,
			}
			if err := tx.Create(&svc).Error; err != nil {
				return err
			}
			svcIDs = append(svcIDs, *svc.ServiceID)
		}

		for _, dbSpec := range req.Spec.Databases {
			if err := datasetAvailable(tx, p.TenantID, dbSpec.DataSource); err != nil {
				return err
			}
			dbID, randErr := randomID()
			if randErr != nil {
				return randErr
			}
			engine := dbSpec.Engine
			if engine == "" {
				engine = "postgres"
			}
			row := store.Database{
				NamespaceID:   ns.ID,
				DatabaseID:    strPtr("db-" + dbID),
				Name:          dbSpec.Name,
				Engine:        engine,
				Operator:      orDefault(dbSpec.Operator, "cloudnative-pg"),
				EngineVersion: strPtr(dbSpec.Version),
				Config:        rawOr(dbSpec.Config, "{}"),
				InitScriptRef: strPtr(dbSpec.InitScriptRef),
				DataSource:    rawOr(dbSpec.DataSource, "{}"),
				RestorePolicy: rawOr(dbSpec.RestorePolicy, "{}"),
				Ephemeral:     rawOr(dbSpec.Ephemeral, "{}"),
				Mock:          rawOr(dbSpec.Mock, "{}"),
				SecretRef:     strPtr(dbSpec.SecretRef),
				Profile:       store.JSONBytes(map[string]any{}),
				Version:       1,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			dbIDs = append(dbIDs, *row.DatabaseID)
		}

		for _, ing := range req.Spec.Ingress {
			ingID, randErr := randomID()
			if randErr != nil {
				return randErr
			}
			row := store.Ingress{
				NamespaceID: ns.ID,
				IngressID:   strPtr("ing-" + ingID),
				Name:        ing.Name,
				Class:       orDefault(ing.Class, "nginx"),
				Hosts:       rawOr(ing.Hosts, "[]"),
				Paths:       store.JSONBytes([]any{}),
				Version:     1,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			ingIDs = append(ingIDs, *row.IngressID)
		}
		return nil
	})
	if txErr != nil {
		return store.Namespace{}, nil, nil, nil, txErr
	}
	return ns, svcIDs, dbIDs, ingIDs, nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func rawOr(raw json.RawMessage, def string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(def)
	}
	return raw
}

// createSpecResponse is the 201 body of create-with-spec.
type createSpecResponse struct {
	Namespace store.Namespace `json:"namespace"`
	Services  []string        `json:"services"`
	Databases []string        `json:"databases"`
	Ingress   []string        `json:"ingress"`
	Status    string          `json:"status,omitempty"`
}

// applySpecAfterCreate runs validate→apply when apply=true; returns the final
// namespace status ("READY" or "INVALID", falling back to stored status).
func (d Deps) applySpecAfterCreate(ctx context.Context, ns store.Namespace, p authz.Principal) string {
	actor := store.ActorJSON(p.TenantID, p.UserID, p.Role)
	checks, err := d.runValidation(ctx, ns, actor)
	if err != nil {
		_ = checks
		return "INVALID"
	}
	if err := d.runApply(ctx, ns, actor); err != nil {
		return ns.Status
	}
	return "READY"
}

// handleCreateSpecErrors maps create-with-spec failures to responses.
// Returns true when a response was written.
func handleCreateSpecErrors(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var se specErr
	if errors.As(err, &se) {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", se.msg)
		return true
	}
	httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "create namespace failed")
	return true
}

// runCreateSpec is the time-bounded create+optional-apply used by d.create.
func (d Deps) runCreateSpec(ctx context.Context, w http.ResponseWriter, r *http.Request, p authz.Principal, req createRequest) {
	ns, svcIDs, dbIDs, ingIDs, err := d.createWithSpec(ctx, p, req)
	if handleCreateSpecErrors(w, err) {
		return
	}
	// Unique slug from the DB (23505 on uq_namespaces_slug) maps to 422.
	d.audit(ctx, r, p, "namespace.create", ns.ID)
	resp := createSpecResponse{Namespace: ns, Services: svcIDs, Databases: dbIDs, Ingress: ingIDs}
	if req.Apply {
		status := d.applySpecAfterCreate(ctx, ns, p)
		resp.Status = status
		var latest store.Namespace
		if err := d.DB.WithContext(ctx).Where("id = ?", ns.ID).First(&latest).Error; err == nil {
			resp.Namespace = latest
		}
	}
	httpx.WriteJSON(w, http.StatusCreated, resp)
}
