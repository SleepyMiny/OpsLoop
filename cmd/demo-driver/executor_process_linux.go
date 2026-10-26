//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Linux local fixture ownership: one fresh process group per command; direct
// children are Waited by os/exec, adopted descendants are reaped only by PGID.
// No system scan, detached-process sandbox, or unrelated-child Wait is used.
const finishIOGrace = 100 * time.Millisecond

var finishSubreaper struct {
	sync.Once
	err error
}

func finishOwnChildren() error {
	finishSubreaper.Do(func() {
		_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 36 /* PR_SET_CHILD_SUBREAPER */, 1, 0, 0, 0, 0)
		if errno != 0 {
			finishSubreaper.err = errno
		}
	})
	return finishSubreaper.err
}
func killFinishGroup(cmd *exec.Cmd) error {
	e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if e == syscall.ESRCH {
		return nil
	}
	return e
}
func configureFinishProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = finishIOGrace
	cmd.Cancel = func() error {
		e := killFinishGroup(cmd)
		if e == nil {
			return os.ErrProcessDone
		}
		return e
	}
}
func cleanupFinishGroup(cmd *exec.Cmd) error {
	if e := killFinishGroup(cmd); e != nil {
		return errors.New("fixture group termination failed")
	}
	until := time.Now().Add(finishIOGrace)
	for {
		var status syscall.WaitStatus
		pid, e := syscall.Wait4(-cmd.Process.Pid, &status, syscall.WNOHANG, nil)
		if e == syscall.ECHILD {
			return nil
		}
		if e != nil && e != syscall.EINTR {
			return errors.New("fixture group reap failed")
		}
		if pid > 0 {
			continue
		}
		if !time.Now().Before(until) {
			return errors.New("fixture group cleanup unconfirmed")
		}
		time.Sleep(time.Millisecond)
	}
}
