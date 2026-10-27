package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Run main in a subprocess with the real dependency init functions. In-process
// Service tests cannot catch collisions with controller-runtime global flags.
func TestBFFCLIHelperProcess(t *testing.T) {
	if os.Getenv("OPSLOOP_BFF_CLI_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestBFFCLIHelpAndMissingConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
		code int
	}{
		{"help", []string{"--help"}, "-kubeconfig string", 0},
		{"missing-config", []string{"--kubeconfig", t.TempDir() + "/missing"}, "Kubernetes configuration unavailable (credential details omitted)", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-test.run=^TestBFFCLIHelperProcess$", "--"}, tc.args...)
			cmd := exec.Command(os.Args[0], args...)
			cmd.Env = append(os.Environ(), "OPSLOOP_BFF_CLI_HELPER=1")
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					code = e.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code || !strings.Contains(string(out), tc.want) || strings.Contains(string(out), "panic:") {
				t.Fatalf("exit=%d output=%s", code, out)
			}
		})
	}
}
