package kg

import (
	"strings"
	"testing"
)

// The LLM extractor is opt-in, so its failure modes must be invisible to the
// graph: every degradation lands on the regex extractor, and model noise
// (unknown types/relations) is dropped at the schema gate rather than stored.

func TestLLMFallback_DelegatesToRegexOnTransportError(t *testing.T) {
	f := NewLLMExtractor("test-key", "").(*llmFallback)
	// No ANTHROPIC_BASE_URL override and a bogus key: transport fails.
	nodes, _, err := f.Extract("Maria Rodriguez joined Acme Corp.")
	if err != nil {
		t.Fatalf("fallback must swallow the transport error: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("fallback produced no nodes; regex extractor should have")
	}
	found := false
	for _, n := range nodes {
		if strings.EqualFold(n.Label, "maria rodriguez") {
			found = true
		}
	}
	if !found {
		t.Errorf("regex fallback missed known entity; got %v", nodeLabels(nodes))
	}
}

func TestParseLLMExtractionJSON_SchemaGate(t *testing.T) {
	raw := `{"nodes":[
		{"id":"a","label":"Alice","entity_type":"person"},
		{"id":"b","label":"BobCorp","entity_type":"wizard"},
		{"id":"c","label":"Charlie","entity_type":""}
	],"edges":[
		{"from":"a","to":"b","relation":"related_to"},
		{"from":"a","to":"c","relation":"casts_spell"}
	]}`
	nodes, edges, err := parseLLMExtractionJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Label != "Alice" {
		t.Fatalf("nodes = %+v, want only Alice (unknown/empty types dropped)", nodes)
	}
	if len(edges) != 1 || edges[0].Relation != "related_to" {
		t.Fatalf("edges = %+v, want only related_to (unknown relations dropped)", edges)
	}
}

func TestExtractorFromEnv_DefaultIsRegex(t *testing.T) {
	t.Setenv("GRAYMATTER_KG_LLM_EXTRACT", "")
	e := ExtractorFromEnv()
	if _, ok := e.(*regexExtractor); !ok {
		t.Fatalf("default extractor = %T, want regex", e)
	}
	if _, ok := ExtractorFromEnv().(*llmFallback); ok {
		t.Fatal("env unset must not enable LLM extraction")
	}
}
