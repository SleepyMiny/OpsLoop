//go:build linux

package main

import (
	"errors"
	"time"
)

type GuardAssessment struct {
	Status       string   `json:"status"`
	Phase        string   `json:"phase,omitempty"`
	Reasons      []string `json:"reasons"`
	Emergency    bool     `json:"emergency"`
	ResumeIntent bool     `json:"resumeIntent"`
}

// assessGuard consumes bytes from the finalizer's snapshot. It cannot acquire a
// store, renew authorization, inspect a process, query a VM or emit Guard events.
func assessGuard(b []byte, c guardConfig) GuardAssessment {
	bad := func(reason string) GuardAssessment {
		return GuardAssessment{Status: "INVALID", Reasons: []string{reason}}
	}
	var s guardState
	if strictDecode(b, &s, true) != nil {
		return bad("GUARD_MALFORMED")
	}
	if _, e := validateGuardRecord(c, &s); e != nil {
		return bad("GUARD_BINDING_OR_INVARIANTS_INVALID")
	}
	if s.ArmWall.IsZero() || s.ArmTick < 0 || !s.Events[0].At.Equal(s.ArmWall) {
		return bad("GUARD_ARM_EVENT_INVALID")
	}
	for _, event := range s.Events {
		_, offset := event.At.Zone()
		if event.At.IsZero() || offset != 0 {
			return bad("GUARD_EVENT_TIME_INVALID")
		}
	}
	a := GuardAssessment{Phase: s.Phase, Emergency: s.Emergency, ResumeIntent: s.ResumeIntent, Reasons: []string{}}
	if s.Emergency || s.ResumeIntent {
		a.Status = "EMERGENCY"
		a.Reasons = append(a.Reasons, "GUARD_EMERGENCY_EVIDENCE")
		if s.Phase == "EMERGENCY_RESTORE_CONFIRMED" {
			a.Reasons = append(a.Reasons, "GUARD_RUNNING_CONFIRMED_NOT_NORMAL_CLEANUP")
		}
		return a
	}
	switch s.Phase {
	case "ABORTED_MANUAL":
		a.Status = "MANUAL"
		a.Reasons = append(a.Reasons, "GUARD_ABORTED_MANUAL")
	case "ARMED", "RESTORING":
		a.Status = "INCOMPLETE"
		a.Reasons = append(a.Reasons, "GUARD_NOT_NORMALLY_CLOSED")
	case "DISARMED":
		if e := normalGuardHistory(s); e != nil {
			return bad(e.Error())
		}
		a.Status = "NORMAL_COMPLETION"
		a.Reasons = append(a.Reasons, "GUARD_HISTORICAL_NORMAL_CLOSE")
	default:
		return bad("GUARD_PHASE_INVALID")
	}
	return a
}

// A read-only query may leave any durable prefix when a save fails. Only a
// fresh domuuid intent can restart it; results never cross query attempts.
// Event sequence orders the record; wall At values are never sorted or clamped.
func normalGuardHistory(s guardState) error {
	bad := func() error { return errors.New("GUARD_NORMAL_HISTORY_INVALID") }
	if len(s.Events) < 10 || s.Events[0].Operation != "" || s.Events[len(s.Events)-1].Kind != "disarmed" || s.Events[len(s.Events)-1].Operation != "" || s.ResumeIntent || s.Emergency {
		return bad()
	}
	last := s.Events[len(s.Events)-1]
	if s.LastTick >= s.HardTick || s.LastTick >= s.ExpiryTick || s.LastTick-s.HeartbeatTick >= s.Config.HeartbeatMS*int64(time.Millisecond) || !last.At.Before(s.Config.AuthorityExpires) {
		return bad()
	}
	// Reuse the historical clock/authority rules with the recorded final tick.
	// Guard hard eligibility is monotonic; bounded wall skew may cross the wall
	// deadline while still satisfying that original contract.
	if e := s.permit(guardMoment{Wall: last.At, Boot: s.Boot, Tick: s.LastTick}); e != nil {
		return bad()
	}
	step := 0
	state := ""
	kinds := []string{"command-intent", "vm-start-attempt", "vm-started", "command-result"}
	for _, e := range s.Events[1 : len(s.Events)-1] {
		if e.Kind == "command-intent" && e.Operation == "domuuid" {
			step, state = 0, ""
		}
		if step >= 8 {
			return bad()
		}
		op := "domuuid"
		if step >= 4 {
			op = "domstate"
		}
		if e.Kind != kinds[step%4] || e.Operation != op {
			return bad()
		}
		if step%4 != 3 {
			if e.Detail != "" {
				return bad()
			}
		} else if op == "domuuid" {
			if e.Detail != s.Config.Target.UUID {
				return bad()
			}
		} else {
			if e.Detail != "paused" && e.Detail != "running" {
				return bad()
			}
			state = e.Detail
		}
		step++
	}
	if step != 8 || state != "running" {
		return bad()
	}
	return nil
}
