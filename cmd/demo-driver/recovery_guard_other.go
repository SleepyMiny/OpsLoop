//go:build !linux

package main

import (
	"errors"
	"io"
)

func runGuard(_ []string, _ io.Writer) error { return errors.New("guard: Linux required") }
