package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wireInteractiveTest sets up testStdinReader and returns a cleanup function.
func wireInteractiveTest(input string) func() {
	testStdinReader = strings.NewReader(input)
	return func() { testStdinReader = nil }
}

func TestAskForAgents_ClaudeCodeOnly(t *testing.T) {
	defer wireInteractiveTest("1\n")()
	agents := knownAgents(".")
	got := askForAgents(agents)
	if len(got) != 1 || got[0] != "claudecode" {
		t.Fatalf("got %v, want [claudecode]", got)
	}
}

func TestAskForAgents_OpenCodeOnly(t *testing.T) {
	defer wireInteractiveTest("3\n")()
	agents := knownAgents(".")
	got := askForAgents(agents)
	if len(got) != 1 || got[0] != "opencode" {
		t.Fatalf("got %v, want [opencode]", got)
	}
}

func TestAskForAgents_MultipleCommaSeparated(t *testing.T) {
	defer wireInteractiveTest("1,3,5\n")()
	agents := knownAgents(".")
	got := askForAgents(agents)
	want := []string{"claudecode", "opencode", "antigravity"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestAskForAgents_MultipleSpaceSeparated(t *testing.T) {
	defer wireInteractiveTest("1 3 5\n")()
	agents := knownAgents(".")
	got := askForAgents(agents)
	if len(got) != 3 {
		t.Fatalf("got %v, want [claudecode opencode antigravity]", got)
	}
}

func TestAskForAgents_MixedSeparators(t *testing.T) {
	defer wireInteractiveTest("1, 3,5\n")()
	agents := knownAgents(".")
	got := askForAgents(agents)
	want := []string{"claudecode", "opencode", "antigravity"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestAskForAgents_None(t *testing.T) {
	defer wireInteractiveTest("0\n")()
	agents := knownAgents(".")
	got := askForAgents(agents)
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestAskForAgents_EmptyInput(t *testing.T) {
	defer wireInteractiveTest("\n")()
	agents := knownAgents(".")
	got := askForAgents(agents)
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestAskForAgents_Deduplicates(t *testing.T) {
	defer wireInteractiveTest("1,2,3,1,2\n")()
	agents := knownAgents(".")
	got := askForAgents(agents)
	want := []string{"claudecode", "cursor", "opencode"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAskForAgents_NoneOverridesOthers(t *testing.T) {
	defer wireInteractiveTest("1,2,0,3\n")()
	agents := knownAgents(".")
	got := askForAgents(agents)
	// "0" overrides all — returns empty
	if len(got) != 0 {
		t.Fatalf("got %v, want empty (0 overrides)", got)
	}
}

func TestAskForAgents_OutOfRangeIgnored(t *testing.T) {
	defer wireInteractiveTest("1,99,3\n")()
	agents := knownAgents(".")
	got := askForAgents(agents)
	want := []string{"claudecode", "opencode"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAskForAgents_NonNumericIgnored(t *testing.T) {
	defer wireInteractiveTest("1,x,3\n")()
	agents := knownAgents(".")
	got := askForAgents(agents)
	want := []string{"claudecode", "opencode"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// --- runInteractiveWizard integration tests ---

func TestInteractive_ClaudeCodeOnly(t *testing.T) {
	defer wireInteractiveTest("1\n")()
	tmpDir := t.TempDir()

	err := runInteractiveWizard(filepath.Join(tmpDir, ".graymatter"), tmpDir, true)
	if err != nil {
		t.Fatal(err)
	}

	// Config file created.
	if _, err := os.Stat(filepath.Join(tmpDir, ".mcp.json")); os.IsNotExist(err) {
		t.Error(".mcp.json should exist")
	}
	// Only CLAUDE.md gets the block, not AGENTS.md.
	claudeData, err := os.ReadFile(filepath.Join(tmpDir, "CLAUDE.md"))
	if err != nil {
		t.Fatal("CLAUDE.md should exist:", err)
	}
	if !strings.Contains(string(claudeData), "memory_search") {
		t.Error("CLAUDE.md should contain memory block")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "AGENTS.md")); !os.IsNotExist(err) {
		t.Error("AGENTS.md should NOT exist for Claude Code only")
	}
}

func TestInteractive_OpenCodeOnly(t *testing.T) {
	defer wireInteractiveTest("3\n")()
	tmpDir := t.TempDir()

	err := runInteractiveWizard(filepath.Join(tmpDir, ".graymatter"), tmpDir, true)
	if err != nil {
		t.Fatal(err)
	}

	// Config file created.
	if _, err := os.Stat(filepath.Join(tmpDir, "opencode.jsonc")); os.IsNotExist(err) {
		t.Error("opencode.jsonc should exist")
	}
	// AGENTS.md gets the block, not CLAUDE.md.
	agentsData, err := os.ReadFile(filepath.Join(tmpDir, "AGENTS.md"))
	if err != nil {
		t.Fatal("AGENTS.md should exist:", err)
	}
	if !strings.Contains(string(agentsData), "memory_search") {
		t.Error("AGENTS.md should contain memory block")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Error("CLAUDE.md should NOT exist for OpenCode only")
	}
}

func TestInteractive_AllAgents(t *testing.T) {
	defer wireInteractiveTest("1,2,3,4,5\n")()
	tmpDir := t.TempDir()
	testHomeOverride = tmpDir
	t.Cleanup(func() { testHomeOverride = "" })

	err := runInteractiveWizard(filepath.Join(tmpDir, ".graymatter"), tmpDir, true)
	if err != nil {
		t.Fatal(err)
	}

	// All config files created.
	for _, f := range []string{".mcp.json", filepath.Join(".cursor", "mcp.json"), "opencode.jsonc", "mcp_config.json"} {
		if _, err := os.Stat(filepath.Join(tmpDir, f)); os.IsNotExist(err) {
			t.Errorf("%s should exist", f)
		}
	}
	// Codex home config.
	codexCfg := filepath.Join(tmpDir, ".codex", "config.toml")
	if _, err := os.Stat(codexCfg); os.IsNotExist(err) {
		t.Error("~/.codex/config.toml should exist")
	}

	// Both instruction files created (Claude Code → CLAUDE.md, others → AGENTS.md).
	for _, f := range []string{"CLAUDE.md", "AGENTS.md"} {
		data, err := os.ReadFile(filepath.Join(tmpDir, f))
		if err != nil {
			t.Fatalf("%s should exist: %v", f, err)
		}
		if !strings.Contains(string(data), "memory_search") {
			t.Errorf("%s should contain memory block", f)
		}
	}
}

func TestInteractive_NoneSelected(t *testing.T) {
	defer wireInteractiveTest("0\n")()
	tmpDir := t.TempDir()

	err := runInteractiveWizard(filepath.Join(tmpDir, ".graymatter"), tmpDir, true)
	if err != nil {
		t.Fatal(err)
	}

	// Data dir exists.
	if _, err := os.Stat(filepath.Join(tmpDir, ".graymatter")); os.IsNotExist(err) {
		t.Error(".graymatter data dir should exist")
	}
	// No config files.
	for _, f := range []string{".mcp.json", "opencode.jsonc", "CLAUDE.md", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(tmpDir, f)); !os.IsNotExist(err) {
			t.Errorf("%s should NOT exist when none selected", f)
		}
	}
}

func TestInteractive_CursorAndOpenCodeShareAGENTSMd(t *testing.T) {
	// Cursor + OpenCode — both use AGENTS.md. Should write it once.
	defer wireInteractiveTest("2,3\n")()
	tmpDir := t.TempDir()

	err := runInteractiveWizard(filepath.Join(tmpDir, ".graymatter"), tmpDir, true)
	if err != nil {
		t.Fatal(err)
	}

	// Both configs present.
	if _, err := os.Stat(filepath.Join(tmpDir, ".cursor", "mcp.json")); os.IsNotExist(err) {
		t.Error(".cursor/mcp.json should exist")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "opencode.jsonc")); os.IsNotExist(err) {
		t.Error("opencode.jsonc should exist")
	}
	// AGENTS.md should have the block.
	data, err := os.ReadFile(filepath.Join(tmpDir, "AGENTS.md"))
	if err != nil {
		t.Fatal("AGENTS.md should exist:", err)
	}
	if !strings.Contains(string(data), "memory_search") {
		t.Error("AGENTS.md should contain memory block")
	}
	// Only one AGENTS.md block (check marker appears exactly twice: begin + end).
	marker := "<!-- graymatter:instructions:begin"
	if strings.Count(string(data), marker) != 1 {
		t.Errorf("expected exactly one instruction block, got %d", strings.Count(string(data), marker))
	}
	// CLAUDE.md should not exist.
	if _, err := os.Stat(filepath.Join(tmpDir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Error("CLAUDE.md should NOT exist")
	}
}

func TestInteractive_DataDirCreated(t *testing.T) {
	defer wireInteractiveTest("1\n")()
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "custom-dir")

	err := runInteractiveWizard(dataDir, tmpDir, true)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		t.Error("custom data dir should exist")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "MEMORY.md")); os.IsNotExist(err) {
		t.Error("MEMORY.md should exist in data dir")
	}
}

func TestInteractive_IdempotentRun(t *testing.T) {
	defer wireInteractiveTest("1\n")()
	tmpDir := t.TempDir()

	err := runInteractiveWizard(filepath.Join(tmpDir, ".graymatter"), tmpDir, true)
	if err != nil {
		t.Fatal(err)
	}

	// Second run — same input, should not error.
	testStdinReader = strings.NewReader("1\n")
	defer func() { testStdinReader = nil }()

	err = runInteractiveWizard(filepath.Join(tmpDir, ".graymatter"), tmpDir, true)
	if err != nil {
		t.Fatal("second run should succeed:", err)
	}
}

func TestInteractive_InstructionFilePreservesUserContent(t *testing.T) {
	defer wireInteractiveTest("3\n")()
	tmpDir := t.TempDir()

	// Pre-create AGENTS.md with user content (no markers).
	userContent := "# My Project\n\nThis is my AGENTS.md.\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "AGENTS.md"), []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runInteractiveWizard(filepath.Join(tmpDir, ".graymatter"), tmpDir, true)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.HasPrefix(content, userContent) {
		t.Error("user content not preserved at top")
	}
	if !strings.Contains(content, "memory_search") {
		t.Error("memory block not appended")
	}
}

func TestInteractive_EmptyInputProducesNoFiles(t *testing.T) {
	defer wireInteractiveTest("\n")()
	tmpDir := t.TempDir()

	err := runInteractiveWizard(filepath.Join(tmpDir, ".graymatter"), tmpDir, true)
	if err != nil {
		t.Fatal(err)
	}

	// Only data dir.
	if _, err := os.Stat(filepath.Join(tmpDir, ".graymatter")); os.IsNotExist(err) {
		t.Error("data dir should exist")
	}
	for _, f := range []string{".mcp.json", "opencode.jsonc", "CLAUDE.md", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(tmpDir, f)); !os.IsNotExist(err) {
			t.Errorf("%s should NOT exist on empty input", f)
		}
	}
}
