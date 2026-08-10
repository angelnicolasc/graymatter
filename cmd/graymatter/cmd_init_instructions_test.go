package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpsertInstructions_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")

	res, err := upsertInstructionsBlock(path)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !res.changed {
		t.Error("expected changed=true on first write")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, want := range []string{instrBeginMarker, instrEndMarker, "memory_search", "memory_reflect", "`agent`, not `agent_id`"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("created file missing %q", want)
		}
	}
}

func TestUpsertInstructions_AppendsPreservingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	const userContent = "# My project\n\nDo not break userspace.\n"
	if err := os.WriteFile(path, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := upsertInstructionsBlock(path)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !res.changed {
		t.Error("expected changed=true when appending")
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	if !strings.HasPrefix(content, "# My project") {
		t.Error("user content was not preserved at the top")
	}
	if !strings.Contains(content, "Do not break userspace.") {
		t.Error("user content line lost")
	}
	if !strings.Contains(content, instrBeginMarker) {
		t.Error("block not appended")
	}
}

func TestUpsertInstructions_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")

	if _, err := upsertInstructionsBlock(path); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first, _ := os.ReadFile(path)

	res, err := upsertInstructionsBlock(path)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if res.changed {
		t.Error("second upsert should be a no-op (changed=false)")
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Error("second upsert altered the file")
	}
}

func TestUpsertInstructions_ReplacesManagedBlockOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	prior := "# Header\n\n" +
		instrBeginMarker + "\nOLD STALE BLOCK CONTENT\n" + instrEndMarker + "\n\n" +
		"## Footer the user wrote\n"
	if err := os.WriteFile(path, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := upsertInstructionsBlock(path)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !res.changed {
		t.Error("expected changed=true when replacing a stale block")
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	if strings.Contains(content, "OLD STALE BLOCK CONTENT") {
		t.Error("stale block content not replaced")
	}
	if !strings.HasPrefix(content, "# Header") {
		t.Error("content before block lost")
	}
	if !strings.Contains(content, "## Footer the user wrote") {
		t.Error("content after block lost")
	}
	if !strings.Contains(content, "memory_search") {
		t.Error("fresh block content missing")
	}
}

// TestInstructionsBlock_IsProcedural guards the property that issue #14 turned
// on: the block has to tell the model what to do and when, not just describe
// the tools. The earlier version listed the five tools and gated the search on
// "when prior context might matter", which a model can read as never. If these
// anchors disappear the briefing has drifted back to being documentation.
func TestInstructionsBlock_IsProcedural(t *testing.T) {
	block := instructionsBlock()

	for _, want := range []string{
		"Every session, without exception", // unconditional session protocol
		"Before your first reply",          // concrete first action
		"__shared__",                       // shared namespace is discoverable
		"What triggers a call",             // trigger table, not prose
		"root directory",                   // agent_id is derivable, not invented
		"checkpoint_resume",
		"checkpoint_save",
		// --global puts this block in projects that may not have GrayMatter
		// wired, so it has to guard on the tools existing. The guard is a
		// capability check on purpose: phrased as judgement it would recreate
		// the very hedge that made the old block ignorable.
		"not in your toolbelt",
		"not a\njudgement call",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block no longer contains %q", want)
		}
	}

	// The conditional phrasing that made the old block ignorable.
	for _, unwanted := range []string{"might matter", "when prior context"} {
		if strings.Contains(block, unwanted) {
			t.Errorf("block reintroduced conditional phrasing %q", unwanted)
		}
	}

	// The template placeholder must be fully rendered: a stray ~ means a code
	// span leaked into the generated markdown.
	if strings.Contains(block, "~") {
		t.Error("block contains an unrendered ~ placeholder")
	}

	// Guard against stray non-ASCII in the briefing the user actually reads,
	// with an allowlist for the glyphs it uses on purpose. Scoped to the body:
	// instrBeginMarker predates this and cannot be retyped without breaking the
	// upsert on every file that already carries the old marker.
	const allowed = "⚠"
	for _, r := range strings.ReplaceAll(blockTmpl, "~", "`") {
		if r > 127 && !strings.ContainsRune(allowed, r) {
			t.Errorf("unexpected non-ASCII rune %q in generated block body", r)
		}
	}
}

