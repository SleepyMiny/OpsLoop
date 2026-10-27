//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var guardID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type guardOwner struct {
	PID   int    `json:"pid"`
	Boot  string `json:"boot"`
	Start string `json:"startTicks"`
}
type guardConfig struct {
	Version          string         `json:"version"`
	Mode             string         `json:"mode"`
	Run              string         `json:"run"`
	Epoch            string         `json:"epoch"`
	Target           recoveryTarget `json:"target"`
	Allowed          string         `json:"allowed"`
	Root             string         `json:"root"`
	Owner            guardOwner     `json:"owner"`
	HardDeadline     time.Time      `json:"hardDeadline"`
	AuthorityExpires time.Time      `json:"authorityExpires"`
	MaxTrialMS       int64          `json:"maxTrialMs"`
	HeartbeatMS      int64          `json:"heartbeatMs"`
	TimeoutMS        int64          `json:"timeoutMs"`
}

func (c guardConfig) binding() string { b, _ := json.Marshal(c); return digestBytes(b) }
func (c guardConfig) validate() error {
	if e := c.Target.validate(); e != nil {
		return e
	}
	if c.Mode == "live-libvirt" && filepath.Base(c.Target.Executable) != "virsh" {
		return errors.New("guard: live executable must be explicitly pinned virsh")
	}
	if c.Version != "opsloop-guard/v1" || (c.Mode != "synthetic-local" && c.Mode != "live-libvirt") || !guardID.MatchString(c.Run) || !guardID.MatchString(c.Epoch) || c.Run == "" || c.Epoch == "" || c.Allowed != "query-resume-only" || !filepath.IsAbs(c.Root) || filepath.Clean(c.Root) != c.Root || c.Owner.PID <= 0 || c.Owner.Start == "" || c.Owner.Boot == "" || c.MaxTrialMS < 100 || c.MaxTrialMS > 3600000 || c.HeartbeatMS < 50 || c.HeartbeatMS > c.MaxTrialMS || c.TimeoutMS < 10 || c.TimeoutMS > 2000 || c.HardDeadline.IsZero() || !c.AuthorityExpires.After(c.HardDeadline) || c.AuthorityExpires.Sub(c.HardDeadline) > time.Minute {
		return errors.New("guard: invalid bounded run configuration")
	}
	return nil
}

type guardMoment struct {
	Wall time.Time
	Boot string
	Tick int64
}

func guardNow() (guardMoment, error) {
	b, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return guardMoment{}, e
	}
	var ts unix.Timespec
	if e = unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); e != nil {
		return guardMoment{}, e
	}
	return guardMoment{time.Now().UTC(), strings.TrimSpace(string(b)), ts.Nano()}, nil
}
func guardProcess(pid int, boot string) (guardOwner, error) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return guardOwner{}, e
	}
	end := strings.LastIndex(string(b), ")")
	if end < 0 {
		return guardOwner{}, errors.New("guard: process identity invalid")
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return guardOwner{}, errors.New("guard: owner exited")
	}
	return guardOwner{pid, boot, fields[19]}, nil
}

type guardState struct {
	Config            guardConfig     `json:"config"`
	Binding           string          `json:"binding"`
	Phase             string          `json:"phase"`
	ArmWall           time.Time       `json:"armWall"`
	Boot              string          `json:"boot"`
	ArmTick           int64           `json:"armTick"`
	HardTick          int64           `json:"hardTick"`
	ExpiryTick        int64           `json:"expiryTick"`
	HeartbeatTick     int64           `json:"heartbeatTick"`
	HeartbeatSequence uint64          `json:"heartbeatSequence"`
	LastTick          int64           `json:"lastTick"`
	ResumeIntent      bool            `json:"resumeIntent"`
	Emergency         bool            `json:"emergency"`
	Events            []recoveryEvent `json:"events"`
	// Explicitly incompatible with normal Observer/Acceptance success receipts.
	AutomaticSuccessEligible bool   `json:"automaticSuccessEligible"`
	ConsumerIntegration      string `json:"consumerIntegration"`
}

func (s *guardState) closed() bool {
	return s.Phase == "DISARMED" || s.Phase == "EMERGENCY_RESTORE_CONFIRMED" || s.Phase == "ABORTED_MANUAL"
}

var errGuardBusy = errors.New("guard: target busy; bounded lock acquisition")

