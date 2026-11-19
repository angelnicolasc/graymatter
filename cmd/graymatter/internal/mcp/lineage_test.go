package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestMemorySearchExplainedLineageWireContract(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()
	store := mem.Advanced().(*memory.Store)
	original, err := store.PutReturningFact(ctx, "project", "The shipment color is amber")
	if err != nil {
		t.Fatal(err)
	}
	head, err := store.ReviseFactsWithOptions(ctx, "project", "The shipment color is green", memory.WriteOptions{}, original)
	if err != nil {
		t.Fatal(err)
	}
	response := callToolJSONRPC(t, s, 1, "memory_search", map[string]any{
		"agent_id": "project", "query": "shipment color", "top_k": 1, "explain": true,
	})
	got := response.Result
	if got.IsError {
		t.Fatalf("MCP explain: %s", got.Content)
	}
	validateStructuredAgainstToolSchema(t, "memory_search", got.StructuredContent)

	var wire map[string]any
	if err := json.Unmarshal(got.StructuredContent, &wire); err != nil {
		t.Fatal(err)
	}
	wireProvenance := wire["explained"].([]any)[0].(map[string]any)["provenance"].(map[string]any)
	wireSource := wireProvenance["replaces"].([]any)[0].(map[string]any)
	wireSource["undeclared_lineage_field"] = true
	schema := compileOutputSchema(t, listToolDefs(t)["memory_search"].OutputSchema)
	if err := schema.Validate(wire); err == nil {
		t.Fatal("the advertised schema accepted an undeclared lineage reference field")
	}
	var payload searchResult
	if err := json.Unmarshal(got.StructuredContent, &payload); err != nil {
		t.Fatal(err)
	}
	sourceHash := memory.NewEvidenceSnapshot(original).ContentHash
	if len(payload.Explained) != 1 {
		t.Fatalf("expected one receipt: %s", got.StructuredContent)
	}
	provenance := payload.Explained[0].Provenance
	if provenance.LineageStatus != memory.LineageVerified ||
		provenance.ContentHash != memory.NewEvidenceSnapshot(head).ContentHash ||
		len(provenance.Replaces) != 1 ||
		provenance.Replaces[0] != (memory.LineageReference{FactID: original.ID, ContentHash: sourceHash}) ||
		len(provenance.DerivedFrom) != 0 ||
		len(provenance.Supersedes) != 1 || provenance.Supersedes[0] != original.ID {
		t.Fatalf("incorrect lineage projection: %+v", provenance)
	}
	var content struct {
		Text string `json:"text"`
	}
	if len(got.Content) != 1 {
		t.Fatalf("expected one prose block: %s", got.Content)
	}
	if err := json.Unmarshal(got.Content[0], &content); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Lineage status", memory.LineageVerified, provenance.ContentHash, "Replaces", original.ID, sourceHash} {
		if !strings.Contains(content.Text, expected) {
			t.Errorf("prose missing %q: %s", expected, content.Text)
		}
	}
}
