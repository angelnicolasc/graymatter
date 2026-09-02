package memory

import (
	"context"
	"testing"
	"time"
)

// Recall drops superseded facts before scoring, which is what makes the answer
// current — and what made the correction invisible: the caller got one value
// with no sign that it replaced anything. Provenance.Supersedes is the other
// half of the tombstone, so these tests hold the line on both properties at
// once: the retired belief stays out of the ranking, and the receipt still
// says it existed.

// reviseChain writes each text in order and points every earlier fact at the
// one that replaced it, the way `graymatter revise` does.
func reviseChain(t *testing.T, s *Store, agent string, clock *explainClock, texts ...string) []Fact {
	t.Helper()
	ctx := context.Background()
	for _, txt := range texts {
		clock.offset += time.Hour
		if err := s.Put(ctx, agent, txt); err != nil {
			t.Fatalf("Put %q: %v", txt, err)
		}
	}
	all, err := s.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	byText := make(map[string]Fact, len(all))
	for _, f := range all {
		byText[f.Text] = f
	}
	ordered := make([]Fact, 0, len(texts))
	for _, txt := range texts {
		f, ok := byText[txt]
		if !ok {
			t.Fatalf("seeded fact missing: %q", txt)
		}
		ordered = append(ordered, f)
	}
	for i := 0; i < len(ordered)-1; i++ {
		victim := ordered[i]
		victim.SupersededBy = ordered[i+1].ID
		if err := s.UpdateFact(agent, victim); err != nil {
			t.Fatalf("supersede %q: %v", victim.Text, err)
		}
	}
	return ordered
}

func TestSupersedesNamesTheWholeChain(t *testing.T) {
	s, clock, done := newExplainStore(t)
	defer done()
	const agent = "explain-agent"

	chain := reviseChain(t, s, agent, clock,
		"the session timeout is 30 minutes",
		"the session timeout was shortened to 22 minutes",
		"the session timeout is 10 minutes after the security review",
	)

	receipts, err := s.RecallExplain(context.Background(), agent, "how long until a session times out?", 5)
	if err != nil {
		t.Fatal(err)
	}
	var live *RecallReceipt
	for i := range receipts {
		if receipts[i].Provenance.FactID == chain[2].ID {
			live = &receipts[i]
		}
		// The retired versions must not come back at all.
		if receipts[i].Provenance.FactID == chain[0].ID || receipts[i].Provenance.FactID == chain[1].ID {
			t.Errorf("a superseded fact was recalled: %q", receipts[i].Text)
		}
	}
	if live == nil {
		t.Fatal("the current value was not recalled")
	}

	got := live.Provenance.Supersedes
	if len(got) != 2 {
		t.Fatalf("Supersedes = %v, want both retired versions", got)
	}
	want := map[string]bool{chain[0].ID: true, chain[1].ID: true}
	for _, id := range got {
		if !want[id] {
			t.Errorf("Supersedes names %q, which is not in the chain", id)
		}
	}
}

// A fact that revised nothing must carry no lineage — otherwise every receipt
// grows a field that means nothing.
func TestSupersedesEmptyForAnUnrevisedFact(t *testing.T) {
	s, clock, done := newExplainStore(t)
	defer done()
	const agent = "explain-agent"
	seedExplainCorpus(t, s, clock)

	receipts, err := s.RecallExplain(context.Background(), agent, explainQuery, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) == 0 {
		t.Fatal("no receipts")
	}
	for _, r := range receipts {
		if len(r.Provenance.Supersedes) != 0 {
			t.Errorf("%q claims to supersede %v", r.Text, r.Provenance.Supersedes)
		}
	}
}

// forget retires with SupersededByAgent — a marker, not a fact ID. It must not
// become a phantom entry hanging off a fact that never existed.
func TestForgetMarkerDoesNotCreateLineage(t *testing.T) {
	s, clock, done := newExplainStore(t)
	defer done()
	const agent = "explain-agent"
	ctx := context.Background()

	for _, txt := range []string{
		"deploys are frozen on Fridays",
		"deployments are signed with the team gpg key before publishing",
	} {
		clock.offset += time.Hour
		if err := s.Put(ctx, agent, txt); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range all {
		if f.Text == "deploys are frozen on Fridays" {
			f.SupersededBy = SupersededByAgent
			if err := s.UpdateFact(agent, f); err != nil {
				t.Fatal(err)
			}
		}
	}

	receipts, err := s.RecallExplain(ctx, agent, explainQuery, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range receipts {
		for _, id := range r.Provenance.Supersedes {
			if id == SupersededByAgent {
				t.Errorf("the forget marker leaked into lineage on %q", r.Text)
			}
		}
		if len(r.Provenance.Supersedes) != 0 {
			t.Errorf("%q gained a lineage from a fact retired with no replacement: %v",
				r.Text, r.Provenance.Supersedes)
		}
	}
}

// A supersede cycle is a doctor finding, not an impossibility. Building a
// receipt must terminate anyway.
func TestLineageTerminatesOnACycle(t *testing.T) {
	s, clock, done := newExplainStore(t)
	defer done()
	const agent = "explain-agent"
	ctx := context.Background()

	for _, txt := range []string{"value is A", "value is B", "the live one mentions value"} {
		clock.offset += time.Hour
		if err := s.Put(ctx, agent, txt); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	byText := map[string]Fact{}
	for _, f := range all {
		byText[f.Text] = f
	}
	a, b, live := byText["value is A"], byText["value is B"], byText["the live one mentions value"]

	// A -> B -> A, and B also retired into the live fact, so the walk has a
	// way in and a loop to fall into once inside.
	a.SupersededBy = b.ID
	b.SupersededBy = a.ID
	if err := s.UpdateFact(agent, a); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateFact(agent, b); err != nil {
		t.Fatal(err)
	}

	dead := make(chan []RecallReceipt, 1)
	go func() {
		r, err := s.RecallExplain(ctx, agent, "value", 5)
		if err != nil {
			dead <- nil
			return
		}
		dead <- r
	}()
	select {
	case got := <-dead:
		if got == nil {
			t.Fatal("RecallExplain failed on a cyclic supersede graph")
		}
		for _, r := range got {
			if r.Provenance.FactID == live.ID {
				return // built a receipt without hanging; that is the contract
			}
		}
	case <-time.After(20 * time.Second):
		t.Fatal("RecallExplain hung on a supersede cycle")
	}
}
