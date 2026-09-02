package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Does usage-alias learning reach the synonym class, or only morphology?
//
// The first evaluation corpus replayed with the affinity gate DISABLED
// (UsageAliasAffinityMin = -1), every promoted pair dumped for semantic
// classification, and the mechanical proxy measured before and after. The
// conservative gate of 3 confines promotion to morphology, which measured
// neutral over real agent behaviour — real agents reformulate by synonymy,
// not by suffix. This run answers with data what gets learned when the gate
// is opened. It needs the same corpus as the test above and skips without it.

func TestUsageAliasAffinity0OnHoldoutV1(t *testing.T) {
	corpus, probes := loadHoldout(t)
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		DecayHalfLife: 8760 * time.Hour,
		// The measured mode: no affinity gate. Every promoted pair is dumped
		// below for classification — nothing here is asserted as sensible.
		UsageAliasLearning:    true,
		UsageAliasAffinityMin: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	// Product-path ingestion: Put first versions, Revise each chain, scripted
	// timeline (same as the v1 integration test — the tombstones and the
	// recency order are what the product writes).
	type chain struct {
		probe holdoutProbe
		lines []string
	}
	var chains []chain
	for _, p := range probes {
		var lines []string
		for _, tok := range append(append([]string{}, p.Stale...), p.Correct) {
			for _, l := range corpus {
				if strings.Contains(strings.ToLower(l), strings.ToLower(tok)) {
					lines = append(lines, l)
					break
				}
			}
		}
		seen := map[string]bool{}
		var uniq []string
		for _, l := range lines {
			if !seen[l] {
				seen[l] = true
				uniq = append(uniq, l)
			}
		}
		if !strings.Contains(strings.ToLower(uniq[len(uniq)-1]), strings.ToLower(p.Correct)) {
			t.Fatalf("%s: newest statement does not carry the correct value", p.ID)
		}
		chains = append(chains, chain{probe: p, lines: uniq})
	}

	later := map[string]bool{}
	for _, c := range chains {
		for _, l := range c.lines[1:] {
			later[l] = true
		}
	}
	for _, line := range corpus {
		if later[line] {
			continue
		}
		if err := s.Put(ctx, "holdout", line); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range chains {
		for i := 0; i < len(c.lines)-1; i++ {
			facts, err := s.List("holdout")
			if err != nil {
				t.Fatal(err)
			}
			var victims []Fact
			for _, f := range facts {
				if f.Text == c.lines[i] && !f.IsSuperseded() {
					victims = append(victims, f)
				}
			}
			if len(victims) == 0 {
				t.Fatalf("%s: nothing to revise for %q", c.probe.ID, c.lines[i])
			}
			if _, err := s.Revise(ctx, "holdout", c.lines[i+1], victims...); err != nil {
				t.Fatal(err)
			}
		}
	}
	order := map[string]int{}
	for i, l := range corpus {
		order[l] = i
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	all, err := s.List("holdout")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range all {
		if idx, ok := order[f.Text]; ok {
			f.CreatedAt = base.Add(time.Duration(idx) * time.Hour)
			if err := s.UpdateFact("holdout", f); err != nil {
				t.Fatal(err)
			}
		}
	}
	v3 := map[string]string{}
	for _, c := range chains {
		v3[c.probe.ID] = c.lines[len(c.lines)-1]
	}

	paraBase, litBase := 0, 0
	for _, p := range probes {
		results, err := s.Recall(ctx, "holdout", p.Query, 8)
		if err != nil {
			t.Fatal(err)
		}
		if scoreFamily(results, p) {
			if p.Paraphrased {
				paraBase++
			} else {
				litBase++
			}
		}
	}
	t.Logf("baseline proxy: paraphrased %d/19 · literal %d/21", paraBase, litBase)

	// Two learning sessions: literal probe (the caller's vocabulary, weak) →
	// reformulated query (the current statement's words, strong). Same shape
	// the real agent produced: a gap, then the words that worked.
	reformulated := func(p holdoutProbe) string {
		line := v3[p.ID]
		probeSet := map[string]bool{}
		for _, tok := range tokenize(p.Query) {
			probeSet[tok] = true
		}
		var toks []string
		for _, tok := range tokenize(line) {
			if !probeSet[tok] {
				toks = append(toks, tok)
			}
			if len(toks) == 4 {
				break
			}
		}
		if len(toks) == 0 {
			return p.Query
		}
		return strings.Join(toks, " ")
	}

	for session := 0; session < 2; session++ {
		for _, p := range probes {
			if _, _, err := s.RecallDetailed(ctx, "holdout", p.Query, 8); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.RecallDetailed(ctx, "holdout", reformulated(p), 8); err != nil {
				t.Fatal(err)
			}
		}
		if session == 0 {
			if n := s.countUsageAliases("holdout"); n != 0 {
				t.Fatalf("k=2 violated: %d aliases after a single observation", n)
			}
		}
	}

	pairs := s.usageAliasTerms("holdout")
	t.Logf("PROMOTED with no affinity gate (%d):", len(pairs))
	for _, pair := range pairs {
		t.Logf("  %s", pair)
	}

	// Structural invariants only — the semantic classification of the pairs
	// is read from the log above and documented in the handover; what the
	// test asserts is the mechanics: k=2 held, every alias source=usage, and
	// the mechanical movement is measured and reported.
	paraAfter, litAfter := 0, 0
	for _, p := range probes {
		results, err := s.Recall(ctx, "holdout", p.Query, 8)
		if err != nil {
			t.Fatal(err)
		}
		if scoreFamily(results, p) {
			if p.Paraphrased {
				paraAfter++
			} else {
				litAfter++
			}
		}
	}
	t.Logf("post proxy: paraphrased %d/19 (baseline %d) · literal %d/21 (baseline %d)", paraAfter, paraBase, litAfter, litBase)
	if litAfter < litBase {
		t.Errorf("literal regressed under the no-gate mode: %d -> %d", litBase, litAfter)
	}
}
