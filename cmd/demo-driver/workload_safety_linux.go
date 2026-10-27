//go:build linux

package main

// Workload safety is an explicit extension of the existing Guard/Finish CLI.
// It shares their stop-only CAS, fence proof, private storage and boot clock.
// It has no VM target, Enforce setter, placement writer or fault injector.
import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
)

type workloadSafetyConfig struct {
	Version             string         `json:"version"`
	Mode                string         `json:"mode"`
	EvidenceKind        string         `json:"evidenceKind"`
	RunID               string         `json:"runID"`
	Epoch               string         `json:"epoch"`
	APIOrigin           string         `json:"apiOrigin"`
	KubeconfigFile      string         `json:"kubeconfigFile"`
	ClusterUID          string         `json:"clusterUID"`
	Namespace           string         `json:"namespace"`
	NamespaceUID        string         `json:"namespaceUID"`
	Policy              string         `json:"policy"`
	PolicyUID           string         `json:"policyUID"`
	PolicyGeneration    int64          `json:"policyGeneration"`
	PolicySpec          map[string]any `json:"policySpec"`
	Workload            string         `json:"workload"`
	DeploymentUID       string         `json:"deploymentUID"`
	Root                string         `json:"root"`
	Owner               guardOwner     `json:"owner"`
	HardDeadline        time.Time      `json:"hardDeadline"`
	AuthorityExpires    time.Time      `json:"authorityExpires"`
	MaxTrialMS          int64          `json:"maxTrialMs"`
	HeartbeatMS         int64          `json:"heartbeatMs"`
	TimeoutMS           int64          `json:"timeoutMs"`
	AuthorizationFile   string         `json:"authorizationFile"`
	AuthorizationSHA256 string         `json:"authorizationSHA256"`
}

func (c workloadSafetyConfig) binding() string { b, _ := json.Marshal(c); return digestBytes(b) }
func (c workloadSafetyConfig) typedSpec() (v1.RecoveryPolicySpec, error) {
	b, e := json.Marshal(c.PolicySpec)
	var p v1.RecoveryPolicySpec
	if e == nil {
		e = strictDecode(b, &p, true)
	}
	return p, e
}
func (c workloadSafetyConfig) validate() error {
	if c.Version != "opsloop-workload-safety/v1" || !validObserverName(c.RunID) || !validObserverName(c.Epoch) || !validObserverName(c.Namespace) || !validObserverName(c.Policy) || !validObserverName(c.Workload) || c.PolicyGeneration < 2 {
		return errors.New("workload safety: invalid explicit target/configuration")
	}
	if _, e := observerURL(c.APIOrigin, true); e != nil {
		return errors.New("workload safety: invalid API origin")
	}
	for _, s := range []string{c.ClusterUID, c.NamespaceUID, c.PolicyUID, c.DeploymentUID, c.Owner.Boot, c.Owner.Start} {
		if s == "" || len(s) > 256 || strings.ContainsAny(s, "\r\n") {
			return errors.New("workload safety: explicit identity required")
		}
	}
	if c.Owner.PID <= 0 || c.MaxTrialMS < 100 || c.MaxTrialMS > 900000 || c.HeartbeatMS < 100 || c.HeartbeatMS > 10000 || c.HeartbeatMS > c.MaxTrialMS || c.TimeoutMS < 10 || c.TimeoutMS > 2000 || c.HardDeadline.IsZero() || !c.AuthorityExpires.After(c.HardDeadline) || c.AuthorityExpires.Sub(c.HardDeadline) > time.Minute || c.AuthorityExpires.Sub(c.HardDeadline) < time.Duration(14*c.TimeoutMS+3000)*time.Millisecond {
		return errors.New("workload safety: invalid owner or finite authority")
	}
	for _, s := range []string{c.KubeconfigFile, c.Root} {
		if !filepath.IsAbs(s) || filepath.Clean(s) != s {
			return errors.New("workload safety: explicit absolute input/storage paths required")
		}
	}
	p, e := c.typedSpec()
	if e != nil || p.Mode != v1.PolicyModeEnforce || !p.Enabled || p.TargetRef.Name != c.Workload || p.Detection.PodUnreadyFor == nil {
		return errors.New("workload safety: exact opt-in Enforce Policy snapshot required")
	}
	if c.Mode == "synthetic-local" {
		u, _ := observerURL(c.APIOrigin, true)
		ip := net.ParseIP(u.Hostname())
		if c.EvidenceKind != "synthetic" || ip == nil || !ip.IsLoopback() {
			return errors.New("workload safety: synthetic requires literal loopback")
		}
	} else if c.Mode == "hybrid-live" {
		if c.EvidenceKind != "observed" || !filepath.IsAbs(c.AuthorizationFile) || !validSHA256(c.AuthorizationSHA256) {
			return errors.New("workload safety: separate live authorization required")
		}
	} else {
		return errors.New("workload safety: unknown execution mode")
	}
	return nil
}