type guardStore struct {
	dir, path  string
	lock       *os.File
	beforeSave func(*guardState) error
}

func guardPrivateDir(path string) error {
	// Parent chain must already exist; no symlink components are accepted.
	for p := path; ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("guard: directory symlink/non-directory rejected")
		}
		if p == "/" {
			break
		}
	}
	st, e := os.Stat(path)
	if e != nil || st.Mode().Perm()&0077 != 0 {
		return errors.New("guard: private directory required")
	}
	return nil
}
func openGuard(c guardConfig) (*guardStore, error) {
	if e := guardPrivateDir(c.Root); e != nil {
		return nil, e
	}
	dir := filepath.Join(c.Root, digestBytes([]byte(c.Target.URI+"\n"+c.Target.UUID)))
	if e := os.Mkdir(dir, 0700); e != nil && !os.IsExist(e) {
		return nil, e
	}
	if e := guardPrivateDir(dir); e != nil {
		return nil, e
	}
	if e := syncFinishDir(c.Root); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(dir, "target.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return nil, e
	}
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("guard: invalid lock")
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errGuardBusy
	}
	// Run/epoch key deliberately excludes configuration hash: changed configuration
	// cannot turn an existing epoch into a fresh authorization.
	key := digestBytes([]byte(c.Run + "\n" + c.Epoch))
	return &guardStore{dir: dir, path: filepath.Join(dir, key+".json"), lock: f}, nil
}

