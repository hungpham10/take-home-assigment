package main

import (
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hungpham/loadtest-platform/pkgs/ltpclient"
)

// runCommand implements `ltp run <action>`.
func (a *app) runCommand(action string, args []string) (int, error) {
	switch action {
	case "start":
		return a.runStart(args)
	case "watch":
		return a.runWatch(args)
	case "stop":
		return a.runStop(args)
	case "report":
		return a.runReport(args)
	case "list":
		return a.runList(args)
	case "":
		return exitUsage, fmt.Errorf("usage: ltp run <start|watch|stop|report|list>")
	default:
		return exitUsage, fmt.Errorf("unknown run action %q", action)
	}
}

func (a *app) runStart(args []string) (int, error) {
	fs := flag.NewFlagSet("run start", flag.ContinueOnError)
	fs.SetOutput(a.err)
	scriptID := fs.String("script", "", "validated script id (required)")
	name := fs.String("name", "", "run name (default: run-<unix>)")
	vus := fs.Int("vus", 20, "virtual users")
	duration := fs.String("duration", "2m", "run duration (Go/k6 syntax: 30s, 10m)")
	timeoutMin := fs.Int("timeout", 15, "reaper timeout in minutes (must exceed duration)")
	if err := fs.Parse(reorderForFlags(fs, args)); err != nil {
		return exitUsage, err
	}
	pos := fs.Args()
	if len(pos) < 1 {
		return exitUsage, fmt.Errorf("usage: ltp run start <ns-id> --script <script-id>")
	}
	if *scriptID == "" {
		return exitUsage, fmt.Errorf("run start: --script is required")
	}
	nsID := pos[0]
	if *name == "" {
		*name = fmt.Sprintf("run-%d", time.Now().Unix())
	}
	lt, err := a.newClient(a.g).CreateLoadTest(ctx(), nsID, ltpclient.CreateLoadTestRequest{
		Name:     *name,
		ScriptID: *scriptID,
		LoadConfig: map[string]any{
			"vusers":          *vus,
			"duration":        *duration,
			"timeout_minutes": *timeoutMin,
		},
	})
	if err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		return exitOK, a.printJSON(lt)
	}
	fmt.Fprintf(a.out, "run %s created (status %s)\n", lt.ID, lt.Status)
	fmt.Fprintf(a.out, "next: ltp run watch %s %s\n", nsID, lt.ID)
	return exitOK, nil
}

func (a *app) runWatch(args []string) (int, error) {
	if len(args) < 2 {
		return exitUsage, fmt.Errorf("usage: ltp run watch <ns-id> <lt-id>")
	}
	nsID, ltID := args[0], args[1]
	lt, err := a.newClient(a.g).WaitForRun(ctx(), nsID, ltID, a.g.every, a.g.wait)
	if err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		if err := a.printJSON(lt); err != nil {
			return exitRuntime, err
		}
	} else {
		fmt.Fprintf(a.out, "run %s → %s\n", lt.ID, lt.Status)
	}
	if lt.Status != "COMPLETED" {
		return exitState, fmt.Errorf("run finished as %s (not COMPLETED)", lt.Status)
	}
	return exitOK, nil
}

func (a *app) runStop(args []string) (int, error) {
	if len(args) < 2 {
		return exitUsage, fmt.Errorf("usage: ltp run stop <ns-id> <lt-id>")
	}
	if err := a.newClient(a.g).StopLoadTest(ctx(), args[0], args[1]); err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		return exitOK, a.printJSON(map[string]string{"id": args[1], "status": "STOPPING"})
	}
	fmt.Fprintf(a.out, "run %s → STOPPING\n", args[1])
	return exitOK, nil
}

// runReport prints the collected k6 summary (plans/45). A run whose summary
// was never collected says so explicitly instead of printing zeros that look
// like a real measurement — the caller can tell "not collected" (no
// result_collected_at) from "collected, nothing to report"
// (result_collected_at set, no summary, result_parse_error explains why).
func (a *app) runReport(args []string) (int, error) {
	if len(args) < 2 {
		return exitUsage, fmt.Errorf("usage: ltp run report <ns-id> <lt-id>")
	}
	lt, err := a.newClient(a.g).GetLoadTest(ctx(), args[0], args[1])
	if err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		return exitOK, a.printJSON(lt)
	}
	tw := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "run\t%s\nname\t%s\nstatus\t%s\nstarted\t%s\nfinished\t%s\n",
		lt.ID, lt.Name, lt.Status, fmtTime(lt.StartedAt), fmtTime(lt.FinishedAt))
	if lt.ResultCollectedAt != nil {
		fmt.Fprintf(tw, "collected\t%s\n", fmtTime(lt.ResultCollectedAt))
	}
	if err := tw.Flush(); err != nil {
		return exitRuntime, err
	}
	if lt.Unreliable {
		fmt.Fprintln(a.err, "warning: namespace was DEGRADED after the run started — result is marked unreliable")
	}

	switch {
	case lt.ResultCollectedAt == nil:
		fmt.Fprintln(a.err, "result summary not collected yet (run is not terminal, or the reconciler has not polled it)")
		return exitOK, nil
	case lt.ResultSummary == nil:
		reason := "no k6 summary in the runner output"
		if lt.ResultParseError != nil && *lt.ResultParseError != "" {
			reason = *lt.ResultParseError
		}
		fmt.Fprintf(a.err, "result summary unavailable: %s\n", reason)
		return exitOK, nil
	}

	s := lt.ResultSummary
	thresholds := "not reported"
	if lt.ThresholdsFailed != nil {
		if *lt.ThresholdsFailed {
			thresholds = "FAILED"
		} else {
			thresholds = "OK"
		}
	} else if len(s.Thresholds) > 0 {
		thresholds = "OK"
	}

	fmt.Fprintf(a.out, "  http_reqs     %s   RPS %.2f\n", commas(s.HTTPReqs), s.RPS)
	fmt.Fprintf(a.out, "  duration      p95 %s   p99 %s   avg %s\n",
		millis(s.P95), millis(s.P99), millis(s.Avg))
	fmt.Fprintf(a.out, "  failed        %.2f%%   checks %s/%s failed\n",
		s.ErrorRate*100, commas(s.ChecksFailed), commas(s.Checks))
	fmt.Fprintf(a.out, "  thresholds    %s\n", thresholds)
	for _, th := range s.Thresholds {
		mark := "✓"
		if !th.OK {
			mark = "✗"
		}
		fmt.Fprintf(a.out, "                %s %s\n", mark, th.String())
	}
	if s.ParseError != "" {
		fmt.Fprintf(a.err, "warning: k6 summary was partial: %s\n", s.ParseError)
	}
	return exitOK, nil
}

// commas groups an integer with thousands separators (12,480).
func commas(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// millis renders a millisecond value compactly (82ms, 1.5s).
func millis(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.2fs", ms/1000)
	}
	if ms == float64(int(ms)) {
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.2fms", ms)
}

func (a *app) runList(args []string) (int, error) {
	if len(args) < 1 {
		return exitUsage, fmt.Errorf("usage: ltp run list <ns-id>")
	}
	list, err := a.newClient(a.g).ListLoadTests(ctx(), args[0])
	if err != nil {
		return exitFor(err), err
	}
	if a.g.jsonOut {
		return exitOK, a.printJSON(list)
	}
	tw := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tSTARTED\tFINISHED")
	for _, lt := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", lt.ID, lt.Name, lt.Status,
			fmtTime(lt.StartedAt), fmtTime(lt.FinishedAt))
	}
	return exitOK, tw.Flush()
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339)
}
