// Package statuswatcher — Loop 2 of the two-loop control model
// (docs/namespace-status-watcher.md, observed K8s → PG). It evaluates
// per-component phase from the cluster state, persists the projection into
// tbl_component_status and aggregates the namespace status in ONE transaction
// via fn_namespace_transition (store.Transition) — never a direct UPDATE of
// status. It never writes K8s.
package statuswatcher

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/mockcontrol"
	"github.com/hungpham/loadtest-platform/pkgs/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Component types (tbl_component_status CHECK constraint, migration 0011).
const (
	compService       = "service"
	compDatabase      = "database"
	compDataLoad      = "data_load"
	compIngress       = "ingress"
	compNetworkPolicy = "network_policy"
	compMock          = "mock"
)

// Phases (same CHECK constraint).
const (
	phasePending     = "PENDING"
	phaseProgressing = "PROGRESSING"
	phaseReady       = "READY"
	phaseFailed      = "FAILED"
	phaseUnknown     = "UNKNOWN"
)

// Config tunes the watcher (docs/namespace-status-watcher.md §8).
type Config struct {
	// Resync re-reconciles every namespace periodically (safety net for lost
	// events). MVP uses polling; informer wiring is plan 21 PR E.
	Resync time.Duration
	// Grace: READY→DEGRADED after a required component is not READY this long.
	Grace time.Duration
	// Stable: DEGRADED→READY after all components READY this long.
	Stable time.Duration
	// ApplyDeadline: APPLYING/DATA_LOADING longer than this → DEGRADED.
	ApplyDeadline time.Duration
	// StalenessThreshold: heartbeat older than this → fail-closed (WATCH-05).
	StalenessThreshold time.Duration
}

// DefaultConfig mirrors the locked timing defaults (grace=60s, stable=60s,
// apply_deadline=15m, resync=5m, staleness=15m).
func DefaultConfig() Config {
	return Config{
		Resync:             5 * time.Minute,
		Grace:              60 * time.Second,
		Stable:             60 * time.Second,
		ApplyDeadline:      15 * time.Minute,
		StalenessThreshold: 15 * time.Minute,
	}
}

// Watcher is the observed→PG loop. Client is the only K8s surface; DB is
// written exclusively through the projection + fn_namespace_transition.
type Watcher struct {
	client k8s.Client
	db     *gorm.DB
	cfg    Config
	now    func() time.Time
	// hysteresis bookkeeping (in-memory; a restart simply resets the windows)
	// hysteresis bookkeeping guarded by hyMutex — workers and the resync
	// loop mutate concurrently (race caught by CI -race).
	hyMutex       sync.Mutex
	degradedSince map[string]time.Time
	readySince    map[string]time.Time
}

// New wires a Watcher.
func New(c k8s.Client, db *gorm.DB, cfg Config) *Watcher {
	if cfg.Resync <= 0 {
		cfg = DefaultConfig()
	}
	return &Watcher{
		client: c, db: db, cfg: cfg,
		now:           func() time.Time { return time.Now().UTC() },
		degradedSince: map[string]time.Time{},
		readySince:    map[string]time.Time{},
	}
}

// SetClock overrides the clock (tests).
func (w *Watcher) SetClock(now func() time.Time) { w.now = now }

var (
	deployGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	ingGVK    = schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"}
	npGVK     = schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}
	cnpgGVK   = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
)

// ReconcileStatus recomputes EVERYTHING for one namespace from the current
// cluster + DB state (level-triggered: event payloads are never trusted).
func (w *Watcher) ReconcileStatus(ctx context.Context, namespaceID string) error {
	var ns store.Namespace
	if err := w.db.WithContext(ctx).Where("id = ?", namespaceID).First(&ns).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil // namespace deleted between event and reconcile — no-op
		}
		return fmt.Errorf("statuswatcher: load namespace: %w", err)
	}
	// Terminal states are out of the watcher's remit.
	switch ns.Status {
	case "DELETED", "DELETING", "DRAFT", "INVALID":
		return nil
	}

	stale := ns.StatusObservedAt != nil && w.now().Sub(*ns.StatusObservedAt) > w.cfg.StalenessThreshold
	desired := ns.DesiredGeneration

	evalCtx := &evalContext{w: w, ctx: ctx, ns: &ns, stale: stale}
	services := evalCtx.evalServices(desired)
	databases := evalCtx.evalDatabases(desired)
	dataLoad := evalCtx.evalDataLoad(desired)
	ingresses := evalCtx.evalIngresses(desired)
	policy := evalCtx.evalNetworkPolicy(desired)

	rows := append([]store.ComponentStatus{}, services...)
	rows = append(rows, databases...)
	rows = append(rows, dataLoad, ingresses, policy)
	rows = append(rows, evalCtx.evalMocks(desired)...)
	if err := w.persistProjection(ctx, &ns, rows, stale); err != nil {
		return err
	}
	return w.aggregate(ctx, &ns, rows, stale)
}

