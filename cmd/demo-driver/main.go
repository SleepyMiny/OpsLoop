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
	return runWithObserverClock(args, stdout, observerWallClock{})
}

func runWithObserverClock(args []string, stdout io.Writer, clock observerClock) error {
	if len(args) == 0 {
		return errors.New("usage: demo-driver probe|report|summarize|observe|finish-run|guard|finalize-run ...")
	}
	switch args[0] {
	case "guard-workload":
		return runWorkloadGuard(args[1:], stdout)
	case "finish-workload":
		return runWorkloadFinish(args[1:], stdout)
	case "finalize-run":
		return runFinalize(args[1:], stdout)
	case "guard":
		return runGuard(args[1:], stdout)
	case "finish-run":
		return runFinish(args[1:], stdout)
	case "observe":
		return runObserve(args[1:], stdout, clock)
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
	return &http.Client{Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{DisableKeepAlives: true}}
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
	acceptance := fl.String("acceptance", "", "runtime-v1 (explicit offline v2 mode)")
	evidencePath := fl.String("evidence", "", "versioned runtime evidence JSON")
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
	if *acceptance != "" && *acceptance != "runtime-v1" {
		return fmt.Errorf("unsupported acceptance mode %q", *acceptance)
	}
	if *acceptance == "runtime-v1" {
		if *evidencePath == "" {
			return errors.New("report: runtime-v1 requires -evidence")
		}
		e, err := readEvidence(*evidencePath)
		if err != nil {
			return err
		}
		in.SourceHashes = map[string]string{}
		for _, src := range []struct {
			role, path string
			value      any
		}{
			{"probe", *probePath, nil}, {"incident", *incidentPath, &in.Incident}, {"pods", *podsPath, &in.Pods}, {"env", *envPath, &in.Env},
		} {
			b, err := boundedFile(src.path)
			if err != nil {
				return err
			}
			in.SourceHashes[src.role] = digestBytes(b)
			if src.value != nil {
				if err := strictDecode(b, src.value, false); err != nil {
					return fmt.Errorf("malformed %s: %w", src.role, err)
				}
			} else {
				in.Probe, err = ReadProbe(strings.NewReader(string(b)))
				if err != nil {
					return err
				}
			}
		}
		result := Acceptance(in, e)
		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(b, '\n'), 0644); err != nil {
			return err
		}
		verdict := "PASS"
		if !result.OverallPass {
			verdict = "FAIL/UNPROVEN"
		}
		if _, err := fmt.Fprintf(stdout, "%s %s %s\n", verdict, result.Scenario, *out); err != nil {
			return err
		}
		if !result.OverallPass {
			return errors.New("runtime-v1 acceptance FAIL/UNPROVEN; structured result written")
		}
		return nil
	}
	if *evidencePath != "" {
		return errors.New("-evidence requires -acceptance runtime-v1")
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
	b, err := boundedFile(path)
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
	var runtimeRows []AcceptanceResult
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "summary.json" {
			return err
		}
		b, err := boundedFile(path)
		if err != nil {
			return err
		}
		var header struct {
			SchemaVersion string `json:"schemaVersion"`
		}
		if err := strictDecode(b, &header, false); err != nil {
			return err
		}
		switch header.SchemaVersion {
		case summarySchemaVersion:
			var s Summary
			if err := strictDecode(b, &s, false); err != nil {
				return err
			}
			rows = append(rows, s)
		case acceptanceVersion:
			var s AcceptanceResult
			if err := strictDecode(b, &s, true); err != nil {
				return err
			}
			if s.Acceptance != "runtime-v1" || (s.EvidenceKind != "synthetic" && s.EvidenceKind != "observed") {
				return fmt.Errorf("invalid v2 summary classification")
			}
			if s.OverallPass && (s.ServiceRecovery.Status != "PASS" || s.Attribution.Status != "PASS" || s.Cleanup.Status != "PASS") {
				return fmt.Errorf("inconsistent v2 overallPass")
			}
			runtimeRows = append(runtimeRows, s)
		default:
			return fmt.Errorf("unsupported summary version %q", header.SchemaVersion)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "Legacy v1 (not Runtime acceptance):")
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
	fmt.Fprintln(w, "\nRuntime v2: offline consistency only; independently proven live recoveries: 0")
	fmt.Fprintln(w, "| evidenceKind | scenario | input PASS/runs |")
	fmt.Fprintln(w, "| --- | --- | --- |")
	slices.SortFunc(runtimeRows, func(a, b AcceptanceResult) int {
		return strings.Compare(a.EvidenceKind+"/"+a.Scenario, b.EvidenceKind+"/"+b.Scenario)
	})
	for i := 0; i < len(runtimeRows); {
		j, passes := i, 0
		for j < len(runtimeRows) && runtimeRows[j].EvidenceKind == runtimeRows[i].EvidenceKind && runtimeRows[j].Scenario == runtimeRows[i].Scenario {
			if runtimeRows[j].OverallPass {
				passes++
			}
			j++
		}
		fmt.Fprintf(w, "| %s | %s | %d/%d |\n", runtimeRows[i].EvidenceKind, runtimeRows[i].Scenario, passes, j-i)
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
