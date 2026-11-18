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

func TestRetentionRecallCompletionOrder(t *testing.T) {
	ctx := context.Background()
	const halfLife = 720 * time.Hour
	const query = "ochre semaphore certificate"
	older := lineageTime
	newer := older.Add(time.Hour)
	boundary := time.Duration(float64(halfLife) * math.Log2(100))
	for _, path := range []struct {
		name    string
		indexed bool
	}{{name: "scan"}, {name: "index", indexed: true}} {
		for _, order := range []struct {
			name    string
			reverse bool
		}{{name: "forward"}, {name: "reverse", reverse: true}} {
			t.Run(path.name+"/"+order.name, func(t *testing.T) {
				dir := t.TempDir()
				s := openLineageStore(t, dir, path.indexed)
				now := older.Add(-201 * 24 * time.Hour)
				s.now = func() time.Time { return now }
				f := putLineageFact(t, s, "unique "+query)
				now = older
				decayed, err := s.decayFact(lineageAgent, f.ID, now, math.Ln2/halfLife.Hours())
				if err != nil || decayed.Weight >= minimumFactWeight {
					t.Fatalf("initial decay = %+v, %v", decayed, err)
				}

				recall := func() {
					t.Helper()
					got, err := s.Recall(ctx, lineageAgent, query, 1)
					if err != nil || len(got) != 1 || got[0] != f.Text {
						t.Fatalf("recall = %q, %v", got, err)
					}
				}
				if order.reverse {
					// Complete a second recall before the first records access.
					s.debugRanking = func(string, []scored) {
						s.debugRanking = nil
						now = newer
						recall()
					}
					recall()
				} else {
					recall()
					now = newer
					recall()
				}

				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s = openLineageStore(t, dir, path.indexed)
				s.now = func() time.Time { return now }
				current, ok := s.loadFact(lineageAgent, f.ID)
				if !ok || current.AccessCount != 2 || current.Weight != decayed.Weight {
					t.Fatalf("reopened row after two recalls = %+v", current)
				}
				if !current.AccessedAt.Equal(newer) {
					t.Errorf("reopened last access = %s, want %s", current.AccessedAt, newer)
				}

				cfg := &testConsolidateCfg{halfLife: halfLife}
				now = older.Add(boundary + 30*time.Minute)
				if err := s.Consolidate(ctx, lineageAgent, cfg); err != nil {
					t.Fatal(err)
				}
				if _, ok := s.loadFact(lineageAgent, f.ID); !ok {
					t.Fatal("maintenance pruned the fact before the latest access cutoff")
				}
				now = newer.Add(boundary + 30*time.Minute)
				if err := s.Consolidate(ctx, lineageAgent, cfg); err != nil {
					t.Fatal(err)
				}
				if _, ok := s.loadFact(lineageAgent, f.ID); ok {
					t.Fatal("maintenance retained the fact after the latest access cutoff")
				}
			})
		}
	}
}

func TestRetentionAccessClockRollback(t *testing.T) {
	ctx := context.Background()
	const query = "saffron certificate renewal"
	for _, path := range []struct {
		name    string
		indexed bool
	}{{name: "scan"}, {name: "index", indexed: true}} {
		t.Run(path.name, func(t *testing.T) {
			s := openLineageStore(t, t.TempDir(), path.indexed)
			f := putLineageFact(t, s, "unique "+query)
			newest := lineageTime.Add(2 * time.Hour)
			now := newest
			s.now = func() time.Time { return now }
			checkAccess := func(surface string, count int) {
				t.Helper()
				got, ok := s.loadFact(lineageAgent, f.ID)
				if !ok || got.AccessCount != count || !got.AccessedAt.Equal(newest) || got.Weight != f.Weight {
					t.Errorf("%s row = %+v, want count %d, access %s, weight %g", surface, got, count, newest, f.Weight)
				}
			}

			if got, err := s.Recall(ctx, lineageAgent, query, 1); err != nil || len(got) != 1 || got[0] != f.Text {
				t.Fatalf("recall = %q, %v", got, err)
			}
			checkAccess("recall", 1)
			if got, err := s.RecallExplain(ctx, lineageAgent, query, 1); err != nil || len(got) != 1 {
				t.Fatalf("explain = %v, %v", got, err)
			}
			checkAccess("equal-time explain", 2)

			now = newest.Add(-time.Hour)
			if got, err := s.BatchRecall(ctx, lineageAgent, []string{query}, 1); err != nil ||
				len(got) != 1 || got[0].Err != nil || len(got[0].Facts) != 1 || got[0].Facts[0] != f.Text {
				t.Fatalf("batch = %v, %v", got, err)
			}
			checkAccess("rollback batch", 3)

			now = newest.Add(-2 * time.Hour)
			if got, err := s.RecallAll(ctx, lineageAgent, query, 1); err != nil || len(got) != 1 || got[0] != f.Text {
				t.Fatalf("recall all = %q, %v", got, err)
			}
			checkAccess("rollback recall all", 4)

			now = newest.Add(time.Hour)
			if got, err := s.RecallPreview(ctx, lineageAgent, query, 1); err != nil || len(got) != 1 {
				t.Fatalf("preview = %v, %v", got, err)
			}
			checkAccess("preview", 4)
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