// evalContext carries one reconcile pass's shared state.
type evalContext struct {
	w     *Watcher
	ctx   context.Context
	ns    *store.Namespace
	stale bool
}

func (e *evalContext) envNS() string { return envreconciler.EnvNamespaceName(e.ns.ID) }

// appliedGeneration extracts the desired_generation the reconciler stamped on
// the object (generation fencing, WATCH-03).
func appliedGeneration(obj *unstructured.Unstructured) int64 {
	v, ok := obj.GetAnnotations()[k8s.AnnotationDesiredGeneration]
	if !ok {
		return 0
	}
	g, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return g
}

// fenced phase: at most PROGRESSING unless the object was applied for the
// current desired generation.
func (e *evalContext) fenced(phase string, obj *unstructured.Unstructured, desired int64, reason string) (string, *string, int64) {
	obs := appliedGeneration(obj)
	if phase == phaseReady && obs != desired {
		r := "generation_fencing"
		return phaseProgressing, &r, obs
	}
	return phase, ptrOf(reason), obs
}

func ptrOf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// evalServices evaluates every namespace service's Deployment.
func (e *evalContext) evalServices(desired int64) []store.ComponentStatus {
	var services []store.Service
	_ = e.w.db.WithContext(e.ctx).Where("namespace_id = ?", e.ns.ID).Find(&services).Error
	rows := make([]store.ComponentStatus, 0, len(services))
	for _, svc := range services {
		row := store.ComponentStatus{
			NamespaceID: e.ns.ID, ComponentType: compService, ComponentID: svc.Name,
			Required: true, DesiredGeneration: desired, ObservedAt: e.w.now(),
		}
		compID := svc.Name
		if svc.ServiceID != nil {
			compID = *svc.ServiceID
		}
		row.ComponentID = compID
		obj, err := e.w.client.Get(e.ctx, deployGVK, e.envNS(), svc.Name)
		switch {
		case err == k8s.ErrNotFound:
			row.Phase, row.Reason = phasePending, ptrOf("not_applied")
		case err != nil:
			row.Phase, row.Reason = phaseUnknown, ptrOf("cluster_error")
		default:
			row.Phase, row.Reason, row.ObservedGeneration = evaluateDeployment(obj, deref(svc.SpecHash))
			row.Phase, row.Reason, _ = e.fenced(row.Phase, obj, desired, derefOr(row.Reason, ""))
		}
		rows = append(rows, row)
	}
	return rows
}

// evaluateDeployment implements the service READY criteria
// (docs/namespace-status-watcher.md §4): observedGeneration == generation,
// Available=True, availableReplicas >= replicas, spec_hash match.
func evaluateDeployment(dep *unstructured.Unstructured, specHash string) (string, *string, int64) {
	gen := dep.GetGeneration()
	obsGen, _, _ := unstructured.NestedInt64(dep.Object, "status", "observedGeneration")
	availRep, _, _ := unstructured.NestedInt64(dep.Object, "status", "availableReplicas")
	wantRep, _, _ := unstructured.NestedNumberAsFloat64(dep.Object, "spec", "replicas")
	available := false
	if conds, found, _ := unstructured.NestedSlice(dep.Object, "status", "conditions"); found {
		for _, c := range conds {
			cm, _ := c.(map[string]any)
			if cm == nil {
				continue
			}
			if cm["type"] == "Available" && cm["status"] == "True" {
				available = true
			}
			if cm["type"] == "Progressing" && cm["reason"] == "ProgressDeadlineExceeded" {
				return phaseFailed, ptrOf("progress_deadline_exceeded"), obsGen
			}
		}
	}
	annHash := dep.GetAnnotations()[k8s.AnnotationSpecHash]
	switch {
	case obsGen != gen:
		return phaseProgressing, ptrOf("generation_pending"), obsGen
	case annHash != specHash:
		return phaseProgressing, ptrOf("spec_hash_mismatch"), obsGen
	case !available:
		return phaseProgressing, ptrOf("not_available"), obsGen
	case availRep < int64(wantRep):
		return phaseProgressing, ptrOf("replicas_pending"), obsGen
	}
	return phaseReady, nil, obsGen
}

