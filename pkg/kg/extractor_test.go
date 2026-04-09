package kg

import (
	"testing"
)

func TestRegexExtractor_MultiWordName(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	nodes, _, err := e.Extract("Maria Rodriguez is the VP Sales at Acme Corp.")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := map[string]bool{}
	for _, n := range nodes {
		found[n.Label] = true
	}
	if !found["Maria Rodriguez"] {
		t.Errorf("expected 'Maria Rodriguez' in nodes, got: %v", nodeLabels(nodes))
	}
}

func TestRegexExtractor_URLExtraction(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	nodes, _, err := e.Extract("See our docs at https://example.com/docs for details.")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := map[string]bool{}
	for _, n := range nodes {
		found[n.Label] = true
		found[n.EntityType] = true
	}
	if !found["https://example.com/docs"] {
		t.Errorf("URL not extracted; nodes: %v", nodeLabels(nodes))
	}
}

func TestRegexExtractor_DateExtraction(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	nodes, _, err := e.Extract("The meeting is on 2026-04-15.")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := map[string]bool{}
	for _, n := range nodes {
		found[n.Label] = true
	}
	if !found["2026-04-15"] {
		t.Errorf("date not extracted; nodes: %v", nodeLabels(nodes))
	}
}

func TestRegexExtractor_MentionExtraction(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	nodes, _, err := e.Extract("Please ping @alice and @bob about the deploy.")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	found := map[string]bool{}
	for _, n := range nodes {
		found[n.Label] = true
	}
	if !found["@alice"] {
		t.Errorf("@alice not extracted; nodes: %v", nodeLabels(nodes))
	}
}

func TestRegexExtractor_EmptyInput(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	nodes, edges, err := e.Extract("")
	if err != nil {
		t.Fatalf("Extract empty: %v", err)
	}
	if len(nodes) != 0 || len(edges) != 0 {
		t.Errorf("expected empty result for empty input")
	}
}

func TestRegexExtractor_EdgesLinkConsecutiveNodes(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	_, edges, err := e.Extract("Maria Rodriguez is at Acme Corp. See https://acme.example.com for info.")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Edges should link consecutive extracted nodes.
	for _, edge := range edges {
		if edge.Relation != "co_mentioned" {
			t.Errorf("unexpected relation %q", edge.Relation)
		}
		if edge.From == "" || edge.To == "" {
			t.Errorf("edge has empty endpoints: %+v", edge)
		}
	}
}

func TestCanonicalID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Maria Rodriguez", "maria rodriguez"},
		{"  ACME Corp  ", "acme corp"},
		{"", ""},
	}
	for _, tc := range tests {
		got := canonicalID(tc.input)
		if got != tc.want {
			t.Errorf("canonicalID(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseLLMExtractionJSON_Valid(t *testing.T) {
	raw := `{"nodes":[{"id":"maria","label":"Maria","entity_type":"person"}],"edges":[{"from":"maria","to":"acme","relation":"works_at"}]}`
	nodes, edges, err := parseLLMExtractionJSON(raw)
	if err != nil {
		t.Fatalf("parseLLMExtractionJSON: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "maria" {
		t.Errorf("nodes = %v", nodes)
	}
	if len(edges) != 1 || edges[0].Relation != "works_at" {
		t.Errorf("edges = %v", edges)
	}
}

func TestParseLLMExtractionJSON_CodeFence(t *testing.T) {
	raw := "```json\n{\"nodes\":[],\"edges\":[]}\n```"
	nodes, edges, err := parseLLMExtractionJSON(raw)
	if err != nil {
		t.Fatalf("parseLLMExtractionJSON with code fence: %v", err)
	}
	if len(nodes) != 0 || len(edges) != 0 {
		t.Errorf("expected empty result")
	}
}

func TestParseLLMExtractionJSON_Invalid(t *testing.T) {
	_, _, err := parseLLMExtractionJSON("not json at all")
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

// nodeLabels returns a slice of node labels for test output.
func nodeLabels(nodes []Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Label
	}
	return out
}
