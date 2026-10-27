//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A small real-file fixture, with all seven required member names. Metadata is
// charged just like payload; no Runtime PASS claim is made for these bytes.
func finalBudgetFixture(t *testing.T) (FinalizeConfig, string, int64) {
	t.Helper()
	dir := t.TempDir()
	c := FinalizeConfig{GuardFile: filepath.Join(dir, "guard"), ObserverDir: dir}
	config := filepath.Join(dir, "config")
	files := []map[string]any{}
	for _, name := range []string{"probe.jsonl", "incident.json", "pods.json", "env.json", "evidence.json", "summary.json", "receipts.json"} {
		payload := []byte("small-payload")
		if e := os.WriteFile(filepath.Join(dir, name), payload, 0600); e != nil {
			t.Fatal(e)
		}
		files = append(files, map[string]any{"ref": name, "sha256": digestBytes(payload), "bytes": len(payload)})
	}
	b, _ := json.Marshal(map[string]any{"version": "opsloop-observer/v1", "files": files})
	total := int64(0)
	for name, data := range map[string][]byte{"config": []byte("{}"), "guard": []byte("{}"), "manifest.json": b, "COMPLETE": b} {
		if e := os.WriteFile(filepath.Join(dir, name), data, 0600); e != nil {
			t.Fatal(e)
		}
		total += int64(len(data))
	}
	return c, config, total + 7*int64(len("small-payload"))
}

func TestFinalizeBudgetBoundaries(t *testing.T) {
	for _, delta := range []int64{1, 0, -1} {
		t.Run(fmt.Sprint(delta), func(t *testing.T) {
			c, config, total := finalBudgetFixture(t)
			calls, later := 0, 0
			stopped := false
			s := captureFinalWith(c, config, total+delta, func(path string, limit int64) ([]byte, int64, error) {
				calls++
				if stopped {
					later++
				}
				b, n, e := finalReadCount(path, limit)
				if errors.Is(e, errFinalReadLimit) {
					stopped = true
				}
				return b, n, e
			})
			if later != 0 || s.BudgetExceeded != (delta < 0) {
				t.Fatal(s, later)
			}
			if delta >= 0 && (s.BytesRead != total || len(s.Problems) != 0 || len(s.Refs) != 11) {
				t.Fatal("boundary control failed", s)
			}
			if delta < 0 && (s.Problems["observer/receipts.json"] != "INPUT_BUDGET_EXCEEDED" || s.Data["observer/receipts.json"] != nil) {
				t.Fatal("one byte over not diagnosed", s)
			}
			result := map[string]any{"budget": total + delta, "inputBytes": total, "actualReadBytes": s.BytesRead, "readerCalls": calls, "postExceededReads": later, "budgetExceeded": s.BudgetExceeded}
			b, _ := json.MarshalIndent(result, "", "  ")
			finalFixExport(t, fmt.Sprintf("budget-boundary-%d.json", delta), b)
			t.Log(string(b))
		})
	}
	// Exhaustion in metadata must prevent all payload reads. Later missing files
	// cannot replace the confirmed budget cause.
	c, config, _ := finalBudgetFixture(t)
	payloadCalls := 0
	s := captureFinalWith(c, config, 3, func(path string, limit int64) ([]byte, int64, error) {
		if strings.HasSuffix(path, ".jsonl") || filepath.Base(path) == "receipts.json" {
			payloadCalls++
		}
		return finalReadCount(path, limit)
	})
	if !s.BudgetExceeded || s.BytesRead != 2 || payloadCalls != 0 || s.Problems["manifest"] != "INPUT_BUDGET_EXCEEDED" {
		t.Fatal("metadata not charged", s, payloadCalls)
	}
}

type finalPartialError struct{ done bool }

func (r *finalPartialError) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, "abc"), errors.New("injected partial read")
}

type finalGrowingFile struct {
	f    *os.File
	path string
	grew bool
}

func (r *finalGrowingFile) Read(p []byte) (int, error) {
	if !r.grew {
		r.grew = true
		f, e := os.OpenFile(r.path, os.O_WRONLY|os.O_APPEND, 0600)
		if e != nil {
			return 0, e
		}
		_, e = f.Write([]byte("grew"))
		f.Close()
		if e != nil {
			return 0, e
		}
	}
	return r.f.Read(p)
}

func TestFinalizeBudgetGrowingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing")
	if e := os.WriteFile(path, []byte("abc"), 0600); e != nil {
		t.Fatal(e)
	}
	f, e := os.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		t.Fatal(e)
	}
	// Observe a legal descriptor size, then append on the first actual Read.
	b, n, e := finalReadBytes(&finalGrowingFile{f: f, path: path}, st.Size())
	if b != nil || n != 4 || !errors.Is(e, errFinalReadLimit) {
		t.Fatal(b, n, e)
	}
	if off, e := f.Seek(0, io.SeekCurrent); e != nil || off != 4 {
		t.Fatal("read beyond probe", off, e)
	}
	t.Logf("observedSize=3 grownSize=7 actualReadBytes=%d detectionBytes=1", n)
}

func TestFinalizeBudgetDiscardedReads(t *testing.T) {
	for _, mode := range []string{"partial-error", "changed", "growing", "hash-size-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			c, config, total := finalBudgetFixture(t)
			budget := total - 7*int64(len("small-payload")) + 2
			calls, later := 0, 0
			failed := false
			s := captureFinalWith(c, config, budget, func(path string, limit int64) ([]byte, int64, error) {
				if filepath.Base(path) == "probe.jsonl" {
					calls++
					failed = true
					if mode == "partial-error" {
						return finalReadBytes(&finalPartialError{}, limit)
					}
					if mode == "growing" {
						// The same bounded stream used after f.Stat: a file grows past
						// its observed size before/during Read. Only limit+1 is read.
						return finalReadBytes(strings.NewReader("grew past limit"), limit)
					}
					if mode == "changed" {
						return nil, 3, errors.New("input changed during read")
					}
					return []byte("abc"), 3, nil
				}
				if failed {
					later++
				}
				return finalReadCount(path, limit)
			})
			if !s.BudgetExceeded || s.BytesRead != budget+1 || calls != 1 || later != 0 || s.Problems["observer/incident.json"] != "INPUT_BUDGET_EXCEEDED" {
				t.Fatal("discarded read escaped budget", s, calls, later)
			}
		})
	}
	// Partial errors below the budget still consume bytes, even with nil data.
	c, config, total := finalBudgetFixture(t)
	s := captureFinalWith(c, config, total-11, func(path string, limit int64) ([]byte, int64, error) {
		if filepath.Base(path) == "probe.jsonl" {
			return finalReadBytes(&finalPartialError{}, limit)
		}
		return finalReadCount(path, limit)
	})
	if !s.BudgetExceeded || s.Problems["observer/probe.jsonl"] != "INPUT_FILE_INVALID" || s.BytesRead != total-23 {
		t.Fatal("partial error reset consumption", s)
	}
}

