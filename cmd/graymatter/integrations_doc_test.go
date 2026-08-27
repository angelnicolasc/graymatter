package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// docs/integrations.md is user-facing setup documentation: a snippet that
// does not parse is a broken first run for whoever copies it. These tests
// make every fenced snippet machine-checked — JSON blocks parse as JSON,
// TOML blocks parse as TOML, YAML blocks pass a structural sanity check (no
// YAML parser dependency in this module) — and pin the page's coverage
// promises: every auto-wired client listed, 20+ clients documented.

const integrationsRelPath = "../../docs/integrations.md"

type fencedBlock struct {
	lang string
	body string
}

func integrationsBlocks(t *testing.T) []fencedBlock {
	t.Helper()
	data, err := os.ReadFile(integrationsRelPath)
	if err != nil {
		t.Fatalf("read %s: %v", integrationsRelPath, err)
	}
	var blocks []fencedBlock
	var cur *fencedBlock
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if cur == nil {
				cur = &fencedBlock{lang: strings.TrimPrefix(trimmed, "```")}
			} else {
				blocks = append(blocks, *cur)
				cur = nil
			}
			continue
		}
		if cur != nil {
			cur.body += line + "\n"
		}
	}
	if cur != nil {
		t.Fatal("unterminated fenced block in integrations.md")
	}
	return blocks
}

func TestIntegrationsDoc_JSONSnippetsParse(t *testing.T) {
	n := 0
	for _, b := range integrationsBlocks(t) {
		if b.lang != "json" {
			continue
		}
		n++
		var v any
		if err := json.Unmarshal([]byte(b.body), &v); err != nil {
			t.Errorf("JSON snippet does not parse: %v\n%s", err, b.body)
		}
	}
	if n < 6 {
		t.Errorf("only %d JSON snippets found; the verified configs must be present", n)
	}
}

func TestIntegrationsDoc_TOMLSnippetsParse(t *testing.T) {
	n := 0
	for _, b := range integrationsBlocks(t) {
		if b.lang != "toml" {
			continue
		}
		n++
		var v map[string]any
		if _, err := toml.Decode(b.body, &v); err != nil {
			t.Errorf("TOML snippet does not parse: %v\n%s", err, b.body)
		}
	}
	if n == 0 {
		t.Error("no TOML snippets found; the Codex config must be documented")
	}
}

func TestIntegrationsDoc_YAMLSnippetsAreStructured(t *testing.T) {
	// No YAML dependency here; the structural bar: every non-comment,
	// non-empty line either carries a `key:` mapping or is an indented
	// continuation. A pasted paragraph would fail this.
	n := 0
	for _, b := range integrationsBlocks(t) {
		if b.lang != "yaml" {
			continue
		}
		n++
		for i, line := range strings.Split(strings.TrimRight(b.body, "\n"), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if !strings.Contains(trimmed, ":") && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(trimmed, "- ") {
				t.Errorf("YAML snippet line %d is neither a mapping, a list item, nor indented: %q", i+1, line)
			}
		}
	}
	if n == 0 {
		t.Error("no YAML snippets found; Goose and Continue configs must be documented")
	}
}

func TestIntegrationsDoc_ClientCoverage(t *testing.T) {
	data, err := os.ReadFile(integrationsRelPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)

	// The playbook's bar: 20+ clients documented.
	clients := []string{
		"Claude Code", "Claude Desktop", "Cursor", "Codex", "OpenCode",
		"Antigravity", "Windsurf", "VS Code", "Gemini CLI", "Goose",
		"Zed", "Cline", "Roo Code", "Kilo Code", "Continue", "Junie",
		"Crush", "Amp", "Amazon Q", "Qwen Code", "JetBrains", "Trae",
		"Warp", "Pi",
	}
	missing := 0
	for _, c := range clients {
		if !strings.Contains(doc, c) {
			t.Errorf("client %q missing from docs/integrations.md", c)
			missing++
		}
	}
	if missing == 0 && len(clients) < 20 {
		t.Errorf("only %d clients asserted; the matrix promises 20+", len(clients))
	}

	// The auto-wired claim must match what init actually writes: the seven
	// writers in cmd_init_writers.go.
	autoWired := []string{".mcp.json", ".cursor/mcp.json", "config.toml", "opencode.jsonc", "mcp_config.json", ".windsurf/mcp.json", ".vscode/mcp.json"}
	for _, path := range autoWired {
		if !strings.Contains(doc, path) {
			t.Errorf("auto-wired config path %q missing from docs/integrations.md", path)
		}
	}

	// The documented stdio contract must match the real command surface.
	if !strings.Contains(doc, "mcp serve") {
		t.Error("integrations.md must document `graymatter mcp serve`")
	}
}
