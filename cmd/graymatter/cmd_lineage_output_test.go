package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestCLILineageExplainAndAuditExports(t *testing.T) {
	dir := t.TempDir()
	oldDataDir, oldJSON := dataDir, jsonOut
	dataDir = dir
	t.Cleanup(func() { dataDir, jsonOut = oldDataDir, oldJSON })
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")

	var original, head memory.Fact
	func() {
		ctx := context.Background()
		store, err := openDirectStore()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		original, err = store.PutReturningFact(ctx, "project", "The shipment color is amber")
		if err != nil {
			t.Fatal(err)
		}
		head, err = store.ReviseFactsWithOptions(ctx, "project", "The shipment color is green", memory.WriteOptions{}, original)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Delete("project", original.ID); err != nil {
			t.Fatal(err)
		}

		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		explain := func() {
			err = runRecallExplain(cmd, store, "project", "shipment color", 1)
		}
		jsonOut = false
		prose := captureStdout(t, explain)
		if err != nil {
			t.Fatal(err)
		}
		wantHash := memory.NewEvidenceSnapshot(original).ContentHash
		for _, want := range []string{"Lineage status", memory.LineageVerified, "Replaces", original.ID, wantHash} {
			if !strings.Contains(prose, want) {
				t.Fatalf("recall explain missing %q: %s", want, prose)
			}
		}
		jsonOut = true
		raw := captureStdout(t, explain)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Facts []memory.RecallReceipt `json:"facts"`
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Facts) != 1 {
			t.Fatalf("unexpected receipt count: %s", raw)
		}
		p := result.Facts[0].Provenance
		if p.FactID != head.ID || p.LineageStatus != memory.LineageVerified ||
			len(p.Replaces) != 1 || p.Replaces[0].ContentHash != wantHash {
			t.Fatalf("incorrect JSON receipt: %s", raw)
		}
	}()

	runExport := func(format string, extra ...string) string {
		t.Helper()
		out := filepath.Join(dir, format+strings.Join(extra, "-"))
		cmd := exportCmd()
		cmd.SetArgs(append([]string{"--format", format, "--out", out, "--agent", "project"}, extra...))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	read := func(path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	legacy := read(filepath.Join(runExport("json"), "memories.json"))
	var facts []memory.Fact
	if err := json.Unmarshal(legacy, &facts); err != nil || len(facts) != 1 || facts[0].ID != head.ID {
		t.Fatalf("legacy facts JSON shape: %v %s", err, legacy)
	}
	complete := read(filepath.Join(runExport("json", "--include-lineage"), "memory-lineage.json"))
	var archive memory.LineageArchive
	if err := json.Unmarshal(complete, &archive); err != nil {
		t.Fatal(err)
	}
	if archive.Version != memory.LineageArchiveVersion || archive.Namespace != "project" ||
		len(archive.Facts) != 1 || archive.Facts[0].ID != head.ID || len(archive.Evidence) != 2 {
		t.Fatalf("wrong archive manifest: %s", complete)
	}
	var foundSource, foundRelation bool
	for _, item := range archive.Evidence {
		if item.Snapshot == nil {
			t.Fatalf("archive contains a missing snapshot: %s", complete)
		}
		switch item.Snapshot.FactID {
		case original.ID:
			foundSource = item.Snapshot.Text == original.Text
		case head.ID:
			foundRelation = item.LineageStatus == memory.LineageVerified &&
				len(item.Replaces) == 1 && item.Replaces[0].ContentHash == memory.NewEvidenceSnapshot(original).ContentHash
		}
	}
	if !foundSource || !foundRelation {
		t.Fatalf("archive lost a deleted source or its relationship: %s", complete)
	}

	markdown := read(filepath.Join(runExport("markdown"), "project.md"))
	vault := runExport("obsidian")
	notes, err := filepath.Glob(filepath.Join(vault, "project", "*.md"))
	if err != nil || len(notes) != 1 {
		t.Fatalf("unexpected vault notes: %v %v", notes, err)
	}
	obsidian := read(notes[0])
	for name, raw := range map[string][]byte{"markdown": markdown, "obsidian": obsidian} {
		for _, want := range []string{"Lineage scope", "fact projection", "Replaces", original.ID, memory.LineageVerified} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s missing %q: %s", name, want, raw)
			}
		}
	}
}
