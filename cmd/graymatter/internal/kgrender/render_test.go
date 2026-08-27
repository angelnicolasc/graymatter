package kgrender

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
)

func testGraph() ([]kg.Node, []kg.Edge) {
	t0 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	nodes := []kg.Node{
		{ID: "person:Maria", Label: "Maria", EntityType: "person", FirstSeen: t0, LastSeen: t0, Weight: 0.9},
		{ID: "project:Acme", Label: "Acme", EntityType: "project", FirstSeen: t0, LastSeen: t0, Weight: 0.5},
		{ID: "tool:Ollama", Label: "Ollama", EntityType: "tool", FirstSeen: t0, LastSeen: t0, Weight: 0.2},
	}
	edges := []kg.Edge{
		{From: "person:Maria", To: "project:Acme", Relation: "mentioned_in", CreatedAt: t0, Weight: 0.7,
			Sources: []string{"01J8FACTB", "01J8FACTA"}},
		{From: "project:Acme", To: "tool:Ollama", Relation: "related_to", CreatedAt: t0, Weight: 0.3},
	}
	return nodes, edges
}

// TestHTML_IsSelfContained: the page must carry everything it needs — no
// resource-loading patterns of any kind. (The w3.org SVG namespace string is
// an XML identifier, not a fetched resource; it is the one URL allowed.)
func TestHTML_IsSelfContained(t *testing.T) {
	nodes, edges := testGraph()
	var buf bytes.Buffer
	if err := HTML(&buf, nodes, edges); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	page := buf.String()

	for _, banned := range []string{"<link", "<img ", "src=", "href=", "fetch(", "XMLHttpRequest", "import(", "@import"} {
		if strings.Contains(page, banned) {
			t.Errorf("page references external resource via %q — it must be fully self-contained", banned)
		}
	}
	if strings.Count(page, "http") != 1 || !strings.Contains(page, "http://www.w3.org/2000/svg") {
		t.Error("the only URL allowed is the SVG XML namespace identifier")
	}
	if !strings.Contains(page, "createElementNS") || !strings.Contains(page, "const GRAPH") {
		t.Error("page must embed the graph data and build the SVG from it")
	}
}

// TestHTML_EscapesAndTooltips: labels derived from stored text must not be
// able to terminate the script, and edges must expose their fact-ID receipts.
func TestHTML_EscapesAndTooltips(t *testing.T) {
	nodes, edges := testGraph()
	nodes = append(nodes, kg.Node{
		ID: "concept:</script><script>alert(1)</script>", Label: "</script> injection", EntityType: "concept",
		Weight: 0.1,
	})
	var buf bytes.Buffer
	if err := HTML(&buf, nodes, edges); err != nil {
		t.Fatalf("HTML: %v", err)
	}
	page := buf.String()

	if strings.Contains(page, "</script>alert") {
		t.Error("a label containing </script> must be neutralised in the embedded JSON")
	}
	// Go's json.Marshal HTML-escapes < as \u003c, which cannot terminate a
	// script block — assert the neutralised form is what ships.
	if !strings.Contains(page, `\u003c/script`) {
		t.Error("embedded payload must carry the HTML-escaped (inert) form of </script>")
	}
	// The receipt tooltip carries the edge's fact IDs verbatim.
	if !strings.Contains(page, "01J8FACTA") {
		t.Error("page must carry the edge's fact-ID receipts for tooltips")
	}
}

// TestHTML_Deterministic: same graph, same bytes. The render is a pure
// function of its input — fixed-seed layout, sorted inputs.
func TestHTML_Deterministic(t *testing.T) {
	nodes, edges := testGraph()
	var a, b bytes.Buffer
	if err := HTML(&a, nodes, edges); err != nil {
		t.Fatal(err)
	}
	if err := HTML(&b, nodes, edges); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("two renders of the same graph differ")
	}

	// Input order must not leak into the output either.
	revNodes := []kg.Node{nodes[2], nodes[0], nodes[1]}
	revEdges := []kg.Edge{edges[1], edges[0]}
	var c bytes.Buffer
	if err := HTML(&c, revNodes, revEdges); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), c.Bytes()) {
		t.Error("render depends on input order; it must sort deterministically")
	}
}

