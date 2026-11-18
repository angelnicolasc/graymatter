package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func seedMaintenanceFacts(t *testing.T, s *Store, count int) []Fact {
	t.Helper()
	facts := make([]Fact, count)
	err := s.db.Update(func(tx *bolt.Tx) error {
		rows, err := tx.Bucket(bucketFacts).CreateBucketIfNotExists([]byte(lineageAgent))
		if err != nil {
			return err
		}
		for i := range facts {
			id := fmt.Sprintf("fact-%05d", i)
			at := lineageTime.Add(-time.Duration(i%120+1) * 24 * time.Hour)
			f := Fact{ID: id, AgentID: lineageAgent, Text: "reference scope " + id, Weight: 1,
				CreatedAt: at, AccessedAt: at, Confidence: "verified", AccessCount: i % 11}
			facts[i] = f
			if err := s.persistFactTx(tx, rows, lineageAgent, f); err != nil {
				return err
			}
		}
		if s.cfg.CandidateRetrieval {
			return idxBumpCount(tx, lineageAgent, count, s.cfg.StemKeywords)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func TestMaintenanceBatchesMeasureWrites(t *testing.T) {
	s := openLineageStore(t, t.TempDir(), false)
	seedMaintenanceFacts(t, s, 1000)
	now := lineageTime
	s.now = func() time.Time { return now }
	txID := func() int {
		t.Helper()
		var id int
		if err := s.db.View(func(tx *bolt.Tx) error { id = tx.ID(); return nil }); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for _, sample := range []struct {
		name string
		step time.Duration
		want int
	}{
		{"initial", 0, 4}, {"unchanged", 0, 0}, {"advance-second", time.Second, 4},
		{"advance-hour", time.Hour, 4},
	} {
		t.Run(sample.name, func(t *testing.T) {
			now = now.Add(sample.step)
			beforeID := txID()
			before := s.db.Stats().TxStats
			started := time.Now()
			if err := s.Consolidate(context.Background(), lineageAgent, defaultTestCfg()); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(started)
			after := s.db.Stats().TxStats
			delta := after.Sub(&before)
			commits := txID() - beforeID
			t.Logf("rows=1000 commits=%d writeAt_calls=%d allocated_pages=%d allocated_bytes=%d spilled_nodes=%d duration=%s",
				commits, delta.GetWrite(), delta.GetPageCount(), delta.GetPageAlloc(), delta.GetSpill(), elapsed)
			if commits != sample.want || (sample.want == 0 && delta.GetWrite() != 0) {
				t.Fatalf("expected %d commits, got %d", sample.want, commits)
			}
		})
	}
}

func TestMaintenanceBatchesMatchPerRowAfterReopen(t *testing.T) {
	ctx := context.Background()
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprint(indexed), func(t *testing.T) {
			dir := t.TempDir()
			batched := openLineageStore(t, dir, indexed)
			reference := openLineageStore(t, t.TempDir(), indexed)
			seedMaintenanceFacts(t, batched, 260)
			facts := seedMaintenanceFacts(t, reference, 260)
			if indexed {
				if usable, err := batched.idxUsable(lineageAgent); err != nil || !usable {
					t.Fatalf("seeded index must be verified before decay: usable=%v err=%v", usable, err)
				}
			}
			// The reference measures equivalence of state, not durable I/O.
			reference.db.NoSync = true
			now := lineageTime
			for _, elapsed := range []time.Duration{0, time.Second, time.Hour} {
				now = now.Add(elapsed)
				batched.now = func() time.Time { return now }
				if err := batched.Consolidate(ctx, lineageAgent, defaultTestCfg()); err != nil {
					t.Fatal(err)
				}
				for _, f := range facts {
					if _, err := reference.decayFact(lineageAgent, f.ID, now, math.Ln2/720); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := batched.Close(); err != nil {
				t.Fatal(err)
			}
			batched = openLineageStore(t, dir, indexed)
			read := func(s *Store) []Fact {
				t.Helper()
				facts, err := s.List(lineageAgent)
				if err != nil {
					t.Fatal(err)
				}
				slices.SortFunc(facts, func(a, b Fact) int { return strings.Compare(a.ID, b.ID) })
				return facts
			}
			if !reflect.DeepEqual(read(batched), read(reference)) {
				t.Fatal("reopened rows differ from the per-row reference")
			}
			before, err := reference.RecallPreview(ctx, lineageAgent, "fact-00200", 4)
			if err != nil {
				t.Fatal(err)
			}
			after, err := batched.RecallPreview(ctx, lineageAgent, "fact-00200", 4)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("index/scan result differs after reopening: %v", err)
			}
		})
	}
}

type cancelBeforeDecayCommit struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelBeforeDecayCommit) Err() error {
	c.checks++
	if c.checks == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestMaintenanceBatchesRollbackAndResume(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openLineageStore(t, dir, false)
	facts := seedMaintenanceFacts(t, s, maintenanceDecayBatchSize+2)
	lambda := math.Ln2 / 720
	if err := s.decayBatch(ctx, lineageAgent, facts[:maintenanceDecayBatchSize], lineageTime, lambda); err != nil {
		t.Fatal(err)
	}
	tail := facts[maintenanceDecayBatchSize:]
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := s.decayBatch(&cancelBeforeDecayCommit{Context: cancelled, cancel: cancel},
		lineageAgent, tail, lineageTime, lambda); !errors.Is(err, context.Canceled) {
		t.Fatalf("batch cancellation = %v", err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return lineageRowBucket(tx, lineageAgent).Put([]byte(tail[1].ID), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.decayBatch(ctx, lineageAgent, tail, lineageTime, lambda); err == nil ||
		!strings.Contains(err.Error(), tail[1].ID) {
		t.Fatalf("batch failed to report invalid persisted row: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openLineageStore(t, dir, false)
	if first, ok := s.loadFact(lineageAgent, facts[0].ID); !ok || first.Weight >= 1 {
		t.Fatalf("committed prefix lost after reopening: %+v", first)
	}
	if first, ok := s.loadFact(lineageAgent, tail[0].ID); !ok || first.Weight != 1 {
		t.Fatalf("aborted batch partially committed: %+v", first)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return s.persistFactTx(tx, lineageRowBucket(tx, lineageAgent), lineageAgent, tail[1])
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Consolidate(ctx, lineageAgent, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		got, ok := s.loadFact(lineageAgent, f.ID)
		if !ok || got.Weight != weightAtAge(f.AccessedAt, lineageTime, lambda) {
			t.Fatalf("retry did not converge: %+v", got)
		}
	}
}

func TestMaintenanceBatchUsesCurrentRows(t *testing.T) {
	s := openLineageStore(t, t.TempDir(), true)
	facts := seedMaintenanceFacts(t, s, 5)
	if err := s.SetPinned(lineageAgent, true, facts[0]); err != nil {
		t.Fatal(err)
	}
	touched := facts[1]
	touched.AccessedAt = lineageTime
	s.touchFacts([]Fact{touched})
	facts[2].Confidence = "unverified"
	if err := s.UpdateFact(lineageAgent, facts[2]); err != nil {
		t.Fatal(err)
	}
	head := reviseLineageFact(t, s, "new head", facts[3])
	if err := s.Delete(lineageAgent, facts[4].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.decayBatch(context.Background(), lineageAgent, facts, lineageTime, math.Ln2/720); err != nil {
		t.Fatal(err)
	}
	pin, _ := s.loadFact(lineageAgent, facts[0].ID)
	access, _ := s.loadFact(lineageAgent, facts[1].ID)
	confidence, _ := s.loadFact(lineageAgent, facts[2].ID)
	retired, _ := s.loadFact(lineageAgent, facts[3].ID)
	if !pin.Pinned || pin.Weight != 1 || !access.AccessedAt.Equal(lineageTime) ||
		access.AccessCount != touched.AccessCount+1 || confidence.Confidence != "unverified" || retired.SupersededBy != head.ID {
		t.Fatalf("concurrent changes lost: pin=%+v access=%+v confidence=%+v retired=%+v", pin, access, confidence, retired)
	}
	if _, ok := s.loadFact(lineageAgent, facts[4].ID); ok {
		t.Fatal("decay batch reinserted deleted fact")
	}
	assertLegacyVersions(t, s, facts[3], head)
}
