package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
)

// seedExportGraph opens the graph on the direct store's handle and creates a
// tiny but real structure: one typed node, one linked placeholder endpoint.
func seedExportGraph(t *testing.T, ds *directStore) {
	t.Helper()
	g, err := kg.Open(ds.store.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Upsert(kg.Node{ID: "data-pipeline", Label: "Data Pipeline", EntityType: "project"}); err != nil {
		t.Fatal(err)
	}
	if err := g.Link(kg.Edge{From: "data-pipeline", To: "etl-jobs", Relation: "related_to"}); err != nil {
		t.Fatal(err)
	}
}

func TestExport_IncludeGraph_WritesEntitiesCanvasAndFacts(t *testing.T) {
	dir := t.TempDir()
	old := dataDir
	dataDir = dir
	t.Cleanup(func() { dataDir = old })
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")

	ds, err := openDirectStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, f := range []string{"Kicked off the data pipeline migration", "Hired a contractor for etl jobs"} {
		if err := ds.Remember(ctx, "proj", f); err != nil {
			t.Fatal(err)
		}
	}
	seedExportGraph(t, ds)
	if err := ds.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "vault")
	cmd := exportCmd()
	cmd.SilenceUsage = true
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--format", "obsidian", "--include-graph", "--out", out})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export --include-graph: %v", err)
	}

	if _, err := os.Stat(filepath.Join(out, "graph-canvas.json")); err != nil {
		t.Errorf("canvas missing: %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	mds := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			mds++
		}
	}
	// fact note(s) + Data Pipeline + auto-upserted etl-jobs placeholder.
	if mds < 3 {
		t.Errorf("expected fact note + 2 entity notes, found %d markdown files: %v", mds, entries)
	}
	// sanitizeFilename swaps spaces for underscores: "Data Pipeline" →
	// "Data_Pipeline.md".
	entityNote := filepath.Join(out, "Data_Pipeline.md")
	if data, err := os.ReadFile(entityNote); err != nil {
		t.Errorf("entity note missing: %v", err)
	} else if !strings.Contains(string(data), "[[etl-jobs]]") {
		t.Errorf("entity note lacks typed backlink:\n%s", data)
	}
}

func TestExport_IncludeGraphRequiresObsidianFormat(t *testing.T) {
	dir := t.TempDir()
	old := dataDir
	dataDir = dir
	t.Cleanup(func() { dataDir = old })
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")

	cmd := exportCmd()
	cmd.SilenceUsage = true
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--format", "markdown", "--include-graph", "--out", filepath.Join(dir, "vault")})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "requires --format obsidian") {
		t.Fatalf("expected loud format error, got %v", err)
	}
}
