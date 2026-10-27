package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/SleepyMiny/OpsLoop/api/v1alpha1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type FinishPod struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}
type FinishConfig struct {
	EvidenceKind           string            `json:"evidenceKind,omitempty"`
	Live                   *finishLiveConfig `json:"live,omitempty"`
	Version                string            `json:"version"`
	Environment            string            `json:"environment"`
	RunID                  string            `json:"runID"`
	ClusterUID             string            `json:"clusterUID"` // explicit kube-system namespace UID, not a guessed cluster name
	Namespace              string            `json:"namespace"`
	NamespaceUID           string            `json:"namespaceUID"`
	Policy                 string            `json:"policy"`
	PolicyUID              string            `json:"policyUID"`
	PolicyGeneration       int64             `json:"policyGeneration"`
	PolicyHash             string            `json:"policyHash"`
	Workload               string            `json:"workload"`
	DeploymentUID          string            `json:"deploymentUID"`
	DeploymentSpecHash     string            `json:"deploymentSpecHash"`
	DeploymentTemplateHash string            `json:"deploymentTemplateHash"`
	APIOrigin              string            `json:"apiOrigin"`
	APICAFile              string            `json:"apiCAFile"`
	BearerFile             string            `json:"bearerFile"`
	Backend                string            `json:"backend"`
	Executable             string            `json:"executable"`
	ExecutableSHA256       string            `json:"executableSHA256"`
	VMURI                  string            `json:"vmURI"`
	VMUUID                 string            `json:"vmUUID"`
	Scenario               string            `json:"scenario"`
	AdditionalChanges      []string          `json:"additionalChanges"`
	DeadlineMS             int64             `json:"deadlineMs"`
	TimeoutMS              int64             `json:"timeoutMs"`
	PollMS                 int64             `json:"pollMs"`
	JournalDir             string            `json:"journalDir"`
	ReceiptFile            string            `json:"receiptFile"`
	Service                string            `json:"service"`
	ServiceUID             string            `json:"serviceUID"`
	ServiceURL             string            `json:"serviceURL"`
	ServiceCAFile          string            `json:"serviceCAFile"`
	ExpectedPod            FinishPod         `json:"expectedPod"`
	ExpectedNode           string            `json:"expectedNode"`
	ExpectedVersion        string            `json:"expectedVersion"`
	OldPods                []FinishPod       `json:"oldPods"` // explicit inventory; nil is not an empty confirmed inventory
}

