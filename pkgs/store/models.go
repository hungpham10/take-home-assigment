package store

import (
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
	ID        string    `gorm:"column:id;primaryKey;type:uuid"`
	Name      string    `gorm:"column:name"`
	Slug      string    `gorm:"column:slug"`
	Status    string    `gorm:"column:status"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName maps Tenant to its PostgreSQL table.
func (Tenant) TableName() string { return "tbl_tenants" }

// BeforeCreate fills the UUID primary key app-side.
func (Tenant) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// TenantHost maps a hostname to a tenant (tbl_tenant_hosts).
type TenantHost struct {
	ID        string    `gorm:"column:id;primaryKey;type:uuid"`
	TenantID  string    `gorm:"column:tenant_id"`
	Host      string    `gorm:"column:host"`
	IsDefault bool      `gorm:"column:is_default"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName maps TenantHost to its PostgreSQL table.
func (TenantHost) TableName() string { return "tbl_tenant_hosts" }

// BeforeCreate fills the UUID primary key app-side.
func (TenantHost) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// AuthProfile is a per-tenant OIDC/JWT config (tbl_tenant_auth_profiles).
type AuthProfile struct {
	ID        string    `gorm:"column:id;primaryKey;type:uuid"`
	TenantID  string    `gorm:"column:tenant_id"`
	Name      string    `gorm:"column:name"`
	Issuer    *string   `gorm:"column:issuer"`
	ClientID  *string   `gorm:"column:client_id"`
	JwksURI   *string   `gorm:"column:jwks_uri"`
	Config    []byte    `gorm:"column:config;type:jsonb"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName maps AuthProfile to its PostgreSQL table.
func (AuthProfile) TableName() string { return "tbl_tenant_auth_profiles" }

// BeforeCreate fills the UUID primary key app-side.
func (AuthProfile) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// User is a tenant-scoped principal (tbl_users).
type User struct {
	ID          string    `gorm:"column:id;primaryKey;type:uuid"`
	TenantID    string    `gorm:"column:tenant_id"`
	Email       string    `gorm:"column:email"`
	DisplayName string    `gorm:"column:display_name"`
	OwnerUserID *string   `gorm:"column:owner_user_id"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName maps User to its PostgreSQL table.
func (User) TableName() string { return "tbl_users" }

// BeforeCreate fills the UUID primary key app-side.
func (User) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// UserRole assigns a role to a user (tbl_user_roles).
type UserRole struct {
	ID        string    `gorm:"column:id;primaryKey;type:uuid"`
	UserID    string    `gorm:"column:user_id"`
	Role      string    `gorm:"column:role"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// TableName maps UserRole to its PostgreSQL table.
func (UserRole) TableName() string { return "tbl_user_roles" }

// BeforeCreate fills the UUID primary key app-side.
func (UserRole) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// AccessToken is a hashed abt_ service token (tbl_access_tokens).
type AccessToken struct {
	ID             string     `gorm:"column:id;primaryKey;type:uuid"`
	UserID         string     `gorm:"column:user_id"`
	Name           string     `gorm:"column:name"`
	TokenHash      string     `gorm:"column:token_hash"`
	Prefix         string     `gorm:"column:prefix"`
	TokenEncrypted []byte     `gorm:"column:token_encrypted;type:bytea"`
	ExpiresAt      *time.Time `gorm:"column:expires_at"`
	RevokedAt      *time.Time `gorm:"column:revoked_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName maps AccessToken to its PostgreSQL table.
func (AccessToken) TableName() string { return "tbl_access_tokens" }

// BeforeCreate fills the UUID primary key app-side.
func (AccessToken) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// AuditLog is the append-only audit trail (tbl_audit_log).
type AuditLog struct {
	ID         int64     `gorm:"column:id;primaryKey"`
	TenantID   *string   `gorm:"column:tenant_id"`
	Actor      []byte    `gorm:"column:actor;type:jsonb"`
	Action     string    `gorm:"column:action"`
	ObjectType string    `gorm:"column:object_type"`
	ObjectID   *string   `gorm:"column:object_id"`
	Details    []byte    `gorm:"column:details;type:jsonb"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

// TableName maps AuditLog to its PostgreSQL table.
func (AuditLog) TableName() string { return "tbl_audit_log" }

// Namespace is the current-state environment row (tbl_namespaces).
type Namespace struct {
	ID               string     `gorm:"column:id;primaryKey;type:uuid"`
	TenantID         string     `gorm:"column:tenant_id"`
	OwnerUserID      string     `gorm:"column:owner_user_id"`
	Name             string     `gorm:"column:name"`
	Description      string     `gorm:"column:description"`
	Status           string     `gorm:"column:status"`
	ReadyForLoadtest bool       `gorm:"column:ready_for_loadtest"`
	Version          int        `gorm:"column:version"`
	TTLExpiresAt     *time.Time `gorm:"column:ttl_expires_at"`
	CleanupPolicy    []byte     `gorm:"column:cleanup_policy;type:jsonb"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

// TableName maps Namespace to its PostgreSQL table.
func (Namespace) TableName() string { return "tbl_namespaces" }

// BeforeCreate fills the UUID primary key app-side.
func (Namespace) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// NamespaceEvent is an append-only status transition log (tbl_namespace_events).
type NamespaceEvent struct {
	ID         int64     `gorm:"column:id;primaryKey"`
	ObjectID   string    `gorm:"column:object_id"`
	FromStatus *string   `gorm:"column:from_status"`
	ToStatus   string    `gorm:"column:to_status"`
	Reason     *string   `gorm:"column:reason"`
	Actor      []byte    `gorm:"column:actor;type:jsonb"`
	Metadata   []byte    `gorm:"column:metadata;type:jsonb"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

// TableName maps NamespaceEvent to its PostgreSQL table.
func (NamespaceEvent) TableName() string { return "tbl_namespace_events" }

// IdempotencyKey stores replayable POST responses (tbl_idempotency_keys).
type IdempotencyKey struct {
	ID             string    `gorm:"column:id;primaryKey;type:uuid"`
	TenantID       string    `gorm:"column:tenant_id"`
	Endpoint       string    `gorm:"column:endpoint"`
	Key            string    `gorm:"column:key"`
	RequestHash    string    `gorm:"column:request_hash"`
	ResponseStatus *int      `gorm:"column:response_status"`
	ResponseBody   *string   `gorm:"column:response_body"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

// TableName maps IdempotencyKey to its PostgreSQL table.
func (IdempotencyKey) TableName() string { return "tbl_idempotency_keys" }

// BeforeCreate fills the UUID primary key app-side.
func (IdempotencyKey) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// Service is a namespace component with an OpenAPI spec (tbl_services).
type Service struct {
	ID            string    `gorm:"column:id;primaryKey;type:uuid"`
	NamespaceID   string    `gorm:"column:namespace_id"`
	Name          string    `gorm:"column:name"`
	OpenAPISpec   []byte    `gorm:"column:openapi_spec;type:jsonb"`
	SpecHash      *string   `gorm:"column:spec_hash"`
	ParsedSummary []byte    `gorm:"column:parsed_summary;type:jsonb"`
	Version       int       `gorm:"column:version"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

// TableName maps Service to its PostgreSQL table.
func (Service) TableName() string { return "tbl_services" }

// BeforeCreate fills the UUID primary key app-side.
func (Service) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// Database is a namespace component (tbl_databases).
type Database struct {
	ID          string    `gorm:"column:id;primaryKey;type:uuid"`
	NamespaceID string    `gorm:"column:namespace_id"`
	Name        string    `gorm:"column:name"`
	Engine      string    `gorm:"column:engine"`
	Profile     []byte    `gorm:"column:profile;type:jsonb"`
	Version     int       `gorm:"column:version"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName maps Database to its PostgreSQL table.
func (Database) TableName() string { return "tbl_databases" }

// BeforeCreate fills the UUID primary key app-side.
func (Database) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// Ingress is a namespace component (tbl_ingress).
type Ingress struct {
	ID          string    `gorm:"column:id;primaryKey;type:uuid"`
	NamespaceID string    `gorm:"column:namespace_id"`
	Name        string    `gorm:"column:name"`
	Host        string    `gorm:"column:host"`
	Paths       []byte    `gorm:"column:paths;type:jsonb"`
	Version     int       `gorm:"column:version"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName maps Ingress to its PostgreSQL table.
func (Ingress) TableName() string { return "tbl_ingress" }

// BeforeCreate fills the UUID primary key app-side.
func (Ingress) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// Dependency references a service/database/mock used by the env (tbl_dependencies).
type Dependency struct {
	ID          string    `gorm:"column:id;primaryKey;type:uuid"`
	NamespaceID string    `gorm:"column:namespace_id"`
	Type        string    `gorm:"column:type"`
	RefID       *string   `gorm:"column:ref_id"`
	Config      []byte    `gorm:"column:config;type:jsonb"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName maps Dependency to its PostgreSQL table.
func (Dependency) TableName() string { return "tbl_dependencies" }

// BeforeCreate fills the UUID primary key app-side.
func (Dependency) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }

// NetworkPolicy is the resolved egress spec (tbl_network_policies).
type NetworkPolicy struct {
	ID                 string    `gorm:"column:id;primaryKey;type:uuid"`
	NamespaceID        string    `gorm:"column:namespace_id"`
	ResolvedFromDeps   bool      `gorm:"column:resolved_from_deps"`
	Spec               []byte    `gorm:"column:spec;type:jsonb"`
	ObservedGeneration int64     `gorm:"column:observed_generation"`
	CreatedAt          time.Time `gorm:"column:created_at"`
	UpdatedAt          time.Time `gorm:"column:updated_at"`
}

// TableName maps NetworkPolicy to its PostgreSQL table.
func (NetworkPolicy) TableName() string { return "tbl_network_policies" }

// BeforeCreate fills the UUID primary key app-side.
func (NetworkPolicy) BeforeCreate(db *gorm.DB) error { return beforeCreateUUID(db) }
