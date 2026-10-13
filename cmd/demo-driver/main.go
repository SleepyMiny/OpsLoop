// Command demo-driver probes the demo API and judges Demo A runs (spec 8.1).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const maxBodyBytes = 64 << 10

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "demo-driver:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: demo-driver probe|report|summarize ...")
	}
	switch args[0] {
	case "probe":
		return runProbe(args[1:])
	case "report":
		return runReport(args[1:], stdout)
	case "summarize":
		if len(args) != 2 {
			return errors.New("usage: demo-driver summarize <dir>")
		}
		return summarize(args[1], stdout)
	}
	return fmt.Errorf("unknown subcommand %q", args[0])
}

func runProbe(args []string) error {
	fl := flag.NewFlagSet("probe", flag.ContinueOnError)
	url := fl.String("url", "", "demo API URL")
	interval := fl.Duration("interval", time.Second, "minimum gap between request starts")
	timeout := fl.Duration("timeout", 2*time.Second, "per-request timeout")
	out := fl.String("out", "", "output JSONL path")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *url == "" || *out == "" {
		return errors.New("probe: -url and -out are required")
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	// Go installs a handler even when the parent shell ignores SIGINT for background jobs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := newProbeClient(*timeout)
	enc := json.NewEncoder(f)
	for {
		start := time.Now()
		rec := probeOnce(ctx, client, *url, start)
		if ctx.Err() != nil {
			return nil // the interrupted request is an artifact of stopping, not an outage sample
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
		// Requests never overlap: the next one starts at max(previous start + interval, previous end).
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(start.Add(*interval))):
		}
	}
}

// newProbeClient dials per request: a kept-alive connection stays conntrack-pinned to the old Pod after it leaves the Endpoints.
func newProbeClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true}}
}

