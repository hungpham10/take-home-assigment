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
	"github.com/hungpham/loadtest-platform/pkgs/idempotency"
	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/runner"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Deps carries the handlers' dependencies.
type Deps struct {
	DB     *gorm.DB
	Runner runner.Runner
	Now    func() time.Time
	// K8s is the cluster client for run-scoped NetworkPolicy (nil in
	// legacy/local mode). When non-nil, run policies are applied before
	// Provision so enforcement exists when runner pods start.
	K8s k8s.Client
	// K8S enables the fail-closed STATUS_STALE gate (plans/21, WATCH-05):
	// with a cluster wired, POST /loadtests returns 409 NAMESPACE_NOT_READY
	// when the watcher heartbeat (status_observed_at) is missing or older
	// than StaleAfter. Legacy mode (K8S=false) skips the gate.
	K8S        bool
	StaleAfter time.Duration
}

// Mount registers the loadtest routes FLAT on the passed-in router (server
// composition root applies authz.RequirePrincipal). All patterns carry the
// /namespaces/{id}/loadtests prefix.
func Mount(r chi.Router, d Deps) {
	prefix := "/namespaces/{id}/loadtests"
	r.With(idempotency.RequireKey).
		Post(prefix, idempotency.Replay("/api/loadtest/v1/loadtests", d.DB, d.create))
	r.Get(prefix, d.list)
	r.Get(prefix+"/{ltID}", d.get)
	r.With(idempotency.RequireKey).
		Post(prefix+"/{ltID}/stop", idempotency.Replay("/api/loadtest/v1/loadtests/stop", d.DB, d.stop))
	r.Get(prefix+"/{ltID}/events", d.events)
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
	// Gate 1b (plans/21, fail-closed): stale watcher heartbeat blocks run
	// creation — the readiness projection cannot be trusted (WATCH-05).
	if d.K8S {
		threshold := d.StaleAfter
		if threshold <= 0 {
			threshold = 15 * time.Minute
		}
		stale := ns.StatusObservedAt == nil || d.now().Sub(*ns.StatusObservedAt) > threshold
		if stale {
			httpx.WriteJSON(w, http.StatusConflict, map[string]any{
				"error":  httpx.Error{Code: "NAMESPACE_NOT_READY", Message: "status heartbeat stale"},
				"reason": "STATUS_STALE",
			})
			return
		}
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
		LoadConfig:  store.JSONBytes(cfg),
		Targets:     store.JSONBytes(targets),
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

	// Run-scoped NetworkPolicy: applied before Provision so enforcement
	// exists when runner pods start. No-op when K8s client is nil
	// (legacy/local mode).
	if d.K8s != nil {
		envNS := envreconciler.EnvNamespaceName(ns.ID)
		runObjs, err := envreconciler.RunNetworkPolicies(envNS, &envreconciler.Env{
			NamespaceID: ns.ID, TenantID: ns.TenantID,
		}, lt.ID, targets)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "run policy build failed: "+err.Error())
			return
		}
		for _, obj := range runObjs {
			if err := d.K8s.Apply(ctx, obj); err != nil {
				httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "run policy apply failed: "+err.Error())
				return
			}
		}
	}

	// Provision synchronously; the reconciler advances PROVISIONING→RUNNING.
	spec := runner.Spec{
		ID: lt.ID, Script: scriptContent(ctx, d.DB, sc), VUs: intOf(cfg["vusers"], 1),
		Duration: strOf(cfg["duration"], "1m"), TargetsJSON: lt.Targets,
	}
	ref, err := d.Runner.Provision(ctx, spec)
	if err != nil {
		_ = store.LTToFailed(ctx, d.DB, lt.ID,
			store.ActorJSON(p.TenantID, p.UserID, p.Role), "provision failed: "+err.Error())
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "provision failed")
		return
	}

	// timeout_at drives the reaper (load_config.timeout_minutes, default 15).
	var timeoutAt *time.Time
	if mins := intOf(cfg["timeout_minutes"], 15); mins > 0 {
		at := d.now().Add(time.Duration(mins) * time.Minute)
		timeoutAt = &at
	}

	// QUEUED→PROVISIONING goes through the state machine; runner_ref and
	// timeout_at are ancillary columns written in the SAME transaction. Never
	// UPDATE status/version directly — the transition fn owns them.
	actor := store.ActorJSON(p.TenantID, p.UserID, p.Role)
	if err := d.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := store.LTToProvisioning(ctx, tx, lt.ID, actor, "provisioned"); err != nil {
			return err
		}
		return tx.Model(&store.LoadTest{}).Where("id = ?", lt.ID).
			Updates(map[string]any{"runner_ref": ref, "timeout_at": timeoutAt}).Error
	}); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "provisioning transition failed")
		return
	}

	// Launch the run only after the transaction committed. Start failure is
	// terminal FAILED (otherwise the reconciler polls a dead ref forever).
	if err := d.Runner.Start(ctx, ref, spec); err != nil {
		_ = store.LTToFailed(ctx, d.DB, lt.ID,
			actor, "runner start failed: "+err.Error())
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "runner start failed")
		return
	}

	// Re-read: the response reflects the committed row (PROVISIONING, ref,
	// timeout_at) rather than the pre-transaction struct.
	var out store.LoadTest
	if err := d.DB.WithContext(ctx).First(&out, "id = ?", lt.ID).Error; err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "load test re-read failed")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

