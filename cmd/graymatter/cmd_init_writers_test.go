package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestWriteClaudeCode_CreatesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	res, err := writeClaudeCodeProject(dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !res.changed {
		t.Fatalf("expected changed=true on first write")
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("invalid JSON written: %v", err)
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if _, ok := servers["graymatter"]; !ok {
		t.Fatalf("graymatter entry missing; got %v", root)
	}
}

func TestWriteClaudeCode_MergesWhenPresent(t *testing.T) {
	dir := t.TempDir()
	pre := `{"mcpServers":{"other":{"command":"other","args":["x"]}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeClaudeCodeProject(dir); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	var root map[string]any
	_ = json.Unmarshal(data, &root)
	servers, _ := root["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatalf("pre-existing 'other' server was clobbered; got %v", servers)
	}
	if _, ok := servers["graymatter"]; !ok {
		t.Fatalf("graymatter not inserted; got %v", servers)
	}
}

func TestWriteClaudeCode_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeClaudeCodeProject(dir); err != nil {
		t.Fatal(err)
	}
	res, err := writeClaudeCodeProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.changed {
		t.Fatalf("expected changed=false on second run, got true")
	}
}

func TestWriteCodex_MergesTOML(t *testing.T) {
	home := t.TempDir()
	testHomeOverride = home
	t.Cleanup(func() { testHomeOverride = "" })

	pre := "[mcp_servers.bar]\ncommand = \"bar\"\nargs = [\"y\"]\n\n[model]\nname = \"o4-mini\"\n"
	codexDir := filepath.Join(home, ".codex")
	_ = os.MkdirAll(codexDir, 0o755)
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeCodexHome(); err != nil {
		t.Fatalf("unexpected: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(codexDir, "config.toml"))
	var root map[string]any
	if err := toml.Unmarshal(data, &root); err != nil {
		t.Fatalf("invalid TOML: %v\n%s", err, data)
	}
	servers, _ := root["mcp_servers"].(map[string]any)
	if _, ok := servers["bar"]; !ok {
		t.Fatalf("pre-existing 'bar' lost: %v", servers)
	}
	if _, ok := servers["graymatter"]; !ok {
		t.Fatalf("graymatter not inserted: %v", servers)
	}
	model, _ := root["model"].(map[string]any)
	if got, _ := model["name"].(string); got != "o4-mini" {
		t.Fatalf("unrelated [model] key lost: %v", root)
	}

	// Second run is a no-op.
	res, err := writeCodexHome()
	if err != nil {
		t.Fatal(err)
	}
	if res.changed {
		t.Fatalf("expected idempotent second run, got changed=true")
	}
}

func TestWriteOpencode_JSONCWithCommentsFailsSoft(t *testing.T) {
	dir := t.TempDir()
	pre := "// user comments here\n{\n  \"model\": \"claude-sonnet\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "opencode.jsonc"), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := writeOpencodeProject(dir)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if res.warn == "" {
		t.Fatalf("expected warning for JSONC-with-comments input")
	}
	if res.changed {
		t.Fatalf("expected changed=false when bailing on JSONC")
	}
	// Make sure the original file was not modified.
	got, _ := os.ReadFile(filepath.Join(dir, "opencode.jsonc"))
	if string(got) != pre {
		t.Fatalf("file was modified despite warning:\n%s", got)
	}
}

func TestWriteOpencode_CreatesCleanJSON(t *testing.T) {
	dir := t.TempDir()
	res, err := writeOpencodeProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.changed {
		t.Fatalf("expected changed=true on fresh create")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "opencode.jsonc"))
	if !strings.Contains(string(data), `"graymatter"`) {
		t.Fatalf("graymatter missing:\n%s", data)
	}
	// Validate it parses as JSON (subset of JSONC).
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("written file is not valid JSON: %v\n%s", err, data)
	}
}

func TestWriteAntigravity_MergesJSON(t *testing.T) {
	dir := t.TempDir()
	res, err := writeAntigravityProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !res.changed {
		t.Fatalf("expected changed=true")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "mcp_config.json"))
	if !strings.Contains(string(data), `"graymatter"`) {
		t.Fatalf("graymatter missing:\n%s", data)
	}
}

func TestParseOnlyFlag(t *testing.T) {
	agents := knownAgents(".")

	got, err := parseOnlyFlag(" Claudecode, Cursor ,codex", agents)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"claudecode": true, "cursor": true, "codex": true}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("missing key %q in %v", k, got)
		}
	}

	empty, err := parseOnlyFlag("", agents)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if empty != nil {
		t.Fatalf("empty string should return nil")
	}

	// An unrecognised id must fail loudly. Now that the instruction files
	// follow the selection, silently ignoring it would write nothing at all
	// and still exit 0.
	if _, err := parseOnlyFlag("opencode,bogus", agents); err == nil {
		t.Fatal("unknown agent id should be an error")
	}
}
