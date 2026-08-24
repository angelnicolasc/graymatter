package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// writeResult is what every MCP writer returns.
//
//   - path: the config file the writer is responsible for (shown to the user).
//   - changed: true if the writer actually modified the file on disk.
//   - skipped: true if the writer deliberately did nothing (e.g. the user
//     passed --skip-codex, or an opt-in client wasn't requested). A skipped
//     writer never sets changed=true.
//   - warn: non-fatal warning to show the user (e.g. "opencode.jsonc has
//     comments, paste this snippet manually"). Writers return warnings
//     instead of errors when the right thing to do is keep going.
type writeResult struct {
	path    string
	changed bool
	skipped bool
	warn    string
}

// mcpEntry is the canonical GrayMatter MCP server block used by every
// JSON-schema client (Claude Code, Cursor, Antigravity). Codex / OpenCode
// have slightly different schemas and build their own.
var mcpEntry = map[string]any{
	"command": "graymatter",
	"args":    []any{"mcp", "serve"},
}

// --- JSON-family writers (Claude Code, Cursor, Antigravity) ------------------

// mergeJSONMCPServers implements the common pattern shared by Claude Code,
// Cursor, and Antigravity: read a JSON file, upsert graymatter under a
// top-level map key (topKey, typically "mcpServers"), write it back.
//
// If the file does not exist, it's created with just our entry.
// If the file exists but is not a JSON object, we leave it untouched and
// return a warning — we never clobber user data.
// If the file exists and graymatter is already present with equivalent
// values, we no-op (changed=false).
func mergeJSONMCPServers(path string, topKey string, entry map[string]any) (writeResult, error) {
	res := writeResult{path: path}

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("read %s: %w", path, err)
	}

	var root map[string]any
	if len(data) == 0 {
		root = map[string]any{}
	} else {
		if err := json.Unmarshal(data, &root); err != nil {
			res.warn = fmt.Sprintf("%s exists but is not valid JSON; skipped (add graymatter manually)", path)
			return res, nil
		}
	}

	servers, _ := root[topKey].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}

	existing, _ := servers["graymatter"].(map[string]any)
	if jsonEqual(existing, entry) {
		return res, nil // already wired
	}

	servers["graymatter"] = entry
	root[topKey] = servers

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return res, fmt.Errorf("marshal %s: %w", path, err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return res, fmt.Errorf("write %s: %w", path, err)
	}

	res.changed = true
	return res, nil
}

func jsonEqual(a, b map[string]any) bool {
	if a == nil || b == nil {
		return false
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func writeClaudeCodeProject(projectDir string) (writeResult, error) {
	return mergeJSONMCPServers(filepath.Join(projectDir, ".mcp.json"), "mcpServers", mcpEntry)
}

func writeCursorProject(projectDir string) (writeResult, error) {
	return mergeJSONMCPServers(filepath.Join(projectDir, ".cursor", "mcp.json"), "mcpServers", mcpEntry)
}

func writeAntigravityProject(projectDir string) (writeResult, error) {
	return mergeJSONMCPServers(filepath.Join(projectDir, "mcp_config.json"), "mcpServers", mcpEntry)
}

func writeWindsurfProject(projectDir string) (writeResult, error) {
	return mergeJSONMCPServers(filepath.Join(projectDir, ".windsurf", "mcp.json"), "mcpServers", mcpEntry)
}

func writeVSCodeCopilotProject(projectDir string) (writeResult, error) {
	return mergeJSONMCPServers(filepath.Join(projectDir, ".vscode", "mcp.json"), "servers", mcpEntry)
}

// --- Codex (TOML, home-scoped) ----------------------------------------------

// codexConfigPath resolves ~/.codex/config.toml honoring USERPROFILE on
// Windows and HOME elsewhere — and letting tests override via either.
func codexConfigPath() (string, error) {
	home, err := resolveHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// writeCodexHome upserts [mcp_servers.graymatter] in ~/.codex/config.toml,
// preserving any unrelated keys the user already has.
func writeCodexHome() (writeResult, error) {
	path, err := codexConfigPath()
	if err != nil {
		return writeResult{}, err
	}
	res := writeResult{path: path}

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("read %s: %w", path, err)
	}

	root := map[string]any{}
	if len(data) > 0 {
		if err := toml.Unmarshal(data, &root); err != nil {
			res.warn = fmt.Sprintf("%s exists but is not valid TOML; skipped (add graymatter manually)", path)
			return res, nil
		}
	}

	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}

	want := map[string]any{
		"command": "graymatter",
		"args":    []any{"mcp", "serve"},
	}

	existing, _ := servers["graymatter"].(map[string]any)
	if tomlServerEquiv(existing, want) {
		return res, nil
	}

	servers["graymatter"] = want
	root["mcp_servers"] = servers

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(root); err != nil {
		return res, fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return res, fmt.Errorf("write %s: %w", path, err)
	}

	res.changed = true
	return res, nil
}

// tomlServerEquiv tolerates []any vs []string for args (depending on which
// side built the map), which is why we can't just reuse jsonEqual.
func tomlServerEquiv(got, want map[string]any) bool {
	if got == nil {
		return false
	}
	if gs, _ := got["command"].(string); gs != want["command"].(string) {
		return false
	}
	wantArgs := want["args"].([]any)
	gotArgs := toAnySlice(got["args"])
	if len(gotArgs) != len(wantArgs) {
		return false
	}
	for i := range wantArgs {
		if fmt.Sprint(gotArgs[i]) != fmt.Sprint(wantArgs[i]) {
			return false
		}
	}
	return true
}

func toAnySlice(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	case []string:
		out := make([]any, len(s))
		for i, x := range s {
			out[i] = x
		}
		return out
	}
	return nil
}

