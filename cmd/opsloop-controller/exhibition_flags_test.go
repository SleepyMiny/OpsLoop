package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExhibitionFlagBoundariesBeforeAnyClientCreation(t *testing.T) {
	for _, tc := range []struct{ name, scope, resource, hash, exhibition, want string }{
		{"scope-conflict", "legacy", "", "", "static", "cannot be combined"},
		{"resource-conflict", "", "conditions", "sha", "static", "cannot be combined"},
		{"missing-file", "", "", "", "/nonexistent/opsloop-exhibition-static", "cannot open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run("http://unused-local-fixture", false, tc.scope, tc.resource, tc.hash, tc.exhibition)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "static.json")
	if err := os.WriteFile(path, []byte(`{"schema":"opsloop-exhibition-authority/v1","publicKey":"not-a-key","targets":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run("http://unused-local-fixture", false, "", "", "", path); err == nil || !strings.Contains(err.Error(), "trust configuration") {
		t.Fatalf("invalid trust root accepted: %v", err)
	}
}
