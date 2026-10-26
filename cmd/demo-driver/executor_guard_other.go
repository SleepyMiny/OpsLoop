//go:build !linux

package main

import (
	"context"
	"errors"
)

func authorizeFinishLive(c FinishConfig) error {
	if c.Live != nil {
		return errors.New("finish: lab-libvirt requires Linux")
	}
	return nil
}
func finishLiveInputHashes(FinishConfig, string) (finishLiveInputs, error) {
	return finishLiveInputs{}, errors.New("finish: Linux required")
}
func (x *finisher) restoreLab(context.Context) (string, error) {
	return "RESTORE_UNCONFIRMED", errors.New("finish: Linux required")
}
func (x *finisher) checkLiveGuard(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if x.c.Live != nil {
		return errors.New("finish: Linux required")
	}
	return nil
}
