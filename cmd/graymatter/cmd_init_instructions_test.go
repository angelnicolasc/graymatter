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
