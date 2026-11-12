// Package version exposes the build version of the platform binaries.
//
// The single source of truth is the git tag (docs/versioning.md): the build
// injects it with
//
//	-ldflags "-X github.com/hungpham/loadtest-platform/pkgs/version.Version=0.1.1 \
//	          -X github.com/hungpham/loadtest-platform/pkgs/version.Commit=1ead5d4 \
//	          -X github.com/hungpham/loadtest-platform/pkgs/version.BuildDate=2026-11-12T09:00:00Z"
//
// When the linker flags are absent (plain `go build` in a git checkout), Commit
// and BuildDate fall back to the VCS metadata Go embeds automatically. Version
// has no such fallback: it stays "dev" unless injected, so a binary can never
// claim a release it was not built for.
package version

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// Injected at build time via -ldflags. Do not set these from application code.
var (
	Version   = "dev"
	Commit    = ""
	BuildDate = ""
)

// Info is the JSON contract of GET /version (docs/versioning.md §2).
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
}

// readBuildInfo is a seam so tests can stub the VCS metadata lookup.
var readBuildInfo = debug.ReadBuildInfo

// Get returns the build information, filling Commit/BuildDate from the VCS
// metadata Go records in the binary when the linker flags were not supplied.
func Get() Info {
	info := Info{Version: Version, Commit: Commit, BuildDate: BuildDate, GoVersion: runtime.Version()}
	if info.Version == "" {
		info.Version = "dev"
	}
	if info.Commit != "" && info.BuildDate != "" {
		return info
	}
	bi, ok := readBuildInfo()
	if !ok || bi == nil {
		return info
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = shortRevision(s.Value)
			}
		case "vcs.time":
			if info.BuildDate == "" {
				info.BuildDate = s.Value
			}
		}
	}
	return info
}

// String renders the one-line version banner used by --version and startup logs:
// "v0.1.1 (1ead5d4, 2026-11-12T09:00:00Z)". Missing fields are omitted.
func String() string {
	info := Get()
	v := info.Version
	if !strings.HasPrefix(v, "v") && v != "dev" {
		v = "v" + v
	}
	parts := make([]string, 0, 2)
	if info.Commit != "" {
		parts = append(parts, info.Commit)
	}
	if info.BuildDate != "" {
		parts = append(parts, info.BuildDate)
	}
	if len(parts) == 0 {
		return v
	}
	return fmt.Sprintf("%s (%s)", v, strings.Join(parts, ", "))
}

func shortRevision(rev string) string {
	const n = 7
	if len(rev) > n {
		return rev[:n]
	}
	return rev
}

// PrintIfRequested handles the --version/-version/version flags shared by both
// binaries. It prints the banner and reports true when the caller should exit 0
// before doing any work (config parsing, DB connection, ...).
func PrintIfRequested(args []string, out io.Writer) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "--version", "-version", "version":
		_, _ = fmt.Fprintln(out, String())
		return true
	default:
		return false
	}
}
