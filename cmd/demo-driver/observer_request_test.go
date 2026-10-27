package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestObserverRequestErrors(t *testing.T) {
	type requestCase struct {
		name, resource, problem, class string
		prior                          bool
		status                         int
	}
	var cases []requestCase
	for _, resource := range []string{"recoveryincidents", "pods", "replicasets", "recoverypolicies", "deployments", "services", "nodes"} {
		cases = append(cases, requestCase{resource + "-initial-404", resource, "http-404", "pending", false, 404}, requestCase{resource + "-pinned-404", resource, "http-404", "gap", true, 404})
	}
	for _, prior := range []bool{false, true} {
		prefix := "list-initial-"
		if prior {
			prefix = "list-observed-"
		}
		for _, failure := range []struct {
			name, problem string
			status        int
		}{{"404", "http-404", 404}, {"403", "http-403", 403}, {"timeout", "timeout-measurement-path", 0}, {"json", "api-json-invalid", 200}} {
			cases = append(cases, requestCase{prefix + failure.name, "endpointslices", failure.problem, "gap", prior, failure.status})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ObserverConfig{RunID: "request", OutputRoot: t.TempDir(), Namespace: "demo", Service: "app", APIOrigin: "https://api.invalid"}
			clock := &observerTestClock{t: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)}
			r, err := newObserverRecorder(c, clock)
			if err != nil {
				t.Fatal(err)
			}
			failing := !tc.prior
			o := &observer{c: c, r: r, apiGuard: &observerGuard{allowed: map[string]bool{}}}
			name := "app"
			if tc.resource == "endpointslices" {
				name = ""
			}
			path := o.apiPath(tc.resource, name)
			o.api = &http.Client{Transport: observerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != "GET" || req.URL.RequestURI() != path {
					t.Fatalf("wrong request: %s %s", req.Method, req.URL)
				}
				if tc.resource == "endpointslices" && (req.URL.Query().Get("limit") != "100" || req.URL.Query().Get("labelSelector") != "kubernetes.io/service-name=app") {
					t.Fatal("real bounded LIST query missing")
				}
				status, body := 200, `{"metadata":{"name":"app","namespace":"demo","uid":"object-uid"}}`
				if tc.resource == "endpointslices" {
					body = `{"items":[{"metadata":{"name":"slice","namespace":"demo","uid":"slice-uid"}}]}`
				}
				if failing {
					status = tc.status
					body = "{"
					if status == 0 {
						return nil, context.DeadlineExceeded
					}
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			if tc.prior {
				if tc.resource == "endpointslices" {
					var list discovery.EndpointSliceList
					_, _, problem, err := o.get(context.Background(), tc.resource, name, &list)
					if err != nil || problem != "" {
						t.Fatalf("successful LIST: %s %v", problem, err)
					}
					if err = r.pin("endpointslice/"+list.Items[0].Name, string(list.Items[0].UID)); err != nil {
						t.Fatal(err)
					}
				} else {
					var obj meta.PartialObjectMetadata
					at, _, problem, err := o.get(context.Background(), tc.resource, name, &obj)
					if err != nil || problem != "" {
						t.Fatalf("successful GET: %s %v", problem, err)
					}
					if err = o.object(tc.resource, name, obj.ObjectMeta, at); err != nil {
						t.Fatal(err)
					}
				}
				failing = true
			}
			var value json.RawMessage
			at, status, problem, err := o.get(context.Background(), tc.resource, name, &value)
			if err != nil || status != tc.status || problem != tc.problem {
				t.Fatalf("get: %d %s %v", status, problem, err)
			}
			if len(r.manifest.Diagnostics) != 1 {
				t.Fatalf("diagnostics: %+v", r.manifest.Diagnostics)
			}
			d := r.manifest.Diagnostics[0]
			if d.Class != tc.class || d.Target != path || d.Proof != "apply" || d.Reason != tc.resource+":"+tc.problem || !reflect.DeepEqual(d.Observed, at) {
				t.Fatalf("diagnostic: %+v", d)
			}
			var raw struct {
				Kind, Path, Problem string
				Observed            Stamp
				HTTPStatus          int
			}
			if err = readJSON(filepath.Join(r.dir, d.Ref), &raw); err != nil {
				t.Fatal(err)
			}
			if raw.Kind != tc.resource || raw.Path != path || raw.Problem != problem || raw.HTTPStatus != status || !reflect.DeepEqual(raw.Observed, at) {
				t.Fatalf("raw capture mismatch: %+v", raw)
			}
			if tc.resource == "endpointslices" && r.manifest.Pins[path] != "" {
				t.Fatal("LIST path must not receive an object UID")
			}
			if tc.resource == "endpointslices" && !tc.prior && tc.status == 404 {
				// A first LIST 404 is a gap, not a permanent run-wide failure.
				// Exercise the existing pre-fault selector contract with this actual
				// request diagnostic; this is a selector unit check, not a full run.
				failing = false
				var list discovery.EndpointSliceList
				if _, _, problem, err := o.get(context.Background(), tc.resource, name, &list); err != nil || problem != "" {
					t.Fatalf("LIST recovery: %s %v", problem, err)
				}
				fault, err := r.stamp(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				o.evidence.FaultRequested = fault
				o.evidence.Apply = ApplyProof{Source: "fresh-get", Ref: "later-proof"}
				o.selectProofs(Env{})
				if o.evidence.Apply.Ref != "later-proof" || r.manifest.Diagnostics[0].Class != "gap" {
					t.Fatal("pre-fault LIST gap must remain recorded without excluding later proof")
				}
			}
		})
	}
}

func TestObserverEndpointList(t *testing.T) {
	cli := os.Getenv("P2C_CLI")
	if cli == "" {
		t.Fatal("P2C_CLI required")
	}
	for _, tc := range []struct {
		name string
		pass bool
	}{
		{"normal", true}, {"endpoint-list-404", false}, {"endpoint-list-initial-404", false}, {"endpoint-list-late-404", true},
		{"incident-created", true}, {"lifecycle", true}, {"endpoints-delayed", true}, {"endpoints-empty-delayed", true},
		{"endpoint-lost", false}, {"endpoint-empty-lost", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newObserverHistoryScene(t, tc.name)
			var out bytes.Buffer
			err := runWithObserverClock([]string{"observe", "-config", s.configPath}, &out, s.clock)
			dir := filepath.Join(s.config.OutputRoot, s.config.RunID)
			var result AcceptanceResult
			var m observerManifest
			var e RuntimeEvidence
			for name, target := range map[string]any{"summary.json": &result, "manifest.json": &m, "evidence.json": &e} {
				if err := readJSON(filepath.Join(dir, name), target); err != nil {
					t.Fatal(err)
				}
			}
			// Export fresh captures even if the non-acceptance assertion detects a regression.
			if root := os.Getenv("P2C_R01_EXAMPLES"); root != "" {
				exportObserverHistory(t, dir, filepath.Join(root, tc.name), m, e)
			}
			if result.OverallPass != tc.pass || (err == nil) != tc.pass {
				t.Fatalf("required interval acceptance: err=%v result=%+v", err, result)
			}
			if result.ServiceRecovery.Status != "PASS" || result.Cleanup.Status != "PASS" {
				t.Fatalf("unrelated verdict changed: %+v", result)
			}
			if !tc.pass && (result.Attribution.Status != "UNPROVEN" || !slices.Contains(result.Attribution.Reasons, "APPLY_CONFIRMATION_MISSING") || e.Apply.Ref != "" || e.Apply.Source != "") {
				t.Fatalf("required gap/loss did not exclude apply proof: %+v %+v", result.Attribution, e.Apply)
			}
			if tc.name == "lifecycle" || tc.name == "incident-created" {
				resources := []string{"recoveryincidents"}
				if tc.name == "lifecycle" {
					resources = append(resources, "pods", "replicasets")
				}
				for _, resource := range resources {
					found := false
					for _, d := range m.Diagnostics {
						if d.Reason == resource+":http-404" {
							found = true
							if d.Class != "pending" || m.Pins[d.Target] == "" {
								t.Fatalf("initial named 404 must become an identified object: %+v", d)
							}
						}
					}
					if !found {
						t.Fatalf("initial %s 404 was never observed", resource)
					}
				}
			}
			if strings.HasPrefix(tc.name, "endpoint-list-") {
				found := false
				var lastGap Stamp
				path := (&observer{c: s.config}).apiPath("endpointslices", "")
				for _, d := range m.Diagnostics {
					if d.Reason == "endpointslices:http-404" {
						found = true
						lastGap = d.Observed
						if d.Class != "gap" || d.Target != path || d.Proof != "apply" {
							t.Fatalf("LIST diagnostic: %+v", d)
						}
						var raw struct {
							Path, Problem string
							HTTPStatus    int
							Observed      Stamp
						}
						if err := readJSON(filepath.Join(dir, d.Ref), &raw); err != nil {
							t.Fatal(err)
						}
						if raw.Path != d.Target || raw.Problem != "http-404" || raw.HTTPStatus != 404 || !reflect.DeepEqual(raw.Observed, d.Observed) {
							t.Fatalf("raw diagnostic link: %+v", raw)
						}
						if tc.name == "endpoint-list-404" && (result.ServiceStableAt == nil || !e.ReadyObserved.At.Before(d.Observed.At) || !d.Observed.At.Before(result.ServiceStableAt.At)) {
							t.Fatal("404 not in ready-to-stable interval")
						}
						if tc.name == "endpoint-list-late-404" && (result.ServiceStableAt == nil || !result.ServiceStableAt.At.Before(d.Observed.At)) {
							t.Fatal("late gap must follow stable interval")
						}
					}
				}
				if !found {
					t.Fatal("LIST 404 was never observed")
				}
				recovered := false
				for _, f := range m.Files {
					if strings.HasPrefix(f.Ref, "raw-") {
						var raw struct {
							Kind       string
							HTTPStatus int
							Observed   Stamp
						}
						if err := readJSON(filepath.Join(dir, f.Ref), &raw); err != nil {
							t.Fatal(err)
						}
						if raw.Kind == "endpointslice-targets" && raw.HTTPStatus == 200 && lastGap.At.Before(raw.Observed.At) {
							recovered = true
						}
					}
				}
				if !recovered {
					t.Fatal("fixture must actually return normal LIST after 404")
				}
				if m.Pins[path] != "" {
					t.Fatal("LIST path must not have a fabricated UID pin")
				}
			}
			verifyBuiltObserverReport(t, cli, s, dir, tc.pass)
			t.Logf("overall=%v service=%s attribution=%s reasons=%v cleanup=%s", result.OverallPass, result.ServiceRecovery.Status, result.Attribution.Status, result.Attribution.Reasons, result.Cleanup.Status)
		})
	}
}