func (c FinishConfig) binding() string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (c FinishConfig) timeout() time.Duration { return time.Duration(c.TimeoutMS) * time.Millisecond }
func (c FinishConfig) validate() error {
	if c.Version != "opsloop-finish/v1" || c.Scenario != "single-vm-pause-resume" || len(c.AdditionalChanges) != 0 || c.AdditionalChanges == nil || c.OldPods == nil {
		return errors.New("finish: only explicit synthetic single-VM scenario without additional changes supported")
	}
	if e := c.validateMode(); e != nil {
		return e
	}
	for _, s := range []string{c.RunID, c.Namespace, c.Policy, c.Workload, c.Service, c.ExpectedPod.Name, c.ExpectedNode} {
		if !validObserverName(s) {
			return errors.New("finish: invalid explicit name")
		}
	}
	for _, s := range []string{c.ClusterUID, c.NamespaceUID, c.PolicyUID, c.DeploymentUID, c.ServiceUID, c.ExpectedPod.UID, c.ExpectedVersion} {
		if s == "" || len(s) > 256 || strings.ContainsAny(s, "\r\n") {
			return errors.New("finish: explicit identities required")
		}
	}
	for _, s := range []string{c.PolicyHash, c.DeploymentSpecHash, c.DeploymentTemplateHash, c.ExecutableSHA256} {
		b, e := hex.DecodeString(s)
		if e != nil || len(b) != 32 {
			return errors.New("finish: SHA256 authorization required")
		}
	}
	for _, p := range c.OldPods {
		if !validObserverName(p.Name) || p.UID == "" {
			return errors.New("finish: invalid old Pod inventory")
		}
	}
	if len(c.OldPods) > 20 || c.PolicyGeneration < 1 || !finishUUID.MatchString(c.VMUUID) {
		return errors.New("finish: invalid test target")
	}
	if c.TimeoutMS < 1 || c.TimeoutMS > 60000 || c.PollMS < 1 || c.DeadlineMS < 1 || c.DeadlineMS > 3600000 || c.DeadlineMS/c.PollMS > 200 {
		return errors.New("finish: invalid deadline/poll limits")
	}
	if _, e := c.finishURL(c.APIOrigin, true); e != nil {
		return e
	}
	u, e := c.finishURL(c.ServiceURL, false)
	if e != nil {
		return e
	}
	if u.Scheme+"://"+u.Host == c.APIOrigin {
		return errors.New("finish: separate Service authority required")
	}
	for _, p := range []string{c.APICAFile, c.BearerFile, c.Executable, c.JournalDir, c.ReceiptFile} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return errors.New("finish: explicit absolute paths required")
		}
	}
	if c.ServiceCAFile != "" && (!filepath.IsAbs(c.ServiceCAFile) || filepath.Clean(c.ServiceCAFile) != c.ServiceCAFile) {
		return errors.New("finish: explicit absolute Service CA path required")
	}
	if (c.Live == nil && filepath.Base(c.Executable) == "virsh") || c.ReceiptFile != filepath.Join(c.JournalDir, "receipts.json") {
		return errors.New("finish: fixture executable and journal-owned receipt required")
	}
	return nil
}
func runFinish(args []string, w io.Writer) error {
	f := flag.NewFlagSet("finish-run", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	config := f.String("config", "", "explicit test config")
	execute := f.Bool("execute", false, "execute approved local fixture")
	confirm := f.String("confirm", "", "canonical config SHA256 from plan")
	if f.Parse(args) != nil || f.NArg() != 0 || *config == "" {
		return errors.New("usage: finish-run -config FILE [-execute -confirm PLAN_SHA256]")
	}
	b, e := observerLocalFile(*config, 1<<20)
	if e != nil {
		return e
	}
	var c FinishConfig
	if strictDecode(b, &c, true) != nil {
		return errors.New("finish: invalid config")
	}
	if e = c.validate(); e != nil {
		return e
	}
	if !*execute {
		_, e = fmt.Fprintf(w, "PLAN run=%s binding=%s backend=%s action=disable-policy,confirm-stop,query-resume,verify-cleanup writes=0 vmCommands=0 credentialsRead=0 network=0 liveAuthorizationVerified=false unmet=execute,exact-confirm,authority,registration,permissions,fresh-Guard,stop-proof liveProof=false\n", c.RunID, c.binding(), c.Backend)
		return e
	}
	if *confirm != c.binding() {
		return errors.New("finish: execution requires exact run/config binding from plan (not an authorization substitute)")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return executeFinishInput(ctx, c, w, observerWallClock{}, fixtureVMRunner{c.Executable, c.ExecutableSHA256}, nil, digestBytes(b))
}

type finisher struct {
	c            FinishConfig
	j            *finishJournal
	api, service *finishClient
	runner       finishVMRunner
	clock        observerClock
	guardCall    func(context.Context, string, func(string) error) (string, error)
	inputHash    string
}

func (c FinishConfig) path(resource, name string) string {
	prefix := "/api/v1/"
	if resource != "namespaces" {
		prefix += "namespaces/" + c.Namespace + "/"
	}
	if resource == "recoverypolicies" {
		prefix = "/apis/opsloop.io/v1alpha1/namespaces/" + c.Namespace + "/"
	}
	if resource == "deployments" {
		prefix = "/apis/apps/v1/namespaces/" + c.Namespace + "/"
	}
	return prefix + resource + "/" + name
}
func executeFinish(parent context.Context, c FinishConfig, w io.Writer, clock observerClock, runner finishVMRunner, hook func(*finishJournal)) error {
	b, _ := json.Marshal(c)
	return executeFinishInput(parent, c, w, clock, runner, hook, digestBytes(b))
}
func executeFinishInput(parent context.Context, c FinishConfig, w io.Writer, clock observerClock, runner finishVMRunner, hook func(*finishJournal), inputHash string) error {
	if e := c.validate(); e != nil {
		return e
	}
	if e := authorizeFinishLive(c); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(c.DeadlineMS)*time.Millisecond)
	defer cancel()
	b, e := observerLocalFile(c.BearerFile, 16<<10)
	if e != nil {
		return e
	}
	token := strings.TrimSpace(string(b))
	if !observerBearer.MatchString(token) {
		return errors.New("finish: invalid synthetic bearer")
	}
	config, _ := json.Marshal(c)
	if bytes.Contains(config, []byte(token)) {
		return errors.New("finish: credential in config blocked")
	}
	j, e := openFinishJournal(c, clock, []byte(token))
	if e != nil {
		return e
	}
	defer j.close()
	if hook != nil {
		hook(j)
	}
	reads := map[string]bool{}
	for _, v := range [][2]string{{"namespaces", "kube-system"}, {"namespaces", c.Namespace}, {"recoverypolicies", c.Policy}, {"deployments", c.Workload}, {"services", c.Service}, {"pods", c.ExpectedPod.Name}} {
		reads[c.path(v[0], v[1])] = true
	}
	for _, p := range c.OldPods {
		reads[c.path("pods", p.Name)] = true
	}
	api, e := newFinishClient(c.APIOrigin, c.APICAFile, token, c.path("recoverypolicies", c.Policy), reads, c.timeout())
	if e != nil {
		return e
	}
	defer api.client.CloseIdleConnections()
	u, _ := c.finishURL(c.ServiceURL, false)
	svc, e := newFinishClient(u.Scheme+"://"+u.Host, c.ServiceCAFile, "", "", map[string]bool{u.RequestURI(): true}, c.timeout())
	if e != nil {
		return e
	}
	defer svc.client.CloseIdleConnections()
	x := &finisher{c: c, j: j, api: api, service: svc, runner: runner, clock: clock, inputHash: inputHash}
	if e := x.bindLiveInputs(); e != nil {
		return e
	}
	status, err := x.finish(ctx)
	// Redelivery preserves original bytes and completion time. A changed current
	// verdict archives the prior publication before replacing it.
	if j.failed {
		return errors.New("finish: journal failure; effects may be unconfirmed; recovery/manual review required")
	}
	published := j.last("receipt-published")
	var publication struct{ Status, SHA256 string }
	if published != nil {
		_ = json.Unmarshal(published.Data, &publication)
	}
	if publication.Status != status || status != "CLEANUP_SUCCEEDED" {
		if pe := x.publish(status); pe != nil {
			return pe
		}
	}
	if c.Live != nil {
		if e := x.publishBinding(status); e != nil {
			return e
		}
	}
	_, we := fmt.Fprintf(w, "finish=%s run=%s actionID=%s liveProof=false\n", status, c.RunID, j.binding)
	if err != nil {
		return err
	}
	return we
}
func (x *finisher) read(ctx context.Context, resource, name string, v any) (int, error) {
	b, status, problem := x.api.request(ctx, x.c.path(resource, name), nil)
	// Store only metadata/projections after decoding, never arbitrary API bodies.
	if e := x.j.add("api-read", struct {
		Resource, Name, Problem string
		HTTP                    int
	}{resource, name, problem, status}); e != nil {
		return status, e
	}
	if problem != "" || strictDecode(b, v, false) != nil {
		return status, errors.New("finish: API observation unavailable")
	}
	return status, nil
}
func (x *finisher) identities(ctx context.Context) error {
	for _, v := range []struct{ Name, UID string }{{"kube-system", x.c.ClusterUID}, {x.c.Namespace, x.c.NamespaceUID}} {
		var n core.Namespace
		if _, e := x.read(ctx, "namespaces", v.Name, &n); e != nil {
			return e
		}
		if n.Name != v.Name || string(n.UID) != v.UID || n.DeletionTimestamp != nil {
			return errors.New("finish: cluster/namespace identity mismatch")
		}
	}
	return nil
}

type finishSnapshot struct {
	Policy     meta.ObjectMeta       `json:"policy"`
	Spec       map[string]any        `json:"spec"`
	SpecHash   string                `json:"specHash"`
	Deployment deploymentObservation `json:"deployment"`
	Proof      StopProof             `json:"proof"`
}

func (x *finisher) snapshot(ctx context.Context) (finishSnapshot, error) {
	var p v1.RecoveryPolicy
	var d apps.Deployment
	var raw struct {
		Spec map[string]any `json:"spec"`
	}
	// Preserve unknown spec fields in the JSON Patch test as well as typed hash.
	b, status, problem := x.api.request(ctx, x.c.path("recoverypolicies", x.c.Policy), nil)
	if e := x.j.add("api-read", struct {
		Resource, Problem string
		HTTP              int
	}{"recoverypolicies", problem, status}); e != nil {
		return finishSnapshot{}, e
	}
	if problem != "" || strictDecode(b, &p, false) != nil || strictDecode(b, &raw, false) != nil {
		return finishSnapshot{}, errors.New("finish: Policy read unavailable")
	}
	if _, e := x.read(ctx, "deployments", x.c.Workload, &d); e != nil {
		return finishSnapshot{}, e
	}
	if p.Name != x.c.Policy || p.Namespace != x.c.Namespace || string(p.UID) != x.c.PolicyUID || p.ResourceVersion == "" || p.DeletionTimestamp != nil || p.Spec.TargetRef.Name != x.c.Workload || d.Name != x.c.Workload || d.Namespace != x.c.Namespace || string(d.UID) != x.c.DeploymentUID || d.ResourceVersion == "" || d.DeletionTimestamp != nil {
		return finishSnapshot{}, errors.New("finish: Policy/Deployment identity mismatch")
	}
	dp := projectDeployment(d)
	if dp.SpecHash != x.c.DeploymentSpecHash || dp.TemplateHash != x.c.DeploymentTemplateHash {
		return finishSnapshot{}, errors.New("finish: Deployment desired state changed")
	}
	enabled, deleting, absent := p.Spec.Enabled, false, p.Status.ActiveIncident == nil
	proof := StopProof{PolicyUID: string(p.UID), Generation: p.Generation, Enabled: &enabled, Mode: string(p.Spec.Mode), Deleting: &deleting, ActiveIncidentAbsent: &absent}
	if dp.Fence != nil {
		proof.LiveFence = *dp.Fence
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "Quiesced" {
			proof.QuiescedStatus = string(c.Status)
			proof.QuiescedReason = c.Reason
			proof.ObservedGeneration = c.ObservedGeneration
			_ = json.Unmarshal([]byte(c.Message), &proof.AcknowledgedFence)
		}
	}
	s := finishSnapshot{projectionMeta(p.ObjectMeta), raw.Spec, v1.PolicyHash(p.Spec), dp, proof}
	if e := x.j.add("snapshot", s); e != nil {
		return s, e
	}
	return s, nil
}
func (x *finisher) stopped(s finishSnapshot) bool {
	p := s.Proof
	f := p.LiveFence
	// This adapter requested disabled, so re-enabling even ObserveOnly invalidates
	// its authorization. Generation and full spec must match this action.
	return falseValue(p.Enabled) && p.Generation == x.c.PolicyGeneration+1 && p.ObservedGeneration == p.Generation && (p.Mode == modeEnforce || p.Mode == modeObserveOnly) && falseValue(p.Deleting) && trueValue(p.ActiveIncidentAbsent) && p.QuiescedStatus == "True" && p.QuiescedReason == "FenceClosed" && f == p.AcknowledgedFence && f.Version == 2 && f.PolicyUID == x.c.PolicyUID && f.PolicyName == x.c.Policy && f.DeploymentUID == x.c.DeploymentUID && f.ClosedThroughGeneration >= p.Generation && x.expectedStoppedSpec(s.Spec)
}
func (x *finisher) expectedStoppedSpec(spec map[string]any) bool {
	e := x.j.last("stop-intent")
	if e == nil {
		return false
	}
	var s finishSnapshot
	if json.Unmarshal(e.Data, &s) != nil {
		return false
	}
	s.Spec["enabled"] = false
	a, _ := json.Marshal(s.Spec)
	b, _ := json.Marshal(spec)
	return bytes.Equal(a, b)
}
func (x *finisher) guard(ctx context.Context) (finishSnapshot, error) {
	if e := x.identities(ctx); e != nil {
		return finishSnapshot{}, e
	}
	a, e := x.snapshot(ctx)
	if e != nil || !x.stopped(a) {
		return a, errors.New("finish: stop unconfirmed or contradicted")
	}
	b, e := x.snapshot(ctx)
	if e != nil || !x.stopped(b) || a.Proof.Generation != b.Proof.Generation || a.Proof.LiveFence != b.Proof.LiveFence {
		return b, errors.New("finish: stop changed between separate GETs")
	}
	return b, nil // bounded observations, not an atomic multi-object snapshot
}
func (x *finisher) finish(ctx context.Context) (string, error) {
	fail := func(s string, e error) (string, error) {
		if je := x.j.add("incomplete", struct{ Status string }{s}); je != nil {
			return "JOURNAL_UNCONFIRMED", je
		}
		return s, e
	}
	if e := x.identities(ctx); e != nil {
		return fail("IDENTITY_REJECTED", e)
	}
	s, e := x.snapshot(ctx)
	if e != nil {
		return fail("STOP_UNCONFIRMED", e)
	}
	if x.j.last("stop-intent") == nil {
		if !trueValue(s.Proof.Enabled) {
			return fail("EXTERNAL_STOP_NO_REQUEST", errors.New("finish: already disabled without own ledger"))
		}
		if s.Policy.Generation != x.c.PolicyGeneration || s.SpecHash != x.c.PolicyHash {
			return fail("AUTHORIZATION_CHANGED", errors.New("finish: approved Policy generation/spec changed"))
		}
		if e = x.j.add("stop-intent", s); e != nil {
			return "JOURNAL_UNCONFIRMED", e
		}
	}
	if x.j.last("stop-dispatch") == nil {
		// A prior intent without dispatch is safe to retry only after full validation.
		for attempt := 0; attempt < 3; attempt++ {
			if e = x.identities(ctx); e != nil {
				return fail("IDENTITY_REJECTED", e)
			}
			s, e = x.snapshot(ctx)
			if e != nil {
				return fail("STOP_UNCONFIRMED", e)
			}
			var initial finishSnapshot
			_ = json.Unmarshal(x.j.last("stop-intent").Data, &initial)
			a, _ := json.Marshal(initial.Spec)
			b, _ := json.Marshal(s.Spec)
			if s.Policy.Generation != x.c.PolicyGeneration || s.SpecHash != x.c.PolicyHash || !bytes.Equal(a, b) || !trueValue(s.Proof.Enabled) {
				return fail("AUTHORIZATION_CHANGED", errors.New("finish: conflict revalidation changed authorization"))
			}
			if e = x.j.add("stop-dispatch", struct {
				Attempt int
				RV      string
			}{attempt + 1, s.Policy.ResourceVersion}); e != nil {
				return "JOURNAL_UNCONFIRMED", e
			}
			if ctx.Err() != nil {
				return fail("STOP_UNCONFIRMED", ctx.Err())
			}
			dispatch := *x.j.last("stop-dispatch")
			if e = authorizeFinishLive(x.c); e != nil {
				return fail("AUTHORIZATION_CHANGED", e)
			}
			requested := x.j.capture()
			_, status, problem := x.api.request(ctx, x.c.path("recoverypolicies", x.c.Policy), stopOnlyPatch(x.c.PolicyUID, s.Policy.ResourceVersion, s.Spec))
			if e = x.j.add("stop-response", struct {
				Dispatch  int
				HTTP      int
				Problem   string
				Requested Stamp
			}{dispatch.Seq, status, problem, requested}); e != nil {
				return "JOURNAL_UNCONFIRMED", e
			}
			if status != 409 {
				break
			}
			if e = x.j.add("stop-conflict", struct{ Attempt int }{attempt + 1}); e != nil {
				return "JOURNAL_UNCONFIRMED", e
			}
			if attempt == 2 {
				return fail("STOP_CONFLICT", errors.New("finish: conflict limit"))
			}
		}
	}
	// Never replay an ambiguous dispatch, including a crash before actual send.
	// Recovery queries effects; lack of effect remains unconfirmed/manual.
	var confirmed finishSnapshot
	for poll := 0; poll < 200; poll++ {
		s, e = x.snapshot(ctx)
		if e != nil {
			return fail("STOP_UNCONFIRMED", e)
		}
		if x.stopped(s) {
			confirmed = s
			break
		}
		if s.Policy.Generation > x.c.PolicyGeneration+1 || (s.Policy.Generation == x.c.PolicyGeneration+1 && !x.expectedStoppedSpec(s.Spec)) {
			return fail("STOP_CONTRADICTED", errors.New("finish: external generation/spec change"))
		}
		if e = x.clock.Sleep(ctx, time.Duration(x.c.PollMS)*time.Millisecond); e != nil {
			return fail("STOP_UNCONFIRMED", errors.New("finish: stop deadline"))
		}
	}
	if confirmed.Policy.UID == "" {
		return fail("STOP_UNCONFIRMED", errors.New("finish: stop not confirmed"))
	}
	if e = x.j.add("stop-confirmed", confirmed); e != nil {
		return "JOURNAL_UNCONFIRMED", e
	}
	if x.c.Live != nil {
		if status, e := x.restoreLab(ctx); e != nil {
			return fail(status, e)
		}
	} else if status, e := x.restoreSynthetic(ctx); e != nil {
		return fail(status, e)
	}
	if _, e = x.guard(ctx); e != nil {
		return fail("STOP_CONTRADICTED", e)
	}
	if e = x.postChecks(ctx); e != nil {
		return fail("CLEANUP_UNCONFIRMED", e)
	}
	if _, e = x.guard(ctx); e != nil {
		return fail("STOP_CONTRADICTED", e)
	}
	if e = x.checkLiveGuard(ctx); e != nil {
		return fail("GUARD_REJECTED", e)
	}
	if e = ctx.Err(); e != nil {
		return fail("CLEANUP_UNCONFIRMED", e)
	}
	if x.j.last("cleanup-confirmed") == nil {
		if e = x.j.addWhileActive(ctx, "cleanup-confirmed", struct{ Scenario string }{x.c.Scenario}); e != nil {
			if !x.j.failed {
				return fail("CLEANUP_UNCONFIRMED", e)
			}
			return "JOURNAL_UNCONFIRMED", e
		}
	}
	return "CLEANUP_SUCCEEDED", nil
}
func (x *finisher) restoreSynthetic(ctx context.Context) (string, error) {
	fail := func(s string, e error) (string, error) { return s, e }
	state, e := x.vmState(ctx)
	if e != nil {
		return fail("RESTORE_UNCONFIRMED", e)
	}
	if x.j.last("resume-dispatch") == nil {
		if state != "paused" {
			return fail("EXTERNAL_RESTORE", errors.New("finish: running without own resume ledger"))
		}
		if _, e = x.guard(ctx); e != nil {
			return fail("STOP_CONTRADICTED", e)
		}
		if x.j.last("resume-intent") == nil {
			if e = x.j.add("resume-intent", struct{ UUID string }{x.c.VMUUID}); e != nil {
				return "JOURNAL_UNCONFIRMED", e
			}
		}
		if e = x.j.add("resume-dispatch", struct{ UUID string }{x.c.VMUUID}); e != nil {
			return "JOURNAL_UNCONFIRMED", e
		}
		if ctx.Err() != nil {
			return fail("RESTORE_UNCONFIRMED", ctx.Err())
		}
		_, commandErr := x.vm(ctx, "resume")
		if x.j.failed {
			return "JOURNAL_UNCONFIRMED", commandErr
		}
		// Even command timeout/nonzero is reconciled by fresh target state, never
		// blindly retried. A running VM alone does not finish cleanup.
		if e = x.j.add("resume-response", struct{ ExitOK bool }{commandErr == nil}); e != nil {
			return "JOURNAL_UNCONFIRMED", e
		}
	}
	state, e = x.vmState(ctx)
	if e != nil || state != "running" {
		return fail("RESTORE_UNCONFIRMED", errors.New("finish: resume effect unconfirmed; manual intervention required"))
	}
	if x.j.last("resume-confirmed") == nil {
		if e = x.j.add("resume-confirmed", struct{ UUID, State string }{x.c.VMUUID, state}); e != nil {
			return "JOURNAL_UNCONFIRMED", e
		}
	}
	return "", nil
}
func (x *finisher) postChecks(ctx context.Context) error {
	var svc core.Service
	if _, e := x.read(ctx, "services", x.c.Service, &svc); e != nil {
		return e
	}
	if svc.Name != x.c.Service || svc.Namespace != x.c.Namespace || string(svc.UID) != x.c.ServiceUID || svc.DeletionTimestamp != nil {
		return errors.New("finish: Service identity changed")
	}
	var pod core.Pod
	if _, e := x.read(ctx, "pods", x.c.ExpectedPod.Name, &pod); e != nil {
		return e
	}
	if pod.Name != x.c.ExpectedPod.Name || pod.Namespace != x.c.Namespace || string(pod.UID) != x.c.ExpectedPod.UID || pod.DeletionTimestamp != nil || pod.Spec.NodeName != x.c.ExpectedNode {
		return errors.New("finish: expected Pod identity changed")
	}
	for _, p := range x.c.OldPods {
		var old core.Pod
		status, e := x.read(ctx, "pods", p.Name, &old)
		if status != 404 && (e != nil || old.Name != p.Name || old.Namespace != x.c.Namespace || string(old.UID) != p.UID) {
			return errors.New("finish: old Pod inventory unconfirmed")
		}
		if e = x.j.add("old-pod-inventory", struct {
			Name, UID string
			APIAbsent bool
		}{p.Name, p.UID, status == 404}); e != nil {
			return e
		}
	}
	u, _ := x.c.finishURL(x.c.ServiceURL, false)
	b, status, problem := x.service.request(ctx, u.RequestURI(), nil)
	var body struct{ Node, Pod, PodUID, Arch, Version string }
	ok := problem == "" && strictDecode(b, &body, false) == nil && body.Node == x.c.ExpectedNode && body.Pod == x.c.ExpectedPod.Name && body.PodUID == x.c.ExpectedPod.UID && body.Version == x.c.ExpectedVersion
	if e := x.j.add("service-postcheck", struct {
		HTTP    int
		Matched bool
	}{status, ok}); e != nil {
		return e
	}
	if !ok {
		return errors.New("finish: Service postcheck unconfirmed")
	}
	return x.j.add("postchecks-confirmed", struct{ WorkloadChecked, OldPodsChecked, ServiceChecked bool }{true, true, true})
}
func (x *finisher) publish(status string) error {
	r := ObserverReceipts{Version: "opsloop-observer-receipts/v1", RunID: x.c.RunID, EvidenceKind: "synthetic", Provider: "finish-executor/local-test-only; identities and raw boundaries in adjacent event journal"}
	if x.c.Live != nil {
		r.EvidenceKind = x.c.evidenceKind()
		r.Provider = "finish-executor/lab-libvirt; adjacent binding companion required; registration unverified; liveProof=false"
	}
	// Dispatch is write-ahead; only a returned request call establishes an actual
	// request attempt here. A dispatch-only crash remains unproven in the receipt.
	if response := x.j.last("stop-response"); response != nil {
		var result struct {
			Dispatch  int
			HTTP      int
			Problem   string
			Requested Stamp
		}
		_ = json.Unmarshal(response.Data, &result)
		for i := range x.j.events {
			event := &x.j.events[i]
			if event.Seq == result.Dispatch && result.HTTP != 409 {
				if stampValid(result.Requested) {
					r.StopRequested = &result.Requested
				}
			}
		}
	}

	if d := x.j.last("resume-dispatch"); d != nil {
		for _, event := range x.j.events {
			if event.Kind != "vm-started" {
				continue
			}
			var start finishVMStart
			if json.Unmarshal(event.Data, &start) != nil || start.Dispatch != d.Seq || start.Operation != "resume" || start.UUID != x.c.VMUUID || !stampValid(start.BeforeStart) || !stampValid(start.AfterStart) || start.AfterStart.At.Before(start.BeforeStart.At) {
				continue
			}
			// Require the referenced call, in the same action and after this dispatch.
			if start.Call <= d.Seq || start.Call >= event.Seq {
				continue
			}
			call := x.j.events[start.Call-1]
			var cv struct{ Operation, UUID string }
			if call.Kind != "vm-call" || json.Unmarshal(call.Data, &cv) != nil || cv.Operation != "resume" || cv.UUID != start.UUID {
				continue
			}
			v := Intervention{Actor: "cleanup", Kind: "restore", TargetUID: x.c.VMUUID, Requested: start.AfterStart, Ref: fmt.Sprintf("event-%06d.json", event.Seq)}
			if c := x.j.last("resume-confirmed"); c != nil && c.Seq > event.Seq {
				v.Confirmed = &c.At
			}
			r.Interventions = append(r.Interventions, v)
		}
	}

	if c := x.j.last("cleanup-confirmed"); c != nil && status == "CLEANUP_SUCCEEDED" {
		r.Cleanup = &CleanupProof{Status: "succeeded", Completed: c.At, WorkloadChecked: finishBool(true), OldPodsChecked: finishBool(true), ServiceChecked: finishBool(true), Ref: fmt.Sprintf("event-%06d.json", c.Seq)}
	} else {
		r.Cleanup = &CleanupProof{Status: "pending", Ref: "journal:incomplete"}
	}
	// No fault/source times, clock bounds or complete actor coverage are invented.
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	if len(x.j.forbidden) > 0 && bytes.Contains(b, x.j.forbidden) {
		return errors.New("finish: credential reflection blocked")
	}
	// Preserve actual prior published bytes before any replacement, including
	// succeeded -> pending. Archive failure leaves current receipt untouched.
	if e = x.archivePublication(); e != nil {
		return e
	}
	// Atomic publish after durable bytes.
	tmp := filepath.Join(x.j.dir, fmt.Sprintf("receipt-%s.tmp", x.j.segment))
	if e = finishExclusive(tmp, b); e != nil {
		return errors.New("finish: receipt persistence failed; journal retains unconfirmed effects")
	}
	if e = x.j.add("receipt-ready", struct{ SHA256 string }{digestBytes(b)}); e != nil {
		return e
	}
	if e = x.j.boundary("receipt-before-rename"); e != nil {
		return e
	}
	if e = os.Rename(tmp, x.c.ReceiptFile); e != nil {
		return e
	}
	if e = x.j.boundary("receipt-after-rename"); e != nil {
		return e
	}
	if e = syncFinishDir(x.j.dir); e != nil {
		return e
	}
	if e = x.j.boundary("receipt-after-sync"); e != nil {
		return e
	}
	return x.j.add("receipt-published", struct{ Status, SHA256 string }{status, digestBytes(b)})
}

func finishBool(v bool) *bool { return &v }
