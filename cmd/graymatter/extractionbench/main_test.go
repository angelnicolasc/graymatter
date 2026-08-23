package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestParseAndScore_IDLevelVsStrict(t *testing.T) {
	facts := []goldFact{
		{ID: "f1", Text: "Maria Rodriguez joined Acme Corp", Gold: []goldEntity{
			{ID: "maria rodriguez", Label: "Maria Rodriguez", Type: "person"},
			{ID: "acme corp", Label: "Acme Corp", Type: "organization"},
		}},
		// The regex types "Atlas Migration" as person; gold says project:
		// ID-level hit, strict miss. That gap is the point of the strict row.
		{ID: "f2", Text: "Atlas Migration shipped on time", Gold: []goldEntity{
			{ID: "atlas migration", Label: "Atlas Migration", Type: "project"},
		}},
	}

	ex := newBenchExtractor()
	var tp, fp, fn, tpStrict int
	perType := map[string]*typeStats{}
	for _, f := range facts {
		scoreFact(ex, f, &tp, &fp, &fn, &tpStrict, perType, nil)
	}

	if tp != 3 || fn != 0 {
		t.Errorf("id-level TP=%d FN=%d, want 3/0", tp, fn)
	}
	// Strict = 3? No: "Atlas Migration" is typed person by the regex while
	// gold says project → strict misses exactly that one.
	if tpStrict != 2 {
		t.Errorf("strict TP=%d, want 2", tpStrict)
	}
	s := perType["project"]
	if s == nil || s.typeMismatch != 1 || s.missed != 0 {
		t.Errorf("project stats wrong: %+v", s)
	}
	p := perType["person"]
	if p == nil || p.gold != 1 || p.typed != 1 {
		t.Errorf("person stats wrong: %+v", p)
	}
}

func TestCorpusIntegrity(t *testing.T) {
	path, ok := resolveGoldPath()
	if !ok {
		t.Skip("corpus not reachable from this working directory")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 100 {
		t.Fatalf("corpus has %d facts, need >= 100 for the gate to mean anything", len(lines))
	}
	seen := map[string]bool{}
	for i, line := range lines {
		var gf goldFact
		if err := json.Unmarshal([]byte(line), &gf); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if gf.ID == "" || gf.Text == "" {
			t.Fatalf("line %d: empty id/text", i+1)
		}
		if seen[gf.ID] {
			t.Errorf("duplicate fact id %s", gf.ID)
		}
		seen[gf.ID] = true
		for _, g := range gf.Gold {
			if g.ID == "" || g.Type == "" {
				t.Fatalf("line %d: incomplete gold entity %+v", i+1, g)
			}
		}
	}
}
