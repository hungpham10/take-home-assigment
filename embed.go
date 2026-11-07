// Package platform exposes the embedded SQL migrations so binaries can run
// them via pkgs/store.Migrate. sql/ at the repo root stays the single source
// of truth; this file only embeds it.
package platform

import "embed"

// SQL is the embedded sql/ migration filesystem (single source of truth).
//go:embed sql/*.sql
var SQL embed.FS

// Migrations returns the embedded sql/ migration filesystem.
func Migrations() embed.FS { return SQL }
