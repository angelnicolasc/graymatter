package main

import (
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/httpauth"
)

// TestMCPHTTPAuth_RefusesAnonymousOnPublicAddr is the H-02 counterpart to the
// REST check: --http :8080 with no credential is how the audit reached
// memory_add from off-box.
func TestMCPHTTPAuth_RefusesAnonymousOnPublicAddr(t *testing.T) {
	for _, addr := range []string{":8080", "0.0.0.0:8080", "10.0.0.5:8080"} {
		cmd, _ := newAuthTestCmd()
		if _, err := resolveMCPHTTPAuth(cmd, addr, "", true); err == nil {
			t.Errorf("--no-auth on %s was accepted; it must be refused", addr)
		}
	}
}

func TestMCPHTTPAuth_AllowsAnonymousOnLoopback(t *testing.T) {
	cmd, buf := newAuthTestCmd()
	opts, err := resolveMCPHTTPAuth(cmd, "127.0.0.1:8080", "", true)
	if err != nil {
		t.Fatalf("--no-auth on loopback: %v", err)
	}
	if len(opts) != 1 {
		t.Errorf("got %d options, want 1", len(opts))
	}
	if !strings.Contains(buf.String(), "WARNING") {
		t.Errorf("--no-auth produced no warning: %q", buf.String())
	}
}

// TestMCPHTTPAuth_SharesTheTokenWithREST — one token file for both listeners,
// so a client configured for one already works with the other.
func TestMCPHTTPAuth_SharesTheTokenWithREST(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(httpauth.TokenEnv, "")
	restore := dataDir
	dataDir = dir
	t.Cleanup(func() { dataDir = restore })

	cmd, buf := newAuthTestCmd()
	if _, err := resolveMCPHTTPAuth(cmd, "127.0.0.1:8080", "", false); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	tok, created, err := httpauth.LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("read back token: %v", err)
	}
	if created {
		t.Error("the MCP command should have created the shared token already")
	}
	if !strings.Contains(buf.String(), tok) {
		t.Errorf("first run did not print the token: %q", buf.String())
	}
}

func TestMCPHTTPAuth_WarnsOnPublicAddr(t *testing.T) {
	cmd, buf := newAuthTestCmd()
	if _, err := resolveMCPHTTPAuth(cmd, "0.0.0.0:8080", "explicit-token", false); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(buf.String(), "WARNING") {
		t.Errorf("public bind produced no warning: %q", buf.String())
	}
}
