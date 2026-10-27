package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Shared P2-D command boundary. The fixture alias preserves the normal finish path.
type fixtureVMRunner = boundedVMRunner
type boundedVMRunner struct{ path, sha string }
type finishLimitedOutput struct{ b []byte }

func (b *finishLimitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.b)+n > 4096 {
		return 0, errors.New("fixture output limit")
	}
	b.b = append(b.b, p...)
	return n, nil
}

// Run is a convenience for direct runner probes; execution uses RunStarted.
func (r boundedVMRunner) Run(ctx context.Context, args []string) (string, error) {
	return r.RunStarted(ctx, args, func(string) error { return nil })
}

// boundary("vm-start-attempt") precedes Start, boundary("vm-started") follows
// successful Start. The latter is durable before waiting for the command.
func (r boundedVMRunner) RunStarted(ctx context.Context, args []string, boundary func(string) error) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if e := r.preflight(); e != nil {
		return "", e
	}
	var e error
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if e = finishOwnChildren(); e != nil {
		return "", errors.New("fixture process ownership unavailable")
	}
	// No shell or inherited environment, credentials, proxy, HOME or libvirt URI.
	cmd := exec.CommandContext(ctx, r.path, args...)
	cmd.Env = []string{"LANG=C", "LC_ALL=C"}
	cmd.Stdin = nil
	var out finishLimitedOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	configureFinishProcess(cmd)
	if e = boundary("vm-start-attempt"); e != nil {
		return "", e
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if e = cmd.Start(); e != nil {
		return "", errors.New("fixture Start failed")
	}
	// Every successful Start reaches Wait, even if the durable started record fails.
	startedErr := boundary("vm-started")
	if startedErr != nil {
		_ = killFinishGroup(cmd)
	}
	e = cmd.Wait()
	cleanupErr := cleanupFinishGroup(cmd)
	if startedErr != nil {
		return "", startedErr
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if cleanupErr != nil {
		return "", cleanupErr
	}
	if e != nil {
		return "", errors.New("fixture command failed or timed out")
	}
	return strings.TrimSpace(string(out.b)), nil
}

// Preflight performs no execution or connection.
func (r boundedVMRunner) preflight() error {
	f, e := os.OpenFile(r.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return errors.New("fixture unavailable")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 || st.Size() > 16<<20 {
		return errors.New("fixture invalid")
	}
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil || hex.EncodeToString(h.Sum(nil)) != r.sha {
		return errors.New("fixture hash mismatch")
	}
	return nil
}
