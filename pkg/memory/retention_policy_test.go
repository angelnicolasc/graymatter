package memory

import (
	"context"
	"math"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestRetentionRecallBetweenDecayAndPrune(t *testing.T) {
	ctx := context.Background()
	for _, indexed := range []bool{false, true} {
		name := "scan"
		if indexed {
			name = "index"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := openLineageStore(t, dir, indexed)
			now := lineageTime
			const halfLife = 720 * time.Hour
			lambda := math.Ln2 / halfLife.Hours()
			s.now = func() time.Time { return now.Add(-201 * 24 * time.Hour) }
			f := putLineageFact(t, s, "required zebra certificate key")
			control := putLineageFact(t, s, "unused prerequisite")
			s.now = func() time.Time { return now }
			decayed, err := s.decayFact(lineageAgent, f.ID, now, lambda)
			if err != nil || decayed.Weight >= minimumFactWeight {
				t.Fatalf("initial decay = %+v, %v", decayed, err)
			}

			got, err := s.Recall(ctx, lineageAgent, "zebra certificate key", 1)
			if err != nil || len(got) != 1 || got[0] != f.Text {
				t.Fatalf("recall = %q, %v", got, err)
			}
			if err := s.pruneFact(lineageAgent, f.ID, now, lambda); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openLineageStore(t, dir, indexed)
			now = now.Add(time.Hour)
			s.now = func() time.Time { return now }
			if err := s.Consolidate(ctx, lineageAgent, &testConsolidateCfg{halfLife: halfLife}); err != nil {
				t.Fatal(err)
			}
			current, ok := s.loadFact(lineageAgent, f.ID)
			if !ok || current.Weight != decayed.Weight || current.AccessCount != 1 || !current.AccessedAt.Equal(lineageTime) {
				t.Fatalf("retained row after reopening and next cycle = %+v", current)
			}
			if _, ok := s.loadFact(lineageAgent, control.ID); ok {
				t.Fatal("unused eligible control survived")
			}
		})
	}
}

func TestRetentionPruneBoundaryAndManualWeight(t *testing.T) {
	now := lineageTime
	const halfLife = 4 * time.Hour
	lambda := math.Ln2 / halfLife.Hours()
	boundary := time.Duration(float64(halfLife) * math.Log2(100))
	cases := []struct {
		name   string
		age    time.Duration
		weight float64
		retire bool
		pin    bool
		prune  bool
	}{
		{name: "just before age crossing", age: boundary - time.Microsecond},
		{name: "just after age crossing", age: boundary + time.Microsecond, prune: true},
		{name: "exact weight floor", age: boundary + time.Hour, weight: minimumFactWeight},
		{name: "manual zero on live recent fact"},
		{name: "manual zero with clock rollback", age: -time.Hour},
		{name: "retirement independent of age", retire: true, prune: true},
		{name: "pin keeps old fact", age: boundary + time.Hour, pin: true},
		{name: "pinned tombstone remains retired", age: boundary + time.Hour, pin: true, retire: true},
	}
	s := openLineageStore(t, t.TempDir(), false)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := putLineageFact(t, s, tc.name)
			f.AccessedAt = now.Add(-tc.age)
			f.Weight = tc.weight
			f.Pinned = tc.pin
			if tc.retire {
				f.SupersededBy = SupersededByAgent
			}
			if err := s.UpdateFact(lineageAgent, f); err != nil {
				t.Fatal(err)
			}
			if err := s.pruneFact(lineageAgent, f.ID, now, lambda); err != nil {
				t.Fatal(err)
			}
			after, present := s.loadFact(lineageAgent, f.ID)
			if present == tc.prune || present && (after.Weight != f.Weight || after.SupersededBy != f.SupersededBy) {
				t.Fatalf("prune outcome present=%v fact=%+v", present, after)
			}
		})
	}
}

