package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The scenario these tests defend against, in full:
//
// An agent learns "we use Lemon Squeezy for billing". Months later the company
// migrates and the agent calls memory_reflect with action=update to correct
// it. The tool reports success. On the next recall the agent is handed both
// statements and has no way to tell which one is current.
//
// Before v0.10.0 that is exactly what happened. memory_reflect's update and
// forget actions set Weight = 0 and reported "Fact suppressed", and Recall
// never read Weight — it ranks by vector, keyword and recency only. The
// retired fact kept being returned until a consolidation cycle happened to
// prune it, which requires the agent to have crossed ConsolidateThreshold
// facts. On a store below the threshold it was returned forever.

const (
	deadFact = "We use Lemon Squeezy for billing and payments"
	liveFact = "We use Polar for billing and payments"
)

// TestRecall_ExcludesSupersededFact is the regression test for that scenario.
func TestRecall_ExcludesSupersededFact(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := s.Put(ctx, "billing", deadFact); err != nil {
		t.Fatalf("put dead: %v", err)
	}
	if err := s.Put(ctx, "billing", liveFact); err != nil {
		t.Fatalf("put live: %v", err)
	}

	supersede(t, s, "billing", deadFact, "the Polar fact")

	got, err := s.Recall(ctx, "billing", "billing provider", 8)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	assertAbsent(t, got, "Lemon Squeezy")
	assertPresent(t, got, "Polar")
}

// TestRecall_SupersededExcludedRegardlessOfWeight pins the precedence rule
// between the three mechanisms that can end a fact's life. A tombstone wins
// over weight: a superseded fact stays out of retrieval even at full weight,
// which is what makes the exclusion immediate rather than dependent on when
// decay and pruning next run.
func TestRecall_SupersededExcludedRegardlessOfWeight(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := s.Put(ctx, "weights", deadFact); err != nil {
		t.Fatalf("put: %v", err)
	}
	facts, err := s.List("weights")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	facts[0].SupersededBy = "some-newer-fact-id"
	facts[0].Weight = 1.0 // maximum weight, freshly created
	if err := s.UpdateFact("weights", facts[0]); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}

	got, err := s.Recall(ctx, "weights", "billing provider", 8)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a superseded fact at weight 1.0 was recalled: %v", got)
	}
}

// TestList_RetainsSupersededFact holds the append-only promise the README
// makes about storage. A contradiction is not a deletion: the tombstoned fact
// is still in the store, still visible to List, export and the TUI. Only
// retrieval skips it, and only pruning by decay ever removes it.
func TestList_RetainsSupersededFact(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := s.Put(ctx, "history", deadFact); err != nil {
		t.Fatalf("put: %v", err)
	}
	supersede(t, s, "history", deadFact, "replacement")

	facts, err := s.List("history")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected the superseded fact to survive in the store, got %d facts", len(facts))
	}
	if facts[0].SupersededBy == "" {
		t.Error("SupersededBy did not persist through the store round-trip")
	}
	if facts[0].Text != deadFact {
		t.Errorf("stored text changed: %q", facts[0].Text)
	}
}

// TestRecallShared_ExcludesSupersededFact covers the shared namespace, which
// reaches retrieval through the same path.
func TestRecallShared_ExcludesSupersededFact(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := s.PutShared(ctx, deadFact); err != nil {
		t.Fatalf("PutShared: %v", err)
	}
	supersede(t, s, SharedAgentID, deadFact, "replacement")

	got, err := s.RecallShared(ctx, "billing provider", 8)
	if err != nil {
		t.Fatalf("RecallShared: %v", err)
	}
	assertAbsent(t, got, "Lemon Squeezy")
}

// TestRecallAll_ExcludesSupersededFact covers the merged agent+shared path.
func TestRecallAll_ExcludesSupersededFact(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := s.PutShared(ctx, deadFact); err != nil {
		t.Fatalf("PutShared: %v", err)
	}
	if err := s.Put(ctx, "merged", liveFact); err != nil {
		t.Fatalf("Put: %v", err)
	}
	supersede(t, s, SharedAgentID, deadFact, "replacement")

	got, err := s.RecallAll(ctx, "merged", "billing provider", 8)
	if err != nil {
		t.Fatalf("RecallAll: %v", err)
	}
	assertAbsent(t, got, "Lemon Squeezy")
	assertPresent(t, got, "Polar")
}

