package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The measurement that put stemming on by default, reproducible from the tree.
//
// graymatter.Config's StemKeywords comment cites "25/35 -> 29/35 at 5k, 10k and
// 30k facts, winning four queries and losing none". Until this test existed
// that sentence lived only in a handover: the in-tree scale curve stopped at
// 3000, so the number a reader could check was not the number the default was
// argued from. That is the same failure as a claim in a handover nobody rereads
// — the fix is a test, not a paragraph.
//
// Two things are asserted, and only one of them is a count:
//
//	the strict subset  no family that is answered without stemming may go
//	                   unanswered with it. This is the REVERT CRITERION for the
//	                   default, and it is a property rather than an aggregate —
//	                   a hit rate that holds can still hide four won and four
//	                   lost. It is also the gate a Snowball migration has to
//	                   clear from scratch, because the +4 was measured with this
//	                   Porter and no other stemmer inherits it.
//	the counts         logged, not asserted. Filler generation differs from the
//	                   lab harness that produced the published figure, so the
//	                   exact 25 and 29 are not pinned; the shape is.
//
// Guarded by GRAYMATTER_SCALE_GATE=1, the same switch as pkg/memory's
// TestP4ScaleGate: building four stores of up to 30 000 facts is minutes of
// work with no business running on every `go test ./...`.
func TestStemmingScaleGate(t *testing.T) {
	if os.Getenv("GRAYMATTER_SCALE_GATE") != "1" {
		t.Skip("set GRAYMATTER_SCALE_GATE=1 to run the 10k/30k stemming gate")
	}
	ctx := context.Background()

	for _, size := range []int{10000, 30000} {
		hit := map[bool]map[string]bool{
			false: {},
			true:  {},
		}
		for _, doStem := range []bool{false, true} {
			dir, err := os.MkdirTemp("", "gmstemscale")
			if err != nil {
				t.Fatal(err)
			}
			store, _ := buildScaled(t, dir, size, doStem)
			for _, f := range families {
				res, err := store.Recall(ctx, agentID, f.Query, topK)
				if err != nil {
					t.Fatal(err)
				}
				joined := strings.ToLower(strings.Join(res, "\n"))
				hit[doStem][f.ID] = strings.Contains(joined, strings.ToLower(f.Correct))
			}
			_ = store.Close()
			_ = os.RemoveAll(dir)
		}

		var lost, won []string
		offHits, onHits := 0, 0
		for _, f := range families {
			if hit[false][f.ID] {
				offHits++
			}
			if hit[true][f.ID] {
				onHits++
			}
			switch {
			case hit[false][f.ID] && !hit[true][f.ID]:
				lost = append(lost, f.ID)
			case !hit[false][f.ID] && hit[true][f.ID]:
				won = append(won, f.ID)
			}
		}

		t.Logf("%6d facts · without stemming %d/%d · with stemming %d/%d · wins %v · loses %v",
			size, offHits, len(families), onHits, len(families), won, lost)

		if len(lost) > 0 {
			t.Errorf("%d facts: stemming lost %d families it wins without: %v; "+
				"the default is ON because of the strict-subset property, and this breaks it",
				size, len(lost), lost)
		}
		if onHits < offHits {
			t.Errorf("%d facts: stemming lowered the hits %d -> %d", size, offHits, onHits)
		}
	}
}

// The claim the default rests on is about the SCAN path and the INDEXED path
// disagreeing on cost while agreeing on answers. The disagreement on cost is
// measured elsewhere; this pins the agreement on answers at a scale the
// identity test does not reach, because a candidate-set path that silently
// stopped matching stemmed postings would show up here as a lower hit count and
// nowhere else.
func TestStemmingAgreesAcrossRetrievalPaths(t *testing.T) {
	if os.Getenv("GRAYMATTER_SCALE_GATE") != "1" {
		t.Skip("set GRAYMATTER_SCALE_GATE=1 to run the 10k path-agreement gate")
	}
	ctx := context.Background()
	const size = 10000

	answers := make([]map[string]bool, 0, 2)
	for _, indexed := range []bool{false, true} {
		dir, err := os.MkdirTemp("", "gmpath")
		if err != nil {
			t.Fatal(err)
		}
		store, _ := buildScaledCfg(t, dir, size, memory.StoreConfig{
			StemKeywords:       true,
			CandidateRetrieval: indexed,
		})
		got := map[string]bool{}
		for _, f := range families {
			res, err := store.Recall(ctx, agentID, f.Query, topK)
			if err != nil {
				t.Fatal(err)
			}
			got[f.ID] = strings.Contains(
				strings.ToLower(strings.Join(res, "\n")), strings.ToLower(f.Correct))
		}
		answers = append(answers, got)
		_ = store.Close()
		_ = os.RemoveAll(dir)
	}

	for _, f := range families {
		if answers[0][f.ID] != answers[1][f.ID] {
			t.Errorf("%s: scan=%v indexed=%v — both paths must answer the same with stemming",
				f.ID, answers[0][f.ID], answers[1][f.ID])
		}
	}
}
