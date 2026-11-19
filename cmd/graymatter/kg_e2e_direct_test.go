package main

import (
	"context"
	"encoding/json"
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
	t.Setenv("GRAYMATTER_KG_LLM_EXTRACT", "0")

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

	facts, err := ds.store.List("proj")
	if err != nil {
		t.Fatal(err)
	}
	factIDs := make(map[string]bool, len(facts))
	for _, fact := range facts {
		factIDs[fact.ID] = true
	}
	edges, err := g.AllEdges()
	if err != nil {
		t.Fatal(err)
	}
	var coMention *kg.Edge
	for i := range edges {
		edge := &edges[i]
		if edge.From == "person:maria rodriguez" && edge.To == "organization:northwind labs" && edge.Relation == "co_mentioned" {
			coMention = edge
			break
		}
	}
	if coMention == nil {
		t.Fatal("missing expected co-mention after consolidation")
	}
	// Decode the persisted edge shape so the same regression exercises the
	// prior writer without depending on the new Go fields.
	raw, err := json.Marshal(coMention)
	if err != nil {
		t.Fatal(err)
	}
	var provenance struct {
		Version   int  `json:"provenance_version"`
		Complete  bool `json:"sources_complete"`
		Truncated bool `json:"sources_truncated"`
		Manual    bool `json:"manual_support"`
		Supports  []struct {
			Namespace string `json:"namespace"`
			FactID    string `json:"fact_id"`
			Digest    string `json:"digest"`
		} `json:"supports"`
	}
	if err := json.Unmarshal(raw, &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance.Version != 1 || provenance.Complete || !provenance.Truncated || provenance.Manual ||
		len(provenance.Supports) != 10 || len(coMention.Sources) != 10 {
		t.Fatalf("overflowed extraction provenance = %+v, sources %v", provenance, coMention.Sources)
	}
	for i, source := range provenance.Supports {
		if source.Namespace != "proj" || source.Digest == "" || !factIDs[source.FactID] || coMention.Sources[i] != source.FactID {
			t.Errorf("support %d = %+v, projected fact ID = %q", i, source, coMention.Sources[i])
		}
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
