package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
)

// MaxScriptSize mirrors the server-side ConfigMap limit (pkgs/script.MaxSize).
// Checking it client-side turns a round-trip-and-422 into an immediate error.
const MaxScriptSize = 1 << 20 // 1 MiB

// scriptCommand implements `ltp script <action>`.
func (a *app) scriptCommand(action string, args []string) (int, error) {
	switch action {
	case "push":
		return a.scriptPush(args)
	case "show":
		return a.scriptShow(args)
	case "":
		return exitUsage, fmt.Errorf("usage: ltp script <push|show>")
	default:
		return exitUsage, fmt.Errorf("unknown script action %q", action)
	}
}

func (a *app) scriptPush(args []string) (int, error) {
	fs := flag.NewFlagSet("script push", flag.ContinueOnError)
	fs.SetOutput(a.err)
	name := fs.String("name", "", "script name (default: file base name)")
	if err := fs.Parse(reorderForFlags(fs, args)); err != nil {
		return exitUsage, err
	}
	pos := fs.Args()
	if len(pos) < 2 {
		return exitUsage, fmt.Errorf("usage: ltp script push <ns-id> <file.js> [--name NAME]")
	}
	nsID, path := pos[0], pos[1]
	content, err := os.ReadFile(path)
	if err != nil {
		return exitRuntime, fmt.Errorf("read script: %w", err)
	}
	if len(content) > MaxScriptSize {
		return exitState, fmt.Errorf(
			"script %s is %d bytes, over the %d-byte (1 MiB) ConfigMap limit",
			path, len(content), MaxScriptSize)
	}
	if len(content) == 0 {
		return exitUsage, fmt.Errorf("script %s is empty", path)
	}
	if *name == "" {
		*name = filepath.Base(path)
	}

	client := a.newClient(a.g)
	sc, err := client.UploadScript(ctx(), nsID, *name, string(content))
	if err != nil {
		return exitFor(err), err
	}
	// Upload returns PENDING_VALIDATION (or a terminal status when validation
	// runs synchronously); poll until terminal so a following `run start` is
	// not rejected by the SCRIPT_NOT_VALIDATED gate.
	sc, ok, err := client.WaitForScriptValidated(ctx(), nsID, sc.ID, a.g.every, a.g.wait)
	if err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		if err := a.printJSON(sc); err != nil {
			return exitRuntime, err
		}
	} else {
		fmt.Fprintf(a.out, "script %s (%s) → %s\n", sc.ID, sc.Name, sc.Status)
	}
	if !ok {
		fmt.Fprintf(a.err, "script is %s; fix the errors and run: ltp script push %s %s\n",
			sc.Status, nsID, path)
		return exitState, fmt.Errorf("script %s not validated", sc.ID)
	}
	if !a.g.jsonOut {
		fmt.Fprintf(a.out, "next: ltp run start %s --script %s\n", nsID, sc.ID)
	}
	return exitOK, nil
}

func (a *app) scriptShow(args []string) (int, error) {
	if len(args) < 2 {
		return exitUsage, fmt.Errorf("usage: ltp script show <ns-id> <script-id>")
	}
	sc, err := a.newClient(a.g).GetScript(ctx(), args[0], args[1])
	if err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		return exitOK, a.printJSON(sc)
	}
	tw := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "id\t%s\nname\t%s\nstatus\t%s\nsize\t%d bytes\n", sc.ID, sc.Name, sc.Status, sc.SizeByte)
	return exitOK, tw.Flush()
}