type workloadAuthorization struct {
	Version           string    `json:"version"`
	ApprovalReference string    `json:"approvalReference"`
	RunID             string    `json:"runID"`
	Epoch             string    `json:"epoch"`
	APIOrigin         string    `json:"apiOrigin"`
	ClusterUID        string    `json:"clusterUID"`
	Namespace         string    `json:"namespace"`
	NamespaceUID      string    `json:"namespaceUID"`
	Policy            string    `json:"policy"`
	PolicyUID         string    `json:"policyUID"`
	PolicyGeneration  int64     `json:"policyGeneration"`
	PolicySpecSHA256  string    `json:"policySpecSHA256"`
	Workload          string    `json:"workload"`
	DeploymentUID     string    `json:"deploymentUID"`
	MaxTrialMS        int64     `json:"maxTrialMs"`
	NotAfter          time.Time `json:"notAfter"`
	Allowed           string    `json:"allowed"`
}

func (c workloadSafetyConfig) authorize() error {
	if c.Mode == "synthetic-local" {
		return nil
	}
	b, e := observerLocalFile(c.AuthorizationFile, 1<<20)
	var a workloadAuthorization
	if e != nil || digestBytes(b) != c.AuthorizationSHA256 || strictDecode(b, &a, true) != nil {
		return errors.New("workload safety: live authorization unavailable/changed")
	}
	spec, _ := json.Marshal(c.PolicySpec)
	if a.Version != "opsloop-workload-authorization/v1" || a.ApprovalReference == "" || a.RunID != c.RunID || a.Epoch != c.Epoch || a.APIOrigin != c.APIOrigin || a.ClusterUID != c.ClusterUID || a.Namespace != c.Namespace || a.NamespaceUID != c.NamespaceUID || a.Policy != c.Policy || a.PolicyUID != c.PolicyUID || a.PolicyGeneration != c.PolicyGeneration || a.PolicySpecSHA256 != digestBytes(spec) || a.Workload != c.Workload || a.DeploymentUID != c.DeploymentUID || a.MaxTrialMS < c.MaxTrialMS || !a.NotAfter.After(c.AuthorityExpires) || a.Allowed != "get-and-disable-owned-policy-only" {
		return errors.New("workload safety: separate authorization scope mismatch")
	}
	return nil
}
func (c workloadSafetyConfig) path(resource string) string {
	switch resource {
	case "cluster":
		return "/api/v1/namespaces/kube-system"
	case "namespace":
		return "/api/v1/namespaces/" + c.Namespace
	case "policy":
		return "/apis/opsloop.io/v1alpha1/namespaces/" + c.Namespace + "/recoverypolicies/" + c.Policy
	case "deployment":
		return "/apis/apps/v1/namespaces/" + c.Namespace + "/deployments/" + c.Workload
	}
	return ""
}

