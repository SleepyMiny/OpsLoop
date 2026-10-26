//go:build !linux

package main

import (
	"errors"
	"io"
)

func runFinalize(_ []string, _ io.Writer) error {
	return errors.New("finalize-run: Linux Guard record format required")
}
