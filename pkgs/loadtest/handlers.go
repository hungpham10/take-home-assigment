// Package loadtest implements /namespaces/{id}/loadtests — creation gated on
// namespace READY + script VALIDATED, lifecycle driven by the Runner and the
// DB state machine (fn_loadtest_transition).
package loadtest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/runner"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Deps carries the handlers' dependencies.
type Deps struct {
	DB     *gorm.DB
	Runner runner.Runner
	Now    func() time.Time
}

// Router mounts loadtest routes (under /namespaces/{id}).
func Router(d Deps) http.Handler {
	r := chi.NewRouter()
	r.With(requireIdempotencyKey).Post("/", d.create)
	r.Get("/", d.list)
	r.Get("/{ltID}", d.get)
	r.With(requireIdempotencyKey).Post("/{ltID}/stop", d.stop)
	r.Get("/{ltID}/events", d.events)
	return r
}

// --- creation --------------------------------------------------------------------

type createRequest struct {
	Name       string          `json:"name"`
	ScriptID   string          `json:"script_id"`
	LoadConfig json.RawMessage `json:"load_config"`
}

func (d Deps) create(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := loadNamespace(w, r, p, d.DB)
	if !ok {
		return
	}
	// Gate 1 (docs): namespace must be READY.
	if !ns.ReadyForLoadtest || ns.Status != "READY" {
		httpx.WriteCodeError(w, "NAMESPACE_NOT_READY",
			"namespace is not READY (status "+ns.Status+")")
		return
	}
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.ScriptID == "" {
		httpx.WriteCodeError(w, "VALIDATION_FAILED", "name and script_id are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Gate 2 (docs): script must be VALIDATED (409 SCRIPT_NOT_VALIDATED).
	var sc store.Script
	if err := d.DB.First(&sc, "id = ? AND namespace_id = ?", req.ScriptID, ns.ID).Error; err != nil {
		httpx.WriteCodeError(w, "OBJECT_NOT_FOUND", "script not found")
		return
	}
	if sc.Status != "VALIDATED" {
		httpx.WriteCodeError(w, "SCRIPT_NOT_VALIDATED",
			"script status is "+sc.Status)
		return
	}

	// Targets from the services' targets[] index; empty → 422.
	targets, err := d.collectTargets(ctx, ns.ID)
	if err != nil || len(targets) == 0 {
		httpx.WriteCodeError(w, "VALIDATION_FAILED",
			"no load-test targets available (register services with OpenAPI specs)")
		return
	}

	cfg := defaultLoadConfig(req.LoadConfig)
	lt := store.LoadTest{
		NamespaceID: ns.ID,
		ScriptID:    sc.ID,
		Name:        req.Name,
		CreatedBy:   p.UserID,
		LoadConfig:  jsonBytes(cfg),
		Targets:     jsonBytes(targets),
		RunnerType:  runnerMode(d.Runner),
		Status:      "QUEUED",
		Version:     1,
	}
	if err := d.DB.WithContext(ctx).Create(&lt).Error; err != nil {
		if isUniqueViolation(err) {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "load test name already exists")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "load test insert failed")
		return
	}
	d.audit(ctx, r, p, "loadtest.create", lt.ID)

	// Provision synchronously; the reconciler advances PROVISIONING→RUNNING.
	ref, err := d.Runner.Provision(ctx, runner.Spec{
		ID: lt.ID, Script: scriptContent(ctx, d.DB, sc), VUs: intOf(cfg["vusers"], 1),
		Duration: strOf(cfg["duration"], "1m"), TargetsJSON: lt.Targets,
	})
	if err != nil {
		_ = store.Transition(ctx, d.DB, "loadtest", lt.ID, "FAILED",
			store.ActorJSON(p.TenantID, p.UserID, p.Role), "provision failed: "+err.Error())
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "provision failed")
		return
	}
	if err := d.DB.Model(&store.LoadTest{}).Where("id = ?", lt.ID).
		Updates(map[string]any{"runner_ref": ref, "status": "PROVISIONING", "version": lt.Version + 1}).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "runner ref update failed")
		return
	}
	lt.RunnerRef = &ref
	lt.Status = "PROVISIONING"
	// timeout_at drives the reaper (load_config.timeout_minutes, default 15).
	if mins := intOf(cfg["timeout_minutes"], 15); mins > 0 {
		at := d.now().Add(time.Duration(mins) * time.Minute)
		lt.TimeoutAt = &at
		_ = d.DB.Model(&store.LoadTest{}).Where("id = ?", lt.ID).Update("timeout_at", &at).Error
	}
	httpx.WriteJSON(w, http.StatusCreated, lt)
}

