//go:build linux

package controller

import (
	"golang.org/x/sys/unix"
	"time"
)

func executionBootTick() (time.Duration, error) {
	var t unix.Timespec
	err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &t)
	return time.Duration(t.Nano()), err
}
