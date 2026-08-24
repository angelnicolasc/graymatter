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

func TestRegexExtractor_EdgesLinkAllPairs(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	_, edges, err := e.Extract("Maria Rodriguez is at Acme Corp. See https://acme.example.com for info.")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Co-mention is a clique: 3 nodes => 3 undirected pairs, all present.
	if len(edges) != 3 {
		t.Errorf("expected 3 co-mention edges for 3 nodes, got %d: %+v", len(edges), edges)
	}
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

// --- v2 extractor improvements (accent safety, determiners, org suffixes,
// role titles, URL trimming). Each test pins one fix from the precision
// bench findings.

func TestRegexExtractor_UnicodeNamesSurvive(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	nodes, _, err := e.Extract("Sebastián Yañez joined the treasury rotation.")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, n := range nodes {
		found[n.ID] = n.EntityType
	}
	if got := found["sebastián yañez"]; got != "person" {
		t.Errorf("accented name missing or mistyped: %v", found)
	}
	if _, broken := found["sebasti"]; broken {
		t.Error("fragment from accent cut-off still present")
	}
}

func TestRegexExtractor_DeterminerStripped(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	nodes, _, err := e.Extract("The Atlas Migration shipped on time.")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, n := range nodes {
		found[n.ID] = true
	}
	if !found["atlas migration"] {
		t.Errorf("name without determiner missing: %v", nodeLabels(nodes))
	}
	if found["the atlas migration"] {
		t.Error("determiner glued into entity id")
	}
}

func TestRegexExtractor_OrgSuffixesRecognized(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	nodes, _, err := e.Extract("Juniper Labs and Willow Creek Capital co-invested; Vertex Analytics followed.")
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, n := range nodes {
		types[n.ID] = n.EntityType
	}
	for _, id := range []string{"juniper labs", "willow creek capital", "vertex analytics"} {
		if types[id] != "organization" {
			t.Errorf("%s typed %q, want organization (all: %v)", id, types[id], types)
		}
	}
}

func TestRegexExtractor_RoleTitles(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	nodes, _, err := e.Extract("VP Finance requested the numbers; the CTO approved them.")
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]string{}
	for _, n := range nodes {
		types[n.ID] = n.EntityType
	}
	if types["vp finance"] != "role" {
		t.Errorf("'vp finance' typed %q, want role", types["vp finance"])
	}
	if types["cto"] != "role" {
		t.Errorf("'cto' typed %q, want role", types["cto"])
	}
}

func TestRegexExtractor_URLTrailingPunctuationTrimmed(t *testing.T) {
	e := NewExtractor(ExtractorConfig{})
	nodes, _, err := e.Extract("Changelog lives at https://example.com/changelog.")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, n := range nodes {
		found[n.ID] = true
	}
	if found["https://example.com/changelog."] {
		t.Error("trailing period captured in reference id")
	}
	if !found["https://example.com/changelog"] {
		t.Error("trimmed URL missing")
	}
}

// --- v2 extractor improvements (accent safety, determiners, org suffixes,
// role titles, URL trimming). Each test pins one fix from the precision
// bench findings.

// nodeLabels returns a slice of node labels for test output.
func nodeLabels(nodes []Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Label
	}
	return out
}