// collectTargets aggregates targets[] from all services' parsed summaries.
func (d Deps) collectTargets(ctx context.Context, nsID string) ([]map[string]any, error) {
	var services []store.Service
	if err := d.DB.WithContext(ctx).Where("namespace_id = ?", nsID).Find(&services).Error; err != nil {
		return nil, err
	}
	var targets []map[string]any
	for _, svc := range services {
		var summary struct {
			Targets []map[string]any `json:"targets"`
		}
		if len(svc.ParsedSummary) > 0 && json.Unmarshal(svc.ParsedSummary, &summary) == nil {
			targets = append(targets, summary.Targets...)
		}
	}
	return targets, nil
}

// defaultLoadConfig fills documented defaults for missing fields.
func defaultLoadConfig(raw json.RawMessage) map[string]any {
	cfg := map[string]any{"vusers": 200, "duration": "10m", "timeout_minutes": 15}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	return cfg
}

// --- lifecycle --------------------------------------------------------------------

func (d Deps) list(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := loadNamespace(w, r, p, d.DB)
	if !ok {
		return
	}
	var tests []store.LoadTest
	if err := d.DB.Where("namespace_id = ?", ns.ID).Order("created_at DESC").Find(&tests).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "list load tests failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"load_tests": tests, "count": len(tests)})
}

func (d Deps) get(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	lt, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, lt)
}

// stop: any active status → STOPPING (reconciler completes → STOPPED).
func (d Deps) stop(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	lt, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	switch lt.Status {
	case "QUEUED", "PROVISIONING", "RUNNING":
	default:
		httpx.WriteCodeError(w, "INVALID_TRANSITION",
			"load test is not active (status "+lt.Status+")")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := store.Transition(ctx, d.DB, "loadtest", lt.ID, "STOPPING",
		store.ActorJSON(p.TenantID, p.UserID, p.Role), "stop requested"); err != nil {
		httpx.WriteCodeError(w, "INVALID_TRANSITION", "stop rejected")
		return
	}
	if lt.RunnerRef != nil {
		_ = d.Runner.Stop(ctx, *lt.RunnerRef)
	}
	d.audit(ctx, r, p, "loadtest.stop", lt.ID)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"id": lt.ID, "status": "STOPPING"})
}

func (d Deps) events(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	lt, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	var events []store.LoadTestEvent
	if err := d.DB.Where("object_id = ?", lt.ID).Order("id").Find(&events).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "events query failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": events})
}

// --- helpers ---------------------------------------------------------------------

func (d Deps) loadOwned(w http.ResponseWriter, r *http.Request, p authz.Principal) (store.LoadTest, bool) {
	ns, ok := loadNamespace(w, r, p, d.DB)
	if !ok {
		return store.LoadTest{}, false
	}
	var lt store.LoadTest
	if err := d.DB.Where("namespace_id = ? AND id = ?", ns.ID, chi.URLParam(r, "ltID")).
		First(&lt).Error; err != nil || lt.ID == "" {
		httpx.WriteCodeError(w, "OBJECT_NOT_FOUND", "load test not found")
		return store.LoadTest{}, false
	}
	return lt, true
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) audit(ctx context.Context, _ *http.Request, p authz.Principal, action, objectID string) {
	_ = d.DB.WithContext(ctx).Create(&store.AuditLog{
		TenantID:   strPtr(p.TenantID),
		Actor:      jsonBytes(map[string]string{"tenant_id": p.TenantID, "user_id": p.UserID, "role": p.Role}),
		Action:     action,
		ObjectType: "loadtest",
		ObjectID:   strPtr(objectID),
		Details:    jsonBytes(map[string]any{}),
	}).Error
}

func requireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == "" {
			httpx.WriteCodeError(w, "VALIDATION_FAILED", "Idempotency-Key header is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// modeReporter lets runners declare their mode (Local reports "local");
// unknown runners default to "k8s".
type modeReporter interface{ Mode() string }

func runnerMode(r runner.Runner) string {
	if m, ok := r.(modeReporter); ok {
		return m.Mode()
	}
	return "k8s"
}

func intOf(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return def
}

func strOf(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func strPtr(s string) *string { return &s }

func jsonBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
