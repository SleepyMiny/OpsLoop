// Package publicstatus is a closed public observation contract, never a recovery authority.
package publicstatus

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
)

const MaxBody = 16384
const MaxInt = int64(1<<53 - 1)

//go:embed schema.json
var schemaBytes []byte

type Sample struct {
	Value      *float64 `json:"value"`
	ObservedAt *int64   `json:"observedAt"`
}
type Source struct {
	State      string  `json:"state"`
	ObservedAt *int64  `json:"observedAt"`
	ErrorCode  *string `json:"errorCode"`
}
type Sources struct {
	Kubernetes Source `json:"kubernetes"`
	Prometheus Source `json:"prometheus"`
}
type Node struct {
	ID              string  `json:"id"`
	Site            string  `json:"site"`
	OS              *string `json:"os"`
	Arch            *string `json:"arch"`
	Readiness       *string `json:"readiness"`
	ReadyObservedAt *int64  `json:"readyObservedAt"`
	CPU             Sample  `json:"cpu"`
	Memory          Sample  `json:"memory"`
	Exporter        Sample  `json:"exporter"`
}
type Workload struct {
	ID         string `json:"id"`
	Desired    *int32 `json:"desired"`
	Ready      *int32 `json:"ready"`
	ObservedAt *int64 `json:"observedAt"`
}
type Policy struct {
	Mode                *string `json:"mode"`
	Enabled             *bool   `json:"enabled"`
	Monitoring          *bool   `json:"monitoring"`
	IncidentCount       *int    `json:"incidentCount"`
	ActiveIncidentCount *int    `json:"activeIncidentCount"`
	ObservedAt          *int64  `json:"observedAt"`
}
type Snapshot struct {
	SchemaVersion int        `json:"schemaVersion"`
	Source        string     `json:"source"`
	CollectedAt   int64      `json:"collectedAt"`
	Sources       Sources    `json:"sources"`
	Nodes         []Node     `json:"nodes"`
	Workloads     []Workload `json:"workloads"`
	Policy        Policy     `json:"policy"`
}

func Ptr[T any](v T) *T { return &v }
func Empty(at int64) Snapshot {
	return Snapshot{SchemaVersion: 1, Source: "observed", CollectedAt: at, Sources: Sources{Source{State: "unavailable", ErrorCode: Ptr("UNREACHABLE")}, Source{State: "unavailable", ErrorCode: Ptr("UNREACHABLE")}}, Nodes: []Node{{ID: "control", Site: "laptop-vm"}, {ID: "physical", Site: "physical-server"}, {ID: "cloud", Site: "azure-vm"}}, Workloads: []Workload{{ID: "controller"}, {ID: "demo-api"}}}
}

// decode checks duplicate keys before any map can discard them, and bounds nesting.
func decode(d *json.Decoder, depth int) (any, error) {
	if depth > 24 {
		return nil, errors.New("JSON depth")
	}
	t, e := d.Token()
	if e != nil {
		return nil, e
	}
	if x, ok := t.(json.Delim); ok {
		switch x {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				s, ok := k.(string)
				if !ok {
					return nil, errors.New("JSON key")
				}
				if _, ok = m[s]; ok {
					return nil, errors.New("duplicate JSON key")
				}
				v, e := decode(d, depth+1)
				if e != nil {
					return nil, e
				}
				m[s] = v
			}
			_, e = d.Token()
			return m, e
		case '[':
			a := []any{}
			for d.More() {
				v, e := decode(d, depth+1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			_, e = d.Token()
			return a, e
		}
	}
	return t, nil
}
func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, e := n.Float64()
	return f, e == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}
