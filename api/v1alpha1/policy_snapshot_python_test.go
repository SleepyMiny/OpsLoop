package v1alpha1

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Real Go type/PolicyHash roundtrip into the production Python comparator.
// No Kubernetes credentials, source rewriting or operating permits.
func TestPolicySnapshotPythonSerialization(t *testing.T) {
	root := filepath.Join("..", "..")
	cmd := exec.Command("python3", "-c", "import json;from tests.policy_snapshot_fixture import policy_spec;print(json.dumps(policy_spec()))")
	cmd.Dir = root
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("OPSLOOP_LOCAL_SNAPSHOT_INPUT"); path != "" {
		raw, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Go rejects non-ASCII digits and truncates fractional ns per segment.
	if _, err := time.ParseDuration("١٢٠s"); err == nil {
		t.Fatal("unexpected Go Unicode grammar")
	}
	lossy, err := time.ParseDuration("1s0.5ns0.5ns")
	if err != nil || lossy != time.Second {
		t.Fatalf("Go segment semantics: %v %v", lossy, err)
	}
	var spec RecoveryPolicySpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	typed, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == string(typed) {
		t.Fatal("fixture must exercise Go serialization")
	}
	var unknown map[string]any
	if err := json.Unmarshal(raw, &unknown); err != nil {
		t.Fatal(err)
	}
	unknown["unknownPolicyField"] = "must-not-disappear"
	extra, _ := json.Marshal(unknown)
	var dropped RecoveryPolicySpec
	if err := json.Unmarshal(extra, &dropped); err != nil {
		t.Fatal(err)
	}
	if PolicyHash(spec) != PolicyHash(dropped) {
		t.Fatal("expected legacy Go unknown-field omission")
	}
	data, _ := json.Marshal(map[string]any{"raw": json.RawMessage(raw), "typed": json.RawMessage(typed), "unknown": unknown, "typedHash": PolicyHash(spec)})
	file := filepath.Join(t.TempDir(), "roundtrip.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("python3", "-c", `import json,sys,copy
from tools.engineering_evidence.seal import policy_snapshots_equal,EvidenceError
v=json.load(open(sys.argv[1]));assert v['raw']!=v['typed']
assert policy_snapshots_equal(v['raw'],v['typed'])
try: policy_snapshots_equal(v['unknown'],v['typed'])
except EvidenceError: pass
else: raise AssertionError('typed hash concealed unknown field')
for key in v['raw']:
 p=copy.deepcopy(v['raw']);del p[key]
 try: policy_snapshots_equal(p,p)
 except EvidenceError: pass
 else: raise AssertionError(('missing field',key))
print('Go serialization -> Python PASS; unknown/missing rejected; typedHash='+v['typedHash'])`, file)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Log(string(out))
}
