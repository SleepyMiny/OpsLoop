package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	envOnce sync.Once
	testEnv *envtest.Environment
	envErr  error
)

// startEnv starts the shared envtest apiserver once; a copy of this lives in api/v1alpha1/crd_test.go.
func startEnv(t *testing.T) *envtest.Environment {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("OPSLOOP_REQUIRE_ENVTEST") == "1" {
			t.Fatal("KUBEBUILDER_ASSETS is not set but OPSLOOP_REQUIRE_ENVTEST=1")
		}
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}
	envOnce.Do(func() {
		testEnv = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
		}
		_, envErr = testEnv.Start()
	})
	if envErr != nil {
		t.Fatalf("start envtest: %v", envErr)
	}
	return testEnv
}

func TestMain(m *testing.M) {
	code := m.Run()
	// Stop even after a failed Start, otherwise kube-apiserver and etcd keep running.
	if testEnv != nil {
		if err := testEnv.Stop(); err != nil {
			// shortcut: Windows envtest Stop is unsupported (SIGTERM); the CI Linux result is authoritative.
			if !(runtime.GOOS == "windows" && strings.Contains(err.Error(), "not supported by windows")) {
				fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
				if code == 0 {
					code = 1
				}
			}
		}
	}
	os.Exit(code)
}