func probeOnce(ctx context.Context, client *http.Client, url string, start time.Time) (rec ProbeRecord) {
	rec.T = start.UTC()
	defer func() { rec.LatencyMs = time.Since(start).Milliseconds() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	resp, err := client.Do(req)
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	defer resp.Body.Close()
	rec.Status = resp.StatusCode
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	switch {
	case err != nil:
		rec.Error = err.Error()
	case len(body) > maxBodyBytes:
		rec.Error = "response body exceeds 64 KiB"
	case resp.StatusCode != http.StatusOK:
		rec.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	default:
		var b struct{ Node, Pod, PodUID, Arch, Version string }
		if err := json.Unmarshal(body, &b); err != nil {
			rec.Error = "invalid JSON: " + err.Error()
		} else {
			rec.OK = true
			rec.Node, rec.Pod, rec.PodUID, rec.Arch, rec.Version = b.Node, b.Pod, b.PodUID, b.Arch, b.Version
		}
	}
	return rec
}

func runReport(args []string, stdout io.Writer) error {
	fl := flag.NewFlagSet("report", flag.ContinueOnError)
	probePath := fl.String("probe", "", "probe.jsonl")
	incidentPath := fl.String("incident", "", "incident.json")
	podsPath := fl.String("pods", "", "pods.json")
	injectAt := fl.String("inject-at", "", "RFC3339Nano injection time")
	mode := fl.String("mode", "", "Enforce|ObserveOnly")
	expectNode := fl.String("expect-node", "", "expected selected Node")
	rejects := map[string]string{}
	fl.Func("expect-reject", "<node>=<REASON>, repeatable", func(v string) error {
		n, reason, ok := strings.Cut(v, "=")
		if !ok || n == "" || reason == "" {
			return fmt.Errorf("want <node>=<REASON>, got %q", v)
		}
		rejects[n] = reason
		return nil
	})
	digests := fl.String("platform-digests", "", "comma-separated platform digests")
	expectVersion := fl.String("expect-version", "", "source SHA the image was built from")
	scenario := fl.String("scenario", "", "scenario id")
	envPath := fl.String("env", "", "env.json")
	out := fl.String("out", "", "summary.json")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *probePath == "" || *incidentPath == "" || *podsPath == "" || *injectAt == "" || *scenario == "" || *envPath == "" || *out == "" {
		return errors.New("report: -probe, -incident, -pods, -inject-at, -scenario, -env and -out are required")
	}
	if *mode != modeEnforce && *mode != modeObserveOnly {
		return fmt.Errorf("report: -mode must be %s or %s", modeEnforce, modeObserveOnly)
	}
	if *expectNode == "" && *scenario != scenarioAllRejected {
		return errors.New("report: -expect-node is required unless -scenario all-rejected")
	}
	at, err := time.Parse(time.RFC3339Nano, *injectAt)
	if err != nil {
		return fmt.Errorf("report: -inject-at: %w", err)
	}
	in := ReportInput{InjectAt: at, Mode: *mode, Scenario: *scenario, ExpectNode: *expectNode,
		ExpectVersion: *expectVersion, ExpectReject: rejects}
	for _, d := range strings.Split(*digests, ",") {
		if d = strings.TrimSpace(d); d != "" {
			in.PlatformDigests = append(in.PlatformDigests, d)
		}
	}
	pf, err := os.Open(*probePath)
	if err != nil {
		return err
	}
	in.Probe, err = ReadProbe(pf)
	pf.Close()
	if err != nil {
		return err
	}
	for path, v := range map[string]any{*incidentPath: &in.Incident, *podsPath: &in.Pods, *envPath: &in.Env} {
		if err := readJSON(path, v); err != nil {
			return err
		}
	}
	s := Report(in)
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	verdict := "FAIL"
	if s.Pass {
		verdict = "PASS"
	}
	_, err = fmt.Fprintf(stdout, "%s %s %s\n", verdict, s.Scenario, *out)
	return err
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// summarize prints every summary.json under dir as a markdown table plus per-scenario totals.
func summarize(dir string, w io.Writer) error {
	var rows []Summary
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "summary.json" {
			return err
		}
		var s Summary
		if err := readJSON(path, &s); err != nil {
			return err
		}
		rows = append(rows, s)
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortFunc(rows, func(a, b Summary) int {
		return strings.Compare(a.Scenario+"/"+a.RunID, b.Scenario+"/"+b.RunID)
	})
	fmt.Fprintln(w, "| scenario | runId | mode | pass | selectedNode | servedArch | attempts | imageCache | tDetect | tDecide | tPlace | tRecover | hostMaxGap | notes |")
	fmt.Fprintln(w, "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, s := range rows {
		fmt.Fprintf(w, "| %s | %s | %s | %t | %s | %s | %d | %s | %s | %s | %s | %s | %s | %s |\n",
			s.Scenario, s.RunID, s.Mode, s.Pass, s.SelectedNode, s.ServedArch, s.Attempts, s.ImageCache,
			fmtSec(s.TDetectSeconds), fmtSec(s.TDecideSeconds), fmtSec(s.TPlaceSeconds), fmtSec(s.TRecoverSeconds),
			fmtSec(s.HostMaxGapSeconds), strings.Join(s.Notes, "; "))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| scenario | PASS/runs | tRecover median (PASS) | tRecover max (PASS) |")
	fmt.Fprintln(w, "| --- | --- | --- | --- |")
	for i := 0; i < len(rows); {
		j, pass := i, 0
		var rec []float64
		for ; j < len(rows) && rows[j].Scenario == rows[i].Scenario; j++ {
			if rows[j].Pass {
				pass++
				if t := rows[j].TRecoverSeconds; t != nil {
					rec = append(rec, *t)
				}
			}
		}
		med, mx := "-", "-"
		if n := len(rec); n > 0 {
			slices.Sort(rec)
			m := rec[n/2]
			if n%2 == 0 {
				m = (rec[n/2-1] + rec[n/2]) / 2
			}
			med, mx = fmtSec(&m), fmtSec(&rec[n-1])
		}
		fmt.Fprintf(w, "| %s | %d/%d | %s | %s |\n", rows[i].Scenario, pass, j-i, med, mx)
		i = j
	}
	return nil
}

func fmtSec(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *v)
}
