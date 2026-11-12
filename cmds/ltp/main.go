// Command ltp is the developer CLI for the load-testing platform: the
// self-service flow (namespace → script → run → report) over the BFF API.
//
// Exit codes are part of the contract because CI wraps this binary:
//
//	0  success
//	1  runtime/transport error (server down, 5xx, malformed response)
//	2  usage error (bad flag, unknown command, missing argument)
//	3  state/precondition failure (409/422: NAMESPACE_NOT_READY,
//	   SCRIPT_NOT_VALIDATED, VUS_LIMIT_EXCEEDED, run did not COMPLETE, …)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/ltpclient"
	"github.com/hungpham/loadtest-platform/pkgs/version"
)

// Exit codes (docs/developer-cli.md).
const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
	exitState   = 3
)

// globals are the flags accepted before the subcommand.
type globals struct {
	server  string
	token   string
	tenant  string
	user    string
	role    string
	jsonOut bool
	timeout time.Duration
	wait    time.Duration
	every   time.Duration
}

// app carries everything a command needs; tests inject streams and a client
// factory instead of touching os.Stdout / the network.
type app struct {
	out io.Writer
	err io.Writer
	g   globals
	// newClient is overridable in tests.
	newClient func(g globals) *ltpclient.Client
}

func defaultGlobals() globals {
	return globals{
		server:  envOr("LTP_SERVER", "http://localhost:8080"),
		token:   envOr("LTP_TOKEN", ""),
		tenant:  envOr("LTP_TENANT", ""),
		user:    envOr("LTP_USER", ""),
		role:    envOr("LTP_ROLE", "user"),
		timeout: 30 * time.Second,
		wait:    10 * time.Minute,
		every:   2 * time.Second,
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	// `--version`/`version` is the "which build is this?" probe (docs/versioning.md);
	// it must answer before any flag parsing or network access.
	if version.PrintIfRequested(args, stdout) {
		return exitOK
	}
	a := &app{
		out: stdout,
		err: stderr,
		g:   defaultGlobals(),
		newClient: func(g globals) *ltpclient.Client {
			return &ltpclient.Client{
				BaseURL:  g.server,
				Token:    g.token,
				TenantID: g.tenant,
				UserID:   g.user,
				Role:     g.role,
				HTTP:     &http.Client{Timeout: g.timeout},
			}
		},
	}
	rest, err := a.parseGlobals(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage())
			return exitOK
		}
		fmt.Fprintf(stderr, "ltp: %v\n\n%s", err, usage())
		return exitUsage
	}
	if len(rest) == 0 || rest[0] == "help" || rest[0] == "--help" || rest[0] == "-h" {
		fmt.Fprint(stdout, usage())
		return exitOK
	}
	code, err := a.dispatch(rest)
	if err != nil {
		a.printError(err)
		return code
	}
	return code
}

// parseGlobals consumes leading global flags and returns the remaining args.
func (a *app) parseGlobals(args []string) ([]string, error) {
	fs := flag.NewFlagSet("ltp", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // we render usage ourselves
	fs.StringVar(&a.g.server, "server", a.g.server, "BFF base URL (env LTP_SERVER)")
	fs.StringVar(&a.g.token, "token", a.g.token, "abt_ bearer token (env LTP_TOKEN)")
	fs.StringVar(&a.g.tenant, "tenant", a.g.tenant, "dev auth: tenant id header (env LTP_TENANT)")
	fs.StringVar(&a.g.user, "user", a.g.user, "dev auth: user id header (env LTP_USER)")
	fs.StringVar(&a.g.role, "role", a.g.role, "dev auth: role header (env LTP_ROLE)")
	fs.BoolVar(&a.g.jsonOut, "json", false, "machine-readable output")
	fs.DurationVar(&a.g.timeout, "timeout", a.g.timeout, "per-request timeout")
	fs.DurationVar(&a.g.wait, "wait-timeout", a.g.wait, "deadline for wait/watch")
	fs.DurationVar(&a.g.every, "poll", a.g.every, "poll interval for wait/watch")
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		// Parse one token at a time so flags may appear in any order before the
		// subcommand; parse the value form too (--server URL).
		tok := args[0]
		consumed := 1
		name := strings.TrimLeft(tok, "-")
		if i := strings.IndexByte(name, '='); i < 0 && globalNeedsValue(name) && len(args) > 1 {
			consumed = 2
		}
		if err := fs.Parse(args[:consumed]); err != nil {
			return nil, err
		}
		args = args[consumed:]
	}
	if a.g.server == "" {
		return nil, errors.New("--server is required")
	}
	return args, nil
}

