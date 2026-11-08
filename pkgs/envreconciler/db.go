package envreconciler

import (
	"context"
	"encoding/json"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// LoadEnvs is the shared EnvLoader implementation: reads namespaces with
// pending work (APPLYING/DATA_LOADING/DELETING) from PG and renders their
// desired state from the component tables. Used by the controller's
// orchestration loop and the integration harness (single implementation —
// duplicated logic is a defect, pkgs/ is the boundary).
func LoadEnvs(db *gorm.DB) EnvLoader {
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
			}
			if ns.Status == "DELETING" {
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
			for _, svc := range services {
				spec := ServiceSpec{Name: svc.Name, SpecHash: derefStr(svc.SpecHash), Mock: svc.Mock}
				var d struct {
					Image    string            `json:"image"`
					Replicas int32             `json:"replicas"`
					Port     int32             `json:"port"`
					Env      map[string]string `json:"env"`
					Health   *HealthProbe      `json:"health"`
				}
				if json.Unmarshal(svc.Deploy, &d) == nil {
					spec.Image, spec.Replicas, spec.Port, spec.Env, spec.Health =
						d.Image, d.Replicas, d.Port, d.Env, d.Health
					if spec.Replicas <= 0 {
						spec.Replicas = 1
					}
					if spec.Port == 0 {
						spec.Port = 8080
					}
				}
				env.Services = append(env.Services, spec)
			}
			var dbs []store.Database
			if err := db.WithContext(ctx).Where("namespace_id = ?", ns.ID).Find(&dbs).Error; err != nil {
				return nil, err
			}
			for _, dbRow := range dbs {
				spec := DatabaseSpec{Name: dbRow.Name}
				var cfg struct {
					Image     string `json:"image"`
					StorageGB int64  `json:"storage_gb"`
				}
				if json.Unmarshal(dbRow.Config, &cfg) == nil {
					spec.Image, spec.StorageGB = cfg.Image, cfg.StorageGB
				}
				if spec.StorageGB <= 0 {
					spec.StorageGB = 5
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
