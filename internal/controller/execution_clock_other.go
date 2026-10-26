//go:build !linux

package controller

import (
	"errors"
	"time"
)

func executionBootTick() (time.Duration, error) {
	return 0, errors.New("execution authority requires Linux boot clock")
}
