package controller

import (
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestHeartbeatPythonTransport exercises the host's real kubectl/JSON Patch path
// against disposable loopback envtest, never the operational kubeconfig.
func TestHeartbeatPythonTransport(t *testing.T) {
	e := startEnv(t)
	cfg := e.Config
	c := clientcmdapi.NewConfig()
	c.Clusters["local"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	c.AuthInfos["local"] = &clientcmdapi.AuthInfo{ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}
	c.Contexts["local"] = &clientcmdapi.Context{Cluster: "local", AuthInfo: "local"}
	c.CurrentContext = "local"
	data, err := clientcmd.Write(*c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "local-kubeconfig")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-m", "tests.heartbeat_envtest", path, filepath.Join(os.Getenv("KUBEBUILDER_ASSETS"), "kubectl"))
	cmd.Dir = filepath.Join("..", "..")
	// No production context or credentials are inherited by the Python test.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "LANG=C", "LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("local heartbeat transport: %v\n%s", err, out)
	}
	t.Log(string(out))
}
