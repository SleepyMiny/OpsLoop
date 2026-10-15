// Package testenv shares one envtest kube-apiserver per test binary.
package testenv

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	once sync.Once
	env  *envtest.Environment
	err  error
)

// Start returns the shared environment with the CRDs in crdDir installed.
// It skips the test when KUBEBUILDER_ASSETS is unset, unless OPSLOOP_REQUIRE_ENVTEST=1.
func Start(t *testing.T, crdDir string) *envtest.Environment {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("OPSLOOP_REQUIRE_ENVTEST") == "1" {
			t.Fatal("KUBEBUILDER_ASSETS is not set but OPSLOOP_REQUIRE_ENVTEST=1")
		}
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}
	once.Do(func() {
		if err = killChildrenOnExit(); err != nil {
			return
		}
		env = &envtest.Environment{CRDDirectoryPaths: []string{crdDir}, ErrorIfCRDPathMissing: true}
		_, err = env.Start()
	})
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	return env
}

// Main runs the package tests and stops the shared environment, even after a failed Start.
func Main(m *testing.M) {
	code := m.Run()
	if env != nil {
		// Windows cannot SIGTERM envtest's processes; killChildrenOnExit ends them when this binary exits.
		if err := env.Stop(); err != nil && !(runtime.GOOS == "windows" && strings.Contains(err.Error(), "not supported by windows")) {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}
