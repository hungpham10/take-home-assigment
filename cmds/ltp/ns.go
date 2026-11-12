package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/ltpclient"
)

// nsCommand implements `ltp ns <action>`.
func (a *app) nsCommand(action string, args []string) (int, error) {
	switch action {
	case "create":
		return a.nsCreate(args)
	case "list":
		return a.nsList(args)
	case "show":
		return a.nsShow(args)
	case "validate":
		return a.nsValidate(args)
	case "apply":
		return a.nsApply(args)
	case "wait":
		return a.nsWait(args)
	case "delete":
		return a.nsDelete(args)
	case "":
		return exitUsage, fmt.Errorf("usage: ltp ns <create|list|show|validate|apply|wait|delete>")
	default:
		return exitUsage, fmt.Errorf("unknown ns action %q", action)
	}
}

func (a *app) nsCreate(args []string) (int, error) {
	fs := flag.NewFlagSet("ns create", flag.ContinueOnError)
	fs.SetOutput(a.err)
	name := fs.String("name", "", "namespace name (required)")
	slug := fs.String("slug", "", "namespace slug")
	specPath := fs.String("spec", "", "namespace spec JSON file (services/databases/…)")
	desc := fs.String("description", "", "free-form description")
	if err := fs.Parse(reorderForFlags(fs, args)); err != nil {
		return exitUsage, err
	}
	if *name == "" {
		return exitUsage, fmt.Errorf("ns create: --name is required")
	}
	req := ltpclient.CreateNamespaceRequest{Name: *name, Slug: *slug, Description: *desc}
	if *specPath != "" {
		raw, err := os.ReadFile(*specPath)
		if err != nil {
			return exitRuntime, fmt.Errorf("read spec: %w", err)
		}
		if !json.Valid(raw) {
			return exitUsage, fmt.Errorf("spec %s is not valid JSON", *specPath)
		}
		req.Spec = raw
	}
	res, err := a.newClient(a.g).CreateNamespaceWithSpec(ctx(), req)
	if err != nil {
		return exitFor(err), err
	}
	ns := res.Namespace
	if a.g.jsonOut {
		return exitOK, a.printJSON(res)
	}
	fmt.Fprintf(a.out, "namespace %s created (status %s)\n", ns.ID, ns.Status)
	if len(res.Services) > 0 || len(res.Databases) > 0 {
		fmt.Fprintf(a.out, "  %d service(s), %d database(s) from the spec\n", len(res.Services), len(res.Databases))
	}
	fmt.Fprintf(a.out, "next: ltp ns apply %s && ltp ns wait %s\n", ns.ID, ns.ID)
	return exitOK, nil
}

func (a *app) nsList(args []string) (int, error) {
	if err := flag.NewFlagSet("ns list", flag.ContinueOnError).Parse(args); err != nil {
		return exitUsage, err
	}
	list, err := a.newClient(a.g).ListNamespaces(ctx())
	if err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		return exitOK, a.printJSON(list)
	}
	tw := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tREADY\tCREATED")
	for _, ns := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", ns.ID, ns.Name, ns.Status, ns.ReadyForLoadtest,
			ns.CreatedAt.Format(time.RFC3339))
	}
	return exitOK, tw.Flush()
}

func (a *app) nsShow(args []string) (int, error) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return exitUsage, fmt.Errorf("usage: ltp ns show <ns-id>")
	}
	ns, err := a.newClient(a.g).GetNamespace(ctx(), args[0])
	if err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		return exitOK, a.printJSON(ns)
	}
	tw := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "id\t%s\nname\t%s\nstatus\t%s\nready_for_loadtest\t%t\nversion\t%d\n",
		ns.ID, ns.Name, ns.Status, ns.ReadyForLoadtest, ns.Version)
	return exitOK, tw.Flush()
}

func (a *app) nsValidate(args []string) (int, error) {
	if len(args) < 1 {
		return exitUsage, fmt.Errorf("usage: ltp ns validate <ns-id>")
	}
	r, err := a.newClient(a.g).ValidateNamespace(ctx(), args[0])
	if err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		return exitOK, a.printJSON(r)
	}
	fmt.Fprintf(a.out, "validation passed (status %s, ready=%t)\n", r.Status, r.Ready)
	return exitOK, nil
}

func (a *app) nsApply(args []string) (int, error) {
	if len(args) < 1 {
		return exitUsage, fmt.Errorf("usage: ltp ns apply <ns-id>")
	}
	res, err := a.newClient(a.g).ApplyNamespace(ctx(), args[0])
	if err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		return exitOK, a.printJSON(res)
	}
	fmt.Fprintf(a.out, "apply accepted (status %s)\n", res.Status)
	fmt.Fprintf(a.out, "next: ltp ns wait %s\n", args[0])
	return exitOK, nil
}

func (a *app) nsWait(args []string) (int, error) {
	if len(args) < 1 {
		return exitUsage, fmt.Errorf("usage: ltp ns wait <ns-id>")
	}
	client := a.newClient(a.g)
	r, err := client.WaitForReady(ctx(), args[0], a.g.every, a.g.wait)
	if err != nil {
		// Timeout still carries the last report: print its blockers so the
		// developer knows what to fix instead of just "timed out".
		if r != nil {
			fmt.Fprintf(a.err, "namespace %s: status %s\n", args[0], r.Status)
			for _, reason := range r.BlockingReasons {
				fmt.Fprintf(a.err, "  blocking: %s\n", reason)
			}
			return exitState, err
		}
		return exitFor(err), err
	}
	if a.g.jsonOut {
		return exitOK, a.printJSON(r)
	}
	fmt.Fprintf(a.out, "namespace %s is READY for load testing\n", args[0])
	return exitOK, nil
}

func (a *app) nsDelete(args []string) (int, error) {
	if len(args) < 1 {
		return exitUsage, fmt.Errorf("usage: ltp ns delete <ns-id>")
	}
	if err := a.newClient(a.g).DeleteNamespace(ctx(), args[0]); err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		return exitOK, a.printJSON(map[string]string{"id": args[0], "status": "DELETING"})
	}
	fmt.Fprintf(a.out, "namespace %s → DELETING\n", args[0])
	return exitOK, nil
}
