// orchestration.go — wiring for the two control loops (plans/21) when the
// cluster path is enabled (K8S_ENABLED=true): Env Reconciler (desired → K8s)
// and Status Watcher (observed K8s → PG). Without the flag nothing here runs
// and the legacy synchronous apply path serves the MVP stack.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/hungpham/loadtest-platform/pkgs/envreconciler"
	"github.com/hungpham/loadtest-platform/pkgs/mockcontrol"
	"github.com/hungpham/loadtest-platform/pkgs/k8s"
	"github.com/hungpham/loadtest-platform/pkgs/statuswatcher"
	"github.com/hungpham/loadtest-platform/pkgs/store"
)

// envLoader reads the namespaces with pending work and renders their desired
// state from the component tables (services/databases/ingresses + the
// resolved network policy).
func envLoader(db *gorm.DB) envreconciler.EnvLoader {
	return func(ctx context.Context) ([]*envreconciler.Env, error) {
		var rows []store.Namespace
		if err := db.WithContext(ctx).
			Where("status IN ?", []string{"APPLYING", "DATA_LOADING", "DELETING"}).
			Find(&rows).Error; err != nil {
			return nil, err
		}
		envs := make([]*envreconciler.Env, 0, len(rows))
		for _, ns := range rows {
			env := &envreconciler.Env{
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
				spec := envreconciler.ServiceSpec{Name: svc.Name, SpecHash: derefS(svc.SpecHash)}
				var d struct {
					Image    string                     `json:"image"`
					Replicas int32                      `json:"replicas"`
					Port     int32                      `json:"port"`
					Env      map[string]string          `json:"env"`
					Health   *envreconciler.HealthProbe `json:"health"`
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
				spec := envreconciler.DatabaseSpec{Name: dbRow.Name}
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
				spec := envreconciler.IngressSpec{Name: ing.Name, IngressClass: ing.Class}
				var hosts []struct {
					Rules []envreconciler.IngressRule `json:"rules"`
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
				var ps envreconciler.PolicySpec
				if json.Unmarshal(policy.Spec, &ps) == nil {
					env.Policy = &ps
				}
			}
			envs = append(envs, env)
		}
		return envs, nil
	}
}

func derefS(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// startOrchestration boots the two loops; both stop when ctx is cancelled.
func startOrchestration(ctx context.Context, db *gorm.DB, apiURL string, resync, grace, stable, applyDeadline, staleness time.Duration) error {
	client, err := k8s.NewDynamicClient(apiURL, "loadtest-controller")
	if err != nil {
		return err
	}
	rc := envreconciler.New(client, envreconciler.Options{})
	mockcontrol.Attach(rc) // binding ConfigMaps + MOCK_* env injection (step 5)
	go rc.Start(ctx, envLoader(db))

	w := statuswatcher.New(client, db, statuswatcher.Config{
		Resync:             resync,
		Grace:              grace,
		Stable:             stable,
		ApplyDeadline:      applyDeadline,
		StalenessThreshold: staleness,
	})
	go w.Start(ctx)
	slog.Info("orchestration loops started", "api", apiURL, "resync", resync)
	return nil
}