// TestHTML_EmptyGraph renders a page that says so rather than a blank screen.
func TestHTML_EmptyGraph(t *testing.T) {
	var buf bytes.Buffer
	if err := HTML(&buf, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "svg") {
		t.Error("empty graph must still render a valid page")
	}
}

// TestDOT_Output: valid Graphviz source — quoted ids, weight-scaled pen
// widths, deterministic order.
func TestDOT_Output(t *testing.T) {
	nodes, edges := testGraph()
	var a, b bytes.Buffer
	if err := DOT(&a, nodes, edges); err != nil {
		t.Fatalf("DOT: %v", err)
	}
	if err := DOT(&b, nodes, edges); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("DOT output is not deterministic")
	}
	out := a.String()
	if !strings.HasPrefix(out, "digraph graymatter {") || !strings.HasSuffix(out, "}\n") {
		t.Errorf("missing graph wrapper: %q", out[:50])
	}
	if !strings.Contains(out, `"person:Maria" -> "project:Acme"`) {
		t.Error("edge line missing or ids not dot-quoted")
	}
	for _, want := range []string{"penwidth=3.10", "penwidth=1.90"} { // 1 + 0.7*3, 1 + 0.3*3
		if !strings.Contains(out, want) {
			t.Errorf("penwidth %s missing (weights must scale the stroke)", want)
		}
	}
}

// TestColorFor: known types are fixed, unknown types are stable and hash into
// the palette.
func TestColorFor(t *testing.T) {
	if got := colorFor("person"); got != "#7da7d9" {
		t.Errorf("colorFor(person) = %s, want the fixed palette entry", got)
	}
	if colorFor("person") != colorFor("person") {
		t.Error("colour must be stable per type")
	}
	unknown := colorFor("zarflorb")
	found := false
	for _, c := range palette {
		if c == unknown {
			found = true
		}
	}
	if !found {
		t.Errorf("unknown type colour %s is not from the palette", unknown)
	}
	if colorFor("otherunknown") == unknown {
		t.Error("two different unknown types hashed to the same colour; acceptable but suspicious — check the hash")
	}
}

// TestHTMLPayload round-trips: the embedded JSON decodes and carries the
// fields the tooltips promise.
func TestHTMLPayload(t *testing.T) {
	nodes, edges := testGraph()
	var buf bytes.Buffer
	if err := HTML(&buf, nodes, edges); err != nil {
		t.Fatal(err)
	}
	page := buf.String()
	start := strings.Index(page, "const GRAPH = ")
	if start < 0 {
		t.Fatal("embedded payload missing")
	}
	end := strings.Index(page[start:], ";\n")
	if end < 0 {
		t.Fatal("payload not terminated")
	}
	payload := strings.ReplaceAll(page[start+len("const GRAPH = "):start+end], `<\/`, "/")
	var decoded struct {
		Nodes []struct {
			ID    string  `json:"id"`
			Label string  `json:"label"`
			Type  string  `json:"type"`
			W     float64 `json:"w"`
		} `json:"nodes"`
		Edges []struct {
			From    string   `json:"from"`
			To      string   `json:"to"`
			Sources []string `json:"sources"`
		} `json:"edges"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("embedded payload is not valid JSON: %v\n%s", err, payload)
	}
	if len(decoded.Nodes) != 3 || len(decoded.Edges) != 2 {
		t.Errorf("payload nodes=%d edges=%d, want 3/2", len(decoded.Nodes), len(decoded.Edges))
	}
	if len(decoded.Edges[0].Sources) != 2 {
		t.Errorf("edge receipts = %v, want the 2 fact IDs", decoded.Edges[0].Sources)
	}
}
