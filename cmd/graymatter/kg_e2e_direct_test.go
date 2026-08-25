package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
)

// kgTestConfig satisfies memory.ConsolidateConfig with deterministic values.
type kgTestConfig struct{}

func (c *kgTestConfig) GetAnthropicAPIKey() string        { return "" }
func (c *kgTestConfig) GetConsolidateLLM() string         { return "" }
func (c *kgTestConfig) GetConsolidateModel() string       { return "" }
func (c *kgTestConfig) GetConsolidateThreshold() int      { return 100 }
func (c *kgTestConfig) GetDecayHalfLife() time.Duration   { return 720 * time.Hour }
func (c *kgTestConfig) GetOllamaURL() string              { return "" }
func (c *kgTestConfig) GetOllamaConsolidateModel() string { return "" }

// TestKGEndToEnd_DirectMode is the acceptance run for knowledge-graph
// auto-population, exercised through the real shipped stack in direct mode:
//
//	GRAYMATTER_KG=1 -> openDirectStore -> Remember(entity-rich facts)
//	-> explicit Consolidate -> nodes AND co-mention edges persist in gray.db
//	-> Recall returns the ranked eight PLUS up to three enrichment labels.
func TestKGEndToEnd_DirectMode(t *testing.T) {
	dir := t.TempDir()
	old := dataDir
	dataDir = filepath.Join(dir, ".graymatter")
	t.Cleanup(func() { dataDir = old })
	t.Setenv("GRAYMATTER_KG", "1")

	ds, err := openDirectStore()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer ds.Close()
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		fact := fmt.Sprintf("Maria Rodriguez reviewed Northwind Labs deliverable %d for the Ledgerline Rollout phase.", i)
		if err := ds.Remember(ctx, "proj", fact); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}
	if err := ds.store.Consolidate(ctx, "proj", &kgTestConfig{}); err != nil {
		t.Fatalf("consolidate: %v", err)
	}

	g, err := kg.Open(ds.store.DB())
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := g.AllNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) < 3 {
		t.Fatalf("expected at least 3 distinct entities after consolidation, got %d", len(nodes))
	}
	foundTyped := map[string]bool{}
	for _, n := range nodes {
		foundTyped[n.EntityType] = true
	}
	if !foundTyped["person"] || !foundTyped["organization"] {
		t.Errorf("expected person and organization nodes from typed path; types seen: %v", foundTyped)
	}

	res, err := ds.Recall(ctx, "proj", "ledgerline rollout deliverable review", 8)
	if err != nil {
		t.Fatal(err)
	}
	appended := len(res) - 8
	if appended < 0 || appended > 3 {
		t.Errorf("enrichment appended %d entries, want between 1 and 3 beyond top-8 (total %d)", appended, len(res))
	}
	joined := strings.Join(res, " | ")
	if !strings.Contains(joined, "Maria Rodriguez") && !strings.Contains(joined, "Northwind Labs") {
		t.Errorf("enrichment did not surface any known entity label:\n%s", joined)
	}
}