func TestFinalizeBudgetFileLimits(t *testing.T) {
	c, config, total := finalBudgetFixture(t)
	// A sparse oversized file is rejected by f.Stat without reading payload.
	f, e := os.OpenFile(filepath.Join(c.ObserverDir, "probe.jsonl"), os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(maxInputBytes + 1); e != nil {
		t.Fatal(e)
	}
	f.Close()
	s := captureFinalWith(c, config, 64<<20, finalReadCount)
	if s.BudgetExceeded || s.Problems["observer/probe.jsonl"] != "INPUT_FILE_INVALID" || s.BytesRead != total-int64(len("small-payload")) {
		t.Fatal("file limit lost", s)
	}
	// Declared size does not limit or authorize actual reads.
	for _, payload := range [][]byte{[]byte("x"), bytes.Repeat([]byte("x"), 20)} {
		os.WriteFile(filepath.Join(c.ObserverDir, "probe.jsonl"), payload, 0600)
		s = captureFinalWith(c, config, 64<<20, finalReadCount)
		if s.BudgetExceeded || s.Problems["observer/probe.jsonl"] != "OBSERVER_FILE_HASH_MISMATCH" || s.BytesRead != total-13+int64(len(payload)) {
			t.Fatal("actual size not charged", s)
		}
	}
	// The stream bound also preserves the per-file one-byte growth probe.
	if b, n, e := finalReadBytes(strings.NewReader("abcd"), 3); b != nil || n != 4 || !errors.Is(e, errFinalReadLimit) {
		t.Fatal(b, n, e)
	}
	if b, n, e := finalReadBytes(&finalPartialError{}, 10); b != nil || n != 3 || e == nil || errors.Is(e, errFinalReadLimit) {
		t.Fatal("partial error count lost", b, n, e)
	}
	// Preserve the manifest count cap and flat/ref/hash validation without reads.
	for _, count := range []int{8192, 8193} {
		files := []map[string]any{}
		for i := 0; i < count; i++ {
			files = append(files, map[string]any{"ref": fmt.Sprintf("empty-%d", i), "sha256": digestBytes(nil), "bytes": 0})
		}
		mb, _ := json.Marshal(map[string]any{"version": "opsloop-observer/v1", "files": files})
		os.WriteFile(filepath.Join(c.ObserverDir, "manifest.json"), mb, 0600)
		calls := 0
		s = captureFinalWith(c, config, 64<<20, func(path string, limit int64) ([]byte, int64, error) {
			if strings.HasPrefix(filepath.Base(path), "empty-") {
				calls++
				return []byte{}, 0, nil
			}
			return finalReadCount(path, limit)
		})
		if count == 8192 && (calls != count || s.Problems["manifest"] != "") || count == 8193 && (calls != 0 || s.Problems["manifest"] != "OBSERVER_MANIFEST_INVALID") {
			t.Fatal("manifest cap changed", count, calls, s.Problems["manifest"])
		}
	}
}

func TestFinalizeBudgetStableCaptures(t *testing.T) {
	for _, failCapture := range []int{1, 2, 0} {
		t.Run(fmt.Sprint(failCapture), func(t *testing.T) {
			c, config, total := finalBudgetFixture(t)
			cb, _ := os.ReadFile(config)
			calls := 0
			s, stable := stableFinalWith(cb, nil, func() finalSnapshot {
				calls++
				budget := total
				if calls == failCapture {
					budget--
				}
				s := captureFinalWith(c, config, budget, finalReadCount)
				return s
			})
			if failCapture == 0 {
				if !stable || calls != 2 {
					t.Fatal("two captures lost", calls, stable)
				}
				return
			}
			r := finalizeAssessment(c, s, stable)
			if stable || calls != failCapture || !s.BudgetExceeded || r.Status != "INPUT_ERROR" || r.OverallPass || slices.Contains(r.Reasons, "INPUT_CHANGED_DURING_FINALIZATION") {
				t.Fatal("budget retried or misreported", calls, stable, r.Reasons)
			}
			t.Logf("budgetCapture=%d captures=%d comparisonComplete=%t readBytes=%d", failCapture, calls, stable, s.BytesRead)
		})
	}
	// A changed pair must stop without a retry, even if a later pair could settle.
	calls := 0
	_, stable := stableFinalWith([]byte("c"), nil, func() finalSnapshot {
		calls++
		return finalSnapshot{Data: map[string][]byte{"config": []byte("c")}, Metadata: map[string]string{"revision": fmt.Sprint(calls)}}
	})
	if stable || calls != 2 {
		t.Fatal("observed change retried", calls, stable)
	}
}

// A bounded physical-file check for the review's budget-12 shape. Instrument
// the production reader directly; rchar is not a physical-I/O/memory metric.
func TestFinalizeBudgetTwelve(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("x"), 8<<20)
	first := filepath.Join(dir, "data-00.bin")
	os.WriteFile(first, payload, 0600)
	files := []map[string]any{}
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("data-%02d.bin", i)
		if i > 0 {
			if e := os.Link(first, filepath.Join(dir, name)); e != nil {
				t.Fatal(e)
			}
		}
		files = append(files, map[string]any{"ref": name, "sha256": digestBytes(payload), "bytes": len(payload)})
	}
	mb, _ := json.Marshal(map[string]any{"version": "opsloop-observer/v1", "files": files})
	for name, data := range map[string][]byte{"config": []byte("{}"), "guard": []byte("{}"), "manifest.json": mb, "COMPLETE": mb} {
		os.WriteFile(filepath.Join(dir, name), data, 0600)
	}
	c := FinalizeConfig{ObserverDir: dir, GuardFile: filepath.Join(dir, "guard")}
	calls, later := 0, 0
	stopped := false
	s := captureFinalWith(c, filepath.Join(dir, "config"), 64<<20, func(path string, limit int64) ([]byte, int64, error) {
		if stopped {
			later++
		}
		calls++
		b, n, e := finalReadCount(path, limit)
		if errors.Is(e, errFinalReadLimit) {
			stopped = true
		}
		return b, n, e
	})
	if !s.BudgetExceeded || s.BytesRead != 7*(8<<20)+4+2*int64(len(mb)) || calls != 12 || later != 0 || s.Problems["observer/data-11.bin"] != "INPUT_BUDGET_EXCEEDED" || s.Data["observer/data-11.bin"] != nil {
		t.Fatal("budget-12 did not stop", s.BytesRead, calls, later, s.Problems)
	}
	for _, ref := range s.Refs {
		if ref.Role == "observer/data-11.bin" {
			t.Fatal("unvisited member marked verified")
		}
	}
	b, _ := json.MarshalIndent(map[string]any{"actualReadBytes": s.BytesRead, "readerCalls": calls, "postExceededReads": later, "payloadBytesDeclared": 12 * len(payload), "lastMemberProblem": s.Problems["observer/data-11.bin"], "measurement": "counted bytes from production finalReadCount; stat preflight rejected eighth payload without Read"}, "", "  ")
	finalFixExport(t, "budget-12.json", b)
	t.Log(string(b))
}

