package memory

import (
	"context"
	"testing"
	"time"
)

// countingTypedExtractor and countingGraph feed the watermark tests.
type countingTypedExtractor struct {
	typedCalls int
	lastText   string
}

func (e *countingTypedExtractor) ExtractIDs(text string) ([]string, error) {
	e.typedCalls++
	e.lastText = text
	return []string{"entity-x"}, nil
}

func (e *countingTypedExtractor) ExtractTyped(text string) ([]EntityRef, []EntityLink, error) {
	e.typedCalls++
	e.lastText = text
	return []EntityRef{{ID: "entity-x", Label: "Entity X", EntityType: "concept"}}, nil, nil
}

type countingGraph struct {
	upserts int
}

func (g *countingGraph) UpsertNode(id, label, entityType string) error {
	g.upserts++
	return nil
}

func (g *countingGraph) NeighborTexts(string, int) ([]string, error) { return nil, nil }

// TestExtraction_WatermarkSkipsUnchangedAndSuperseded covers A7 plus the
// tombstone filter: unchanged live facts are extracted once and never again;
// editing a fact re-extracts exactly that one; retired facts are never fed
// to the extractor at all.
func TestExtraction_WatermarkSkipsUnchangedAndSuperseded(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ex := &countingTypedExtractor{}
	g := &countingGraph{}
	s.SetKG(g, ex)

	ctx := context.Background()
	const agent = "wm-agent"
	for _, txt := range []string{"watermark fact one", "watermark fact two", "watermark fact three"} {
		if err := s.Put(ctx, agent, txt); err != nil {
			t.Fatal(err)
		}
	}
	cfg := defaultTestCfg()

	if err := s.Consolidate(ctx, agent, cfg); err != nil {
		t.Fatal(err)
	}
	if ex.typedCalls != 3 {
		t.Fatalf("first pass extracted %d facts, want 3", ex.typedCalls)
	}

	if err := s.Consolidate(ctx, agent, cfg); err != nil {
		t.Fatal(err)
	}
	if ex.typedCalls != 3 {
		t.Errorf("unchanged store re-extracted: %d calls total, want 3", ex.typedCalls)
	}

	// Edit one fact's text; retire another. Next pass: exactly one extract,
	// none for the tombstone.
	facts, _ := s.List(agent)
	var edited, retired Fact
	n := 0
	for _, fct := range facts {
		switch n {
		case 0:
			edited = fct
		case 1:
			retired = fct
		}
		n++
	}
	edited.Text += " (revised)"
	if err := s.UpdateFact(agent, edited); err != nil {
		t.Fatal(err)
	}
	retired.SupersededBy = SupersededByAgent
	if err := s.UpdateFact(agent, retired); err != nil {
		t.Fatal(err)
	}

	if err := s.Consolidate(ctx, agent, cfg); err != nil {
		t.Fatal(err)
	}
	if ex.typedCalls != 4 {
		t.Errorf("third pass made %d total calls, want 4 (only the edited fact)", ex.typedCalls)
	}
	if ex.lastText != edited.Text {
		t.Errorf("extractor saw %q, want the revised text", ex.lastText)
	}
}

func TestExtraction_OnlyEligibleFactsAreWatermarked(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	s.now = func() time.Time { return time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC) }
	extractor := &countingTypedExtractor{}
	s.SetKG(&edgeWritingGraph{}, extractor)

	ctx := context.Background()
	const agent = "extraction-eligibility"
	want := make(map[string]bool)
	for _, item := range []struct {
		text    string
		pinned  bool
		retired bool
		extract bool
	}{
		{"normal content", false, false, true},
		{"pinned content", true, false, true},
		{"retired content", false, true, false},
		{" \u2003 ", false, false, false},
	} {
		row, err := s.PutReturningFact(ctx, agent, item.text)
		if err != nil {
			t.Fatal(err)
		}
		row.Pinned = item.pinned
		if item.retired {
			row.SupersededBy = SupersededByAgent
		}
		if err := s.UpdateFact(agent, row); err != nil {
			t.Fatal(err)
		}
		want[row.ID] = item.extract
	}
	alias, err := s.PutAlias(ctx, agent, "broker", []string{"event bus"})
	if err != nil {
		t.Fatal(err)
	}
	want[alias.ID] = false

	if err := s.Consolidate(ctx, agent, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	if extractor.typedCalls != 2 {
		t.Errorf("extractor saw %d facts, want normal and pinned content only", extractor.typedCalls)
	}
	for id, expected := range want {
		if _, got := s.extractedSignature(agent, id); got != expected {
			t.Errorf("watermark present for %s = %v, want %v", id, got, expected)
		}
	}
}

// TestDelete_ClearsExtractionWatermark: a deleted fact's signature must go
// with it, so re-adding the same text later extracts afresh under its new ID.
func TestDelete_ClearsExtractionWatermark(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ex := &countingTypedExtractor{}
	s.SetKG(&countingGraph{}, ex)

	ctx := context.Background()
	const agent = "del-wm-agent"
	if err := s.Put(ctx, agent, "ephemeral fact"); err != nil {
		t.Fatal(err)
	}
	if err := s.Consolidate(ctx, agent, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	facts, _ := s.List(agent)
	id := facts[0].ID
	if _, ok := s.extractedSignature(agent, id); !ok {
		t.Fatal("signature not recorded on first extraction")
	}

	if err := s.Delete(agent, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.extractedSignature(agent, id); ok {
		t.Error("watermark outlived its fact")
	}

	// Same text, new ULID: must be treated as unseen.
	if err := s.Put(ctx, agent, "ephemeral fact"); err != nil {
		t.Fatal(err)
	}
	if err := s.Consolidate(ctx, agent, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	if ex.typedCalls != 2 {
		t.Errorf("extractor called %d times, want 2 (once per incarnation)", ex.typedCalls)
	}
}

func TestExtraction_ExcludesAliasesAndBlankFactsDuringMaintenance(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ex := &countingTypedExtractor{}
	s.SetKG(&countingGraph{}, ex)

	ctx := context.Background()
	const agent = "eligibility"
	if _, err := s.PutAlias(ctx, agent, "broker", []string{"acquiring"}); err != nil {
		t.Fatal(err)
	}
	blank, err := s.PutWithOptionsReturningFact(ctx, agent, "legacy content", WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	blank.Text = "  "
	if err := s.UpdateFact(agent, blank); err != nil {
		t.Fatal(err)
	}
	const content = "ordinary live content"
	if err := s.Put(ctx, agent, content); err != nil {
		t.Fatal(err)
	}
	if err := s.Consolidate(ctx, agent, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	if ex.typedCalls != 1 || ex.lastText != content {
		t.Fatalf("extracted %d inputs with last %q, want only %q", ex.typedCalls, ex.lastText, content)
	}
}