// TestConsolidate_DecayAndPruneStillOwnSupersededFacts completes the
// precedence rule from the other end. The tombstone governs retrieval and
// nothing else: decay keeps running on a superseded fact, and pruning is
// still what finally removes it. One mechanism per question, no overlap.
func TestConsolidate_DecayAndPruneStillOwnSupersededFacts(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ctx := context.Background()

	if err := s.Put(ctx, "decayed", deadFact); err != nil {
		t.Fatalf("put: %v", err)
	}
	facts, _ := s.List("decayed")
	f := facts[0]
	f.SupersededBy = "replacement"
	// Backdate well past the pruning horizon: many half-lives of no access.
	f.AccessedAt = time.Now().UTC().Add(-365 * 24 * time.Hour)
	if err := s.UpdateFact("decayed", f); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}

	if err := s.Consolidate(ctx, "decayed", defaultTestCfg()); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	remaining, err := s.List("decayed")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected pruning to remove the decayed superseded fact, %d remain (weight %v)",
			len(remaining), remaining[0].Weight)
	}
}

// TestFact_SupersededByIsBackwardCompatible checks the field is additive on
// disk. Stores written by earlier versions have no superseded_by key, and must
// load as live facts rather than failing or arriving tombstoned.
func TestFact_SupersededByIsBackwardCompatible(t *testing.T) {
	// A fact exactly as v0.9.0 and earlier serialised it.
	legacy := `{"id":"01H000000000000000000000","agent_id":"a","text":"old fact",` +
		`"created_at":"2026-01-01T00:00:00Z","accessed_at":"2026-01-01T00:00:00Z",` +
		`"access_count":3,"weight":0.8}`

	f, err := unmarshalFact([]byte(legacy))
	if err != nil {
		t.Fatalf("a v0.9.0 fact no longer unmarshals: %v", err)
	}
	if f.SupersededBy != "" {
		t.Errorf("legacy fact loaded as superseded (%q); every stored fact would vanish from recall", f.SupersededBy)
	}
	if f.Text != "old fact" || f.AccessCount != 3 {
		t.Errorf("legacy fields did not survive: %+v", f)
	}

	// And a live fact must not start writing the key.
	raw, err := Fact{ID: "x", Text: "t"}.marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "superseded_by") {
		t.Errorf("omitempty is not holding; live facts write the key: %s", raw)
	}

	// A tombstoned fact must round-trip.
	raw, err = Fact{ID: "x", Text: "t", SupersededBy: "y"}.marshal()
	if err != nil {
		t.Fatalf("marshal tombstoned: %v", err)
	}
	var back Fact
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal tombstoned: %v", err)
	}
	if back.SupersededBy != "y" {
		t.Errorf("SupersededBy did not round-trip: %+v", back)
	}
}

// --- helpers ---

// supersede tombstones the fact whose text matches, the way a caller is
// expected to: read, set the field, write it back through UpdateFact.
func supersede(t *testing.T, s *Store, agentID, text, by string) {
	t.Helper()
	facts, err := s.List(agentID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, f := range facts {
		if f.Text == text {
			f.SupersededBy = by
			if err := s.UpdateFact(agentID, f); err != nil {
				t.Fatalf("UpdateFact: %v", err)
			}
			return
		}
	}
	t.Fatalf("fact to supersede not found: %q", text)
}

func assertAbsent(t *testing.T, got []string, needle string) {
	t.Helper()
	for _, g := range got {
		if strings.Contains(g, needle) {
			t.Errorf("superseded fact still recalled: %q\nfull result: %v", g, got)
		}
	}
}

func assertPresent(t *testing.T, got []string, needle string) {
	t.Helper()
	for _, g := range got {
		if strings.Contains(g, needle) {
			return
		}
	}
	t.Errorf("expected a fact containing %q, got %v", needle, got)
}
