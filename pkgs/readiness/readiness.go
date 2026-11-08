// Package readiness — the readiness projection/gate logic, split out of
// pkgs/namespace per docs/architecture.md §5.4 Stage 1 (plans/25). It is
// pure business logic: HTTP handlers in pkgs/namespace call it, the package
// itself imports only store + serviceopenapi (§5.3 A-block rule). The Status
// Watcher (pkgs/statuswatcher) owns the OBSERVED side of readiness; this
// package owns the DESIRED/spec side computed from PG rows.
package readiness

import (
	"context"
	"encoding/json"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/serviceopenapi"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// Report is the GET /readiness payload shape (docs/namespace-management-api.md):
// one component block per readiness dimension plus the aggregated verdict.
type Report struct {
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

// Block is the per-component shape. Pointers keep 0 values visible while
// letting components omit fields their shape does not define.
type Block struct {
	Ready    *int     `json:"ready,omitempty"`
	Total    *int     `json:"total,omitempty"`
	Required *bool    `json:"required,omitempty"`
	Applied  *bool    `json:"applied,omitempty"`
	Resolved *int     `json:"resolved,omitempty"`
	Blocking []string `json:"blocking"`
}

func intPtr(i int) *int    { return &i }
func boolPtr(b bool) *bool { return &b }

// mockFlag is readiness's minimal view of a component's mock JSONB (only the
// enabled flag matters for the gate; the richer shape stays in pkgs/namespace
// and pkgs/mockcontrol).
type mockFlag struct {
	Enabled bool `json:"enabled"`
}

// RuleRef is readiness's minimal view of one host rule (only the
// service reference matters for the gate).
type RuleRef struct {
	ServiceID string `json:"service_id"`
}

type ingressHostRef struct {
	Rules []RuleRef `json:"rules"`
}

// RulesOf decodes stored tbl_ingress.hosts JSONB into service references. It
// accepts the hosts[]-of-objects shape (plans/18) and falls back to the
// legacy flat rules array (pre-0010 rows / create-with-spec bodies).
func RulesOf(raw json.RawMessage) []RuleRef {
	if len(raw) == 0 {
		return nil
	}
	var hosts []ingressHostRef
	if err := json.Unmarshal(raw, &hosts); err == nil {
		var out []RuleRef
		for _, h := range hosts {
			out = append(out, h.Rules...)
		}
		return out
	}
	var flat []RuleRef
	if err := json.Unmarshal(raw, &flat); err == nil {
		return flat
	}
	return nil
}

// Gates computes every readiness component WITHOUT changing state. It is
// shared by GET /readiness, runValidation (transition gate) and runApply
// (mock-capacity gate). Moved verbatim from pkgs/namespace (plans/25).
func Gates(ctx context.Context, db *gorm.DB, ns store.Namespace) Report {
	report := Report{Components: map[string]any{}}

	// --- services: every service has an OpenAPI spec AND it parses ----------
	var services []store.Service
	_ = db.WithContext(ctx).Where("namespace_id = ?", ns.ID).Find(&services).Error
	svcBlock := Block{Total: intPtr(len(services)), Blocking: []string{}}
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
	_ = db.WithContext(ctx).Where("namespace_id = ?", ns.ID).Find(&databases).Error
	dbBlock := Block{Total: intPtr(len(databases)), Blocking: []string{}}
	restoreDatasets := map[string]bool{}
	for _, dbRow := range databases {
		var ds struct {
			DatasetID string `json:"dataset_id"`
			Mode      string `json:"mode"`
		}
		if len(dbRow.DataSource) == 0 || json.Unmarshal(dbRow.DataSource, &ds) != nil || ds.DatasetID == "" {
			continue // no data_source → counts as ready
		}
		dataset, available := datasetForTenant(ctx, db, ns.TenantID, ds.DatasetID)
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
	dataLoad := Block{Blocking: []string{}}
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
	_ = db.WithContext(ctx).Where("namespace_id = ?", ns.ID).Find(&ingresses).Error
	ingBlock := Block{Total: intPtr(len(ingresses)), Blocking: []string{}}
	for _, ing := range ingresses {
		for _, rule := range RulesOf(ing.Hosts) {
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
	depBlock := Block{Blocking: []string{}}
	for _, svc := range services {
		var deps []struct {
			ServiceID string `json:"service_id"`
		}
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
	policyOK := db.WithContext(ctx).Where("namespace_id = ?", ns.ID).First(&policy).Error == nil
	var depRows int64
	_ = db.WithContext(ctx).Model(&store.Dependency{}).
		Where("namespace_id = ?", ns.ID).Count(&depRows).Error
	polBlock := Block{Applied: boolPtr(policyOK), Blocking: []string{}}
	if !policyOK && depRows > 0 {
		polBlock.Blocking = append(polBlock.Blocking, "network policy not applied")
	}
	report.Components["network_policy"] = polBlock

	// --- mocks: required when any service/database enables mocks. MVP has no
	// mock-service runtime yet, so a required-and-unavailable mock blocks
	// apply (409 INSUFFICIENT_MOCK_CAPACITY) — see runApply.
	mocksRequired := false
	for _, svc := range services {
		var m mockFlag
		if json.Unmarshal(svc.Mock, &m) == nil && m.Enabled {
			mocksRequired = true
		}
	}
	for _, dbRow := range databases {
		var m mockFlag
		if json.Unmarshal(dbRow.Mock, &m) == nil && m.Enabled {
			mocksRequired = true
		}
	}
	mockBlock := Block{Blocking: []string{}}
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
	for _, block := range []Block{svcBlock, dbBlock, dataLoad, ingBlock, depBlock, polBlock} {
		report.BlockingReasons = append(report.BlockingReasons, block.Blocking...)
	}
	report.AllOK = len(report.BlockingReasons) == 0
	return report
}

// datasetForTenant loads an AVAILABLE dataset in the given tenant (nil, false
// on query error — fail closed).
func datasetForTenant(ctx context.Context, db *gorm.DB, tenantID, datasetID string) (*store.Dataset, bool) {
	var ds store.Dataset
	if err := db.WithContext(ctx).
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

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// BlockNameOf maps tbl_component_status.component_type (singular) to the
// readiness payload block names (docs/namespace-management-api.md §3).
func BlockNameOf(componentType string) string {
	switch componentType {
	case "service":
		return "services"
	case "database":
		return "databases"
	case "mock":
		return "mocks"
	default:
		return componentType // data_load, ingress, network_policy
	}
}

// ProjectionReport builds the readiness payload from the Status Watcher's
// tbl_component_status projection + tbl_namespaces.blocking_reasons.
// ok=false when the projection is empty (watcher has not run yet).
func ProjectionReport(ctx context.Context, db *gorm.DB, ns store.Namespace) (Report, bool) {
	var rows []store.ComponentStatus
	if err := db.WithContext(ctx).
		Where("namespace_id = ?", ns.ID).Find(&rows).Error; err != nil || len(rows) == 0 {
		return Report{}, false
	}
	report := Report{Components: map[string]any{}}
	for _, row := range rows {
		block, _ := report.Components[BlockNameOf(row.ComponentType)].(Block)
		if block.Blocking == nil {
			block.Blocking = []string{}
		}
		if block.Ready == nil {
			block.Ready, block.Total = intPtr(0), intPtr(0)
		}
		*block.Total++
		switch {
		case row.Phase == "READY":
			*block.Ready++
		case row.Reason != nil:
			block.Blocking = append(block.Blocking,
				row.ComponentType+"/"+row.ComponentID+": "+*row.Reason)
		}
		if row.Required {
			block.Required = boolPtr(true)
		}
		report.Components[BlockNameOf(row.ComponentType)] = block
	}
	report.BlockingReasons = []string{}
	if len(ns.BlockingReasons) > 0 {
		_ = json.Unmarshal(ns.BlockingReasons, &report.BlockingReasons)
	}
	report.AllOK = len(report.BlockingReasons) == 0
	return report, true
}
