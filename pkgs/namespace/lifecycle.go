package namespace

import (
	"context"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/serviceopenapi"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// readinessCheck is one item of the readiness checklist.
type readinessCheck struct {
	Gate   string `json:"gate"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// readiness computes the gate checklist WITHOUT changing state.
func (d Deps) readinessGates(ctx context.Context, nsID string) ([]readinessCheck, bool) {
	checks := []readinessCheck{}

	// Gate 1: every service has a valid indexed spec (missing spec → 422 gate)
	var services []store.Service
	_ = d.DB.WithContext(ctx).Where("namespace_id = ?", nsID).Find(&services).Error
	if len(services) == 0 {
		checks = append(checks, readinessCheck{Gate: "services", Detail: "no services registered"})
	} else {
		okAll, detail := true, ""
		for _, svc := range services {
			if len(svc.OpenAPISpec) == 0 {
				okAll = false
				detail = "service " + svc.Name + " has no OpenAPI spec (422)"
				break
			}
			if _, err := serviceopenapi.ValidateAndIndex(svc.OpenAPISpec); err != nil {
				okAll = false
				detail = "service " + svc.Name + ": " + err.Error()
				break
			}
		}
		checks = append(checks, readinessCheck{Gate: "services", Passed: okAll, Detail: detail})
	}

	// Gate 2: dependencies resolved → network policies
	var depCount int64
	_ = d.DB.WithContext(ctx).Model(&store.Dependency{}).
		Where("namespace_id = ?", nsID).Count(&depCount).Error
	var policy store.NetworkPolicy
	policyOK := d.DB.WithContext(ctx).Where("namespace_id = ?", nsID).First(&policy).Error == nil
	checks = append(checks, readinessCheck{
		Gate:   "network_policies",
		Passed: depCount == 0 || policyOK,
		Detail: detailIf(!policyOK && depCount > 0, "dependencies present but policies not resolved"),
	})

	// Gate 3: mock capacity (hook — real check sau khi có mock-service)
	checks = append(checks, readinessCheck{Gate: "mock_capacity", Passed: true})

	allOK := true
	for _, c := range checks {
		allOK = allOK && c.Passed
	}
	return checks, allOK
}

func detailIf(cond bool, msg string) string {
	if cond {
		return msg
	}
	return ""
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

	// DRAFT/INVALID → VALIDATING
	if err := store.Transition(ctx, d.DB, "namespace", ns.ID, "VALIDATING", actor, "validate requested"); err != nil {
		httpx.WriteCodeError(w, "INVALID_TRANSITION",
			"namespace cannot be validated from status "+ns.Status)
		return
	}

	// Gates run outside the transition transaction so failure can mark INVALID
	// (transition map: VALIDATING → INVALID) instead of silently rolling back.
	checks, allOK := d.readinessGates(ctx, ns.ID)
	if !allOK {
		_ = store.Transition(ctx, d.DB, "namespace", ns.ID, "INVALID", actor, "readiness gates failed")
		d.audit(ctx, r, p, "namespace.validate.failed", ns.ID)
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  httpx.Error{Code: "VALIDATION_FAILED", Message: "readiness gates failed"},
			"checks": checks,
		})
		return
	}

	// VALIDATING → APPLYING
	if err := store.Transition(ctx, d.DB, "namespace", ns.ID, "APPLYING", actor, "validation passed"); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "INTERNAL", "post-validation transition failed")
		return
	}
	d.audit(ctx, r, p, "namespace.validate", ns.ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": ns.ID, "status": "APPLYING"})
}

// applyNamespace: APPLYING → DATA_LOADING → READY (+ ready_for_loadtest=true).
// Also resolves dependencies → network policy spec.
func (d Deps) applyNamespace(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	actor := store.ActorJSON(p.TenantID, p.UserID, p.Role)

	err := d.DB.Transaction(func(tx *gorm.DB) error {
		if err := store.Transition(ctx, tx, "namespace", ns.ID, "DATA_LOADING", actor, "apply requested"); err != nil {
			return err
		}
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
			Spec: jsonBytes(spec), ObservedGeneration: 1,
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
	if err != nil {
		httpx.WriteCodeError(w, "INVALID_TRANSITION",
			"namespace cannot be applied from status "+ns.Status+" (run /validate first)")
		return
	}
	d.audit(ctx, r, p, "namespace.apply", ns.ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"id": ns.ID, "status": "READY", "ready_for_loadtest": "true"})
}

// readiness returns the checklist without mutating anything.
func (d Deps) readiness(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	checks, allOK := d.readinessGates(ctx, ns.ID)
	status := "NOT_READY"
	if ns.ReadyForLoadtest && allOK {
		status = "READY"
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": ns.ID, "status": ns.Status, "ready": allOK, "ready_for_loadtest": ns.ReadyForLoadtest,
		"effective": status, "checks": checks,
	})
}