func globalNeedsValue(name string) bool {
	switch name {
	case "server", "token", "tenant", "user", "role", "timeout", "wait-timeout", "poll":
		return true
	}
	return false
}

// reorderForFlags moves flags before positionals so that both
// `ltp run start ns-1 --script sc-1` and `ltp run start --script sc-1 ns-1`
// work. The stdlib flag package stops at the first non-flag argument, and
// putting positionals first is the natural way to type these commands.
func reorderForFlags(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if len(arg) < 2 || arg[0] != '-' {
			positionals = append(positionals, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if strings.ContainsRune(name, '=') {
			flags = append(flags, arg)
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			// Unknown flag: keep it so flag.Parse reports the usage error.
			flags = append(flags, arg)
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			flags = append(flags, arg)
			continue
		}
		flags = append(flags, arg)
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positionals...)
}

// dispatch routes <group> <action> [flags] [args].
func (a *app) dispatch(args []string) (int, error) {
	group := args[0]
	action, rest := "", []string{}
	if len(args) > 1 {
		action = args[1]
	}
	if len(args) > 2 {
		rest = args[2:]
	}
	switch group {
	case "ns", "namespace":
		return a.nsCommand(action, rest)
	case "script":
		return a.scriptCommand(action, rest)
	case "run", "loadtest":
		return a.runCommand(action, rest)
	default:
		return exitUsage, fmt.Errorf("unknown command %q\n\n%s", group, usage())
	}
}

// printError maps a client error to an exit code and a human message.
func (a *app) printError(err error) {
	var apiErr *ltpclient.Error
	if errors.As(err, &apiErr) {
		fmt.Fprintf(a.err, "error: %s\n", apiErr.Error())
		return
	}
	fmt.Fprintf(a.err, "error: %v\n", err)
}

// exitFor maps an error to the CLI exit code (409/422 → 3).
func exitFor(err error) int {
	if err == nil {
		return exitOK
	}
	if ltpclient.IsStateFailure(err) {
		return exitState
	}
	return exitRuntime
}

// --- output helpers --------------------------------------------------------

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// usage is the single source of truth for the help text; docs/developer-cli.md
// mirrors it and plan 41 diffs the two.
func usage() string {
	return `ltp — developer CLI for the load-testing platform

Usage:
  ltp [global flags] <group> <action> [flags] [args]

Global flags:
  --server URL        BFF base URL              (env LTP_SERVER, default http://localhost:8080)
  --token TOKEN       abt_ bearer token         (env LTP_TOKEN)
  --tenant ID         dev auth: X-Tenant-Id     (env LTP_TENANT)
  --user ID           dev auth: X-User-Id       (env LTP_USER)
  --role ROLE         dev auth: X-User-Role     (env LTP_ROLE, default user)
  --json              machine-readable output
  --timeout DURATION  per-request timeout       (default 30s)
  --wait-timeout D    deadline for wait/watch   (default 10m)
  --poll DURATION     poll interval for wait/watch (default 2s)

Groups:
  ns create   --name NAME [--slug SLUG] [--spec file.json]
  ns list
  ns show     <ns-id>
  ns validate <ns-id>
  ns apply    <ns-id>
  ns wait     <ns-id>
  ns delete   <ns-id>

  script push <ns-id> <file.js> [--name NAME]
  script show <ns-id> <script-id>

  run start   <ns-id> --script <script-id> [--name NAME] [--vus N] [--duration 2m] [--timeout 15]
  run watch   <ns-id> <lt-id>
  run stop    <ns-id> <lt-id>
  run report  <ns-id> <lt-id>

  version             print the build banner (same as --version)

Exit codes:
  0 success · 1 runtime error · 2 usage error · 3 state failure (409/422, run not COMPLETED)
`
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ctx is the context used by commands (kept small so tests can cancel).
func ctx() context.Context { return context.Background() }
