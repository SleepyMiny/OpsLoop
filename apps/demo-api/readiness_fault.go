package main

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Fault injection is disabled by default. An explicitly approved dedicated Pod
// can fail its own readiness/HTTP until a bounded monotonic deadline. It cannot
// change Kubernetes objects, another Pod, a Node, or its shutdown readiness.
type readinessFault struct {
	mu      sync.Mutex
	uid, ns string
	token   string
	maxTTL  time.Duration
	now     func() time.Time
	used    bool
	until   time.Time
}

func faultFromEnvironment(m meta) (*readinessFault, error) {
	if os.Getenv("OPSLOOP_ENABLE_READINESS_FAULT") != "true" {
		return nil, nil
	}
	token := os.Getenv("OPSLOOP_FAULT_TOKEN")
	decoded, err := hex.DecodeString(token)
	maxSeconds, parseErr := strconv.Atoi(os.Getenv("OPSLOOP_FAULT_MAX_SECONDS"))
	if err != nil || len(decoded) != 32 || len(token) != 64 || parseErr != nil || maxSeconds < 30 || maxSeconds > 600 || m.PodUID == "" || m.Namespace == "" || os.Getenv("OPSLOOP_FAULT_NAMESPACE") != m.Namespace {
		return nil, errors.New("invalid explicit readiness fault configuration")
	}
	return &readinessFault{uid: m.PodUID, ns: m.Namespace, token: token, maxTTL: time.Duration(maxSeconds) * time.Second, now: time.Now}, nil
}

func (f *readinessFault) active() bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used && f.now().Before(f.until)
}

func (f *readinessFault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	deny := func(code int) { writeJSON(w, code, map[string]string{"status": "rejected"}) }
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
		deny(http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-OpsLoop-Fault-Token")), []byte(f.token)) != 1 {
		deny(http.StatusForbidden)
		return
	}
	var request struct {
		PodUID          string `json:"podUID"`
		Namespace       string `json:"namespace"`
		DurationSeconds int64  `json:"durationSeconds"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&request) != nil || d.Decode(&struct{}{}) != io.EOF {
		deny(http.StatusBadRequest)
		return
	}
	if request.PodUID != f.uid || request.Namespace != f.ns {
		deny(http.StatusConflict)
		return
	}
	if request.DurationSeconds < 30 || request.DurationSeconds > int64(f.maxTTL/time.Second) {
		deny(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.used {
		deny(http.StatusConflict)
		return
	}
	f.used = true
	f.until = f.now().Add(time.Duration(request.DurationSeconds) * time.Second)
	writeJSON(w, http.StatusAccepted, struct {
		Status    string    `json:"status"`
		PodUID    string    `json:"podUID"`
		Namespace string    `json:"namespace"`
		ExpiresAt time.Time `json:"expiresAt"`
	}{"fault-active", f.uid, f.ns, f.until.UTC()})
}
