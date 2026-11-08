package namespace

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/readiness"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// errValidationFailed marks readiness-gate failure inside runValidation.
var errValidationFailed = errors.New("readiness gates failed")

// errMockCapacity marks the apply-time mock-capacity gate (plans/17: mock-enabled
// specs cannot reach READY until a mock-service runtime exists — fail closed).
var errMockCapacity = errors.New("mock runtime not available (MVP stub)")

// applyValidationErr marks an apply-time restore-source gate failure: the
// datasets a database restores from are missing/unavailable or their
// engine/engine_version mismatch the target database → 422 VALIDATION_FAILED.
type applyValidationErr struct{ reasons []string }

func (e applyValidationErr) Error() string { return strings.Join(e.reasons, "; ") }

// runValidation is the status-machine core of POST /{id}/validate, callable
// without HTTP objects: DRAFT/INVALID → VALIDATING → gates → INVALID|APPLYING.
// On gate failure it transitions to INVALID and returns the failing checks.
// The mock-capacity gate is NOT enforced here (it is an apply-time gate).
func (d Deps) runValidation(ctx context.Context, ns store.Namespace, actor string) (readiness.Report, error) {
	if err := store.Transition(ctx, d.DB, "namespace", ns.ID, "VALIDATING", actor, "validate requested"); err != nil {
		return readiness.Report{}, err
	}
	// Gates run outside the transition transaction so failure can mark INVALID
	// (transition map: VALIDATING → INVALID) instead of silently rolling back.
	report := readiness.Gates(ctx, d.DB, ns)
	if !report.AllOK {
		_ = store.Transition(ctx, d.DB, "namespace", ns.ID, "INVALID", actor, "readiness gates failed")
		return report, errValidationFailed
	}
	// VALIDATING → APPLYING
	if err := store.Transition(ctx, d.DB, "namespace", ns.ID, "APPLYING", actor, "validation passed"); err != nil {
		return readiness.Report{}, err
	}
	return report, nil
}

// runApply is the status-machine core of POST /{id}/apply, callable without
// HTTP objects.
//   - K8S mode (cluster wired): gates → resolve dependencies → network policy
//     spec → bump desired_generation. The status stays APPLYING; the Env
//     Reconciler renders/applies the manifests and the Status Watcher closes
//     the loop APPLYING→DATA_LOADING→READY (plans/21, generation fencing).
//   - MVP mode (no cluster): legacy synchronous APPLYING → DATA_LOADING →
//     READY (+ ready_for_loadtest=true) so the standalone stack still works.
func (d Deps) runApply(ctx context.Context, ns store.Namespace, actor string) error {
	// Mock-capacity gate BEFORE transitioning: mock-enabled specs must not
	// reach READY without a mock runtime (plans/17, docs §mocks).
	// Restore-source gate: dataset engine/engine_version mismatches (and
	// unavailable/missing restore datasets) block apply with VALIDATION_FAILED.
	report := readiness.Gates(ctx, d.DB, ns)
	// With the cluster path the mock runtime exists (shared mock-service +
	// binding ConfigMaps); the watcher's mock evaluator decides readiness.
	// Legacy mode has no runtime → fail closed (plans/17).
	if report.MocksBlocking && !d.K8S {
		return errMockCapacity
	}
	var restoreReasons []string
	restoreReasons = append(restoreReasons, report.Components["databases"].(readiness.Block).Blocking...)
	restoreReasons = append(restoreReasons, report.Components["data_load"].(readiness.Block).Blocking...)
	if len(restoreReasons) > 0 {
		return applyValidationErr{reasons: restoreReasons}
	}
	if d.K8S {
		return d.applyDesired(ctx, ns, actor)
	}
	return d.applyLegacy(ctx, ns, actor)
}

// applyDesired resolves the desired state in PG and hands off to the two
// control loops. It writes NO readiness and NO terminal status — the watcher
// owns APPLYING→READY (one writer per direction, namespace-status-watcher.md §1).
func (d Deps) applyDesired(_ context.Context, ns store.Namespace, _ string) error {
	return d.DB.Transaction(func(tx *gorm.DB) error {
		// Resolve dependencies → egress policy spec (allow-list).
		var deps []store.Dependency
		if err := tx.Where("namespace_id = ?", ns.ID).Find(&deps).Error; err != nil {
			return err
		}
		spec := map[string]any{"egress": []map[string]any{}}
		for _, dep := range deps {
			spec["egress"] = append(spec["egress"].([]map[string]any), map[string]any{
				"type": dep.Type, "ref_id": dep.RefID,
			})
		}
		// ObservedGeneration stays 0 until the watcher confirms the applied
		// generation — the constant 1 here was the MVP shortcut, removed.
		policy := store.NetworkPolicy{
			NamespaceID: ns.ID, ResolvedFromDeps: true, Spec: store.JSONBytes(spec),
		}
		var existing store.NetworkPolicy
		if err := tx.Where("namespace_id = ?", ns.ID).First(&existing).Error; err == nil {
			policy.ID = existing.ID
			if err := tx.Save(&policy).Error; err != nil {
				return err
			}
		} else if err := tx.Create(&policy).Error; err != nil {
			return err
		}
		// Bump desired_generation in the SAME tx as the policy spec so the
		// watcher can never see a new spec under an old generation.
		return tx.Model(&store.Namespace{}).
			Where("id = ?", ns.ID).
			Updates(map[string]any{
				"desired_generation": gorm.Expr("desired_generation + 1"),
				"ready_for_loadtest": false,
				"blocking_reasons":   store.JSONBytes(map[string]any{}),
			}).Error
	})
}

