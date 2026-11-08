package namespace

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/authz"
	"github.com/hungpham/loadtest-platform/pkgs/httpx"
	"github.com/hungpham/loadtest-platform/pkgs/serviceopenapi"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// readinessReport is the GET /readiness payload shape (docs/namespace-management-api.md):
// one component block per readiness dimension plus the aggregated verdict.
type readinessReport struct {
	Components map[string]any `json:"components"`
	// BlockingReasons collects every component's blocking entries.
	BlockingReasons []string `json:"blocking_reasons"`
	// AllOK is the aggregate gate verdict (true when no blocking reasons).
	AllOK bool `json:"-"`
	// MocksBlocking is true when any service/database enables mocks but no
	// mock-service runtime is available (MVP). It contributes a blocking
	// reason to the report but is enforced as an APPLY-time gate (409
	// INSUFFICIENT_MOCK_CAPACITY), not at validate time.
	MocksBlocking bool `json:"-"`
}

// componentBlock is the per-component shape. Pointers keep 0 values visible
// while letting components omit fields their shape does not define.
type componentBlock struct {
	Ready    *int     `json:"ready,omitempty"`
	Total    *int     `json:"total,omitempty"`
	Required *bool    `json:"required,omitempty"`
	Applied  *bool    `json:"applied,omitempty"`
	Resolved *int     `json:"resolved,omitempty"`
	Blocking []string `json:"blocking"`
}

func intPtr(i int) *int    { return &i }
func boolPtr(b bool) *bool { return &b }