// collectTargets aggregates targets[] from all services' parsed summaries.
// Each target is augmented with service_id (from tbl_services.service_id)
// and port (from tbl_services.deploy) so the run-scoped NetworkPolicy can
// resolve (pod selector, port) per target.
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
		if len(svc.ParsedSummary) == 0 || json.Unmarshal(svc.ParsedSummary, &summary) != nil {
			continue
		}
		// Resolve port from deploy JSONB (default 8080).
		var deploy struct {
			Port int32 `json:"port"`
		}
		port := int32(8080)
		if json.Unmarshal(svc.Deploy, &deploy) == nil && deploy.Port > 0 {
			port = deploy.Port
		}
		for _, t := range summary.Targets {
			if t == nil {
				continue
			}
			t["service_id"] = derefStr(svc.ServiceID)
			t["port"] = port
			targets = append(targets, t)
		}
	}
	return targets, nil
}

// derefStr returns the string value of *string, or "" if nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	httpx.WriteJSON(w, http.StatusOK, d.loadtestWithFlag(ctx, lt))
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
	if err := store.LTToStopping(ctx, d.DB, lt.ID,
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
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"events":     events,
		"unreliable": d.namespaceDegradedAfter(r.Context(), lt),
	})
}

// namespaceDegradedAfter reports whether the namespace logged a DEGRADED
// event (metadata namespace_degraded_at, written by the Status Watcher)
// after the run started — plans/22 docs §6: the run keeps running, its
// result is flagged unreliable.
func (d Deps) namespaceDegradedAfter(ctx context.Context, lt store.LoadTest) bool {
	var count int64
	if err := d.DB.WithContext(ctx).Raw(
		// Runs not started yet (PROVISIONING/QUEUED) compare against created_at
		// — a namespace degraded before the run starts still taints the result.
		`SELECT count(*) FROM tbl_namespace_events e
		 JOIN tbl_load_tests l ON l.namespace_id = e.object_id
		 WHERE l.id = ? AND e.to_status = 'DEGRADED'
		   AND e.created_at > COALESCE(l.started_at, l.created_at)`,
		lt.ID).Scan(&count).Error; err != nil {
		return false // fail open for reporting only
	}
	return count > 0
}

// loadtestWithFlag returns the loadtest payload merged with the unreliable
// flag (docs §6 — DEGRADED-after-start marks results unreliable).
func (d Deps) loadtestWithFlag(ctx context.Context, lt store.LoadTest) map[string]any {
	out := map[string]any{}
	b, _ := json.Marshal(lt)
	_ = json.Unmarshal(b, &out)
	out["unreliable"] = d.namespaceDegradedAfter(ctx, lt)
	return out
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
		TenantID:   store.StrPtr(p.TenantID),
		Actor:      []byte(store.ActorJSON(p.TenantID, p.UserID, p.Role)),
		Action:     action,
		ObjectType: "loadtest",
		ObjectID:   store.StrPtr(objectID),
		Details:    store.JSONBytes(map[string]any{}),
	}).Error
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
