package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// writeBinary drops a stand-in executable and returns its path.
func writeBinary(t *testing.T, dir, name, content string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	return path
}

// writeManifest serialises m next to the binary and returns the manifest path.
func writeManifest(t *testing.T, dir string, m PluginManifest) string {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

// TestInstall_RequiresChecksum is the first half of H-06: nothing used to tie
// a manifest to the bytes it would run.
func TestInstall_RequiresChecksum(t *testing.T) {
	tests := []struct {
		name   string
		sha256 string
		want   string
	}{
		{"absent", "", "missing required field: sha256"},
		{"not hex", "zzzz", "not a 64-character hex digest"},
		{"too short", strings.Repeat("a", 63), "not a 64-character hex digest"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := writeBinary(t, dir, "p", "binary bytes")
			mp := writeManifest(t, dir, PluginManifest{
				Name: "p", Version: "1", Binary: bin, SHA256: tc.sha256,
			})

			err := Install(mp, filepath.Join(dir, "plugins"))
			if err == nil {
				t.Fatal("Install accepted a manifest with no usable checksum")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestInstall_RejectsChecksumMismatch is the MITM / swapped-binary case.
func TestInstall_RejectsChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")

	honest := writeBinary(t, dir, "honest", "the reviewed code")
	digest := mustHash(t, honest)

	// The manifest was published for the honest binary; the file on disk is
	// something else by the time it is installed.
	swapped := writeBinary(t, dir, "honest", "malware")
	mp := writeManifest(t, dir, PluginManifest{
		Name: "swapped", Version: "1", Binary: swapped, SHA256: digest,
	})

	err := Install(mp, pluginDir)
	if err == nil {
		t.Fatal("Install accepted a binary that does not match the manifest")
	}
	if !strings.Contains(err.Error(), "does not match the manifest") {
		t.Errorf("error = %v, want a checksum mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(pluginDir, "swapped")); !os.IsNotExist(statErr) {
		t.Error("a rejected install left a plugin directory behind")
	}
}

// TestInstall_RejectsPlaintextHTTP — a manifest names the executable that will
// run here, and http:// lets anyone on the path rewrite it in transit.
func TestInstall_RejectsPlaintextHTTP(t *testing.T) {
	dir := t.TempDir()
	bin := writeBinary(t, dir, "served", "served bytes")

	body, err := json.Marshal(PluginManifest{
		Name: "served", Version: "1", Binary: bin, SHA256: mustHash(t, bin),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	pluginDir := filepath.Join(dir, "plugins")
	url := srv.URL + "/manifest.json"

	err = Install(url, pluginDir)
	if err == nil {
		t.Fatal("Install fetched a manifest over plaintext http")
	}
	if !strings.Contains(err.Error(), "plaintext http") {
		t.Errorf("error = %v, want it to name the transport", err)
	}

	// --insecure is the documented escape hatch for local development.
	if err := Install(url, pluginDir, WithInsecureHTTP(), WithConfirm(func(PluginManifest) error { return nil })); err != nil {
		t.Fatalf("Install with WithInsecureHTTP: %v", err)
	}
}

// TestInstall_ConfinesBinaryToThePluginsDir is the other half of H-06: a
// manifest could name any executable anywhere on the machine, and that path is
// what got recorded and later run.
func TestInstall_ConfinesBinaryToThePluginsDir(t *testing.T) {
	base := t.TempDir()
	pluginDir := filepath.Join(base, "data", "plugins")

	// The "arbitrary executable elsewhere on the machine" case.
	outside := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bin := writeBinary(t, outside, "whatever", "arbitrary executable")

	mp := writeManifest(t, base, PluginManifest{
		Name: "confined", Version: "1", Binary: bin, SHA256: mustHash(t, bin),
		Tools: []MCPToolSpec{{Name: "confined_tool"}},
	})
	if err := Install(mp, pluginDir); err != nil {
		t.Fatalf("Install: %v", err)
	}

	installed, err := List(pluginDir)
	if err != nil || len(installed) != 1 {
		t.Fatalf("List = %v, %v", installed, err)
	}

	absPluginDir, err := filepath.Abs(pluginDir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	got := installed[0].Binary
	if !strings.HasPrefix(got, filepath.Join(absPluginDir, "confined")+string(os.PathSeparator)) {
		t.Errorf("recorded binary %q is not inside the plugin's own directory", got)
	}
	if got == bin {
		t.Error("the manifest still points at the external executable")
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("copied binary missing: %v", err)
	}

	// Deleting the original must not disarm the plugin: the copy is the thing.
	if err := os.Remove(bin); err != nil {
		t.Fatalf("remove original: %v", err)
	}
	if err := VerifyBinary(installed[0]); err != nil {
		t.Errorf("installed plugin stopped verifying once the source went away: %v", err)
	}
}

// TestInstall_ConfirmCallbackCanRefuse — installing a plugin is granting code
// execution, so it has to be refusable, and refusing has to leave nothing.
func TestInstall_ConfirmCallbackCanRefuse(t *testing.T) {
	base := t.TempDir()
	pluginDir := filepath.Join(base, "plugins")
	bin := writeBinary(t, base, "p", "bytes")
	mp := writeManifest(t, base, PluginManifest{
		Name: "declined", Version: "2.1", Binary: bin, SHA256: mustHash(t, bin),
		Tools: []MCPToolSpec{{Name: "t1"}, {Name: "t2"}},
	})

	declined := errors.New("user said no")
	var seen PluginManifest
	err := Install(mp, pluginDir, WithConfirm(func(m PluginManifest) error {
		seen = m
		return declined
	}))
	if !errors.Is(err, declined) {
		t.Fatalf("Install error = %v, want the reviewer's error", err)
	}

	// The reviewer must see enough to make the decision, including where the
	// binary will land.
	if seen.Name != "declined" || seen.Version != "2.1" || len(seen.Tools) != 2 {
		t.Errorf("reviewer saw an incomplete manifest: %+v", seen)
	}
	if seen.SHA256 != mustHash(t, bin) {
		t.Errorf("reviewer saw sha256 %q, want the verified digest", seen.SHA256)
	}
	if !strings.Contains(seen.Binary, filepath.Join("plugins", "declined", binSubdir)) {
		t.Errorf("reviewer saw binary %q, want the install destination", seen.Binary)
	}

	if _, err := os.Stat(filepath.Join(pluginDir, "declined")); !os.IsNotExist(err) {
		t.Error("a declined install left files behind")
	}
}

// TestCall_RefusesTamperedBinary — the digest is checked before exec, so an
// executable swapped after install does not get to run.
func TestCall_RefusesTamperedBinary(t *testing.T) {
	srcDir := t.TempDir()
	binPath := buildEchoPlugin(t, srcDir)

	manifest := PluginManifest{
		Name:   "echo",
		Binary: binPath,
		Tools:  []MCPToolSpec{{Name: "echo_hello"}},
		SHA256: mustHash(t, binPath),
	}

	// Sanity: it runs while the bytes match.
	if _, err := Call(context.Background(), manifest, "echo_hello", nil); err != nil {
		t.Fatalf("Call before tampering: %v", err)
	}

	if err := os.WriteFile(binPath, []byte("replaced"), 0o755); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, err := Call(context.Background(), manifest, "echo_hello", nil)
	if err == nil {
		t.Fatal("Call ran a binary that no longer matches its manifest")
	}
	if !strings.Contains(err.Error(), "refusing to run") {
		t.Errorf("error = %v, want a refusal", err)
	}
}

// TestCall_RefusesUnverifiableBinary covers plugins installed before the
// checksum requirement: refuse rather than trust, and say what to do.
func TestCall_RefusesUnverifiableBinary(t *testing.T) {
	dir := t.TempDir()
	bin := writeBinary(t, dir, "legacy", "bytes")

	_, err := Call(context.Background(), PluginManifest{Name: "legacy", Binary: bin}, "t", nil)
	if err == nil {
		t.Fatal("Call ran a plugin with no recorded digest")
	}
	if !strings.Contains(err.Error(), "reinstall") {
		t.Errorf("error = %v, want it to tell the user to reinstall", err)
	}
}

// TestInstall_NamesThePlaceholderDigest — the example manifest cannot ship a
// real digest, because it belongs to a binary the reader compiles. Reporting
// that as an ordinary mismatch against 64 zeros reads like a broken example,
// so the error names the placeholder and hands over the value to paste.
func TestInstall_NamesThePlaceholderDigest(t *testing.T) {
	dir := t.TempDir()
	bin := writeBinary(t, dir, "hello", "compiled bytes")
	real := mustHash(t, bin)

	mp := writeManifest(t, dir, PluginManifest{
		Name: "hello", Version: "1.0.0", Binary: bin, SHA256: placeholderSHA256,
	})

	err := Install(mp, filepath.Join(dir, "plugins"))
	if err == nil {
		t.Fatal("Install accepted the placeholder digest")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("error = %v, want it to name the placeholder", err)
	}
	if !strings.Contains(err.Error(), real) {
		t.Errorf("error = %v, want it to carry the real digest %s to paste", err, real)
	}
}

// TestExampleManifest_MatchesThePlaceholder keeps the shipped example and the
// constant from drifting apart; if they do, the helpful error stops firing and
// the reader is back to a confusing mismatch.
func TestExampleManifest_MatchesThePlaceholder(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "examples", "plugin-hello", "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("example manifest not readable from here: %v", err)
	}
	var m PluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("the shipped example manifest is not valid JSON: %v", err)
	}
	if m.SHA256 != placeholderSHA256 {
		t.Errorf("example manifest sha256 = %q, want the placeholder constant", m.SHA256)
	}
	if !sha256Re.MatchString(m.SHA256) {
		t.Errorf("example manifest sha256 %q would be rejected by the format check", m.SHA256)
	}
	if m.Name == "" || m.Binary == "" {
		t.Errorf("example manifest is missing a required field: %+v", m)
	}
}

// TestDocExampleDigest_IsWellFormed guards docs/plugin-protocol.md, whose
// sample digest was an ellipsis the validator would have rejected.
func TestDocExampleDigest_IsWellFormed(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "docs", "plugin-protocol.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("plugin protocol doc not readable from here: %v", err)
	}

	re := regexp.MustCompile(`"sha256":\s*"([^"]*)"`)
	matches := re.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		t.Fatal("the plugin protocol doc no longer shows a sha256 example")
	}
	for _, m := range matches {
		if !sha256Re.MatchString(m[1]) {
			t.Errorf("doc shows sha256 %q, which the validator would reject", m[1])
		}
	}
}