func TestRetentionPruneBeforeTouchDoesNotRecreate(t *testing.T) {
	s := openLineageStore(t, t.TempDir(), true)
	now := lineageTime
	s.now = func() time.Time { return now.Add(-201 * 24 * time.Hour) }
	f := putLineageFact(t, s, "retention order control")
	s.now = func() time.Time { return now }
	lambda := math.Ln2 / 720
	decayed, err := s.decayFact(lineageAgent, f.ID, now, lambda)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pruneFact(lineageAgent, f.ID, now, lambda); err != nil {
		t.Fatal(err)
	}
	s.touchFacts([]Fact{decayed})
	if _, ok := s.loadFact(lineageAgent, f.ID); ok {
		t.Fatal("stale touch recreated an already pruned fact")
	}
}

func TestRetentionTouchingSurfacesAndPreview(t *testing.T) {
	ctx := context.Background()
	now := lineageTime
	for _, surface := range []string{"explain", "batch", "all", "preview"} {
		t.Run(surface, func(t *testing.T) {
			s := openLineageStore(t, t.TempDir(), true)
			s.now = func() time.Time { return now.Add(-201 * 24 * time.Hour) }
			f := putLineageFact(t, s, "unique terracotta warranty")
			s.now = func() time.Time { return now }
			lambda := math.Ln2 / 720
			if _, err := s.decayFact(lineageAgent, f.ID, now, lambda); err != nil {
				t.Fatal(err)
			}
			switch surface {
			case "explain":
				if got, err := s.RecallExplain(ctx, lineageAgent, "terracotta warranty", 1); err != nil || len(got) != 1 {
					t.Fatalf("explain: %v, %v", got, err)
				}
			case "batch":
				if got, err := s.BatchRecall(ctx, lineageAgent, []string{"terracotta warranty"}, 1); err != nil || len(got) != 1 {
					t.Fatalf("batch: %v, %v", got, err)
				}
			case "all":
				if got, err := s.RecallAll(ctx, lineageAgent, "terracotta warranty", 1); err != nil || len(got) != 1 {
					t.Fatalf("all: %v, %v", got, err)
				}
			case "preview":
				if got, err := s.RecallPreview(ctx, lineageAgent, "terracotta warranty", 1); err != nil || len(got) != 1 {
					t.Fatalf("preview: %v, %v", got, err)
				}
			}
			if err := s.pruneFact(lineageAgent, f.ID, now, lambda); err != nil {
				t.Fatal(err)
			}
			after, present := s.loadFact(lineageAgent, f.ID)
			if surface == "preview" {
				if present {
					t.Fatal("preview changed pruning eligibility")
				}
			} else if !present || after.AccessCount != 1 || !after.AccessedAt.Equal(now) {
				t.Fatalf("touch did not protect the fact: %+v", after)
			}
		})
	}
}

func TestRetentionTouchIsScopedToNamespace(t *testing.T) {
	s := openLineageStore(t, t.TempDir(), false)
	now := lineageTime
	s.now = func() time.Time { return now.Add(-201 * 24 * time.Hour) }
	first := putLineageFact(t, s, "scoped memory")
	other, err := s.PutReturningFact(context.Background(), "other", "separate scoped memory")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		rows := lineageRowBucket(tx, "other")
		if err := rows.Delete([]byte(other.ID)); err != nil {
			return err
		}
		other.ID = first.ID
		raw, err := other.marshal()
		if err != nil {
			return err
		}
		return rows.Put([]byte(other.ID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	first.AccessedAt = now
	s.touchFacts([]Fact{first})
	lambda := math.Ln2 / 720
	for _, f := range []Fact{first, other} {
		if _, err := s.decayFact(f.AgentID, f.ID, now, lambda); err != nil {
			t.Fatal(err)
		}
		if err := s.pruneFact(f.AgentID, f.ID, now, lambda); err != nil {
			t.Fatal(err)
		}
	}
	if f, ok := s.loadFact(lineageAgent, first.ID); !ok || f.AccessCount != 1 || !f.AccessedAt.Equal(now) {
		t.Fatalf("touch before decay did not preserve its scoped row: %+v", f)
	}
	if _, ok := s.loadFact("other", other.ID); ok {
		t.Fatal("a touch in another namespace protected an unused fact")
	}
}
