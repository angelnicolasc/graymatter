package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestLineageLegacyGCBeforeAndAfterCapture(t *testing.T) {
	ctx := context.Background()
	for _, captureFirst := range []bool{false, true} {
		name := "gc-before-capture"
		if captureFirst {
			name = "capture-before-gc"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := openLineageStore(t, dir, true)
			a := putLineageFact(t, s, "original source A")
			b := reviseLineageFact(t, s, "historical replacement B", a)
			c := reviseLineageFact(t, s, "versioned head C", b)
			pruneLineageRow(t, s, a.ID)
			pruneLineageRow(t, s, b.ID)
			d := putLineageFact(t, s, "legacy replacement D")
			replaceWithoutLineage(t, s, c, d)
			if captureFirst {
				if err := s.Consolidate(ctx, lineageAgent, defaultTestCfg()); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			s = openLineageStore(t, dir, true)
			now := lineageTime.Add(evidenceOrphanGrace + time.Second)
			s.now = func() time.Time { return now }
			// Delete exercises collection without running maintenance.
			unrelated := putLineageFact(t, s, "unrelated source")
			unrelated = reviseLineageFact(t, s, "unrelated revision", unrelated)
			if err := s.Delete(lineageAgent, unrelated.ID); err != nil {
				t.Fatal(err)
			}
			assertLegacyVersions(t, s, a, b, c)
			if !captureFirst {
				if err := s.Consolidate(ctx, lineageAgent, defaultTestCfg()); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			s = openLineageStore(t, dir, true)
			s.now = func() time.Time { return now }
			captured := inspectVersion(t, s, d)
			if captured.LineageStatus != LineageLegacyIncomplete || len(captured.LegacySources) != 1 ||
				captured.LegacySources[0] != lineageRef(NewEvidenceSnapshot(c)) ||
				len(captured.Replaces)+len(captured.DerivedFrom) != 0 {
				t.Fatalf("captured ambiguous relationship: %+v", captured)
			}
			assertLegacyVersions(t, s, a, b, c)

			if err := s.Delete(lineageAgent, d.ID); err != nil {
				t.Fatal(err)
			}
			assertLegacyVersions(t, s, a, b, c)
			if err := s.sweepLineage(lineageAgent, now.Add(evidenceOrphanGrace+time.Second)); err != nil {
				t.Fatal(err)
			}
			assertExpiredLegacyVersions(t, s, a, b, c)
		})
	}
}

func TestLineageLegacyGCVersionedSuccessorAndIndependentRoots(t *testing.T) {
	ctx := context.Background()
	s := openLineageStore(t, t.TempDir(), false)
	originA := putLineageFact(t, s, "ancestor A")
	headA := reviseLineageFact(t, s, "head A", originA)
	originB := putLineageFact(t, s, "ancestor B")
	headB := reviseLineageFact(t, s, "head B", originB)
	pruneLineageRow(t, s, originA.ID)
	pruneLineageRow(t, s, originB.ID)
	middle := putLineageFact(t, s, "independent source for versioned successor")
	middle = reviseLineageFact(t, s, "already versioned successor", middle)
	replaceWithoutLineage(t, s, headA, middle)

	lastA := putLineageFact(t, s, "live successor A")
	lastB := putLineageFact(t, s, "live successor B")
	replaceWithoutLineage(t, s, middle, lastA)
	replaceWithoutLineage(t, s, headB, lastB)

	now := lineageTime.Add(evidenceOrphanGrace + time.Second)
	s.now = func() time.Time { return now }
	if err := s.Consolidate(ctx, lineageAgent, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	if f, ok := s.loadFact(lineageAgent, headA.ID); !ok || f.SupersededBy != middle.ID {
		t.Fatalf("maintenance pruned the uncaptured bridge: %+v", f)
	}
	assertLegacyVersions(t, s, originA, headA, originB, headB)
	if err := s.Delete(lineageAgent, lastA.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.sweepLineage(lineageAgent, now.Add(evidenceOrphanGrace+time.Second)); err != nil {
		t.Fatal(err)
	}
	assertExpiredLegacyVersions(t, s, originA, headA)
	assertLegacyVersions(t, s, originB, headB)
}

func TestLineageLegacyGCQuotaKeepsUncapturedHistory(t *testing.T) {
	ctx := context.Background()
	s := openLineageStore(t, t.TempDir(), false)
	a := putLineageFact(t, s, "pruned ancestor")
	b := reviseLineageFact(t, s, "versioned predecessor", a)
	pruneLineageRow(t, s, a.ID)
	c := putLineageFact(t, s, "live legacy successor")
	replaceWithoutLineage(t, s, b, c)
	oversized := putLineageFact(t, s, strings.Repeat("\x00", 200_000))
	replaceWithoutLineage(t, s, oversized, c)
	s.now = func() time.Time { return lineageTime.Add(evidenceOrphanGrace + time.Second) }
	if err := s.Consolidate(ctx, lineageAgent, defaultTestCfg()); !errors.Is(err, ErrEvidenceQuota) {
		t.Fatalf("expected quota backpressure: %v", err)
	}
	if got, err := s.InspectLineage(ctx, lineageAgent, c.ID, ""); err != nil ||
		got.LineageStatus != LineageLegacyIncomplete || got.CapturedAt != nil {
		t.Fatalf("failed capture reported a versioned result: %+v, %v", got, err)
	}
	if _, ok := s.loadFact(lineageAgent, b.ID); !ok {
		t.Fatal("quota failure pruned the uncaptured bridge")
	}
	assertLegacyVersions(t, s, a, b)
}

func replaceWithoutLineage(t *testing.T, s *Store, from, to Fact) {
	t.Helper()
	from.SupersededBy = to.ID
	from.Weight = 0
	from.LineageProjection = LineageProjection{}
	if err := s.UpdateFact(lineageAgent, from); err != nil {
		t.Fatal(err)
	}
}

func assertLegacyVersions(t *testing.T, s *Store, facts ...Fact) {
	t.Helper()
	for _, fact := range facts {
		got := inspectVersion(t, s, fact)
		if got.Snapshot == nil || got.Snapshot.Text != fact.Text {
			t.Fatalf("incorrect historical snapshot for %s: %+v", fact.ID, got)
		}
	}
}

func assertExpiredLegacyVersions(t *testing.T, s *Store, facts ...Fact) {
	t.Helper()
	for _, fact := range facts {
		_, err := s.InspectLineage(context.Background(), lineageAgent, fact.ID, NewEvidenceSnapshot(fact).ContentHash)
		if !errors.Is(err, ErrFactNotFound) {
			t.Fatalf("orphan %s survived grace: %v", fact.ID, err)
		}
	}
}

func TestLineageL09DivergentLiveHeadRetainsHistoryThroughGrace(t *testing.T) {
	ctx := context.Background()
	s := openLineageStore(t, t.TempDir(), false)
	source := putLineageFact(t, s, "original lineage evidence")
	head := reviseLineageFact(t, s, "established lineage head", source)
	old := NewEvidenceSnapshot(head)
	current := head
	current.Text = "divergent content from an older writer"
	current.LineageProjection = LineageProjection{}
	raw, err := current.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return lineageRowBucket(tx, lineageAgent).Put([]byte(current.ID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	later := lineageTime.Add(2 * evidenceOrphanGrace)
	if err := s.sweepLineage(lineageAgent, later); err != nil {
		t.Fatal(err)
	}
	got, err := s.InspectLineage(ctx, lineageAgent, current.ID, "")
	if err != nil || got.LineageStatus != LineageContentChanged || got.HistoricalHash != old.ContentHash ||
		got.ContentHash != NewEvidenceSnapshot(current).ContentHash || len(got.Replaces) != 0 {
		t.Fatalf("divergent canonical projection = %+v, %v", got, err)
	}
	if inspectVersion(t, s, head).Snapshot.Text != head.Text || inspectVersion(t, s, source).Snapshot.Text != source.Text {
		t.Fatal("live divergence lost historical versions")
	}
	s.now = func() time.Time { return later }
	replacement := reviseLineageFact(t, s, "new explicitly revised head", current)
	if len(replacement.Replaces) != 1 || replacement.Replaces[0] != lineageRef(NewEvidenceSnapshot(current)) {
		t.Fatalf("new revision inherited an older source version: %+v", replacement.Replaces)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		snapshots, _ := evidenceNamespace(tx, bucketEvidenceSnapshots, lineageAgent, false)
		record, err := evidenceEntry(snapshots, old.key())
		if err != nil {
			return err
		}
		if record.OrphanSince == nil || !record.OrphanSince.Equal(later) {
			t.Fatalf("known root loss did not establish grace: %+v", record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.sweepLineage(lineageAgent, later.Add(evidenceOrphanGrace)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InspectLineage(ctx, lineageAgent, head.ID, old.ContentHash); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("unowned historical version remained past grace: %v", err)
	}
	if inspectVersion(t, s, current).Snapshot.Text != current.Text {
		t.Fatal("new revision lost its exact source version")
	}
}

func TestLineageLegacyGCJournalKeepsAnUncapturedPredecessor(t *testing.T) {
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	s.now = func() time.Time { return journalTestTime }
	origin := putLineageFact(t, s, "original ancestral version")
	predecessor := reviseLineageFact(t, s, "uncaptured predecessor", origin)
	pruneLineageRow(t, s, origin.ID)
	middle := putLineageFact(t, s, "independent versioned source")
	middle = reviseLineageFact(t, s, "version with independent journal owner", middle)
	witness := putLineageFact(t, s, "journal witness")
	in := reviewFixture("ollama", "fixture", "conflict", middle, witness)
	in.Findings = []ReviewFinding{{Kind: ReviewKindConflict, SourceIDs: []string{middle.ID, witness.ID}, Note: "different observations"}}
	commitReviewFixture(t, s, in)

	replaceWithoutLineage(t, s, predecessor, middle)
	last := putLineageFact(t, s, "eventual legacy successor")
	replaceWithoutLineage(t, s, middle, last)
	if err := s.Delete(lineageAgent, last.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.sweepLineage(lineageAgent, journalTestTime); err != nil {
		t.Fatal(err)
	}
	now := journalTestTime.Add(evidenceOrphanGrace + time.Second)
	s.now = func() time.Time { return now }
	if err := s.Consolidate(context.Background(), lineageAgent, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	assertLegacyVersions(t, s, origin, predecessor, middle)
}