// clientcmd.Load parses bytes only. No deferred loader, exec auth, plugin,
// file-backed credential, environment proxy, discovery or in-cluster fallback.
func newWorkloadClient(c workloadSafetyConfig) (*finishClient, error) {
	b, e := observerLocalFile(c.KubeconfigFile, 1<<20)
	if e != nil {
		return nil, errors.New("workload safety: explicit credential input unavailable")
	}
	k, e := clientcmd.Load(b)
	if e != nil {
		return nil, errors.New("workload safety: credential document invalid")
	}
	ctx := k.Contexts[k.CurrentContext]
	if ctx == nil {
		return nil, errors.New("workload safety: explicit current context required")
	}
	cluster := k.Clusters[ctx.Cluster]
	user := k.AuthInfos[ctx.AuthInfo]
	if cluster == nil || user == nil || cluster.Server != c.APIOrigin || cluster.InsecureSkipTLSVerify || cluster.ProxyURL != "" || cluster.CertificateAuthority != "" || len(cluster.CertificateAuthorityData) == 0 || cluster.TLSServerName != "" || user.Exec != nil || user.AuthProvider != nil || user.ClientCertificate != "" || user.ClientKey != "" || user.TokenFile != "" || user.Username != "" || user.Password != "" || user.Impersonate != "" || user.ImpersonateUID != "" || len(user.ImpersonateGroups) > 0 || len(user.ImpersonateUserExtra) > 0 {
		return nil, errors.New("workload safety: static embedded identity without fallback required")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cluster.CertificateAuthorityData) {
		return nil, errors.New("workload safety: CA invalid")
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	hasCert := len(user.ClientCertificateData) > 0 || len(user.ClientKeyData) > 0
	if user.Token != "" && hasCert || user.Token == "" && !hasCert {
		return nil, errors.New("workload safety: one explicit authentication method required")
	}
	if hasCert {
		cert, err := tls.X509KeyPair(user.ClientCertificateData, user.ClientKeyData)
		if err != nil {
			return nil, errors.New("workload safety: client certificate invalid")
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil || !leaf.NotAfter.After(c.AuthorityExpires) {
			return nil, errors.New("workload safety: certificate lifetime insufficient")
		}
		tc.Certificates = []tls.Certificate{cert}
	} else if !observerBearer.MatchString(user.Token) {
		return nil, errors.New("workload safety: invalid static token")
	}
	timeout := time.Duration(c.TimeoutMS) * time.Millisecond
	tr := &http.Transport{Proxy: nil, TLSClientConfig: tc, DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 16 << 10, ResponseHeaderTimeout: timeout, TLSHandshakeTimeout: timeout, DialContext: (&net.Dialer{Timeout: timeout}).DialContext}
	reads := map[string]bool{}
	for _, key := range []string{"cluster", "namespace", "policy", "deployment"} {
		reads[c.path(key)] = true
	}
	return &finishClient{client: &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, origin: c.APIOrigin, bearer: user.Token, policyPath: c.path("policy"), reads: reads}, nil
}
func workloadGET[T any](ctx context.Context, client *finishClient, path string) (T, []byte, error) {
	var v T
	b, _, problem := client.request(ctx, path, nil)
	if problem != "" || strictDecode(b, &v, false) != nil {
		return v, nil, errors.New("workload safety: required API observation unavailable")
	}
	return v, b, nil
}
func workloadIdentities(ctx context.Context, c workloadSafetyConfig, client *finishClient) error {
	for _, x := range []struct{ path, uid string }{{c.path("cluster"), c.ClusterUID}, {c.path("namespace"), c.NamespaceUID}} {
		n, _, e := workloadGET[core.Namespace](ctx, client, x.path)
		if e != nil || string(n.UID) != x.uid || n.DeletionTimestamp != nil {
			return errors.New("workload safety: cluster/Namespace identity unavailable")
		}
	}
	return nil
}
func sameWorkloadSpec(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func (c workloadSafetyConfig) expectedSpec(mode string, enabled bool) map[string]any {
	b, _ := json.Marshal(c.PolicySpec)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	out["mode"] = mode
	out["enabled"] = enabled
	return out
}

type workloadPolicy struct {
	Policy v1.RecoveryPolicy
	Spec   map[string]any
}

func readWorkloadPolicy(ctx context.Context, c workloadSafetyConfig, api *finishClient) (workloadPolicy, error) {
	p, b, e := workloadGET[v1.RecoveryPolicy](ctx, api, c.path("policy"))
	var raw struct {
		Spec map[string]any `json:"spec"`
	}
	if e != nil || strictDecode(b, &raw, false) != nil || p.Name != c.Policy || p.Namespace != c.Namespace || string(p.UID) != c.PolicyUID || p.DeletionTimestamp != nil || p.ResourceVersion == "" || p.Spec.TargetRef.Name != c.Workload {
		return workloadPolicy{}, errors.New("workload safety: Policy identity unavailable")
	}
	return workloadPolicy{p, raw.Spec}, nil
}
func (c workloadSafetyConfig) allowedPolicy(p workloadPolicy) bool {
	g := p.Policy.Generation
	return g == c.PolicyGeneration && sameWorkloadSpec(p.Spec, c.PolicySpec) || g == c.PolicyGeneration-1 && sameWorkloadSpec(p.Spec, c.expectedSpec(modeObserveOnly, true)) || g == c.PolicyGeneration+1 && sameWorkloadSpec(p.Spec, c.expectedSpec(modeEnforce, false)) || g == c.PolicyGeneration && sameWorkloadSpec(p.Spec, c.expectedSpec(modeObserveOnly, false))
}
func workloadStopProof(c workloadSafetyConfig, p workloadPolicy, d apps.Deployment) (*StopProof, error) {
	if !c.allowedPolicy(p) || p.Policy.Spec.Enabled || d.Name != c.Workload || d.Namespace != c.Namespace || string(d.UID) != c.DeploymentUID || d.DeletionTimestamp != nil {
		return nil, errors.New("workload safety: stopped identity/spec not proved")
	}
	enabled, deleting, absent := false, false, p.Policy.Status.ActiveIncident == nil
	proof := &StopProof{PolicyUID: c.PolicyUID, Generation: p.Policy.Generation, Enabled: &enabled, Mode: string(p.Policy.Spec.Mode), Deleting: &deleting, ActiveIncidentAbsent: &absent}
	dp := projectDeployment(d)
	if dp.Fence != nil {
		proof.LiveFence = *dp.Fence
	}
	for _, condition := range p.Policy.Status.Conditions {
		if condition.Type == "Quiesced" {
			proof.QuiescedStatus = string(condition.Status)
			proof.QuiescedReason = condition.Reason
			proof.ObservedGeneration = condition.ObservedGeneration
			_ = json.Unmarshal([]byte(condition.Message), &proof.AcknowledgedFence)
		}
	}
	f := proof.LiveFence
	if !absent || proof.ObservedGeneration != proof.Generation || proof.QuiescedStatus != "True" || proof.QuiescedReason != "FenceClosed" || f != proof.AcknowledgedFence || f.Version != 2 || f.PolicyUID != c.PolicyUID || f.PolicyName != c.Policy || f.DeploymentUID != c.DeploymentUID || f.ClosedThroughGeneration < proof.Generation {
		return nil, errors.New("workload safety: strong Controller fence acknowledgement pending")
	}
	return proof, nil
}

type workloadState struct {
	Binding               string     `json:"binding"`
	Phase                 string     `json:"phase"`
	ArmWall               time.Time  `json:"armWall"`
	Boot                  string     `json:"boot"`
	ArmTick               int64      `json:"armTick"`
	HardTick              int64      `json:"hardTick"`
	ExpiryTick            int64      `json:"expiryTick"`
	HeartbeatTick         int64      `json:"heartbeatTick"`
	HeartbeatSequence     uint64     `json:"heartbeatSequence"`
	WatchTick             int64      `json:"watchTick"`
	Reason                string     `json:"reason,omitempty"`
	StopDispatched        bool       `json:"stopDispatched"`
	StopDispatchCount     int        `json:"stopDispatchCount"`
	KnownRejected         bool       `json:"knownRejected"`
	StopResponseConfirmed bool       `json:"stopResponseConfirmed"`
	StopGeneration        int64      `json:"stopGeneration,omitempty"`
	StopMode              string     `json:"stopMode,omitempty"`
	Proof                 *StopProof `json:"proof,omitempty"`
	LastProblem           string     `json:"lastProblem,omitempty"`
}

func (s workloadState) closed() bool {
	return s.Phase == "STOP_CONFIRMED" || s.Phase == "MANUAL_REQUIRED"
}
func (s workloadState) permit(c workloadSafetyConfig, n guardMoment) error {
	if n.Boot != s.Boot || n.Tick < s.ArmTick || n.Tick >= s.ExpiryTick || n.Wall.Before(s.ArmWall) || !n.Wall.Before(c.AuthorityExpires) {
		return errors.New("workload safety: original finite authority unavailable/expired")
	}
	return nil
}

type workloadEvent struct {
	Sequence     int           `json:"sequence"`
	PreviousHash string        `json:"previousHash"`
	At           guardMoment   `json:"at"`
	State        workloadState `json:"state"`
}
type workloadStore struct {
	dir      string
	lock     *os.File
	lastHash string
	sequence int
	first    *workloadState
}

func openWorkloadStore(c workloadSafetyConfig) (*workloadStore, error) {
	if e := guardPrivateDir(c.Root); e != nil {
		return nil, e
	}
	key := digestBytes([]byte(c.ClusterUID + "\n" + c.NamespaceUID + "\n" + c.PolicyUID))
	dir := filepath.Join(c.Root, key)
	if e := os.Mkdir(dir, 0700); e != nil && !os.IsExist(e) {
		return nil, e
	}
	if e := guardPrivateDir(dir); e != nil {
		return nil, e
	}
	if e := syncFinishDir(c.Root); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errGuardBusy
	}
	return &workloadStore{dir: dir, lock: f}, nil
}
func (s *workloadStore) close() {
	_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	_ = s.lock.Close()
}
func (s *workloadStore) load(c workloadSafetyConfig) (*workloadState, error) {
	entries, e := os.ReadDir(s.dir)
	if e != nil {
		return nil, e
	}
	if len(entries) > 2001 {
		return nil, errors.New("workload safety: journal event budget exceeded")
	}
	var state *workloadState
	for _, entry := range entries {
		if entry.Name() == "lock" {
			continue
		}
		name := fmt.Sprintf("event-%06d.json", s.sequence+1)
		info, err := entry.Info()
		if err != nil || entry.Name() != name || !info.Mode().IsRegular() || entry.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("workload safety: journal corrupt/foreign entry; no overwrite")
		}
		b, err := observerLocalFile(filepath.Join(s.dir, name), 1<<20)
		var v workloadEvent
		if err != nil || strictDecode(b, &v, true) != nil || v.Sequence != s.sequence+1 || v.PreviousHash != s.lastHash || v.State.Binding != c.binding() {
			return nil, errors.New("workload safety: journal identity/hash-chain invalid")
		}
		if s.first == nil {
			first := v.State
			s.first = &first
			if first.Phase != "ARMED_UNWATCHED" || first.ArmTick <= 0 || first.HardTick <= first.ArmTick || first.ExpiryTick <= first.HardTick || first.HardTick-first.ArmTick > c.MaxTrialMS*int64(time.Millisecond) || first.ExpiryTick-first.HardTick > int64(time.Minute) || first.Boot != c.Owner.Boot || first.StopDispatched {
				return nil, errors.New("workload safety: original durable arm bounds invalid")
			}
		} else if v.State.ArmTick != s.first.ArmTick || v.State.HardTick != s.first.HardTick || v.State.ExpiryTick != s.first.ExpiryTick || v.State.Boot != s.first.Boot || !v.State.ArmWall.Equal(s.first.ArmWall) || state != nil && state.StopDispatched && !v.State.StopDispatched {
			return nil, errors.New("workload safety: original deadline/dispatch was changed")
		}
		copy := v.State
		state = &copy
		s.sequence++
		s.lastHash = digestBytes(b)
	}
	return state, nil
}
func (s *workloadStore) save(c workloadSafetyConfig, state *workloadState, n guardMoment) error {
	if state.Binding != c.binding() || s.sequence >= 2000 {
		return errors.New("workload safety: journal binding/budget rejected")
	}
	v := workloadEvent{s.sequence + 1, s.lastHash, n, *state}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if e = finishExclusive(filepath.Join(s.dir, fmt.Sprintf("event-%06d.json", v.Sequence)), b); e != nil {
		return errors.New("workload safety: durable event write failed; inspect without replay")
	}
	s.sequence++
	s.lastHash = digestBytes(b)
	return nil
}
func acquireWorkloadStore(c workloadSafetyConfig) (*workloadStore, error) {
	deadline := time.Now().Add(6*time.Duration(c.TimeoutMS)*time.Millisecond + 2*time.Second)
	for {
		store, e := openWorkloadStore(c)
		if e != errGuardBusy {
			return store, e
		}
		if !time.Now().Before(deadline) {
			return nil, errGuardBusy
		}
		time.Sleep(20 * time.Millisecond)
	}
}
func armWorkload(c workloadSafetyConfig, store *workloadStore, api *finishClient, n guardMoment) (*workloadState, error) {
	prior, e := store.load(c)
	if e != nil {
		return nil, e
	}
	if prior != nil {
		return nil, errors.New("workload safety: epoch exists; no rearm or deadline extension")
	}
	owner, e := guardProcess(c.Owner.PID, n.Boot)
	if e != nil || owner != c.Owner || n.Boot != c.Owner.Boot {
		return nil, errors.New("workload safety: owner identity unavailable")
	}
	duration := c.HardDeadline.Sub(n.Wall)
	if duration > time.Duration(c.MaxTrialMS)*time.Millisecond {
		duration = time.Duration(c.MaxTrialMS) * time.Millisecond
	}
	if duration < 100*time.Millisecond {
		return nil, errors.New("workload safety: insufficient original arm horizon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Duration(c.TimeoutMS)*time.Millisecond)
	defer cancel()
	if e = workloadIdentities(ctx, c, api); e != nil {
		return nil, e
	}
	p, e := readWorkloadPolicy(ctx, c, api)
	if e != nil || p.Policy.Generation != c.PolicyGeneration-1 || !sameWorkloadSpec(p.Spec, c.expectedSpec(modeObserveOnly, true)) || p.Policy.Status.ActiveIncident != nil {
		return nil, errors.New("workload safety: arm requires original quiescent ObserveOnly generation")
	}
	d, _, e := workloadGET[apps.Deployment](ctx, api, c.path("deployment"))
	if e != nil || string(d.UID) != c.DeploymentUID || d.DeletionTimestamp != nil || d.Status.ReadyReplicas != 1 || d.Status.ObservedGeneration != d.Generation {
		return nil, errors.New("workload safety: arm Deployment identity/Ready unavailable")
	}
	fresh, e := guardNow()
	if e != nil || fresh.Boot != n.Boot || fresh.Tick < n.Tick || fresh.Tick >= n.Tick+int64(duration) {
		return nil, errors.New("workload safety: arm admission exhausted original horizon")
	}
	state := &workloadState{Binding: c.binding(), Phase: "ARMED_UNWATCHED", ArmWall: n.Wall, Boot: n.Boot, ArmTick: n.Tick, HardTick: n.Tick + int64(duration), ExpiryTick: n.Tick + int64(duration) + int64(c.AuthorityExpires.Sub(c.HardDeadline)), HeartbeatTick: n.Tick}
	if e = state.permit(c, fresh); e != nil {
		return nil, e
	}
	return state, store.save(c, state, fresh)
}
func stopWorkload(c workloadSafetyConfig, store *workloadStore, state *workloadState, api *finishClient, reason string) error {
	n, e := guardNow()
	if e != nil {
		return errors.New("workload safety: clock unavailable; no new dispatch")
	}
	if e = state.permit(c, n); e != nil {
		state.Phase = "MANUAL_REQUIRED"
		state.LastProblem = "finite-authority-unavailable"
		_ = store.save(c, state, n)
		return e
	}
	if state.closed() {
		return nil
	}
	if state.Phase == "ARMED" || state.Phase == "ARMED_UNWATCHED" {
		state.Phase = "STOP_INTENT"
		state.Reason = reason
		if e = store.save(c, state, n); e != nil {
			return e
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Duration(c.TimeoutMS)*time.Millisecond)
	defer cancel()
	if e = workloadIdentities(ctx, c, api); e != nil {
		return e
	}
	p, e := readWorkloadPolicy(ctx, c, api)
	if e != nil {
		return e
	}
	if !c.allowedPolicy(p) {
		return errors.New("workload safety: Policy drift; no mutation of changed spec")
	}
	if p.Policy.Spec.Enabled {
		if state.StopDispatched && !state.KnownRejected || state.StopDispatchCount >= 3 {
			return errors.New("workload safety: prior stop dispatch unconfirmed/exhausted; read reconciliation only, no retry")
		}
		n, e = guardNow()
		if e != nil {
			return errors.New("workload safety: clock unavailable before stop intent")
		}
		if e = state.permit(c, n); e != nil {
			return e
		}
		state.StopDispatched = true
		state.StopDispatchCount++
		state.KnownRejected = false
		state.StopGeneration = p.Policy.Generation + 1
		state.StopMode = string(p.Policy.Spec.Mode)
		state.Phase = "STOP_DISPATCHING"
		if e = store.save(c, state, n); e != nil {
			return e
		}
		n, e = guardNow()
		if e != nil {
			return errors.New("workload safety: clock unavailable before dispatch")
		}
		if e = state.permit(c, n); e != nil {
			return e
		}
		body, status, problem := api.request(ctx, c.path("policy"), stopOnlyPatch(c.PolicyUID, p.Policy.ResourceVersion, p.Spec))
		state.Phase = "STOP_RECONCILING"
		state.LastProblem = problem
		state.KnownRejected = status == http.StatusConflict && problem == "http-rejected"
		if problem == "" {
			var got v1.RecoveryPolicy
			var raw struct {
				Spec map[string]any `json:"spec"`
			}
			if strictDecode(body, &got, false) != nil || strictDecode(body, &raw, false) != nil || string(got.UID) != c.PolicyUID || got.Generation != state.StopGeneration || !sameWorkloadSpec(raw.Spec, c.expectedSpec(state.StopMode, false)) {
				state.LastProblem = "stop-response-identity-unconfirmed"
			} else {
				state.StopResponseConfirmed = true
			}
		}
		n, e = guardNow()
		if e != nil {
			return errors.New("workload safety: clock unavailable after dispatch; no replay")
		}
		if e = store.save(c, state, n); e != nil {
			return e
		}
		if state.LastProblem != "" {
			return errors.New("workload safety: stop response unconfirmed; reconcile reads without replay")
		}
	}
	// Two independent fresh Policy/Deployment pairs. A disabled read is not a
	// Controller acknowledgement, and never proves a data-plane Service response.
	var first *StopProof
	for i := 0; i < 2; i++ {
		p, e = readWorkloadPolicy(ctx, c, api)
		if e != nil {
			return e
		}
		d, _, err := workloadGET[apps.Deployment](ctx, api, c.path("deployment"))
		if err != nil {
			return err
		}
		proof, err := workloadStopProof(c, p, d)
		if err != nil {
			return err
		}
		if state.StopDispatched && (proof.Generation != state.StopGeneration || proof.Mode != state.StopMode) {
			return errors.New("workload safety: stopped generation not bound to intent")
		}
		if first != nil {
			x, _ := json.Marshal(first)
			y, _ := json.Marshal(proof)
			if !bytes.Equal(x, y) {
				return errors.New("workload safety: stop acknowledgement changed between reads")
			}
		}
		first = proof
	}
	n, e = guardNow()
	if e != nil {
		return errors.New("workload safety: clock unavailable before proof publication")
	}
	if e = state.permit(c, n); e != nil {
		return e
	}
	state.Phase = "STOP_CONFIRMED"
	state.Proof = first
	state.LastProblem = ""
	return store.save(c, state, n)
}
func workloadOutput(w io.Writer, c workloadSafetyConfig, state *workloadState) error {
	permit := false
	if n, e := guardNow(); e == nil && state != nil && state.Phase == "ARMED" && state.permit(c, n) == nil && n.Tick < state.HardTick && n.Tick-state.HeartbeatTick < c.HeartbeatMS*int64(time.Millisecond) && state.WatchTick > 0 && n.Tick-state.WatchTick < 2*int64(time.Second) {
		owner, err := guardProcess(c.Owner.PID, n.Boot)
		permit = err == nil && owner == c.Owner
	}
	return json.NewEncoder(w).Encode(struct {
		Schema                           string         `json:"schema"`
		RunID                            string         `json:"runID"`
		EvidenceKind                     string         `json:"evidenceKind"`
		State                            *workloadState `json:"state"`
		SafetyPermitOnlyNotAuthorization bool           `json:"safetyPermitOnlyNotAuthorization"`
		ServiceChecked                   bool           `json:"serviceChecked"`
		CleanupChecked                   bool           `json:"cleanupChecked"`
		OverallPass                      bool           `json:"overallPass"`
		LiveProof                        bool           `json:"liveProof"`
	}{"opsloop-workload-safety-receipt/v1", c.RunID, c.EvidenceKind, state, permit, false, false, false, false})
}

func runWorkloadGuard(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: guard-workload plan|arm|watch|heartbeat|status|stop -config <file>")
	}
	action := args[0]
	f := flag.NewFlagSet("guard-workload", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	file := f.String("config", "", "explicit config")
	execute := f.Bool("execute", false, "execute authorized bounded action")
	confirm := f.String("confirm", "", "configuration binding")
	sequence := f.Uint64("sequence", 0, "heartbeat sequence")
	if e := f.Parse(args[1:]); e != nil || f.NArg() != 0 {
		return errors.New("workload safety: invalid flags")
	}
	if action != "plan" && action != "arm" && action != "watch" && action != "heartbeat" && action != "status" && action != "stop" {
		return errors.New("workload safety: invalid action")
	}
	b, e := observerLocalFile(*file, 1<<20)
	var c workloadSafetyConfig
	if e != nil || strictDecode(b, &c, true) != nil {
		return errors.New("workload safety: invalid config input")
	}
	if e = c.validate(); e != nil {
		return e
	}
	if action == "plan" || !*execute {
		return json.NewEncoder(w).Encode(map[string]any{"configBinding": c.binding(), "credentialReads": 0, "APIRequests": 0, "policyWrite": "disable-owned-policy-only", "VMOperations": 0, "phase": "UNARMED"})
	}
	if *confirm != c.binding() {
		return errors.New("workload safety: configuration confirmation mismatch")
	}
	if e = c.authorize(); e != nil {
		return e
	}
	api, e := newWorkloadClient(c)
	if e != nil {
		return e
	}
	defer api.client.CloseIdleConnections()
	for {
		store, e := acquireWorkloadStore(c)
		if e != nil {
			return e
		}
		state, e := store.load(c)
		if e != nil {
			store.close()
			return e
		}
		n, e := guardNow()
		if e != nil {
			store.close()
			return e
		}
		if action == "arm" {
			if state != nil {
				store.close()
				return errors.New("workload safety: epoch already armed; no extension")
			}
			state, e = armWorkload(c, store, api, n)
			store.close()
			if e != nil {
				return e
			}
			return workloadOutput(w, c, state)
		}
		if state == nil {
			store.close()
			return errors.New("workload safety: durable arm required")
		}
		if action == "status" {
			store.close()
			return workloadOutput(w, c, state)
		}
		if state.closed() {
			store.close()
			if action == "heartbeat" {
				return errors.New("workload safety: closed epoch cannot heartbeat")
			}
			if state.Phase == "MANUAL_REQUIRED" {
				_ = workloadOutput(w, c, state)
				return errors.New("workload safety: manual review required")
			}
			return workloadOutput(w, c, state)
		}
		if action == "heartbeat" {
			owner, oe := guardProcess(c.Owner.PID, n.Boot)
			e = state.permit(c, n)
			if e == nil && (state.Phase != "ARMED" || n.Tick >= state.HardTick || n.Tick-state.HeartbeatTick >= c.HeartbeatMS*int64(time.Millisecond) || *sequence <= state.HeartbeatSequence || oe != nil || owner != c.Owner) {
				e = errors.New("workload safety: late/foreign/nonincreasing heartbeat")
			}
			if e == nil {
				state.HeartbeatTick = n.Tick
				state.HeartbeatSequence = *sequence
				e = store.save(c, state, n)
			}
			store.close()
			if e != nil {
				return e
			}
			return workloadOutput(w, c, state)
		}
		reason := "FINISH_REQUEST"
		if action == "watch" {
			owner, oe := guardProcess(c.Owner.PID, n.Boot)
			reason = ""
			switch {
			case store.sequence >= 1900:
				reason = "JOURNAL_BUDGET"
			case state.permit(c, n) != nil:
				reason = "AUTHORITY_UNAVAILABLE"
			case n.Tick >= state.HardTick:
				reason = "HARD_DEADLINE"
			case oe != nil || owner != c.Owner:
				reason = "OWNER_EXITED"
			case n.Tick-state.HeartbeatTick >= c.HeartbeatMS*int64(time.Millisecond):
				reason = "HEARTBEAT_EXPIRED"
			case state.Phase != "ARMED" && state.Phase != "ARMED_UNWATCHED":
				reason = state.Reason
			}
		}
		if reason != "" {
			e = stopWorkload(c, store, state, api, reason)
		} else {
			if state.Phase != "ARMED" || n.Tick-state.WatchTick >= int64(time.Second) {
				state.Phase = "ARMED"
				state.WatchTick = n.Tick
				e = store.save(c, state, n)
			}
		}
		store.close()
		if action == "stop" {
			if e != nil {
				return e
			}
			return workloadOutput(w, c, state)
		}
		if state.closed() {
			if state.Phase == "MANUAL_REQUIRED" {
				_ = workloadOutput(w, c, state)
				return errors.New("workload safety: finite authority ended without strong acknowledgement")
			}
			return workloadOutput(w, c, state)
		}
		// Read errors leave an explicit unfinished stop, never a PASS or another PATCH.
		time.Sleep(250 * time.Millisecond)
	}
}
func runWorkloadFinish(args []string, w io.Writer) error {
	return runWorkloadGuard(append([]string{"stop"}, args...), w)
}
