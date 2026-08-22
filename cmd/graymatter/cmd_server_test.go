package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/httpauth"
)

// newAuthTestCmd returns a bare command whose output is captured, plus the
// buffer. resolveServerAuth writes warnings and the generated token there.
func newAuthTestCmd() (*cobra.Command, *bytes.Buffer) {
	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "server"}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	return cmd, &buf
}

// TestServerAuth_DefaultAddrIsLoopback pins the H-01 default: ":8080" meant
// every interface, so a laptop on any shared network published the whole
// memory store.
func TestServerAuth_DefaultAddrIsLoopback(t *testing.T) {
	def := serverCmd().Flags().Lookup("addr").DefValue
	if !httpauth.IsLoopback(def) {
		t.Errorf("default --addr = %q, which is reachable from the network", def)
	}
}

// TestServerAuth_RefusesAnonymousOnPublicAddr is the combination that made
// H-01 critical: no credential, reachable from the LAN.
func TestServerAuth_RefusesAnonymousOnPublicAddr(t *testing.T) {
	for _, addr := range []string{":8080", "0.0.0.0:8080", "192.168.1.10:8080"} {
		cmd, _ := newAuthTestCmd()
		_, err := resolveServerAuth(cmd, addr, "", true)
		if err == nil {
			t.Errorf("--no-auth on %s was accepted; it must be refused", addr)
			continue
		}
		if !strings.Contains(err.Error(), addr) {
			t.Errorf("error for %s does not name the address: %v", addr, err)
		}
	}
}

// TestServerAuth_AllowsAnonymousOnLoopback keeps the migration path open for
// single-user local setups that scripted against the old behaviour.
func TestServerAuth_AllowsAnonymousOnLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		cmd, buf := newAuthTestCmd()
		opts, err := resolveServerAuth(cmd, addr, "", true)
		if err != nil {
			t.Errorf("--no-auth on %s: %v", addr, err)
			continue
		}
		if len(opts) != 1 {
			t.Errorf("--no-auth on %s: got %d options, want 1", addr, len(opts))
		}
		if !strings.Contains(buf.String(), "WARNING") {
			t.Errorf("--no-auth on %s produced no warning: %q", addr, buf.String())
		}
	}
}

// TestServerAuth_GeneratesAndReusesToken covers the first-run experience: the
// token is printed once, stored, and reused silently afterwards.
func TestServerAuth_GeneratesAndReusesToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(httpauth.TokenEnv, "")
	restore := dataDir
	dataDir = dir
	t.Cleanup(func() { dataDir = restore })

	cmd, buf := newAuthTestCmd()
	if _, err := resolveServerAuth(cmd, "127.0.0.1:8080", "", false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := buf.String()
	if !strings.Contains(first, "Generated API token") {
		t.Errorf("first run did not print the token: %q", first)
	}

	tok, _, err := httpauth.LoadOrCreateToken(dir)
	if err != nil {
		t.Fatalf("read back token: %v", err)
	}
	if !strings.Contains(first, tok) {
		t.Errorf("printed token does not match the stored one; printed: %q", first)
	}

	cmd2, buf2 := newAuthTestCmd()
	if _, err := resolveServerAuth(cmd2, "127.0.0.1:8080", "", false); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if strings.Contains(buf2.String(), tok) {
		t.Errorf("second run reprinted the credential: %q", buf2.String())
	}
}

// TestServerAuth_WarnsOnPublicAddrWithToken — authentication makes a public
// bind survivable, not advisable. The warning still fires.
func TestServerAuth_WarnsOnPublicAddrWithToken(t *testing.T) {
	cmd, buf := newAuthTestCmd()
	if _, err := resolveServerAuth(cmd, "0.0.0.0:8080", "explicit-token", false); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(buf.String(), "WARNING") {
		t.Errorf("public bind produced no warning: %q", buf.String())
	}

	cmd2, buf2 := newAuthTestCmd()
	if _, err := resolveServerAuth(cmd2, "127.0.0.1:8080", "explicit-token", false); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if strings.Contains(buf2.String(), "WARNING") {
		t.Errorf("loopback bind should not warn: %q", buf2.String())
	}
}

// TestServerAuth_ExplicitTokenSkipsTheTokenFile — --token and the environment
// variable exist for setups where writing next to the store is awkward.
func TestServerAuth_ExplicitTokenSkipsTheTokenFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(httpauth.TokenEnv, "")
	restore := dataDir
	dataDir = dir
	t.Cleanup(func() { dataDir = restore })

	cmd, _ := newAuthTestCmd()
	if _, err := resolveServerAuth(cmd, "127.0.0.1:8080", "explicit-token", false); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := os.Stat(httpauth.TokenFilePath(dir)); !os.IsNotExist(err) {
		t.Error("an explicit --token should not create the token file")
	}
}