// applyLegacy is the pre-plan-21 synchronous path (kept for the MVP stack
// without a cluster): APPLYING → DATA_LOADING → READY in one tx.
func (d Deps) applyLegacy(ctx context.Context, ns store.Namespace, actor string) error {
	return d.DB.Transaction(func(tx *gorm.DB) error {
		if err := store.Transition(ctx, tx, "namespace", ns.ID, "DATA_LOADING", actor, "apply requested"); err != nil {
			return err
		}
		// MVP restore stub: after DATA_LOADING the actual pg_restore runs in a
		// later plan (K8s pg_restore Job — plans/18 §6). The dataset
		// existence / AVAILABLE status / engine compatibility were verified by
		// the readiness gates, and apply transitions straight to READY.
		// Resolve dependencies → egress policy spec (MVP: direct allow-list).
		var deps []store.Dependency
		if err := tx.Where("namespace_id = ?", ns.ID).Find(&deps).Error; err != nil {
			return err
		}
		spec := map[string]any{"egress": []map[string]any{}}
		for _, dep := range deps {
			spec["egress"] = append(spec["egress"].([]map[string]any), map[string]any{
				"type": dep.Type, "ref_id": dep.RefID,
			})
		}
		policy := store.NetworkPolicy{
			NamespaceID: ns.ID, ResolvedFromDeps: true,
			Spec: store.JSONBytes(spec), ObservedGeneration: 1,
		}
		var existing store.NetworkPolicy
		if err := tx.Where("namespace_id = ?", ns.ID).First(&existing).Error; err == nil {
			policy.ID = existing.ID
			if err := tx.Save(&policy).Error; err != nil {
				return err
			}
		} else if err := tx.Create(&policy).Error; err != nil {
			return err
		}
		if err := store.Transition(ctx, tx, "namespace", ns.ID, "READY", actor, "policies resolved"); err != nil {
			return err
		}
		return tx.Model(&store.Namespace{}).
			Where("id = ?", ns.ID).
			Update("ready_for_loadtest", true).Error
	})
}

// validateNamespace: DRAFT/INVALID → VALIDATING → INVALID|APPLYING (422 on bad specs).
func (d Deps) validateNamespace(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	actor := store.ActorJSON(p.TenantID, p.UserID, p.Role)

	report, err := d.runValidation(ctx, ns, actor)
	if errors.Is(err, errValidationFailed) {
		d.audit(ctx, r, p, "namespace.validate.failed", ns.ID)
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":            httpx.Error{Code: "VALIDATION_FAILED", Message: "readiness gates failed"},
			"components":       report.Components,
			"blocking_reasons": report.BlockingReasons,
		})
		return
	}
	if err != nil {
		httpx.WriteCodeError(w, "INVALID_TRANSITION",
			"namespace cannot be validated from status "+ns.Status)
		return
	}
	d.audit(ctx, r, p, "namespace.validate", ns.ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": ns.ID, "status": "APPLYING"})
}

// applyNamespace: APPLYING → DATA_LOADING → READY (+ ready_for_loadtest=true).
// 409 INSUFFICIENT_MOCK_CAPACITY when mocks are required but no runtime exists.
func (d Deps) applyNamespace(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	actor := store.ActorJSON(p.TenantID, p.UserID, p.Role)

	err := d.runApply(ctx, ns, actor)
	var ave applyValidationErr
	switch {
	case errors.As(err, &ave):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":            httpx.Error{Code: "VALIDATION_FAILED", Message: "restore sources failed validation"},
			"blocking_reasons": ave.reasons,
		})
		return
	case errors.Is(err, errMockCapacity):
		httpx.WriteCodeError(w, "INSUFFICIENT_MOCK_CAPACITY", err.Error())
		return
	case err != nil:
		httpx.WriteCodeError(w, "INVALID_TRANSITION",
			"namespace cannot be applied from status "+ns.Status+" (run /validate first)")
		return
	}
	d.audit(ctx, r, p, "namespace.apply", ns.ID)
	if d.K8S {
		// Async hand-off: the Env Reconciler + Status Watcher finish the job;
		// the response reflects the current status (APPLYING), not READY.
		httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
			"id": ns.ID, "status": "APPLYING", "desired_generation": ns.DesiredGeneration + 1,
		})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": ns.ID, "status": "READY", "ready_for_loadtest": "true"})
}

// readiness returns the component checklist without mutating anything.
// K8S mode (plans/22): projection-backed — reads tbl_component_status +
// tbl_namespaces.blocking_reasons, computed by the Status Watcher
// (docs/namespace-management-api.md §3). Legacy mode keeps the on-the-fly
// readinessGates computation.
func (d Deps) readiness(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if d.K8S {
		report, ok := readiness.ProjectionReport(ctx, d.DB, ns)
		if ok {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"namespace_id":       ns.ID,
				"status":             ns.Status,
				"ready_for_loadtest": ns.ReadyForLoadtest,
				"checked_at":         d.nowUTC(),
				"components":         report.Components,
				"ready":              report.AllOK,
				"blocking_reasons":   report.BlockingReasons,
			})
			return
		}
		// No projection yet (watcher has not written) → fall through to the
		// spec-level gates so the payload is never empty.
	}
	report := readiness.Gates(ctx, d.DB, ns)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"namespace_id":       ns.ID,
		"status":             ns.Status,
		"ready_for_loadtest": ns.ReadyForLoadtest,
		"checked_at":         d.nowUTC(),
		"components":         report.Components,
		// ready counts the mock gate too: a mock-required namespace without a
		// runtime is NOT ready even though /validate would pass it.
		"ready":            report.AllOK && !report.MocksBlocking,
		"blocking_reasons": report.BlockingReasons,
	})
}

// nowUTC returns the current time (injectable via Deps.Now when set).
func (d Deps) nowUTC() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}