// CLI callers may briefly retry contention, never a dead executor journal lock.
func acquireGuard(c guardConfig) (*guardStore, error) {
	until := time.Now().Add(6*time.Duration(c.TimeoutMS)*time.Millisecond + 2*time.Second)
	for {
		g, e := openGuard(c)
		if e != errGuardBusy {
			return g, e
		}
		if !time.Now().Before(until) {
			return nil, e
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (g *guardStore) close() { g.lock.Close() }

// Shared bounded, no-follow data reader; it never creates directories or opens a Guard.
func guardFileBytes(path string, limit int64) ([]byte, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("guard: invalid durable file")
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > limit {
		return nil, errors.New("guard: input grew beyond limit")
	}
	return b, nil
}
func guardRead(path string, v any) error {
	b, e := guardFileBytes(path, 2<<20)
	if e != nil {
		return e
	}
	return strictDecode(b, v, true)
}
func (g *guardStore) load(c guardConfig) (*guardState, error) {
	var s guardState
	if e := guardRead(g.path, &s); e != nil {
		return nil, e
	}
	return validateGuardRecord(c, &s)
}

// Pure historical validation: no current authority/owner checks or live actions.
func validateGuardRecord(c guardConfig, s *guardState) (*guardState, error) {
	if s.Binding != c.binding() || s.Config.binding() != s.Binding || s.Boot != c.Owner.Boot || s.HardTick <= s.ArmTick || s.ExpiryTick <= s.HardTick || s.LastTick < s.ArmTick || s.HeartbeatTick < s.ArmTick || s.AutomaticSuccessEligible || s.ConsumerIntegration != "UNINTEGRATED" {
		return nil, errors.New("guard: durable identity/invariants invalid")
	}
	hard := c.HardDeadline.Sub(s.ArmWall)
	if max := time.Duration(c.MaxTrialMS) * time.Millisecond; hard > max {
		hard = max
	}
	if hard <= 0 || s.HardTick-s.ArmTick != int64(hard) || s.ExpiryTick-s.ArmTick != int64(c.AuthorityExpires.Sub(s.ArmWall)) || s.HeartbeatTick > s.LastTick || len(s.Events) == 0 || s.Events[0].Kind != "armed" {
		return nil, errors.New("guard: durable time/history invalid")
	}
	hasIntent, hasEmergency, hasEffect, hasDisarm, hasManual := false, false, false, false, false
	for _, e := range s.Events {
		switch e.Kind {
		case "resume-intent":
			hasIntent = true
		case "EMERGENCY_RESTORE":
			hasEmergency = true
		case "effect-confirmed":
			hasEffect = true
		case "disarmed":
			hasDisarm = true
		case "manual-review":
			hasManual = true
		}
	}
	if s.ResumeIntent != hasIntent || s.Emergency != hasEmergency || (s.Phase == "EMERGENCY_RESTORE_CONFIRMED" && (!hasEffect || !hasEmergency)) || (s.Phase == "DISARMED" && (!hasDisarm || hasEmergency)) || (s.Phase == "ABORTED_MANUAL" && !hasManual) {
		return nil, errors.New("guard: durable phase/evidence mismatch")
	}

	for i, e := range s.Events {
		if e.Sequence != i+1 || e.Actor != "recovery-guard" {
			return nil, errors.New("guard: durable event invalid")
		}
	}
	switch s.Phase {
	case "ARMED", "RESTORING", "DISARMED", "EMERGENCY_RESTORE_CONFIRMED", "ABORTED_MANUAL":
	default:
		return nil, errors.New("guard: invalid durable phase")
	}
	return s, nil
}
func (g *guardStore) save(s *guardState) error {
	if g.beforeSave != nil {
		if e := g.beforeSave(s); e != nil {
			return e
		}
	}
	b, e := json.MarshalIndent(s, "", "  ")
	if e != nil {
		return e
	}
	if len(b) > 2<<20 {
		return errors.New("guard: state output limit")
	}
	f, e := os.CreateTemp(g.dir, ".pending-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, g.path); e != nil {
		return e
	}
	return syncFinishDir(g.dir)
}
func (s *guardState) clockCheck(n guardMoment) error {
	if n.Boot != s.Boot || n.Tick < s.LastTick || n.Tick < s.ArmTick {
		return errors.New("guard: boot/monotonic time uncertain")
	}
	skew := n.Wall.Sub(s.ArmWall) - time.Duration(n.Tick-s.ArmTick)
	if skew < -2*time.Second || skew > 2*time.Second {
		return errors.New("guard: wall clock uncertain")
	}
	return nil
}
func (s *guardState) permit(n guardMoment) error {
	if e := s.clockCheck(n); e != nil {
		return e
	}
	if n.Tick >= s.ExpiryTick || !n.Wall.Before(s.Config.AuthorityExpires) {
		return errors.New("guard: restoration authority expired")
	}
	return nil
}

// The same eligibility check is used before querying and immediately before
// normal disarm. A running observation alone cannot release responsibility.
func (s *guardState) check(n guardMoment) (string, error) {
	if e := s.permit(n); e != nil {
		return "", e
	}
	owner, e := guardProcess(s.Config.Owner.PID, n.Boot)
	switch {
	case n.Tick >= s.HardTick:
		return "hard-deadline", nil
	case e != nil || owner != s.Config.Owner:
		return "owner-exited", nil
	case n.Tick-s.HeartbeatTick >= s.Config.HeartbeatMS*int64(time.Millisecond):
		return "heartbeat-lost", nil
	case s.Phase == "RESTORING" || s.Emergency:
		return "prior-emergency", nil
	default:
		return "", nil
	}
}
func (g *guardStore) event(s *guardState, n guardMoment, kind, op, detail string) error {
	next := *s
	if n.Tick > next.LastTick {
		next.LastTick = n.Tick
	}
	next.Events = append(next.Events, recoveryEvent{len(next.Events) + 1, "recovery-guard", kind, op, detail, n.Wall})
	if e := g.save(&next); e != nil {
		return e
	}
	*s = next
	return nil
}
func guardArm(g *guardStore, c guardConfig, n guardMoment) (*guardState, error) {
	if _, e := os.Lstat(g.path); e == nil {
		return nil, errors.New("guard: epoch already exists; use watch to restart")
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	entries, e := os.ReadDir(g.dir)
	if e != nil {
		return nil, e
	}
	for _, f := range entries {
		if strings.HasSuffix(f.Name(), ".json") {
			var old guardState
			if e := guardRead(filepath.Join(g.dir, f.Name()), &old); e != nil {
				return nil, e
			}
			if e := old.Config.validate(); e != nil {
				return nil, e
			}
			previous := *g
			previous.path = filepath.Join(g.dir, f.Name())
			if old.Config.Root != c.Root || old.Config.Target.URI != c.Target.URI || old.Config.Target.UUID != c.Target.UUID {
				return nil, errors.New("guard: foreign target history")
			}
			if _, e := previous.load(old.Config); e != nil {
				return nil, e
			}
			if !old.closed() {
				return nil, errors.New("guard: target already armed")
			}
		}
	}
	if c.Owner.Boot != n.Boot || !n.Wall.Before(c.HardDeadline) || !n.Wall.Before(c.AuthorityExpires) {
		return nil, errors.New("guard: expired or foreign boot arm")
	}
	owner, e := guardProcess(c.Owner.PID, n.Boot)
	if e != nil || owner != c.Owner {
		return nil, errors.New("guard: owner identity mismatch")
	}
	hard := c.HardDeadline.Sub(n.Wall)
	if max := time.Duration(c.MaxTrialMS) * time.Millisecond; hard > max {
		hard = max
	}
	s := &guardState{Config: c, Binding: c.binding(), Phase: "ARMED", ArmWall: n.Wall, Boot: n.Boot, ArmTick: n.Tick, HardTick: n.Tick + int64(hard), ExpiryTick: n.Tick + int64(c.AuthorityExpires.Sub(n.Wall)), HeartbeatTick: n.Tick, LastTick: n.Tick, ConsumerIntegration: "UNINTEGRATED"}
	if e := g.event(s, n, "armed", "", "durable; preparation complete"); e != nil {
		return nil, e
	}
	return s, nil
}

type guardEngine struct {
	now    func() (guardMoment, error)
	runner finishVMRunner
}

func (x guardEngine) backend(g *guardStore, s *guardState) *recoveryBackend {
	return &recoveryBackend{target: s.Config.Target, runner: x.runner, timeout: time.Duration(s.Config.TimeoutMS) * time.Millisecond, permit: func() error {
		n, e := x.now()
		if e != nil {
			return e
		}
		return s.permit(n)
	}, record: func(kind, op, detail string) error {
		n, e := x.now()
		if e != nil {
			return e
		}
		return g.event(s, n, kind, op, detail)
	}}
}
func (x guardEngine) record(g *guardStore, s *guardState, kind, op, detail string) error {
	n, e := x.now()
	if e != nil {
		return e
	}
	return g.event(s, n, kind, op, detail)
}
func (x guardEngine) manual(g *guardStore, s *guardState, reason string) error {
	next := *s
	next.Phase = "ABORTED_MANUAL"
	// At is the local event creation observation, not the step's decision time.
	// On clock/save failure retain durable responsibility; do not recursively
	// attempt another failure event or substitute an older timestamp.
	if e := x.record(g, &next, "manual-review", "", reason); e != nil {
		return fmt.Errorf("guard: durable evidence failure: %w", e)
	}
	*s = next
	return errors.New("guard: " + reason)
}
func (g *guardStore) emergency(s *guardState, n guardMoment, reason string) error {
	if s.Emergency && s.Phase == "RESTORING" {
		return nil
	}
	next := *s
	next.Emergency = true
	next.Phase = "RESTORING"
	if e := g.event(&next, n, "EMERGENCY_RESTORE", "", reason); e != nil {
		return e
	}
	*s = next
	return nil
}
func (x guardEngine) step(ctx context.Context, g *guardStore, s *guardState, disarm bool) error {
	n, e := x.now()
	if e != nil {
		return e
	}
	if s.closed() {
		if s.Phase == "DISARMED" && disarm {
			return nil
		}
		return errors.New("guard: closed epoch")
	}
	reason, e := s.check(n)
	if e != nil {
		return x.manual(g, s, e.Error())
	}
	if reason != "" {
		if e = g.emergency(s, n, reason); e != nil {
			return e
		}
	}
	if disarm && reason != "" {
		return errors.New("guard: disarm after emergency/expiry rejected")
	}
	if reason == "" && !disarm {
		return nil
	}
	// Lock is held only for this bounded operation, never while watching/waiting.
	budget := 6*time.Duration(s.Config.TimeoutMS)*time.Millisecond + time.Second
	remaining := time.Duration(s.ExpiryTick - n.Tick)
	if budget > remaining {
		budget = remaining
	}
	op, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	b := x.backend(g, s)
	// A failed event read/write must stop this operation, including after a
	// resume Start. Do not issue a follow-up query or fabricate a manual event.
	var evidenceErr error
	permit := b.permit
	b.permit = func() error {
		if e := permit(); e != nil {
			evidenceErr = fmt.Errorf("command authorization failed; manual review required: %w", e)
			return evidenceErr
		}
		return nil
	}
	record := b.record
	b.record = func(kind, op, detail string) error {
		if evidenceErr != nil {
			return evidenceErr
		}
		e := record(kind, op, detail)
		if e != nil {
			evidenceErr = e
		}
		return e
	}
	state, e := b.state(op)
	if evidenceErr != nil {
		return fmt.Errorf("guard: durable evidence failure: %w", evidenceErr)
	}
	if e != nil {
		return x.manual(g, s, "identity/state unconfirmed")
	}
	if disarm {
		n, e = x.now()
		if e != nil {
			return fmt.Errorf("guard: final disarm clock unavailable; manual review required, no event recorded: %w", e)
		}
		reason, e = s.check(n)
		if e != nil {
			return x.manual(g, s, e.Error())
		}
		if reason != "" {
			if e = g.emergency(s, n, reason); e != nil {
				return e
			}
			return errors.New("guard: disarm after emergency/expiry rejected")
		}
		if state != "running" {
			return errors.New("guard: disarm requires fresh running confirmation")
		}
		next := *s
		next.Phase = "DISARMED"
		if e = g.event(&next, n, "disarmed", "", "fresh running; no stop/service claim"); e != nil {
			return e
		}
		*s = next
		return nil
	}
	if state == "paused" {
		// Normal Finish participates in this same target lock. Preserve an
		// unresolved normal dispatch instead of issuing a second resume.
		attempt, err := guardNormalAttempt(g, s.Config)
		if err != nil || attempt != nil {
			return x.manual(g, s, "prior normal resume attempt unresolved; no replay")
		}
		if s.ResumeIntent {
			return x.manual(g, s, "prior resume intent unresolved; no replay")
		}
		next := *s
		next.ResumeIntent = true
		if e = x.record(g, &next, "resume-intent", "resume", "single attempt for this epoch"); e != nil {
			return e
		}
		*s = next
		// A lost response, timeout or post-Start evidence error does not prove effect.
		resumeErr := b.resume(op)
		if evidenceErr != nil {
			return fmt.Errorf("guard: durable evidence failure: %w", evidenceErr)
		}
		state, e = b.state(op)
		if evidenceErr != nil {
			return fmt.Errorf("guard: durable evidence failure: %w", evidenceErr)
		}
		if e != nil || state != "running" {
			return x.manual(g, s, "restore unconfirmed; manual review")
		}
		if resumeErr != nil {
			// Preserve the error even when a fresh independent query confirms effect.
			if e = x.record(g, s, "resume-response-unconfirmed", "resume", "fresh effect query follows command error"); e != nil {
				return e
			}
		}
	}
	next := *s
	next.Phase = "EMERGENCY_RESTORE_CONFIRMED"
	if e = x.record(g, &next, "effect-confirmed", "", "running only; no StopProof/service/cleanup claim"); e != nil {
		return e
	}
	*s = next
	return nil
}

// A local attestation is an explicit approval boundary, not a replacement for
// verified target registration, host permissions or an external approval system.
type guardLiveAuthorization struct {
	Version           string    `json:"version"`
	ConfigHash        string    `json:"configHash"`
	ApprovalReference string    `json:"approvalReference"`
	Operator          string    `json:"operator"`
	Expires           time.Time `json:"expires"`
	Scope             string    `json:"scope"`
}

func guardAuthorize(c guardConfig, path string) error {
	if c.Mode == "synthetic-local" {
		if path != "" {
			return errors.New("guard: live approval not applicable to synthetic mode")
		}
		return nil
	}
	var a guardLiveAuthorization
	if path == "" {
		return errors.New("guard: separate live authorization required; live registration/permissions unverified")
	}
	if e := guardRead(path, &a); e != nil {
		return e
	}
	if a.Version != "opsloop-guard-authorization/v1" || a.ConfigHash != c.binding() || a.ApprovalReference == "" || a.Operator == "" || a.Scope != "dedicated-test-target-query-resume" || a.Expires.Before(c.AuthorityExpires) || !time.Now().Before(a.Expires) {
		return errors.New("guard: live authorization mismatch/expired")
	}
	return nil
}
func runGuard(args []string, w io.Writer) error {
	if len(args) == 0 {
		args = []string{"plan"}
	}
	action := args[0]
	f := flag.NewFlagSet("guard "+action, flag.ContinueOnError)
	config := f.String("config", "", "explicit configuration")
	execute := f.Bool("execute", false, "enable bounded actions")
	confirm := f.String("confirm", "", "exact configuration hash; not live authorization")
	approval := f.String("live-authorization", "", "separate externally approved live scope attestation")
	seq := f.Uint64("sequence", 0, "strictly increasing heartbeat sequence")
	pid := f.Int("pid", 0, "explicit local executor PID for owner identity")
	if e := f.Parse(args[1:]); e != nil {
		return e
	}
	if f.NArg() != 0 {
		return errors.New("guard: unexpected arguments")
	}
	if action == "owner" {
		if *pid <= 0 {
			return errors.New("guard: explicit -pid required")
		}
		n, e := guardNow()
		if e != nil {
			return e
		}
		o, e := guardProcess(*pid, n.Boot)
		if e != nil {
			return e
		}
		return json.NewEncoder(w).Encode(o)
	}
	if action != "plan" && action != "arm" && action != "watch" && action != "heartbeat" && action != "disarm" && action != "status" {
		return errors.New("guard: unknown action")
	}
	var c guardConfig
	if e := guardRead(*config, &c); e != nil {
		return e
	}
	if e := c.validate(); e != nil {
		return e
	}
	if action == "plan" || !*execute {
		return json.NewEncoder(w).Encode(map[string]any{"phase": "UNARMED", "configHash": c.binding(), "commands": 0, "liveAuthorizationVerified": false, "consumerIntegration": "UNINTEGRATED"})
	}
	if *confirm != c.binding() {
		return errors.New("guard: configuration confirmation mismatch")
	}
	if e := guardAuthorize(c, *approval); e != nil {
		return e
	}
	engine := guardEngine{guardNow, boundedVMRunner{c.Target.Executable, c.Target.SHA256}}
	g, e := acquireGuard(c)
	if e != nil {
		return e
	}
	if action == "arm" {
		if e := (boundedVMRunner{c.Target.Executable, c.Target.SHA256}).preflight(); e != nil {
			g.close()
			return e
		}
		n, e := guardNow()
		if e != nil {
			g.close()
			return e
		}
		s, e := guardArm(g, c, n)
		g.close()
		if e != nil {
			return e
		}
		// Arm is durable and owner/time/identity configuration preparation completed.
		// No VM query is necessary to arm; future injector must verify ready epoch.
		if e = json.NewEncoder(w).Encode(map[string]any{"phase": s.Phase, "run": c.Run, "epoch": c.Epoch, "configHash": s.Binding, "hardTick": s.HardTick}); e != nil {
			return e
		}
	} else {
		s, e := g.load(c)
		if e != nil {
			g.close()
			return e
		}
		switch action {
		case "status":
			g.close()
			return json.NewEncoder(w).Encode(s)
		case "heartbeat":
			n, e := guardNow()
			if e == nil {
				e = s.permit(n)
			}
			owner, oe := guardProcess(c.Owner.PID, c.Owner.Boot)
			if e == nil && (s.Phase != "ARMED" || s.Emergency || n.Tick >= s.HardTick || n.Tick-s.HeartbeatTick >= c.HeartbeatMS*int64(time.Millisecond) || *seq <= s.HeartbeatSequence || owner != c.Owner || oe != nil) {
				e = errors.New("guard: stale/foreign/late heartbeat")
			}
			if e == nil {
				s.HeartbeatSequence = *seq
				s.HeartbeatTick = n.Tick
				s.LastTick = n.Tick
				e = g.save(s)
			}
			g.close()
			return e
		case "disarm":
			e = engine.step(context.Background(), g, s, true)
			g.close()
			return e
		}
		g.close()
	}
	// Separate CLI process: no signal relationship, goroutine or parent defer is
	// required. Restart reads the same durable epoch and original absolute limits.
	busySince := time.Time{}
	for {
		g, e := openGuard(c)
		if e == errGuardBusy {
			if busySince.IsZero() {
				busySince = time.Now()
			}
			if time.Since(busySince) > 6*time.Duration(c.TimeoutMS)*time.Millisecond+2*time.Second {
				return errGuardBusy
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if e != nil {
			return e
		}
		busySince = time.Time{}
		s, e := g.load(c)
		if e != nil {
			g.close()
			return e
		}
		if s.closed() {
			g.close()
			if s.Phase == "DISARMED" {
				return nil
			}
			return errors.New("guard: experiment aborted; " + s.Phase)
		}
		e = engine.step(context.Background(), g, s, false)
		g.close()
		if e != nil {
			return e
		}
		if s.closed() {
			return errors.New("guard: experiment aborted; " + s.Phase)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
