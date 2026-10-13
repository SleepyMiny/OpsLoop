//go:build !windows

package testenv

// killChildrenOnExit is a no-op off Windows, where envtest's Stop terminates its processes.
func killChildrenOnExit() error { return nil }
