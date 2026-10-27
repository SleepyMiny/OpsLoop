package main

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"runtime"
	"time"
)

// This backend generates the production argv even when its command boundary is
// injected. It has no names, discovery, default connection, shell or generic op.
type recoveryTarget struct {
	URI        string `json:"uri"`
	UUID       string `json:"uuid"`
	Executable string `json:"executable"`
	SHA256     string `json:"sha256"`
}

var recoveryHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (t recoveryTarget) validate() error {
	if runtime.GOOS != "linux" || (t.URI != "qemu:///system" && t.URI != "qemu:///session") || !finishUUID.MatchString(t.UUID) || !filepath.IsAbs(t.Executable) || !recoveryHash.MatchString(t.SHA256) {
		return errors.New("guard: explicit Linux local qemu URI, UUID and pinned absolute executable required")
	}
	return nil
}

type recoveryEvent struct {
	Sequence  int       `json:"sequence"`
	Actor     string    `json:"actor"`
	Kind      string    `json:"kind"`
	Operation string    `json:"operation,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	At        time.Time `json:"at"`
}
type recoveryBackend struct {
	target  recoveryTarget
	runner  finishVMRunner
	timeout time.Duration
	// permit is checked again immediately before each Start, including resume.
	permit func() error
	record func(kind, op, detail string) error
}

func (b *recoveryBackend) call(ctx context.Context, op string) (string, error) {
	if err := b.target.validate(); err != nil {
		return "", err
	}
	if op != "domuuid" && op != "domstate" && op != "resume" {
		return "", errors.New("guard: operation blocked")
	}
	if b.permit == nil || b.record == nil || b.runner == nil || b.timeout <= 0 {
		return "", errors.New("guard: incomplete command boundary")
	}
	if err := b.permit(); err != nil {
		return "", err
	}
	if err := b.record("command-intent", op, ""); err != nil {
		return "", err
	}
	call, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	out, err := b.runner.RunStarted(call, []string{"--connect", b.target.URI, op, b.target.UUID}, func(kind string) error {
		if kind == "vm-start-attempt" {
			if e := b.permit(); e != nil {
				return e
			}
		}
		return b.record(kind, op, "")
	})
	detail := "unconfirmed"
	if err == nil {
		switch {
		case op == "resume":
			detail = "exit-zero-not-confirmation"
		case op == "domuuid" && out == b.target.UUID:
			detail = out
		case op == "domstate" && (out == "running" || out == "paused" || out == "shut off"):
			detail = out
		}
	}
	if e := b.record("command-result", op, detail); e != nil {
		return "", e
	}
	return out, err
}
func (b *recoveryBackend) state(ctx context.Context) (string, error) {
	id, e := b.call(ctx, "domuuid")
	if e != nil || id != b.target.UUID {
		return "", errors.New("guard: identity unconfirmed; manual review")
	}
	s, e := b.call(ctx, "domstate")
	if e != nil || (s != "running" && s != "paused") {
		return "", errors.New("guard: state unconfirmed; manual review")
	}
	return s, nil
}
func (b *recoveryBackend) resume(ctx context.Context) error { _, e := b.call(ctx, "resume"); return e }
