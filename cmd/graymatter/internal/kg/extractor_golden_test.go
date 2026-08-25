package kg

// The extraction gate: a curated corpus of facts with hand-labelled
// expectations, plus a global noise sweep. Every heuristic change to the
// extractor must keep this green — the thresholds and lists in extractor.go
// cite the 320-fact measurement this fixture set was curated from.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type goldenWant struct {
	Label string `json:"label"`
	Type  string `json:"type"`
}

type goldenFact struct {
	Text   string       `json:"text"`
	Want   []goldenWant `json:"want"`
	Forbid []string     `json:"forbid"`
}

type goldenFile struct {
	Facts []goldenFact `json:"facts"`
}

func TestExtractorGoldenCorpus(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden_facts.json")
	if err != nil {
		t.Fatalf("read golden fixtures: %v", err)
	}
	var gf goldenFile
	if err := json.Unmarshal(raw, &gf); err != nil {
		t.Fatalf("parse golden fixtures: %v", err)
	}
	if len(gf.Facts) == 0 {
		t.Fatal("empty fixture set")
	}

	e := NewExtractor(ExtractorConfig{})
	for _, f := range gf.Facts {
		nodes, _, err := e.Extract(f.Text)
		if err != nil {
			t.Fatalf("%q: extract: %v", f.Text, err)
		}
		byID := map[string]Node{}
		for _, n := range nodes {
			byID[n.ID] = n
		}

		for _, w := range f.Want {
			n, ok := byID[strings.ToLower(w.Label)]
			if !ok {
				t.Errorf("%q: expected entity %q missing; got %v", f.Text, w.Label, nodeLabels(nodes))
				continue
			}
			if w.Type != "" && n.EntityType != w.Type {
				t.Errorf("%q: %q typed %q, want %q", f.Text, w.Label, n.EntityType, w.Type)
			}
		}
		for _, id := range f.Forbid {
			if _, ok := byID[strings.ToLower(id)]; ok {
				t.Errorf("%q: forbidden entity %q present; got %v", f.Text, id, nodeLabels(nodes))
			}
		}

		// Global noise sweep, independent of per-fact expectations: none of
		// the measured noise classes may reappear in any form.
		for _, n := range nodes {
			switch {
			case singleCapStop[strings.ToLower(n.Label)]:
				t.Errorf("%q: stopword entity %q", f.Text, n.Label)
			case strings.Contains(n.Label, "http") || n.EntityType == "reference":
				t.Errorf("%q: URL entity %q", f.Text, n.Label)
			case n.EntityType == "date":
				t.Errorf("%q: date entity %q", f.Text, n.Label)
			case n.EntityType == "fact":
				t.Errorf("%q: legacy 'fact' type survived the concept rename: %q", f.Text, n.Label)
			}
		}
	}
}