// TestGlobalInstructionPaths_HonoursXDG pins where --global writes. OpenCode
// resolves its global config through XDG_CONFIG_HOME when that is set, so
// hardcoding ~/.config would put the block somewhere nothing reads and make
// --global look like it worked while doing nothing.
func TestGlobalInstructionPaths_HonoursXDG(t *testing.T) {
	home := t.TempDir()
	testHomeOverride = home
	t.Cleanup(func() { testHomeOverride = "" })

	// Unset: ~/.config/opencode on every platform, Windows included.
	t.Setenv("XDG_CONFIG_HOME", "")
	paths, err := globalInstructionPaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "opencode", "AGENTS.md"); paths[1] != want {
		t.Errorf("default opencode path = %q, want %q", paths[1], want)
	}

	// Set: it wins.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	paths, err = globalInstructionPaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(xdg, "opencode", "AGENTS.md"); paths[1] != want {
		t.Errorf("XDG opencode path = %q, want %q", paths[1], want)
	}

	// Claude Code's own path is home-based and unaffected by XDG.
	if want := filepath.Join(home, ".claude", "CLAUDE.md"); paths[0] != want {
		t.Errorf("claude path = %q, want %q", paths[0], want)
	}
}

func TestWriteGlobalInstructionFiles(t *testing.T) {
	home := t.TempDir()
	testHomeOverride = home
	t.Cleanup(func() { testHomeOverride = "" })
	// XDG_CONFIG_HOME outranks the home directory for the OpenCode path, and it
	// is set on some CI runners. Without pinning it this test would assert
	// against the wrong location and, worse, write into the real config dir.
	t.Setenv("XDG_CONFIG_HOME", "")

	results := writeGlobalInstructionFiles()
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	for _, rel := range []string{
		filepath.Join(".claude", "CLAUDE.md"),
		filepath.Join(".config", "opencode", "AGENTS.md"),
	} {
		data, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil {
			t.Fatalf("%s not written: %v", rel, err)
		}
		if !strings.Contains(string(data), "memory_search") {
			t.Errorf("%s missing the memory block", rel)
		}
	}

	// Second run is a no-op: the block is managed by markers, so re-running
	// init --global must not stack duplicates in a user's global file.
	for _, res := range writeGlobalInstructionFiles() {
		if res.changed {
			t.Errorf("second run rewrote %s", res.path)
		}
	}
}

func TestWriteGlobalInstructionFiles_PreservesUserContent(t *testing.T) {
	home := t.TempDir()
	testHomeOverride = home
	t.Cleanup(func() { testHomeOverride = "" })
	t.Setenv("XDG_CONFIG_HOME", "")

	claude := filepath.Join(home, ".claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(claude), 0o755); err != nil {
		t.Fatal(err)
	}
	const userContent = "# My global rules\n\nAlways answer in Spanish.\n"
	if err := os.WriteFile(claude, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	writeGlobalInstructionFiles()

	data, _ := os.ReadFile(claude)
	if !strings.HasPrefix(string(data), userContent) {
		t.Error("existing global instructions were not preserved")
	}
	if !strings.Contains(string(data), "memory_search") {
		t.Error("memory block not appended to the global file")
	}
}

func TestHasInstructionsBlock(t *testing.T) {
	dir := t.TempDir()

	managed := filepath.Join(dir, "CLAUDE.md")
	if _, err := upsertInstructionsBlock(managed); err != nil {
		t.Fatal(err)
	}
	if !hasInstructionsBlock(managed) {
		t.Error("managed file should be detected")
	}

	custom := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(custom, []byte("Use the GrayMatter MCP tools.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasInstructionsBlock(custom) {
		t.Error("hand-written graymatter mention should be detected")
	}

	unrelated := filepath.Join(dir, "OTHER.md")
	if err := os.WriteFile(unrelated, []byte("nothing to see\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasInstructionsBlock(unrelated) {
		t.Error("unrelated file should not be detected")
	}
	if hasInstructionsBlock(filepath.Join(dir, "MISSING.md")) {
		t.Error("missing file should not be detected")
	}
}