// --- OpenCode (JSONC, project-scoped) ---------------------------------------

// OpenCode accepts JSONC: JSON with // line comments and trailing commas.
// We treat it as JSON: if the file parses clean, we merge under `mcp`; if it
// contains comments we bail with a warning plus the exact snippet for the
// user to paste. This avoids needing a full JSONC parser.
var jsoncCommentRe = regexp.MustCompile(`(?m)(^|\s)//.*$`)

func writeOpencodeProject(projectDir string) (writeResult, error) {
	path := filepath.Join(projectDir, "opencode.jsonc")
	res := writeResult{path: path}

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("read %s: %w", path, err)
	}

	entry := map[string]any{
		"type":    "local",
		"command": []any{"graymatter", "mcp", "serve"},
		"enabled": true,
	}

	if len(data) == 0 {
		root := map[string]any{
			"$schema": "https://opencode.ai/config.json",
			"mcp": map[string]any{
				"graymatter": entry,
			},
		}
		out, _ := json.MarshalIndent(root, "", "  ")
		out = append(out, '\n')
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return res, fmt.Errorf("write %s: %w", path, err)
		}
		res.changed = true
		return res, nil
	}

	if jsoncCommentRe.Match(data) {
		res.warn = fmt.Sprintf("%s uses JSONC comments; paste this under \"mcp\":\n%s", path, opencodeSnippet())
		return res, nil
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		res.warn = fmt.Sprintf("%s exists but is not valid JSON; skipped (add graymatter manually)", path)
		return res, nil
	}

	mcp, _ := root["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	if existing, _ := mcp["graymatter"].(map[string]any); jsonEqual(existing, entry) {
		return res, nil
	}
	mcp["graymatter"] = entry
	root["mcp"] = mcp

	out, _ := json.MarshalIndent(root, "", "  ")
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return res, fmt.Errorf("write %s: %w", path, err)
	}
	res.changed = true
	return res, nil
}

func opencodeSnippet() string {
	return strings.TrimSpace(`
  "mcp": {
    "graymatter": {
      "type": "local",
      "command": ["graymatter", "mcp", "serve"],
      "enabled": true
    }
  }
`)
}

// --- Test hook --------------------------------------------------------------

// Injected by tests to pin the home directory when resolving ~/.codex.
// Nil in production.
var testHomeOverride string

func resolveHome() (string, error) {
	if testHomeOverride != "" {
		return testHomeOverride, nil
	}
	if runtime.GOOS == "windows" {
		if h := os.Getenv("USERPROFILE"); h != "" {
			return h, nil
		}
	}
	return os.UserHomeDir()
}
