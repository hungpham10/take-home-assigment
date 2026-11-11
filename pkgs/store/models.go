package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// beforeCreate fills UUID primary keys app-side (column DEFAULTs are only a
// fallback for raw SQL/psql usage).
func beforeCreateUUID(db *gorm.DB) error {
	if db.Statement.Schema != nil {
		for _, f := range db.Statement.Schema.PrimaryFields {
			if v, ok := f.ValueOf(db.Statement.Context, db.Statement.ReflectValue); !ok || v == "" {
				_ = f.Set(db.Statement.Context, db.Statement.ReflectValue, uuid.NewString())
			}
		}
	}
	return nil
}

// GORM models for the platform schema (sql/ migrations are the source of
// truth — these structs only map columns; migrations never auto-run from them).

// Tenant is an org boundary (tbl_tenants).
type Tenant struct {
	ID        string    `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	Name      string    `gorm:"column:name" json:"name"`
	Slug      string    `gorm:"column:slug" json:"slug"`
	Status    string    `gorm:"column:status" json:"status"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps Tenant to its PostgreSQL table.
func (Tenant) TableName() string { return "tbl_tenants" }

// BeforeCreate fills the UUID primary key app-side.
func (Tenant) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// TenantHost maps a hostname to a tenant (tbl_tenant_hosts).
type TenantHost struct {
	ID        string    `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	TenantID  string    `gorm:"column:tenant_id" json:"tenant_id"`
	Host      string    `gorm:"column:host" json:"host"`
	IsDefault bool      `gorm:"column:is_default" json:"is_default"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps TenantHost to its PostgreSQL table.
func (TenantHost) TableName() string { return "tbl_tenant_hosts" }

// BeforeCreate fills the UUID primary key app-side.
func (TenantHost) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// AuthProfile is a per-tenant OIDC/JWT config (tbl_tenant_auth_profiles).
type AuthProfile struct {
	ID        string    `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	TenantID  string    `gorm:"column:tenant_id" json:"tenant_id"`
	Name      string    `gorm:"column:name" json:"name"`
	Issuer    *string   `gorm:"column:issuer" json:"issuer"`
	ClientID  *string   `gorm:"column:client_id" json:"client_id"`
	JwksURI   *string   `gorm:"column:jwks_uri" json:"jwks_uri"`
	Config    []byte    `gorm:"column:config;type:jsonb" json:"config"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps AuthProfile to its PostgreSQL table.
func (AuthProfile) TableName() string { return "tbl_tenant_auth_profiles" }

// BeforeCreate fills the UUID primary key app-side.
func (AuthProfile) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// User is a tenant-scoped principal (tbl_users).
type User struct {
	ID          string    `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	TenantID    string    `gorm:"column:tenant_id" json:"tenant_id"`
	Email       string    `gorm:"column:email" json:"email"`
	DisplayName string    `gorm:"column:display_name" json:"display_name"`
	OwnerUserID *string   `gorm:"column:owner_user_id" json:"owner_user_id"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps User to its PostgreSQL table.
func (User) TableName() string { return "tbl_users" }

// BeforeCreate fills the UUID primary key app-side.
func (User) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// UserRole assigns a role to a user (tbl_user_roles).
type UserRole struct {
	ID        string    `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	UserID    string    `gorm:"column:user_id" json:"user_id"`
	Role      string    `gorm:"column:role" json:"role"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
}

// TableName maps UserRole to its PostgreSQL table.
func (UserRole) TableName() string { return "tbl_user_roles" }

// BeforeCreate fills the UUID primary key app-side.
func (UserRole) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// UserExternalIdentity links an IdP subject to a local user
// (tbl_user_external_identities, plan 37). OIDC `sub` is opaque, so this is the
// only bridge to tbl_users.id.
type UserExternalIdentity struct {
	ID        string    `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	UserID    string    `gorm:"column:user_id" json:"user_id"`
	Issuer    string    `gorm:"column:issuer" json:"issuer"`
	Subject   string    `gorm:"column:subject" json:"subject"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
}

// TableName maps UserExternalIdentity to its PostgreSQL table.
func (UserExternalIdentity) TableName() string { return "tbl_user_external_identities" }

// BeforeCreate fills the UUID primary key app-side.
func (UserExternalIdentity) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// AccessToken is a hashed abt_ service token (tbl_access_tokens).
type AccessToken struct {
	ID             string     `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	UserID         string     `gorm:"column:user_id" json:"user_id"`
	Name           string     `gorm:"column:name" json:"name"`
	TokenHash      string     `gorm:"column:token_hash" json:"token_hash"`
	Prefix         string     `gorm:"column:prefix" json:"prefix"`
	TokenEncrypted []byte     `gorm:"column:token_encrypted;type:bytea" json:"token_encrypted"`
	ExpiresAt      *time.Time `gorm:"column:expires_at" json:"expires_at"`
	RevokedAt      *time.Time `gorm:"column:revoked_at" json:"revoked_at"`
	CreatedAt      time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps AccessToken to its PostgreSQL table.
func (AccessToken) TableName() string { return "tbl_access_tokens" }

// BeforeCreate fills the UUID primary key app-side.
func (AccessToken) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// AuditLog is the append-only audit trail (tbl_audit_log).
type AuditLog struct {
	ID         int64     `gorm:"column:id;primaryKey" json:"id"`
	TenantID   *string   `gorm:"column:tenant_id" json:"tenant_id"`
	Actor      []byte    `gorm:"column:actor;type:jsonb" json:"actor"`
	Action     string    `gorm:"column:action" json:"action"`
	ObjectType string    `gorm:"column:object_type" json:"object_type"`
	ObjectID   *string   `gorm:"column:object_id" json:"object_id"`
	Details    []byte    `gorm:"column:details;type:jsonb" json:"details"`
	CreatedAt  time.Time `gorm:"column:created_at" json:"created_at"`
}

// TableName maps AuditLog to its PostgreSQL table.
func (AuditLog) TableName() string { return "tbl_audit_log" }

// Namespace is the current-state environment row (tbl_namespaces).
type Namespace struct {
	ID               string  `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	TenantID         string  `gorm:"column:tenant_id" json:"tenant_id"`
	OwnerUserID      string  `gorm:"column:owner_user_id" json:"owner_user_id"`
	Name             string  `gorm:"column:name" json:"name"`
	Slug             *string `gorm:"column:slug" json:"slug,omitempty"`
	Visibility       *string `gorm:"column:visibility;default:'tenant'" json:"visibility,omitempty"`
	Description      string  `gorm:"column:description" json:"description"`
	Status           string  `gorm:"column:status" json:"status"`
	ReadyForLoadtest bool    `gorm:"column:ready_for_loadtest" json:"ready_for_loadtest"`
	// DesiredGeneration is bumped by apply/spec changes; the Status Watcher
	// compares it against observed_generation (generation fencing, WATCH-03).
	DesiredGeneration int64      `gorm:"column:desired_generation" json:"-"`
	BlockingReasons   []byte     `gorm:"column:blocking_reasons;type:jsonb" json:"-"`
	StatusObservedAt  *time.Time `gorm:"column:status_observed_at" json:"-"`
	Version           int        `gorm:"column:version" json:"version"`
	TTLExpiresAt      *time.Time `gorm:"column:ttl_expires_at" json:"ttl_expires_at"`
	CleanupPolicy     []byte     `gorm:"column:cleanup_policy;type:jsonb" json:"cleanup_policy"`
	CreatedAt         time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps Namespace to its PostgreSQL table.
func (Namespace) TableName() string { return "tbl_namespaces" }

// BeforeCreate fills the UUID primary key app-side.
func (Namespace) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// NamespaceEvent is an append-only status transition log (tbl_namespace_events).
type NamespaceEvent struct {
	ID         int64     `gorm:"column:id;primaryKey" json:"id"`
	ObjectID   string    `gorm:"column:object_id" json:"object_id"`
	FromStatus *string   `gorm:"column:from_status" json:"from_status"`
	ToStatus   string    `gorm:"column:to_status" json:"to_status"`
	Reason     *string   `gorm:"column:reason" json:"reason"`
	Actor      []byte    `gorm:"column:actor;type:jsonb" json:"actor"`
	Metadata   []byte    `gorm:"column:metadata;type:jsonb" json:"metadata"`
	CreatedAt  time.Time `gorm:"column:created_at" json:"created_at"`
}

// TableName maps NamespaceEvent to its PostgreSQL table.
func (NamespaceEvent) TableName() string { return "tbl_namespace_events" }

// IdempotencyKey stores replayable POST responses (tbl_idempotency_keys).
type IdempotencyKey struct {
	ID             string    `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	TenantID       string    `gorm:"column:tenant_id" json:"tenant_id"`
	Endpoint       string    `gorm:"column:endpoint" json:"endpoint"`
	Key            string    `gorm:"column:key" json:"key"`
	RequestHash    string    `gorm:"column:request_hash" json:"request_hash"`
	ResponseStatus *int      `gorm:"column:response_status" json:"response_status"`
	ResponseBody   *string   `gorm:"column:response_body" json:"response_body"`
	CreatedAt      time.Time `gorm:"column:created_at" json:"created_at"`
}

// TableName maps IdempotencyKey to its PostgreSQL table.
func (IdempotencyKey) TableName() string { return "tbl_idempotency_keys" }

// BeforeCreate fills the UUID primary key app-side.
func (IdempotencyKey) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// Service is a namespace component with an OpenAPI spec (tbl_services).
type Service struct {
	ID               string          `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	NamespaceID      string          `gorm:"column:namespace_id" json:"namespace_id"`
	ServiceID        *string         `gorm:"column:service_id" json:"service_id,omitempty"`
	Name             string          `gorm:"column:name" json:"name"`
	Description      string          `gorm:"column:description" json:"description"`
	Deploy           json.RawMessage `gorm:"column:deploy;type:jsonb;default:'{}'" json:"deploy"`
	Mock             json.RawMessage `gorm:"column:mock;type:jsonb;default:'{}'" json:"mock"`
	DatabaseBindings json.RawMessage `gorm:"column:database_bindings;type:jsonb;default:'[]'" json:"database_bindings"`
	Dependencies     json.RawMessage `gorm:"column:dependencies;type:jsonb;default:'[]'" json:"dependencies"`
	OpenAPISpec      []byte          `gorm:"column:openapi_spec;type:jsonb" json:"openapi_spec"`
	SpecHash         *string         `gorm:"column:spec_hash" json:"spec_hash"`
	ParsedSummary    []byte          `gorm:"column:parsed_summary;type:jsonb" json:"parsed_summary"`
	Version          int             `gorm:"column:version" json:"version"`
	CreatedAt        time.Time       `gorm:"column:created_at" json:"created_at"`
	UpdatedAt        time.Time       `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps Service to its PostgreSQL table.
func (Service) TableName() string { return "tbl_services" }

// BeforeCreate fills the UUID primary key app-side.
func (Service) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// Database is a namespace component (tbl_databases). Note: Version (INT) is
// the optimistic-concurrency counter; EngineVersion is the user-facing
// database engine version (e.g. "16").
type Database struct {
	ID            string          `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	NamespaceID   string          `gorm:"column:namespace_id" json:"namespace_id"`
	DatabaseID    *string         `gorm:"column:database_id" json:"database_id,omitempty"`
	Name          string          `gorm:"column:name" json:"name"`
	Engine        string          `gorm:"column:engine" json:"engine"`
	Operator      string          `gorm:"column:operator" json:"operator"`
	EngineVersion *string         `gorm:"column:engine_version" json:"engine_version,omitempty"`
	Config        json.RawMessage `gorm:"column:config;type:jsonb" json:"config"`
	InitScriptRef *string         `gorm:"column:init_script_ref" json:"init_script_ref,omitempty"`
	DataSource    json.RawMessage `gorm:"column:data_source;type:jsonb" json:"data_source"`
	RestorePolicy json.RawMessage `gorm:"column:restore_policy;type:jsonb" json:"restore_policy"`
	Ephemeral     json.RawMessage `gorm:"column:ephemeral;type:jsonb" json:"ephemeral"`
	Mock          json.RawMessage `gorm:"column:mock;type:jsonb" json:"mock"`
	SecretRef     *string         `gorm:"column:secret_ref" json:"secret_ref,omitempty"`
	Profile       []byte          `gorm:"column:profile;type:jsonb" json:"profile"`
	Version       int             `gorm:"column:version" json:"version"`
	CreatedAt     time.Time       `gorm:"column:created_at" json:"created_at"`
	UpdatedAt     time.Time       `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps Database to its PostgreSQL table.
func (Database) TableName() string { return "tbl_databases" }

// BeforeCreate fills the UUID primary key app-side.
func (Database) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// Ingress is a namespace component (tbl_ingress).
type Ingress struct {
	ID          string          `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	NamespaceID string          `gorm:"column:namespace_id" json:"namespace_id"`
	IngressID   *string         `gorm:"column:ingress_id" json:"ingress_id,omitempty"`
	Name        string          `gorm:"column:name" json:"name"`
	Class       string          `gorm:"column:class" json:"class"`
	Hosts       json.RawMessage `gorm:"column:hosts;type:jsonb;default:'[]'" json:"hosts"`
	Host        string          `gorm:"column:host" json:"host"`
	Paths       []byte          `gorm:"column:paths;type:jsonb" json:"paths"`
	Version     int             `gorm:"column:version" json:"version"`
	CreatedAt   time.Time       `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time       `gorm:"column:updated_at" json:"updated_at"`
}

// Dataset is a non-prod data snapshot used for restores (tbl_datasets).
type Dataset struct {
	ID                string          `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	TenantID          string          `gorm:"column:tenant_id" json:"tenant_id"`
	NamespaceID       *string         `gorm:"column:namespace_id" json:"namespace_id,omitempty"`
	OwnerUserID       *string         `gorm:"column:owner_user_id" json:"owner_user_id,omitempty"`
	Name              string          `gorm:"column:name" json:"name"`
	SourceRef         string          `gorm:"column:source_ref" json:"source_ref"`
	SizeBytes         int64           `gorm:"column:size_bytes" json:"size_bytes"`
	PIIClassification string          `gorm:"column:pii_classification" json:"pii_classification"`
	Status            string          `gorm:"column:status" json:"status"`
	RestoreConfig     []byte          `gorm:"column:restore_config;type:jsonb" json:"restore_config"`
	Checksum          json.RawMessage `gorm:"column:checksum;type:jsonb" json:"checksum"`
	Engine            *string         `gorm:"column:engine" json:"engine,omitempty"`
	EngineVersion     *string         `gorm:"column:engine_version" json:"engine_version,omitempty"`
	Location          json.RawMessage `gorm:"column:location;type:jsonb" json:"location"`
	RestoreFormat     *string         `gorm:"column:restore_format" json:"restore_format,omitempty"`
	SchemaVersion     *string         `gorm:"column:schema_version" json:"schema_version,omitempty"`
	RetentionUntil    *time.Time      `gorm:"column:retention_until" json:"retention_until,omitempty"`
	Version           int             `gorm:"column:version" json:"version"`
	CreatedAt         time.Time       `gorm:"column:created_at" json:"created_at"`
	UpdatedAt         time.Time       `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps Ingress to its PostgreSQL table.
func (Ingress) TableName() string { return "tbl_ingress" }

// BeforeCreate fills the UUID primary key app-side.
func (Ingress) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// TableName maps Dataset to its PostgreSQL table.
func (Dataset) TableName() string { return "tbl_datasets" }

// BeforeCreate fills the UUID primary key app-side.
func (Dataset) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// Dependency references a service/database/mock used by the env (tbl_dependencies).
type Dependency struct {
	ID          string    `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	NamespaceID string    `gorm:"column:namespace_id" json:"namespace_id"`
	Type        string    `gorm:"column:type" json:"type"`
	RefID       *string   `gorm:"column:ref_id" json:"ref_id"`
	Config      []byte    `gorm:"column:config;type:jsonb" json:"config"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps Dependency to its PostgreSQL table.
func (Dependency) TableName() string { return "tbl_dependencies" }

// BeforeCreate fills the UUID primary key app-side.
func (Dependency) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// NetworkPolicy is the resolved egress spec (tbl_network_policies).
type NetworkPolicy struct {
	ID                 string    `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	NamespaceID        string    `gorm:"column:namespace_id" json:"namespace_id"`
	ResolvedFromDeps   bool      `gorm:"column:resolved_from_deps" json:"resolved_from_deps"`
	Spec               []byte    `gorm:"column:spec;type:jsonb" json:"spec"`
	ObservedGeneration int64     `gorm:"column:observed_generation" json:"observed_generation"`
	CreatedAt          time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt          time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps NetworkPolicy to its PostgreSQL table.
func (NetworkPolicy) TableName() string { return "tbl_network_policies" }

// BeforeCreate fills the UUID primary key app-side.
func (NetworkPolicy) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// Script is the current-state k6 script row (tbl_scripts).
type Script struct {
	ID          string    `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	NamespaceID string    `gorm:"column:namespace_id" json:"namespace_id"`
	Name        string    `gorm:"column:name" json:"name"`
	ContentRef  string    `gorm:"column:content_ref" json:"content_ref"`
	ContentHash string    `gorm:"column:content_hash" json:"content_hash"`
	SizeBytes   int       `gorm:"column:size_bytes" json:"size_bytes"`
	Status      string    `gorm:"column:status" json:"status"`
	Version     int       `gorm:"column:version" json:"version"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps Script to its PostgreSQL table.
func (Script) TableName() string { return "tbl_scripts" }

// BeforeCreate fills the UUID primary key app-side.
func (Script) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// ScriptValidation records one validation attempt (tbl_script_validations).
type ScriptValidation struct {
	ID          int64     `gorm:"column:id;primaryKey" json:"id"`
	ScriptID    string    `gorm:"column:script_id" json:"script_id"`
	Attempt     int       `gorm:"column:attempt" json:"attempt"`
	Status      string    `gorm:"column:status" json:"status"`
	Errors      []byte    `gorm:"column:errors;type:jsonb" json:"errors"`
	ValidatedAt time.Time `gorm:"column:validated_at" json:"validated_at"`
}

// TableName maps ScriptValidation to its PostgreSQL table.
func (ScriptValidation) TableName() string { return "tbl_script_validations" }

// LoadTest is the current-state load test row (tbl_load_tests).
type LoadTest struct {
	ID          string     `gorm:"column:id;primaryKey;type:uuid" json:"id"`
	NamespaceID string     `gorm:"column:namespace_id" json:"namespace_id"`
	ScriptID    string     `gorm:"column:script_id" json:"script_id"`
	Name        string     `gorm:"column:name" json:"name"`
	CreatedBy   string     `gorm:"column:created_by" json:"created_by"`
	LoadConfig  []byte     `gorm:"column:load_config;type:jsonb" json:"load_config"`
	Targets     []byte     `gorm:"column:targets;type:jsonb" json:"targets"`
	RunnerType  string     `gorm:"column:runner_type" json:"runner_type"`
	RunnerRef   *string    `gorm:"column:runner_ref" json:"runner_ref"`
	Status      string     `gorm:"column:status" json:"status"`
	StartedAt   *time.Time `gorm:"column:started_at" json:"started_at"`
	FinishedAt  *time.Time `gorm:"column:finished_at" json:"finished_at"`
	TimeoutAt   *time.Time `gorm:"column:timeout_at" json:"timeout_at"`
	Version     int        `gorm:"column:version" json:"version"`
	CreatedAt   time.Time  `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

// TableName maps LoadTest to its PostgreSQL table.
func (LoadTest) TableName() string { return "tbl_load_tests" }

// BeforeCreate fills the UUID primary key app-side.
func (LoadTest) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// LoadTestEvent is an append-only transition log (tbl_loadtest_events).
type LoadTestEvent struct {
	ID         int64     `gorm:"column:id;primaryKey" json:"id"`
	ObjectID   string    `gorm:"column:object_id" json:"object_id"`
	FromStatus *string   `gorm:"column:from_status" json:"from_status"`
	ToStatus   string    `gorm:"column:to_status" json:"to_status"`
	Reason     *string   `gorm:"column:reason" json:"reason"`
	Actor      []byte    `gorm:"column:actor;type:jsonb" json:"actor"`
	Metadata   []byte    `gorm:"column:metadata;type:jsonb" json:"metadata"`
	CreatedAt  time.Time `gorm:"column:created_at" json:"created_at"`
}

// TableName maps LoadTestEvent to its PostgreSQL table.
func (LoadTestEvent) TableName() string { return "tbl_loadtest_events" }

// TestEvent is the runtime event stream (tbl_test_events).
type TestEvent struct {
	ID         int64     `gorm:"column:id;primaryKey" json:"id"`
	LoadTestID string    `gorm:"column:load_test_id" json:"load_test_id"`
	Seq        int64     `gorm:"column:seq" json:"seq"`
	Type       string    `gorm:"column:type" json:"type"`
	Payload    []byte    `gorm:"column:payload;type:jsonb" json:"payload"`
	CreatedAt  time.Time `gorm:"column:created_at" json:"created_at"`
}

// TableName maps TestEvent to its PostgreSQL table.
func (TestEvent) TableName() string { return "tbl_test_events" }

// ComponentStatus is one row of the Status Watcher projection
// (tbl_component_status, migration 0011): the persisted per-component state
// backing GET /readiness (docs/namespace-status-watcher.md §2).
type ComponentStatus struct {
	ID                 int64      `gorm:"column:id;primaryKey" json:"-"`
	NamespaceID        string     `gorm:"column:namespace_id" json:"namespace_id"`
	ComponentType      string     `gorm:"column:component_type" json:"component_type"`
	ComponentID        string     `gorm:"column:component_id" json:"component_id"`
	Required           bool       `gorm:"column:required" json:"required"`
	DesiredGeneration  int64      `gorm:"column:desired_generation" json:"desired_generation"`
	ObservedGeneration int64      `gorm:"column:observed_generation" json:"observed_generation"`
	Phase              string     `gorm:"column:phase" json:"phase"`
	Reason             *string    `gorm:"column:reason" json:"reason,omitempty"`
	Message            *string    `gorm:"column:message" json:"message,omitempty"`
	LastTransitionAt   *time.Time `gorm:"column:last_transition_at" json:"last_transition_at,omitempty"`
	ObservedAt         time.Time  `gorm:"column:observed_at" json:"observed_at"`
}

// TableName maps ComponentStatus to its PostgreSQL table.
func (ComponentStatus) TableName() string { return "tbl_component_status" }