// readinessGates computes every readiness component WITHOUT changing state.
// It is shared by GET /readiness, runValidation (transition gate) and runApply
// (mock-capacity gate).
func (d Deps) readinessGates(ctx context.Context, ns store.Namespace) readinessReport {
	report := readinessReport{Components: map[string]any{}}

	// --- services: every service has an OpenAPI spec AND it parses ----------
	var services []store.Service
	_ = d.DB.WithContext(ctx).Where("namespace_id = ?", ns.ID).Find(&services).Error
	svcBlock := componentBlock{Total: intPtr(len(services)), Blocking: []string{}}
	if len(services) == 0 {
		svcBlock.Blocking = append(svcBlock.Blocking, "no services registered")
	}
	for _, svc := range services {
		if len(svc.OpenAPISpec) == 0 {
			svcBlock.Blocking = append(svcBlock.Blocking, "service "+svc.Name+": no OpenAPI spec")
			continue
		}
		if _, err := serviceopenapi.ValidateAndIndex(svc.OpenAPISpec); err != nil {
			svcBlock.Blocking = append(svcBlock.Blocking, "service "+svc.Name+": "+err.Error())
		}
	}
	svcBlock.Ready = intPtr(*svcBlock.Total - len(svcBlock.Blocking))
	report.Components["services"] = svcBlock

	// service_ids/names for dependency + ingress reference checks.
	svcRefs := map[string]bool{}
	for _, svc := range services {
		if svc.ServiceID != nil {
			svcRefs[*svc.ServiceID] = true
		}
		svcRefs[svc.Name] = true
	}

	// --- databases: data_source.dataset_id must point to an AVAILABLE dataset
	var databases []store.Database
	_ = d.DB.WithContext(ctx).Where("namespace_id = ?", ns.ID).Find(&databases).Error
	dbBlock := componentBlock{Total: intPtr(len(databases)), Blocking: []string{}}
	restoreDatasets := map[string]bool{}
	for _, dbRow := range databases {
		var ds struct {
			DatasetID string `json:"dataset_id"`
			Mode      string `json:"mode"`
		}
		if len(dbRow.DataSource) == 0 || json.Unmarshal(dbRow.DataSource, &ds) != nil || ds.DatasetID == "" {
			continue // no data_source → counts as ready
		}
		dataset, available := d.datasetForTenant(ctx, ns.TenantID, ds.DatasetID)
		if !available {
			dbBlock.Blocking = append(dbBlock.Blocking,
				"database "+dbRow.Name+": dataset "+ds.DatasetID+" not available")
		} else {
			// Restore-source compatibility: dataset engine/engine_version must
			// match the database when both sides declare them.
			if reason := datasetEngineMismatch(dataset, &dbRow); reason != "" {
				dbBlock.Blocking = append(dbBlock.Blocking, "database "+dbRow.Name+": "+reason)
			}
		}
		if ds.Mode == "restore" {
			restoreDatasets[ds.DatasetID] = available
		}
	}
	dbBlock.Ready = intPtr(*dbBlock.Total - len(dbBlock.Blocking))
	report.Components["databases"] = dbBlock

	// --- data_load: required when any database restores from a dataset -------
	dataLoad := componentBlock{Blocking: []string{}}
	dataLoad.Required = boolPtr(len(restoreDatasets) > 0)
	total, ready := 0, 0
	for id, available := range restoreDatasets {
		total++
		if available {
			ready++
			continue
		}
		dataLoad.Blocking = append(dataLoad.Blocking, "dataset "+id+" not available")
	}
	dataLoad.Total, dataLoad.Ready = intPtr(total), intPtr(ready)
	report.Components["data_load"] = dataLoad

	// --- ingress: every host rule references an existing service --------------
	var ingresses []store.Ingress
	_ = d.DB.WithContext(ctx).Where("namespace_id = ?", ns.ID).Find(&ingresses).Error
	ingBlock := componentBlock{Total: intPtr(len(ingresses)), Blocking: []string{}}
	for _, ing := range ingresses {
		for _, rule := range ingressRulesOf(ing.Hosts) {
			if !svcRefs[rule.ServiceID] {
				ingBlock.Blocking = append(ingBlock.Blocking,
					"ingress "+deref(ing.IngressID)+": unknown service "+rule.ServiceID)
			}
		}
	}
	ingBlock.Ready = intPtr(*ingBlock.Total - len(ingBlock.Blocking))
	report.Components["ingress"] = ingBlock

	// --- dependencies: services' dependencies[] refs must exist ---------------
	var resolved int
	depBlock := componentBlock{Blocking: []string{}}
	for _, svc := range services {
		var deps []dependencyRef
		if err := json.Unmarshal(svc.Dependencies, &deps); err != nil {
			continue
		}
		for _, dep := range deps {
			if dep.ServiceID == "" {
				continue
			}
			if svcRefs[dep.ServiceID] {
				resolved++
				continue
			}
			depBlock.Blocking = append(depBlock.Blocking,
				"service "+svc.Name+" depends on unknown service "+dep.ServiceID)
		}
	}
	depBlock.Resolved = intPtr(resolved)
	report.Components["dependencies"] = depBlock

	// --- network_policy: applied = a resolved policy row exists (from apply).
	// A policy is only REQUIRED when legacy tbl_dependencies rows exist —
	// pre-apply namespaces with no dependencies are not blocked by it.
	var policy store.NetworkPolicy
	policyOK := d.DB.WithContext(ctx).Where("namespace_id = ?", ns.ID).First(&policy).Error == nil
	var depRows int64
	_ = d.DB.WithContext(ctx).Model(&store.Dependency{}).
		Where("namespace_id = ?", ns.ID).Count(&depRows).Error
	polBlock := componentBlock{Applied: boolPtr(policyOK), Blocking: []string{}}
	if !policyOK && depRows > 0 {
		polBlock.Blocking = append(polBlock.Blocking, "network policy not applied")
	}
	report.Components["network_policy"] = polBlock

	// --- mocks: required when any service/database enables mocks. MVP has no
	// mock-service runtime yet, so a required-and-unavailable mock blocks
	// apply (409 INSUFFICIENT_MOCK_CAPACITY) — see runApply.
	mocksRequired := false
	for _, svc := range services {
		var m mockDef
		if json.Unmarshal(svc.Mock, &m) == nil && m.Enabled {
			mocksRequired = true
		}
	}
	for _, dbRow := range databases {
		var m mockDef
		if json.Unmarshal(dbRow.Mock, &m) == nil && m.Enabled {
			mocksRequired = true
		}
	}
	mockBlock := componentBlock{Blocking: []string{}}
	mockBlock.Required = boolPtr(mocksRequired)
	if mocksRequired {
		// No mock-service runtime exists yet → nothing can be ready.
		mockBlock.Ready, mockBlock.Total = intPtr(0), intPtr(0)
		mockBlock.Blocking = append(mockBlock.Blocking, "mock runtime not available (MVP stub)")
		report.MocksBlocking = true
	}
	report.Components["mocks"] = mockBlock

	// --- aggregate -------------------------------------------------------------
	// AllOK is the VALIDATE-time verdict: the mock-capacity gate is enforced
	// at APPLY time (runApply → 409 INSUFFICIENT_MOCK_CAPACITY), so it is
	// reported in BlockingReasons but does not fail validation.
	for _, block := range []componentBlock{svcBlock, dbBlock, dataLoad, ingBlock, depBlock, polBlock} {
		report.BlockingReasons = append(report.BlockingReasons, block.Blocking...)
	}
	report.AllOK = len(report.BlockingReasons) == 0
	return report
}

