package envreconciler

import (
	"context"
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// LoadEnvs is the shared EnvLoader implementation: reads namespaces with
// pending work (APPLYING/DATA_LOADING/DELETING) from PG and renders their
// desired state from the component tables. Used by the controller's
// orchestration loop and the integration harness (single implementation —
// duplicated logic is a defect, pkgs/ is the boundary). net is injected once
// by the controller and stamped onto every Env so renderers (default-deny,
// run-scoped policy) use a single source of truth for platform networking.
func LoadEnvs(db *gorm.DB, net PlatformNet) EnvLoader {
	return func(ctx context.Context) ([]*Env, error) {
		var rows []store.Namespace
		if err := db.WithContext(ctx).
			Where("status IN ?", []string{"APPLYING", "DATA_LOADING", "DELETING"}).
			Find(&rows).Error; err != nil {
			return nil, err
		}
		envs := make([]*Env, 0, len(rows))
		for _, ns := range rows {
			env := &Env{
				NamespaceID: ns.ID, Status: ns.Status,
				Generation: ns.DesiredGeneration,
				Net:         net,
			}
			if ns.Status == "DELETING" {
				// Delete reaper deadline input (plans/48): last `→ DELETING`
				// transition event timestamp, fallback updated_at (same pattern
				// as the watcher's pastApplyDeadline). Missing both = deadline
				// not measurable (seeded state) and the reaper just waits.
				var since time.Time
				_ = db.WithContext(ctx).Raw(
					`SELECT created_at FROM tbl_namespace_events
					 WHERE object_id = ? AND to_status = 'DELETING'
					 ORDER BY id DESC LIMIT 1`, ns.ID).Scan(&since)
				if since.IsZero() {
					since = ns.UpdatedAt
				}
				env.DeletingSince = since
				envs = append(envs, env) // teardown needs no components
				continue
			}
			var tenant struct{ TenantID string }
			if err := db.WithContext(ctx).Model(&store.Namespace{}).
				Select("tenant_id").Where("id = ?", ns.ID).Scan(&tenant).Error; err == nil {
				env.TenantID = tenant.TenantID
			}
			var services []store.Service
			if err := db.WithContext(ctx).Where("namespace_id = ?", ns.ID).Find(&services).Error; err != nil {
				return nil, err
			}
			// service id -> {name, port} so dependency service_id refs can be
			// resolved to the pod label (loadtest.io/component-id=<name>) used
			// by per-service egress policies.
			svcByID := map[string]struct {
				name string
				port int32
			}{}
			for _, svc := range services {
				port := int32(8080)
				var deploy struct {
					Port int32 `json:"port"`
				}
				if json.Unmarshal(svc.Deploy, &deploy) == nil && deploy.Port > 0 {
					port = deploy.Port
				}
				id := derefStr(svc.ServiceID)
				svcByID[id] = struct {
					name string
					port int32
				}{svc.Name, port}
			}
			var dbs []store.Database
			if err := db.WithContext(ctx).Where("namespace_id = ?", ns.ID).Find(&dbs).Error; err != nil {
				return nil, err
			}
			// database id -> {name, port} for resolving database_bindings.
			dbByID := map[string]struct {
				name string
				port int32
			}{}
			// database id -> set of service names bound to it (for per-DB
			// ingress policy).
			dbBoundServices := map[string]map[string]bool{}
			for _, dbRow := range dbs {
				dbSpec := DatabaseSpec{Name: dbRow.Name, Operator: dbRow.Operator}
				dbByID[dbRow.ID] = struct {
					name string
					port int32
				}{dbRow.Name, dbPort(dbSpec)}
				dbBoundServices[dbRow.ID] = map[string]bool{}
			}
			for _, svc := range services {
				spec := ServiceSpec{Name: svc.Name, SpecHash: derefStr(svc.SpecHash), Mock: svc.Mock}
				var d struct {
					Image          string            `json:"image"`
					Replicas       int32             `json:"replicas"`
					Port           int32             `json:"port"`
					Env            map[string]string `json:"env"`
					Health         *HealthProbe      `json:"health"`
					ConfigMap      map[string]string `json:"config_map"`
					Secrets        map[string]string `json:"secrets"`
					HPA            *HPASpec          `json:"hpa"`
					ServiceAccount bool              `json:"service_account"`
				}
				if json.Unmarshal(svc.Deploy, &d) == nil {
					spec.Image, spec.Replicas, spec.Port, spec.Env, spec.Health =
						d.Image, d.Replicas, d.Port, d.Env, d.Health
					spec.ConfigMap, spec.Secrets, spec.HPA, spec.ServiceAccount =
						d.ConfigMap, d.Secrets, d.HPA, d.ServiceAccount
					if spec.Replicas <= 0 {
						spec.Replicas = 1
					}
					if spec.Port == 0 {
						spec.Port = 8080
					}
				}
				// Resolve this service's declared dependencies (services +
				// databases) into egress rules keyed by the target's pod label.
				if len(svc.Dependencies) > 0 {
					var deps []struct {
						ServiceID string `json:"service_id"`
					}
					if json.Unmarshal(svc.Dependencies, &deps) == nil {
						for _, dep := range deps {
							if t, ok := svcByID[dep.ServiceID]; ok && t.name != "" {
								spec.Dependencies = append(spec.Dependencies, EgressRule{
									Type:  "service",
									RefID: t.name,
									Ports: []int32{t.port},
								})
							}
						}
					}
				}
				if len(svc.DatabaseBindings) > 0 {
					var bindings []struct {
						DatabaseID string `json:"database_id"`
					}
					if json.Unmarshal(svc.DatabaseBindings, &bindings) == nil {
						for _, b := range bindings {
							db, ok := dbByID[b.DatabaseID]
							if !ok || db.name == "" {
								continue
							}
							spec.Dependencies = append(spec.Dependencies, EgressRule{
								Type:  "database",
								RefID: db.name,
								Ports: []int32{db.port},
							})
							// Record this service as bound to this DB so the
							// DB ingress policy allows only bound services.
							if bound, exists := dbBoundServices[b.DatabaseID]; exists {
								bound[svc.Name] = true
							}
						}
					}
				}
				env.Services = append(env.Services, spec)
			}
			for _, dbRow := range dbs {
				spec := DatabaseSpec{Name: dbRow.Name, Operator: dbRow.Operator}
				var cfg struct {
					Image     string         `json:"image"`
					StorageGB int64          `json:"storage_gb"`
					Version   string         `json:"version"`
					NodeCount int            `json:"node_count"`
					Extra     map[string]any `json:"extra"`
				}
				if json.Unmarshal(dbRow.Config, &cfg) == nil {
					spec.Image, spec.StorageGB = cfg.Image, cfg.StorageGB
					if cfg.Extra != nil {
						spec.ExtraConfig = cfg.Extra
					} else {
						// ECK fields live at the top level of config when no
						// explicit extra map is provided.
						spec.ExtraConfig = map[string]any{}
						if cfg.Version != "" {
							spec.ExtraConfig["version"] = cfg.Version
						}
						if cfg.NodeCount > 0 {
							spec.ExtraConfig["node_count"] = float64(cfg.NodeCount)
						}
					}
				}
				if spec.StorageGB <= 0 {
					spec.StorageGB = 5
				}
				if spec.Operator == "" {
					spec.Operator = "cloudnative-pg" // default operator (0003/0010)
				}
				boundSet := dbBoundServices[dbRow.ID]
				spec.BoundServices = make([]string, 0, len(boundSet))
				for name := range boundSet {
					spec.BoundServices = append(spec.BoundServices, name)
				}
				env.Databases = append(env.Databases, spec)
			}
			var ingresses []store.Ingress
			if err := db.WithContext(ctx).Where("namespace_id = ?", ns.ID).Find(&ingresses).Error; err != nil {
				return nil, err
			}
			for _, ing := range ingresses {
				spec := IngressSpec{Name: ing.Name, IngressClass: ing.Class}
				var hosts []struct {
					Rules []IngressRule `json:"rules"`
				}
				if json.Unmarshal(ing.Hosts, &hosts) == nil {
					for _, h := range hosts {
						spec.Rules = append(spec.Rules, h.Rules...)
					}
				}
				env.Ingresses = append(env.Ingresses, spec)
			}
			var policy store.NetworkPolicy
			if err := db.WithContext(ctx).Where("namespace_id = ?", ns.ID).First(&policy).Error; err == nil {
				env.Policy = PolicySpecOf(policy.Spec)
			}
			envs = append(envs, env)
		}
		return envs, nil
	}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// LoadGeneration returns a loader that reads desired_generation for a
// namespace from PG. Used by the reconciler's prune generation check
// (§2.2.4 backstop).
func LoadGeneration(db *gorm.DB) func(ctx context.Context, namespaceID string) (int64, error) {
	return func(ctx context.Context, namespaceID string) (int64, error) {
		var gen int64
		err := db.WithContext(ctx).Model(&store.Namespace{}).
			Where("id = ?", namespaceID).Pluck("desired_generation", &gen).Error
		return gen, err
	}
}
