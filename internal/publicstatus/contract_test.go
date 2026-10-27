package publicstatus

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("../../shared/status-synthetic.json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestSharedSchemaAndHMACVector(t *testing.T) {
	b, e := os.ReadFile("../../shared/public-status.schema.json")
	if e != nil || !bytes.Equal(b, schemaBytes) {
		t.Fatal("embedded schema drift")
	}
	var v struct {
		TestOnlyKeyText string `json:"testOnlyKeyText"`
		KeyID           string `json:"keyId"`
		Seq             uint64 `json:"seq"`
		SentAt          int64  `json:"sentAt"`
		Raw             string `json:"rawBodyUtf8"`
		Signature       string `json:"signatureHex"`
		Canonical       string `json:"canonicalUtf8"`
	}
	b, e = os.ReadFile("../../shared/status-hmac-vector.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	if got := Canonical("https://opsloop.sleepyminy44.workers.dev", v.KeyID, v.Seq, v.SentAt, []byte(v.Raw)); got != v.Canonical {
		t.Fatal("canonical vector mismatch")
	}
	if Sign([]byte(v.TestOnlyKeyText), "https://opsloop.sleepyminy44.workers.dev", v.KeyID, v.Seq, v.SentAt, []byte(v.Raw)) != v.Signature {
		t.Fatal("signature mismatch")
	}
}
func TestStrictSchemaPrivacyAndTime(t *testing.T) {
	raw := fixture(t)
	s, e := Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Marshal(); e != nil {
		t.Fatal(e)
	}
	for name, mutate := range map[string]func(map[string]any){"secret": func(m map[string]any) { m["token"] = "SYNTHETIC_PRIVATE_SENTINEL" }, "version": func(m map[string]any) { m["schemaVersion"] = true }, "node ID": func(m map[string]any) { m["nodes"].([]any)[0].(map[string]any)["id"] = "private-node" }, "IP": func(m map[string]any) { m["nodes"].([]any)[0].(map[string]any)["internalIP"] = "192.0.2.5" }, "future": func(m map[string]any) {
		m["nodes"].([]any)[0].(map[string]any)["cpu"].(map[string]any)["observedAt"] = s.CollectedAt + 5001
	}, "duplicate node": func(m map[string]any) { m["nodes"].([]any)[1] = m["nodes"].([]any)[0] }} {
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			mutate(m)
			b, _ := json.Marshal(m)
			if _, e := Parse(b); e == nil {
				t.Fatal("invalid schema accepted")
			}
		})
	}
	for _, bad := range [][]byte{append([]byte(`{"schemaVersion":2,`), bytes.TrimSpace(raw)[1:]...), append(bytes.Repeat([]byte(" "), MaxBody), raw...), append(raw, []byte(` {}`)...), []byte(`{"x":NaN}`)} {
		if _, e := Parse(bad); e == nil {
			t.Fatal("malformed/oversized/duplicate accepted")
		}
	}
}
func TestEmptyFailureHeartbeatAndNoOriginalIdentifiers(t *testing.T) {
	s := Empty(1800000000000)
	b, e := s.Marshal()
	if e != nil {
		t.Fatal(e)
	}
	for _, x := range []string{"uid", "namespace", "internalIP", "spec", "SYNTHETIC_PRIVATE_SENTINEL"} {
		if bytes.Contains(b, []byte(`"`+x+`"`)) {
			t.Fatalf("private field %s", x)
		}
	}
	s.Nodes[0].Readiness = Ptr("Ready")
	s.Nodes[0].ReadyObservedAt = Ptr(s.CollectedAt)
	if _, e = s.Marshal(); e == nil {
		t.Fatal("failure disguised as ready")
	}
}
func TestSequenceDurabilityPermissionsAndNoReset(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	if s, e := OpenSequence(dir, false); e == nil {
		s.Close()
		t.Fatal("implicit state initialization")
	}
	s, e := OpenSequence(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	if other, e := OpenSequence(dir, false); e == nil {
		other.Close()
		t.Fatal("concurrent writer admitted")
	}
	n, e := s.Next()
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
	s.Close()
	s, e = OpenSequence(dir, false)
	if e != nil {
		t.Fatal(e)
	}
	n, e = s.Next()
	if e != nil || n != 2 {
		t.Fatal(n, e)
	}
	s.Close()
	if s, e = OpenSequence(dir, true); e == nil {
		s.Close()
		t.Fatal("counter reset admitted")
	}
	key := filepath.Join(dir, "key")
	_ = os.WriteFile(key, bytes.Repeat([]byte("k"), 40), 0600)
	if _, e = SigningKey(key); e != nil {
		t.Fatal(e)
	}
	_ = os.Chmod(key, 0644)
	if _, e = SigningKey(key); e == nil {
		t.Fatal("public credential file accepted")
	}
	_ = os.Chmod(key, 0600)
	link := filepath.Join(dir, "link")
	_ = os.Symlink(key, link)
	if _, e = SigningKey(link); e == nil {
		t.Fatal("credential symlink accepted")
	}
}
func TestSenderTLSOnlyRedirectAndMonotonicRequests(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	keyFile := filepath.Join(dir, "key")
	key := strings.Repeat("SYNTHETIC-", 5)
	_ = os.WriteFile(keyFile, []byte(key), 0600)
	seq, e := OpenSequence(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	defer seq.Close()
	raw := fixture(t)
	calls := 0
	var signatures []string
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect"); w.WriteHeader(500) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, raw) {
			t.Error("body changed")
		}
		if r.Method != "POST" || r.URL.Path != "/api/collect/status" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected route/credential")
		}
		signatures = append(signatures, r.Header.Get("X-OpsLoop-Signature"))
		http.Redirect(w, r, target.URL, 302)
	}))
	defer server.Close()
	sender := Sender{Origin: server.URL, KeyID: "k1", KeyFile: keyFile, Sequence: seq, Client: server.Client()}
	for i := 0; i < 2; i++ {
		if sender.Send(context.Background(), raw, time.Now()) == nil {
			t.Fatal("redirect reported success")
		}
	}
	if calls != 2 || hmac.Equal([]byte(signatures[0]), []byte(signatures[1])) {
		t.Fatal("request replayed")
	}
	sender.Origin = "http://127.0.0.1"
	if _, e := sender.Request(context.Background(), raw, time.Now()); e == nil {
		t.Fatal("plaintext publish accepted")
	}
}

func TestConcurrentSequenceAllocationsAreUnique(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	s, e := OpenSequence(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ch := make(chan uint64, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			n, e := s.Next()
			if e != nil {
				errs <- e
				return
			}
			ch <- n
		}()
	}
	seen := map[uint64]bool{}
	for i := 0; i < 8; i++ {
		select {
		case n := <-ch:
			if seen[n] {
				t.Fatal("duplicate counter")
			}
			seen[n] = true
		case e := <-errs:
			t.Fatal(e)
		}
	}
	if len(seen) != 8 {
		t.Fatal("lost counter")
	}
}

func TestJSONNumericRepresentationsAndUnsupportedSchema(t *testing.T) {
	raw := bytes.Replace(fixture(t), []byte(`"schemaVersion": 1`), []byte(`"schemaVersion": 1e0`), 1)
	if _, e := Parse(raw); e != nil {
		t.Fatal("safe integral numeric representation rejected", e)
	}
	if schemaCheck("unreviewed", map[string]any{"type": "string"}) == nil {
		t.Fatal("unsupported schema type was ignored")
	}
	if schemaCheck(json.Number("1"), map[string]any{"type": "number", "unreviewedKeyword": true}) == nil {
		t.Fatal("unsupported schema keyword ignored")
	}
}
