package controller

import (
	"path/filepath"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/SleepyMiny/OpsLoop/internal/testenv"
)

func startEnv(t *testing.T) *envtest.Environment {
	t.Helper()
	return testenv.Start(t, filepath.Join("..", "..", "config", "crd", "bases"))
}

func TestMain(m *testing.M) { testenv.Main(m) }