// evalDatabases evaluates every database's CNPG Cluster.
func (e *evalContext) evalDatabases(desired int64) []store.ComponentStatus {
	var dbs []store.Database
	_ = e.w.db.WithContext(e.ctx).Where("namespace_id = ?", e.ns.ID).Find(&dbs).Error
	rows := make([]store.ComponentStatus, 0, len(dbs))
	for _, db := range dbs {
		row := store.ComponentStatus{
			NamespaceID: e.ns.ID, ComponentType: compDatabase, ComponentID: db.Name,
			Required: true, DesiredGeneration: desired, ObservedAt: e.w.now(),
		}
		obj, err := e.w.client.Get(e.ctx, cnpgGVK, e.envNS(), db.Name)
		switch {
		case err == k8s.ErrNotFound:
			row.Phase, row.Reason = phasePending, ptrOf("not_applied")
		case err != nil:
			row.Phase, row.Reason = phaseUnknown, ptrOf("cluster_error")
		default:
			ready := false
			if conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions"); found {
				for _, c := range conds {
					cm, _ := c.(map[string]any)
					if cm != nil && cm["type"] == "Ready" && cm["status"] == "True" {
						ready = true
					}
				}
			}
			if ready {
				row.Phase, row.ObservedGeneration = phaseReady, appliedGeneration(obj)
			} else {
				row.Phase, row.Reason, row.ObservedGeneration = phaseProgressing, ptrOf("cnpg_not_ready"), appliedGeneration(obj)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// evalDataLoad: MVP — no restore Job is rendered yet, so data_load is READY
// when the namespace declares no restore data source (nothing to do), and
// PENDING otherwise until the restore Job lands (plans/18 §6 follow-up).
func (e *evalContext) evalDataLoad(desired int64) store.ComponentStatus {
	var count int64
	_ = e.w.db.WithContext(e.ctx).Model(&store.Database{}).
		Where("namespace_id = ? AND data_source IS NOT NULL AND data_source::text NOT IN ('null','{}','\"\"')", e.ns.ID).
		Count(&count).Error
	row := store.ComponentStatus{
		NamespaceID: e.ns.ID, ComponentType: compDataLoad, ComponentID: "-",
		Required: count > 0, DesiredGeneration: desired, ObservedAt: e.w.now(),
	}
	if count == 0 {
		row.Phase = phaseReady
	} else {
		row.Phase, row.Reason = phasePending, ptrOf("restore_job_not_implemented")
	}
	return row
}

// evalIngresses evaluates every ingress object (existence + spec_hash; K8s
// Ingress has no meaningful status for us).
func (e *evalContext) evalIngresses(desired int64) store.ComponentStatus {
	var ingresses []store.Ingress
	_ = e.w.db.WithContext(e.ctx).Where("namespace_id = ?", e.ns.ID).Find(&ingresses).Error
	row := store.ComponentStatus{
		NamespaceID: e.ns.ID, ComponentType: compIngress, ComponentID: "-",
		Required: len(ingresses) > 0, DesiredGeneration: desired, ObservedAt: e.w.now(),
	}
	if len(ingresses) == 0 {
		row.Phase = phaseReady // nothing declared → nothing to wait for
		return row
	}
	missing, mismatch := 0, 0
	for _, ing := range ingresses {
		obj, err := e.w.client.Get(e.ctx, ingGVK, e.envNS(), ing.Name)
		switch {
		case err == k8s.ErrNotFound:
			missing++
		case err != nil:
			mismatch++
		case obj.GetAnnotations()[k8s.AnnotationSpecHash] == "":
			mismatch++ // rendered without a hash → re-apply needed
		}
	}
	switch {
	case missing > 0:
		row.Phase, row.Reason = phasePending, ptrOf("not_applied")
	case mismatch > 0:
		row.Phase, row.Reason = phaseProgressing, ptrOf("spec_hash_mismatch")
	default:
		row.Phase = phaseReady
	}
	return row
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// evalNetworkPolicy: K8s has no status subresource — "applied" = the resolved
// default-deny exists with a matching spec_hash (recomputed identically to
// the reconciler's render).
func (e *evalContext) evalNetworkPolicy(desired int64) store.ComponentStatus {
	row := store.ComponentStatus{
		NamespaceID: e.ns.ID, ComponentType: compNetworkPolicy, ComponentID: "default-deny",
		Required: true, DesiredGeneration: desired, ObservedAt: e.w.now(),
	}
	var policy store.NetworkPolicy
	var depRows int64
	_ = e.w.db.WithContext(e.ctx).Model(&store.Dependency{}).
		Where("namespace_id = ?", e.ns.ID).Count(&depRows).Error
	if err := e.w.db.WithContext(e.ctx).Where("namespace_id = ?", e.ns.ID).First(&policy).Error; err != nil {
		// No resolved policy: required only when legacy tbl_dependencies rows
		// exist (same semantics as the legacy readinessGates) — pre-apply
		// namespaces with no dependencies are not blocked by it.
		if depRows == 0 {
			row.Required = false
			row.Phase = phaseReady
			return row
		}
		row.Phase, row.Reason = phasePending, ptrOf("no_resolved_policy")
		return row
	}
	obj, err := e.w.client.Get(e.ctx, npGVK, e.envNS(), "default-deny")
	switch {
	case err == k8s.ErrNotFound:
		row.Phase, row.Reason = phasePending, ptrOf("not_applied")
	case err != nil:
		row.Phase, row.Reason = phaseUnknown, ptrOf("cluster_error")
	case obj.GetAnnotations()[k8s.AnnotationSpecHash] != envreconciler.HashSpec(envreconciler.PolicySpecOf(policy.Spec)):
		row.Phase, row.Reason = phaseProgressing, ptrOf("spec_hash_mismatch")
	default:
		row.Phase, row.ObservedGeneration = phaseReady, appliedGeneration(obj)
	}
	return row
}

// evalMocks evaluates mock-enabled components via their binding ConfigMap
// (MOCK-VER-01): READY when the binding exists and its generation annotation
// matches the namespace's desired_generation — the mock runtime reports
// observed_generation from the snapshot it serves.
func (e *evalContext) evalMocks(desired int64) []store.ComponentStatus {
	var services []store.Service
	_ = e.w.db.WithContext(e.ctx).Where("namespace_id = ?", e.ns.ID).Find(&services).Error
	rows := make([]store.ComponentStatus, 0, 1)
	for _, svc := range services {
		if mockcontrol.DecodeMockDef(svc.Mock) == nil {
			continue
		}
		row := store.ComponentStatus{
			NamespaceID: e.ns.ID, ComponentType: compMock, ComponentID: svc.Name,
			Required: true, DesiredGeneration: desired, ObservedAt: e.w.now(),
		}
		cmName := "mock-binding-" + svc.Name
		cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
		obj, err := e.w.client.Get(e.ctx, cmGVK, e.envNS(), cmName)
		switch {
		case err == k8s.ErrNotFound:
			row.Phase, row.Reason = phasePending, ptrOf("mock_binding_missing")
		case err != nil:
			row.Phase, row.Reason = phaseUnknown, ptrOf("cluster_error")
		case obj.GetAnnotations()[k8s.AnnotationSpecHash] != fmt.Sprintf("mock-gen-%d", desired):
			row.Phase, row.Reason = phaseProgressing, ptrOf("mock_generation_pending")
		default:
			row.Phase, row.ObservedGeneration = phaseReady, desired
		}
		rows = append(rows, row)
	}
	return rows
}

// persistProjection upserts the tbl_component_status rows (UNIQUE(ns, type,
// component_id)) and the heartbeat; it does NOT touch namespace status.
func (w *Watcher) persistProjection(ctx context.Context, ns *store.Namespace, rows []store.ComponentStatus, stale bool) error {
	now := w.now()
	return w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i := range rows {
			row := rows[i]
			row.ObservedAt = now
			if stale {
				r := "STATUS_STALE"
				row.Reason = &r
			}
			var last store.ComponentStatus
			if err := tx.Where("namespace_id = ? AND component_type = ? AND component_id = ?",
				row.NamespaceID, row.ComponentType, row.ComponentID).First(&last).Error; err == nil {
				if last.Phase != row.Phase {
					row.LastTransitionAt = &now
				} else {
					row.LastTransitionAt = last.LastTransitionAt
				}
				row.ID = last.ID
				if err := tx.Model(&store.ComponentStatus{}).Where("id = ?", last.ID).Updates(map[string]any{
					"required": row.Required, "desired_generation": row.DesiredGeneration,
					"observed_generation": row.ObservedGeneration, "phase": row.Phase,
					"reason": row.Reason, "message": row.Message,
					"last_transition_at": row.LastTransitionAt, "observed_at": row.ObservedAt,
				}).Error; err != nil {
					return err
				}
				continue
			}
			// Upsert: concurrent reconciles (4 workers + resync tick) of the
			// same namespace race the First/Create window — ON CONFLICT
			// resolves it instead of a duplicate-key error loop (caught on
			// kind in CI; workers rarely overlapped against the fake).
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "namespace_id"}, {Name: "component_type"}, {Name: "component_id"},
				},
				DoUpdates: clause.AssignmentColumns([]string{
					"required", "desired_generation", "observed_generation", "phase",
					"reason", "message", "observed_at",
				}),
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		return tx.Model(&store.Namespace{}).Where("id = ?", ns.ID).
			Update("status_observed_at", now).Error
	})
}

// aggregate applies the namespace aggregation rules (docs §5) and writes
// status changes through fn_namespace_transition in the same pass. Illegal
// transitions (e.g. a racing user transition) are logged and retried on the
// next pass — no crash loop.
func (w *Watcher) aggregate(ctx context.Context, ns *store.Namespace, rows []store.ComponentStatus, stale bool) error {
	var anyFailed, anyPending bool
	var blocking []string
	for _, r := range rows {
		if !r.Required {
			continue
		}
		switch r.Phase {
		case phaseReady:
		case phaseFailed:
			anyFailed = true
			blocking = append(blocking, fmt.Sprintf("%s/%s: %s", r.ComponentType, r.ComponentID, derefOr(r.Reason, "failed")))
		case phasePending, phaseProgressing, phaseUnknown:
			anyPending = true
			blocking = append(blocking, fmt.Sprintf("%s/%s: %s", r.ComponentType, r.ComponentID, derefOr(r.Reason, "pending")))
		}
	}
	// All required components ready = no failure AND nothing pending (a
	// vacuous namespace with no required components is NOT ready — fail
	// closed when the projection is empty).
	allReady := len(rows) > 0 && !anyFailed && !anyPending
	if stale {
		allReady = false
		blocking = append(blocking, "status heartbeat stale (STATUS_STALE)")
	}
	readyForLoadtest := allReady && !anyFailed && ns.Status == "READY"

	next := ""
	applyDeadlineExceeded := ns.Status == "APPLYING" || ns.Status == "DATA_LOADING"
	if applyDeadlineExceeded {
		applyDeadlineExceeded = !allReady && w.pastApplyDeadline(ctx, ns)
	}
	switch ns.Status {
	case "APPLYING", "DATA_LOADING":
		switch {
		case anyFailed:
			next = "INVALID"
		case allReady && !anyPending:
			next = "READY"
		case applyDeadlineExceeded:
			// Stuck mid-apply past the deadline → DEGRADED (docs/namespace-
			// status-watcher.md §5); recovery re-runs validate.
			next = "DEGRADED"
			blocking = append(blocking, "apply_deadline_exceeded")
		case anyPending && ns.Status == "APPLYING":
			// data_load still in progress keeps APPLYING; restore-driven
			// namespaces move to DATA_LOADING (seed transition exists).
			if hasPhase(rows, compDataLoad, phasePending) {
				next = "DATA_LOADING"
			}
		}
	case "READY":
		if !allReady || anyFailed {
			if w.degradedFor(ns.ID, w.cfg.Grace) {
				next = "DEGRADED"
			}
		} else {
			w.clearHysteresis(ns.ID)
		}
	case "DEGRADED":
		if allReady && !anyFailed && w.stableFor(ns.ID, w.cfg.Stable) {
			next = "READY"
		} else {
			w.clearHysteresis(ns.ID)
		}
	}

	// Fail-closed staleness drops ready_for_loadtest regardless of transition.
	if stale || readyForLoadtest != ns.ReadyForLoadtest || len(blocking) > 0 || next != "" {
		updates := map[string]any{
			"ready_for_loadtest": readyForLoadtest && !stale,
			"blocking_reasons":   marshalJSON(blocking),
		}
		if err := w.db.WithContext(ctx).Model(&store.Namespace{}).
			Where("id = ?", ns.ID).Updates(updates).Error; err != nil {
			return fmt.Errorf("statuswatcher: update readiness: %w", err)
		}
	}
	if next != "" && next != ns.Status {
		if next == store.NamespaceStatusDegraded {
			// DEGRADED kèm namespace_degraded_at (docs §6) qua wrapper central.
			if err := store.NSToDegraded(ctx, w.db, ns.ID, store.ActorJSON("system", "statuswatcher", "system"), "watcher aggregation"); err != nil {
				slog.Warn("statuswatcher: transition rejected", "namespace", ns.ID, "to", next, "err", err)
				return nil // retry on the next pass; no crash loop
			}
			if err := w.db.WithContext(ctx).Model(&store.Namespace{}).
				Where("id = ?", ns.ID).Update("ready_for_loadtest", false).Error; err != nil {
				return err
			}
			delete(w.readySince, ns.ID)
			delete(w.degradedSince, ns.ID)
			return nil
		}
		// next ∈ {READY, DATA_LOADING, INVALID} — wrapper central.
		var nsTransition func(context.Context, *gorm.DB, string, string, string) error
		switch next {
		case store.NamespaceStatusReady:
			nsTransition = store.NSToReady
		case store.NamespaceStatusDataLoading:
			nsTransition = store.NSToDataLoading
		case store.NamespaceStatusInvalid:
			nsTransition = store.NSToInvalid
		default:
			slog.Warn("statuswatcher: unknown aggregated transition", "to", next)
			return nil
		}
		if err := nsTransition(ctx, w.db, ns.ID, store.ActorJSON("system", "statuswatcher", "system"), "watcher aggregation"); err != nil {
			slog.Warn("statuswatcher: transition rejected", "namespace", ns.ID, "to", next, "err", err)
			return nil // retry on the next pass; no crash loop
		}
		if next == store.NamespaceStatusReady {
			// ready_for_loadtest phải set CÙNG pass với transition READY —
			// trước đây chỉ update ở pass kế (resync 5m) nên lag tới 5 phút.
			if err := w.db.WithContext(ctx).Model(&store.Namespace{}).
				Where("id = ?", ns.ID).Update("ready_for_loadtest", true).Error; err != nil {
				return err
			}
			w.clearHysteresis(ns.ID)
		}
	}
	return nil
}

// pastApplyDeadline reports whether the namespace has been in its current
// APPLYING/DATA_LOADING leg longer than cfg.ApplyDeadline (based on the last
// transition event timestamp; missing event = not past deadline).
func (w *Watcher) pastApplyDeadline(ctx context.Context, ns *store.Namespace) bool {
	var createdAt time.Time
	_ = w.db.WithContext(ctx).Raw(
		`SELECT created_at FROM tbl_namespace_events
		 WHERE object_id = ? AND to_status IN ('APPLYING','DATA_LOADING')
		 ORDER BY id DESC LIMIT 1`, ns.ID).Scan(&createdAt)
	if createdAt.IsZero() {
		// No transition event (e.g. seeded state) — fall back to updated_at.
		createdAt = ns.UpdatedAt
	}
	if createdAt.IsZero() {
		return false
	}
	return w.now().Sub(createdAt) >= w.cfg.ApplyDeadline
}

// degradedFor/track the grace window for READY→DEGRADED (WATCH-06 hysteresis).
func (w *Watcher) degradedFor(nsID string, grace time.Duration) bool {
	w.hyMutex.Lock()
	defer w.hyMutex.Unlock()
	since, ok := w.degradedSince[nsID]
	if !ok {
		w.degradedSince[nsID] = w.now()
		return false
	}
	return w.now().Sub(since) >= grace
}

// stableFor tracks the stable window for DEGRADED→READY.
func (w *Watcher) stableFor(nsID string, stable time.Duration) bool {
	w.hyMutex.Lock()
	defer w.hyMutex.Unlock()
	since, ok := w.readySince[nsID]
	if !ok {
		w.readySince[nsID] = w.now()
		return false
	}
	return w.now().Sub(since) >= stable
}

// clearHysteresis forgets both windows for a namespace.
func (w *Watcher) clearHysteresis(nsID string) {
	w.hyMutex.Lock()
	defer w.hyMutex.Unlock()
	delete(w.degradedSince, nsID)
	delete(w.readySince, nsID)
}

func hasPhase(rows []store.ComponentStatus, compType, phase string) bool {
	for _, r := range rows {
		if r.ComponentType == compType && r.Phase == phase {
			return true
		}
	}
	return false
}

func marshalJSON(v any) []byte { return store.JSONBytes(v) }

// Start runs the event source + workers + resync loop until ctx is
// cancelled (docs/namespace-status-watcher.md S3): k8s.Client.Watch
// consumers per GVK enqueue namespace keys into the dedup/rate-limited
// workqueue; workers run ReconcileStatus (level-triggered - event payloads
// are never trusted); the polling resync ticker stays as the safety net for
// dropped/reordered events and startup relist.
func (w *Watcher) Start(ctx context.Context) {
	q := NewQueue()
	w.runWatches(ctx, q)
	for range 4 {
		go w.worker(ctx, q)
	}
	ticker := time.NewTicker(w.cfg.Resync)
	defer ticker.Stop()
	for {
		if err := w.Tick(ctx); err != nil {
			slog.Warn("statuswatcher: tick", "err", err)
		}
		select {
		case <-ctx.Done():
			q.q.ShutDown()
			return
		case <-ticker.C:
		}
	}
}

// worker drains the queue, reconciling one namespace per key.
func (w *Watcher) worker(ctx context.Context, q *Queue) {
	for {
		key, ok := q.Pop(ctx)
		if !ok || ctx.Err() != nil {
			return
		}
		if err := w.ReconcileStatus(ctx, key); err != nil {
			slog.Warn("statuswatcher: reconcile", "namespace", key, "err", err)
			q.AddAfter(key, time.Second) // retry with backoff
		}
	}
}

// Tick reconciles every namespace with pending work.
func (w *Watcher) Tick(ctx context.Context) error {
	var ids []string
	// Batch: mỗi tick xử lý tối đa tickBatchSize namespace, ưu tiên những
	// namespace CHƯA từng được reconcile (status_observed_at NULL) — backlog
	// lớn không được phép serialize vô hạn trong 1 tick.
	const tickBatchSize = 100
	if err := w.db.WithContext(ctx).Model(&store.Namespace{}).
		Where("status IN ?", []string{"APPLYING", "DATA_LOADING", "READY", "DEGRADED"}).
		Order("status_observed_at ASC NULLS FIRST, updated_at DESC").
		Limit(tickBatchSize).
		Pluck("id", &ids).Error; err != nil {
		return fmt.Errorf("statuswatcher: list namespaces: %w", err)
	}
	// One broken namespace must not stall the others (level-triggered: the
	// next resync retries it) — log and continue, surface only the last err.
	var lastErr error
	for _, id := range ids {
		if err := w.ReconcileStatus(ctx, id); err != nil {
			slog.Warn("statuswatcher: reconcile", "namespace", id, "err", err)
			lastErr = err
		}
	}
	return lastErr
}