// The full CLI budget check is attached to the valid collector fixture so that
// unrelated malformed Runtime inputs cannot count as the target failure.
func finalBudgetCLI(t *testing.T, base FinalizeConfig) {
	t.Helper()
	c := base
	c.OutputDir = finalTestOutput(t, "budget-result")
	before := captureFinal(c, filepath.Join(c.Guard.Root, "finalize-config.json"))
	payload := bytes.Repeat([]byte("x"), 8<<20)
	first := filepath.Join(c.ObserverDir, "budget-0.bin")
	os.WriteFile(first, payload, 0600)
	var m observerManifest
	if e := strictDecode(before.Data["manifest"], &m, true); e != nil {
		t.Fatal(e)
	}
	mb, _ := json.Marshal(m)
	var raw map[string]any
	json.Unmarshal(mb, &raw)
	files := raw["files"].([]any)
	for i := 0; i < 9; i++ {
		name := fmt.Sprintf("budget-%d.bin", i)
		if i > 0 {
			if e := os.Link(first, filepath.Join(c.ObserverDir, name)); e != nil {
				t.Fatal(e)
			}
		}
		files = append(files, map[string]any{"ref": name, "sha256": digestBytes(payload), "bytes": len(payload)})
	}
	raw["files"] = files
	mb, _ = json.Marshal(raw)
	os.WriteFile(filepath.Join(c.ObserverDir, "manifest.json"), mb, 0600)
	os.WriteFile(filepath.Join(c.ObserverDir, "COMPLETE"), mb, 0600)
	config := filepath.Join(t.TempDir(), "config.json")
	observerTestJSON(t, config, c)
	output, code := finalCLI(t, config)
	r := finalReadResult(t, c)
	if code == 0 || r.Status != "INPUT_ERROR" || r.OverallPass || !strings.Contains(strings.Join(r.Reasons, ","), "INPUT_BUDGET_EXCEEDED") || slices.Contains(r.Reasons, "INPUT_CHANGED_DURING_FINALIZATION") || r.RuntimeAcceptance == nil || !r.RuntimeAcceptance.OverallPass {
		t.Fatal("CLI budget cause masked", code, string(output), r.Reasons)
	}
	var prior AcceptanceResult
	strictDecode(before.Data["observer/summary.json"], &prior, true)
	if !reflect.DeepEqual(prior, *r.RuntimeAcceptance) {
		t.Fatal("budget changed Runtime details")
	}
	gb, _ := os.ReadFile(c.GuardFile)
	if !bytes.Equal(gb, before.Data["guard"]) {
		t.Fatal("CLI changed Guard")
	}
	resultBytes, _ := os.ReadFile(filepath.Join(c.OutputDir, "run-result.json"))
	finalFixExport(t, "budget-cli-result.json", resultBytes)
	// Neither this failed publication nor the existing normal PASS is reused.
	output, code = finalCLI(t, config)
	after, _ := os.ReadFile(filepath.Join(c.OutputDir, "run-result.json"))
	if code == 0 || !bytes.Contains(output, []byte("OUTPUT_EXISTS")) || !bytes.Equal(resultBytes, after) {
		t.Fatal("budget output reused", code, string(output))
	}
	t.Logf("budget CLI exit=1 status=%s runtimePass=%t reasons=%v duplicateRefused=true", r.Status, r.RuntimeAcceptance.OverallPass, r.Reasons)
}
