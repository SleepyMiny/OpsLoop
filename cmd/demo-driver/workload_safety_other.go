//go:build !linux

package main

import (
	"errors"
	"io"
)

func runWorkloadGuard(_ []string, _ io.Writer) error {
	return errors.New("workload safety: Linux required")
}
func runWorkloadFinish(_ []string, _ io.Writer) error {
	return errors.New("workload safety: Linux required")
}