func schemaCheck(v any, r map[string]any) error {
	bad := errors.New("public schema rejected")
	allowed := map[string]bool{"$schema": true, "type": true, "properties": true, "required": true, "additionalProperties": true, "items": true, "minItems": true, "maxItems": true, "minimum": true, "maximum": true, "enum": true, "const": true, "anyOf": true}
	for k := range r {
		if !allowed[k] {
			return errors.New("unsupported public schema keyword")
		}
	}
	if typ, ok := r["type"]; ok {
		if typ != "object" && typ != "array" && typ != "null" && typ != "boolean" && typ != "integer" && typ != "number" {
			return errors.New("unsupported public schema type")
		}
	}
	if len(r) == 0 {
		return bad
	}
	if a, ok := r["anyOf"].([]any); ok {
		for _, q := range a {
			if schemaCheck(v, q.(map[string]any)) == nil {
				return nil
			}
		}
		return bad
	}
	if c, ok := r["const"]; ok {
		if n, ok := c.(float64); ok {
			f, ok := number(v)
			if !ok || f != n {
				return bad
			}
		} else if str, ok := c.(string); ok {
			value, ok := v.(string)
			if !ok || value != str {
				return bad
			}
		} else {
			return bad
		}
	}
	if a, ok := r["enum"].([]any); ok {
		found := false
		for _, c := range a {
			switch x := c.(type) {
			case nil:
				found = found || v == nil
			case float64:
				f, ok := number(v)
				found = found || ok && f == x
			case string:
				s, ok := v.(string)
				found = found || ok && s == x
			}
		}
		if !found {
			return bad
		}
	}
	switch r["type"] {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return bad
		}
		p := r["properties"].(map[string]any)
		if len(m) != len(p) {
			return bad
		}
		for k, s := range p {
			x, ok := m[k]
			if !ok {
				return bad
			}
			if e := schemaCheck(x, s.(map[string]any)); e != nil {
				return e
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok || len(a) < int(r["minItems"].(float64)) || len(a) > int(r["maxItems"].(float64)) {
			return bad
		}
		for _, x := range a {
			if e := schemaCheck(x, r["items"].(map[string]any)); e != nil {
				return e
			}
		}
	case "null":
		if v != nil {
			return bad
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return bad
		}
	case "number", "integer":
		f, ok := number(v)
		if !ok {
			return bad
		}
		if r["type"] == "integer" && (f != math.Trunc(f) || math.Abs(f) > float64(MaxInt)) {
			return bad
		}
		if min, ok := r["minimum"].(float64); ok && f < min {
			return bad
		}
		if max, ok := r["maximum"].(float64); ok && f > max {
			return bad
		}
	}
	return nil
}
func Parse(raw []byte) (Snapshot, error) {
	var out Snapshot
	if len(raw) > MaxBody {
		return out, errors.New("public body exceeds limit")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, e := decode(d, 0)
	if e != nil {
		return out, errors.New("invalid public JSON")
	}
	if _, e = d.Token(); e != io.EOF {
		return out, errors.New("trailing JSON")
	}
	var rule map[string]any
	if e = json.Unmarshal(schemaBytes, &rule); e != nil {
		return out, e
	}
	if e = schemaCheck(v, rule); e != nil {
		return out, e
	}
	// Normalize safe numeric JSON representations (1, 1.0, 1e0) after schema
	// validation, so typed Go integers agree with JavaScript/JSON Schema semantics.
	typed, e := json.Marshal(normalizeNumbers(v))
	if e != nil {
		return out, errors.New("public normalization failed")
	}
	if e = json.Unmarshal(typed, &out); e != nil {
		return out, errors.New("public typed conversion")
	}
	return out, out.ValidateSemantics()
}
func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		f, _ := x.Float64()
		return f
	case map[string]any:
		for k, n := range x {
			x[k] = normalizeNumbers(n)
		}
		return x
	case []any:
		for i, n := range x {
			x[i] = normalizeNumbers(n)
		}
		return x
	default:
		return v
	}
}
func (s Snapshot) Marshal() ([]byte, error) {
	b, e := json.Marshal(s)
	if e != nil {
		return nil, errors.New("invalid public values")
	}
	_, e = Parse(b)
	return b, e
}
func (s Snapshot) ValidateSemantics() error {
	bad := errors.New("public semantic contract rejected")
	future := func(t *int64) bool { return t != nil && *t > s.CollectedAt+5000 }
	for _, q := range []Source{s.Sources.Kubernetes, s.Sources.Prometheus} {
		if future(q.ObservedAt) || q.State == "ok" && (q.ObservedAt == nil || q.ErrorCode != nil) || q.State != "ok" && q.ErrorCode == nil || q.State == "unavailable" && q.ObservedAt != nil {
			return bad
		}
	}
	sites := map[string]string{"control": "laptop-vm", "physical": "physical-server", "cloud": "azure-vm"}
	seen := map[string]bool{}
	for _, n := range s.Nodes {
		if seen[n.ID] || sites[n.ID] != n.Site || future(n.ReadyObservedAt) || (n.Readiness == nil) != (n.ReadyObservedAt == nil) {
			return bad
		}
		seen[n.ID] = true
		for _, m := range []Sample{n.CPU, n.Memory, n.Exporter} {
			if future(m.ObservedAt) || (m.Value == nil) != (m.ObservedAt == nil) {
				return bad
			}
			if s.Sources.Prometheus.State == "unavailable" && m.Value != nil {
				return bad
			}
		}
		if s.Sources.Kubernetes.State == "unavailable" && (n.OS != nil || n.Arch != nil || n.Readiness != nil || n.ReadyObservedAt != nil) {
			return bad
		}
	}
	seen = map[string]bool{}
	for _, w := range s.Workloads {
		if seen[w.ID] || future(w.ObservedAt) || (w.ObservedAt == nil) != (w.Desired == nil && w.Ready == nil) {
			return bad
		}
		seen[w.ID] = true
		if s.Sources.Kubernetes.State == "unavailable" && w.ObservedAt != nil {
			return bad
		}
	}
	p := s.Policy
	if future(p.ObservedAt) || p.IncidentCount != nil && p.ActiveIncidentCount != nil && *p.ActiveIncidentCount > *p.IncidentCount {
		return bad
	}
	if p.ObservedAt == nil && (p.Mode != nil || p.Enabled != nil || p.Monitoring != nil || p.IncidentCount != nil || p.ActiveIncidentCount != nil) {
		return bad
	}
	if s.Sources.Kubernetes.State == "unavailable" && p.ObservedAt != nil {
		return bad
	}
	return nil
}
func Canonical(origin, kid string, seq uint64, sent int64, raw []byte) string {
	h := sha256.Sum256(raw)
	return "OPSLOOP-STATUS-V1\n" + origin + "\nPOST\n/api/collect/status\nhome-v1\n" + kid + "\n" + strconv.FormatUint(seq, 10) + "\n" + strconv.FormatInt(sent, 10) + "\n" + hex.EncodeToString(h[:])
}
func Sign(key []byte, origin, kid string, seq uint64, sent int64, raw []byte) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(Canonical(origin, kid, seq, sent, raw)))
	return hex.EncodeToString(m.Sum(nil))
}
