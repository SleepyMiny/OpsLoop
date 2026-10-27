//go:build !linux

package main

import (
	"errors"
	"os/exec"
)

// This adapter's process ownership contract is currently tested on Linux only.
func finishOwnChildren() error               { return errors.New("fixture process ownership requires Linux") }
func configureFinishProcess(cmd *exec.Cmd)   {}
func killFinishGroup(cmd *exec.Cmd) error    { return cmd.Process.Kill() }
func cleanupFinishGroup(cmd *exec.Cmd) error { return nil }