// datasetForTenant loads an AVAILABLE dataset in the given tenant (nil, false
// on query error — fail closed).
func (d Deps) datasetForTenant(ctx context.Context, tenantID, datasetID string) (*store.Dataset, bool) {
	var ds store.Dataset
	if err := d.DB.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND status = ?", datasetID, tenantID, "AVAILABLE").
		First(&ds).Error; err != nil {
		return nil, false
	}
	return &ds, true
}

// datasetEngineMismatch returns a blocking reason ("" when compatible) when a
// restore dataset's engine/engine_version do not match the target database.
func datasetEngineMismatch(ds *store.Dataset, dbRow *store.Database) string {
	if ds == nil {
		return ""
	}
	if ds.Engine != nil && *ds.Engine != "" && *ds.Engine != dbRow.Engine {
		return "dataset " + ds.ID + " engine mismatch (dataset " + *ds.Engine +
			" vs database " + dbRow.Engine + ")"
	}
	if ds.EngineVersion != nil && *ds.EngineVersion != "" &&
		dbRow.EngineVersion != nil && *dbRow.EngineVersion != "" &&
		*ds.EngineVersion != *dbRow.EngineVersion {
		return "dataset " + ds.ID + " engine_version mismatch (dataset " + *ds.EngineVersion +
			" vs database " + *dbRow.EngineVersion + ")"
	}
	return ""
}

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
func (d Deps) runValidation(ctx context.Context, ns store.Namespace, actor string) (readinessReport, error) {
	if err := store.Transition(ctx, d.DB, "namespace", ns.ID, "VALIDATING", actor, "validate requested"); err != nil {
		return readinessReport{}, err
	}
	// Gates run outside the transition transaction so failure can mark INVALID
	// (transition map: VALIDATING → INVALID) instead of silently rolling back.
	report := d.readinessGates(ctx, ns)
	if !report.AllOK {
		_ = store.Transition(ctx, d.DB, "namespace", ns.ID, "INVALID", actor, "readiness gates failed")
		return report, errValidationFailed
	}
	// VALIDATING → APPLYING
	if err := store.Transition(ctx, d.DB, "namespace", ns.ID, "APPLYING", actor, "validation passed"); err != nil {
		return readinessReport{}, err
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
	report := d.readinessGates(ctx, ns)
	// With the cluster path the mock runtime exists (shared mock-service +
	// binding ConfigMaps); the watcher's mock evaluator decides readiness.
	// Legacy mode has no runtime → fail closed (plans/17).
	if report.MocksBlocking && !d.K8S {
		return errMockCapacity
	}
	var restoreReasons []string
	restoreReasons = append(restoreReasons, report.Components["databases"].(componentBlock).Blocking...)
	restoreReasons = append(restoreReasons, report.Components["data_load"].(componentBlock).Blocking...)
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
func (d Deps) readiness(w http.ResponseWriter, r *http.Request) {
	p := authz.PrincipalOf(r)
	ns, ok := d.loadOwned(w, r, p)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	report := d.readinessGates(ctx, ns)
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
