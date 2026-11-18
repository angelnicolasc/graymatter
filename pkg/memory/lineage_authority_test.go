package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	bolt "go.etcd.io/bbolt"
)

const lineageAgent = "lineage-fixture"

var lineageTime = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

func openLineageStore(t testing.TB, dir string, indexed bool) *Store {
	t.Helper()
	s, err := Open(StoreConfig{DataDir: dir, Embedder: embedding.NewKeyword(), CandidateRetrieval: indexed})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return lineageTime }
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func putLineageFact(t testing.TB, s *Store, text string) Fact {
	t.Helper()
	f, err := s.PutReturningFact(context.Background(), lineageAgent, text)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func reviseLineageFact(t testing.TB, s *Store, text string, sources ...Fact) Fact {
	t.Helper()
	f, err := s.ReviseFactsWithOptions(context.Background(), lineageAgent, text, WriteOptions{}, sources...)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func pruneLineageRow(t testing.TB, s *Store, id string) {
	t.Helper()
	f, ok := s.loadFact(lineageAgent, id)
	if !ok {
		t.Fatalf("missing row before prune: %s", id)
	}
	f.Weight = 0
	if err := s.UpdateFact(lineageAgent, f); err != nil {
		t.Fatal(err)
	}
	if err := s.pruneFact(lineageAgent, id, f.AccessedAt.Add(201*24*time.Hour), math.Ln2/720); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.loadFact(lineageAgent, id); ok {
		t.Fatalf("prune retained canonical row: %s", id)
	}
}

func inspectVersion(t testing.TB, s *Store, f Fact) LineageInspection {
	t.Helper()
	out, err := s.InspectLineage(context.Background(), f.AgentID, f.ID, NewEvidenceSnapshot(f).ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLineageL01L04L11AtomicWritersAndPruning(t *testing.T) {
	ctx := context.Background()
	for _, action := range []string{"revise", "curate"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			s := openLineageStore(t, dir, true)
			pruned := putLineageFact(t, s, "prune first")
			pruneLineageRow(t, s, pruned.ID)
			before := revisionDBSnapshot(t, s)
			if f, err := s.ReviseFactsWithOptions(ctx, lineageAgent, "cannot replace", WriteOptions{}, pruned); !errors.Is(err, ErrFactChanged) || f.ID != "" {
				t.Fatalf("prune-first revision = %+v, %v", f, err)
			}
			if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
				t.Fatal("prune-first rejection changed durable data")
			}

			source := putLineageFact(t, s, "the protocol uses eight bytes")
			var head Fact
			var err error
			if action == "curate" {
				head, err = s.CurateFact(ctx, lineageAgent, source.ID, "revise", "the protocol uses sixteen bytes", source)
			} else {
				head, err = s.ReviseFactsWithOptions(ctx, lineageAgent, "the protocol uses sixteen bytes", WriteOptions{}, source)
			}
			if err != nil || head.LineageStatus != LineageVerified || !slices.Equal(head.Replaces, []LineageReference{lineageRef(NewEvidenceSnapshot(source))}) {
				t.Fatalf("replacement = %+v, %v", head, err)
			}
			pruneLineageRow(t, s, source.ID)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openLineageStore(t, dir, true)
			historical := inspectVersion(t, s, source)
			if historical.Snapshot == nil || *historical.Snapshot != NewEvidenceSnapshot(source) {
				t.Fatalf("pruned source evidence differs after reopen: %+v", historical)
			}
			stored := inspectVersion(t, s, head)
			if !slices.Equal(stored.Replaces, head.Replaces) || stored.LineageStatus != LineageVerified {
				t.Fatalf("head lost durable replacement relationship: %+v", stored)
			}
			preview, err := s.RecallPreview(ctx, lineageAgent, "protocol", 4)
			if err != nil || len(preview) != 1 || !slices.Equal(preview[0].Provenance.Supersedes, []string{source.ID}) {
				t.Fatalf("preview lost source or returned retired content: %+v, %v", preview, err)
			}
		})
	}
}

func TestLineageL05L17ProjectionsAndMetadataWriters(t *testing.T) {
	ctx := context.Background()
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed-%t", indexed), func(t *testing.T) {
			s := openLineageStore(t, t.TempDir(), indexed)
			source := putLineageFact(t, s, "warehouse inventory earlier")
			head := reviseLineageFact(t, s, "warehouse inventory verified", source)
			authority := func() []revisionBucketEntry {
				var rows []revisionBucketEntry
				for _, entry := range revisionDBSnapshot(t, s) {
					if strings.HasPrefix(entry.path, "evidence_") || strings.HasPrefix(entry.path, "lineage_") {
						rows = append(rows, entry)
					}
				}
				return rows
			}
			initial := authority()
			if _, err := s.Recall(ctx, lineageAgent, "warehouse", 4); err != nil {
				t.Fatal(err)
			}
			for _, pin := range []bool{true, false} {
				if err := s.SetPinned(lineageAgent, pin, head); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.decayFact(lineageAgent, head.ID, lineageTime.Add(24*time.Hour), 0.1); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(initial, authority()) {
				t.Fatal("access, decay or pin metadata changed evidence")
			}
			// Emulate an old writer omitting the projection fields in canonical JSON.
			if err := s.db.Update(func(tx *bolt.Tx) error {
				b := lineageRowBucket(tx, lineageAgent)
				raw, err := unmarshalFact(b.Get([]byte(head.ID)))
				if err != nil {
					return err
				}
				raw.LineageProjection = LineageProjection{}
				data, err := raw.marshal()
				if err != nil {
					return err
				}
				return b.Put([]byte(raw.ID), data)
			}); err != nil {
				t.Fatal(err)
			}
			path := s.db.Path()
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			full, err := s.List(lineageAgent)
			if err != nil {
				t.Fatal(err)
			}
			lite, err := s.listLite(lineageAgent)
			if err != nil {
				t.Fatal(err)
			}
			page, err := s.ListFacts(ctx, lineageAgent, "active", "", "", 1)
			if err != nil {
				t.Fatal(err)
			}
			preview, err := s.RecallPreview(ctx, lineageAgent, "warehouse", 4)
			if err != nil || len(preview) != 1 || len(page.Facts) != 1 {
				t.Fatalf("preview or page unavailable: %+v, %+v, %v", preview, page, err)
			}
			for _, projection := range []LineageProjection{lineageProjectionFor(full, head.ID), lineageProjectionFor(lite, head.ID), page.Facts[0].LineageProjection, preview[0].Provenance.LineageProjection} {
				if !reflect.DeepEqual(head.LineageProjection, projection) {
					t.Fatalf("projection differs from authority: %+v", projection)
				}
			}
			encoded, err := json.Marshal(page.Facts[0])
			var roundtrip Fact
			if err != nil || json.Unmarshal(encoded, &roundtrip) != nil || !reflect.DeepEqual(head.LineageProjection, roundtrip.LineageProjection) {
				t.Fatalf("JSON projection roundtrip: %s, %v", encoded, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("inspection and preview changed database bytes")
			}
		})
	}
}

func TestLineageL10ExactTraversalForkCycleAndMissing(t *testing.T) {
	s := openLineageStore(t, t.TempDir(), false)
	a := putLineageFact(t, s, "source A")
	b := putLineageFact(t, s, "source B")
	first := putLineageFact(t, s, "historical derivation")
	fork := putLineageFact(t, s, "another derivation")
	aSnapshot, bSnapshot := NewEvidenceSnapshot(a), NewEvidenceSnapshot(b)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		var added int64
		for _, head := range []Fact{first, fork} {
			snapshot := NewEvidenceSnapshot(head)
			record := lineageRecord{
				Head: lineageRef(snapshot), Derived: []LineageReference{lineageRef(aSnapshot), lineageRef(bSnapshot)},
			}
			if err := writeLineageTx(tx, snapshot, []EvidenceSnapshot{aSnapshot, bSnapshot}, record, &added); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	head := reviseLineageFact(t, s, "newest derivation", first, fork)
	for _, f := range []Fact{first, fork, a, b} {
		pruneLineageRow(t, s, f.ID)
	}
	if err := s.sweepLineage(lineageAgent, lineageTime.Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, f := range []Fact{first, fork} {
		if got := inspectVersion(t, s, f); len(got.DerivedFrom) != 2 || got.LineageStatus != LineageVerified {
			t.Fatalf("missing derivation type or fork: %+v", got)
		}
	}
	if _, err := s.InspectLineage(context.Background(), "other", a.ID, aSnapshot.ContentHash); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("namespace lookup leaked evidence: %v", err)
	}
	headSnapshot := NewEvidenceSnapshot(head)
	for _, status := range []string{LineageCycle, LineageMissing} {
		if err := s.db.Update(func(tx *bolt.Tx) error {
			records, _ := evidenceNamespace(tx, bucketLineageRecords, lineageAgent, false)
			record, _, err := lineageRecordAt(records, lineageAgent, NewEvidenceSnapshot(first).key())
			if err != nil {
				return err
			}
			ref := lineageRef(headSnapshot)
			if status == LineageMissing {
				ref.FactID = "missing"
			}
			record.Derived = append([]LineageReference{lineageRef(aSnapshot), lineageRef(bSnapshot)}, ref)
			raw, err := json.Marshal(record)
			if err != nil {
				return err
			}
			return records.Put([]byte(record.Head.key(lineageAgent)), raw)
		}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if got := inspectVersion(t, s, head); got.LineageStatus != status {
				t.Fatalf("walk status = %s, want %s", got.LineageStatus, status)
			}
		}
	}
}

func TestLineageL13FirstObservedOrphanAndGrace(t *testing.T) {
	dir := t.TempDir()
	s := openLineageStore(t, dir, false)
	source := putLineageFact(t, s, "historical limit")
	head := reviseLineageFact(t, s, "current limit", source)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		// A writer without the lineage buckets' contract deletes the row.
		return lineageRowBucket(tx, lineageAgent).Delete([]byte(head.ID))
	}); err != nil {
		t.Fatal(err)
	}
	observe := lineageTime.Add(90 * 24 * time.Hour)
	if err := s.sweepLineage(lineageAgent, observe); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openLineageStore(t, dir, false)
	if err := s.sweepLineage(lineageAgent, observe.Add(evidenceOrphanGrace-time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, f := range []Fact{head, source} {
		if got := inspectVersion(t, s, f); got.Snapshot == nil {
			t.Fatal("evidence disappeared inside grace")
		}
	}
	if err := s.sweepLineage(lineageAgent, observe.Add(evidenceOrphanGrace)); err != nil {
		t.Fatal(err)
	}
	for _, f := range []Fact{head, source} {
		if _, err := s.InspectLineage(context.Background(), lineageAgent, f.ID, NewEvidenceSnapshot(f).ContentHash); !errors.Is(err, ErrFactNotFound) {
			t.Fatalf("grace did not expire for %s: %v", f.ID, err)
		}
	}
}

func TestLineageL15LegacyObservationWithoutFabricatedTypes(t *testing.T) {
	ctx := context.Background()
	s := openLineageStore(t, t.TempDir(), false)
	source := putLineageFact(t, s, "old untyped revision source")
	head := putLineageFact(t, s, "old untyped revision head")
	source.SupersededBy = head.ID
	source.Weight = 0
	if err := s.UpdateFact(lineageAgent, source); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListFacts(ctx, lineageAgent, "active", "", "", 1)
	if err != nil || len(page.Facts) != 1 || page.Facts[0].LineageStatus != LineageLegacyIncomplete || len(page.Facts[0].LegacySources) != 1 ||
		page.Facts[0].LegacySources[0] != (LineageReference{FactID: source.ID}) {
		t.Fatalf("legacy page inferred a relationship or version: %+v, %v", page, err)
	}
	if err := s.Consolidate(ctx, lineageAgent, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.loadFact(lineageAgent, source.ID); ok {
		t.Fatal("ordinary canonical pruning was suppressed")
	}
	got := inspectVersion(t, s, head)
	if got.LineageStatus != LineageLegacyIncomplete || got.CapturedAt == nil || !got.CapturedAt.Equal(lineageTime) ||
		len(got.Replaces) != 0 || len(got.DerivedFrom) != 0 || len(got.LegacySources) != 1 || got.LegacySources[0].ContentHash == "" {
		t.Fatalf("wrong historical capture: %+v", got)
	}
	if _, err := s.InspectLineage(ctx, lineageAgent, "missing", ""); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("missing historical text was manufactured: %v", err)
	}
}

func TestLineageL16BackupRestoreAndFactsOnlyRefusal(t *testing.T) {
	ctx := context.Background()
	s := openLineageStore(t, t.TempDir(), false)
	source := putLineageFact(t, s, "backup older source")
	head := reviseLineageFact(t, s, "backup corrected source", source)
	pruneLineageRow(t, s, source.ID)
	var image bytes.Buffer
	if err := s.Backup(ctx, &image); err != nil {
		t.Fatal(err)
	}
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, filepath.Base(s.db.Path()))
	if err := RestoreLineageBackup(ctx, bytes.NewReader(image.Bytes()), target); err != nil {
		t.Fatal(err)
	}
	restored := openLineageStore(t, targetDir, false)
	for _, f := range []Fact{source, head} {
		if !reflect.DeepEqual(inspectVersion(t, s, f), inspectVersion(t, restored, f)) {
			t.Fatalf("restore changed evidence for %s", f.ID)
		}
	}
	if err := RestoreLineageBackup(ctx, bytes.NewReader(image.Bytes()), target); err == nil {
		t.Fatal("restore overwrote an existing destination")
	}
	factsOnly, err := json.Marshal([]Fact{head})
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "facts-only.db")
	if err := RestoreLineageBackup(ctx, bytes.NewReader(factsOnly), bad); err == nil {
		t.Fatal("facts-only JSON passed the database restore contract")
	}
	if _, err := os.Stat(bad); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid backup created a destination: %v", err)
	}
}

func TestLineageL19EncodedQuotaRollsBack(t *testing.T) {
	s := openLineageStore(t, t.TempDir(), false)
	source := putLineageFact(t, s, strings.Repeat("\x00", 200_000))
	before := revisionDBSnapshot(t, s)
	f, err := s.ReviseFactsWithOptions(context.Background(), lineageAgent, "small replacement", WriteOptions{}, source)
	if f.ID != "" || !errors.Is(err, ErrEvidenceQuota) || !strings.Contains(err.Error(), "new logical bytes") {
		t.Fatalf("over-budget revision = %s, %v", f.ID, err)
	}
	if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatal("quota rejection changed canonical rows, references or indexes")
	}
}

func BenchmarkLineageL20Preview(b *testing.B) {
	for _, fixture := range []struct {
		name  string
		depth int
		forks int
	}{{"long-chain", 96, 0}, {"forked-history", 24, 48}} {
		b.Run(fixture.name, func(b *testing.B) {
			ctx := context.Background()
			s := openLineageStore(b, b.TempDir(), true)
			s.db.NoSync = true // Keep fixture preparation outside the measurement.
			root := putLineageFact(b, s, "profile lineage original")
			head := root
			for i := range fixture.depth {
				old := head
				head = reviseLineageFact(b, s, fmt.Sprintf("profile lineage revision %d", i), old)
				pruneLineageRow(b, s, old.ID)
			}
			for i := range fixture.forks {
				source := putLineageFact(b, s, fmt.Sprintf("profile lineage fork source %d", i))
				reviseLineageFact(b, s, fmt.Sprintf("profile lineage fork head %d", i), source)
				pruneLineageRow(b, s, source.ID)
			}
			s.db.NoSync = false
			if err := s.db.Sync(); err != nil {
				b.Fatal(err)
			}
			archive, err := s.ExportLineage(ctx, lineageAgent)
			if err != nil {
				b.Fatal(err)
			}
			info, err := os.Stat(s.db.Path())
			if err != nil {
				b.Fatal(err)
			}
			var logical int64
			if err := s.db.View(func(tx *bolt.Tx) error {
				var err error
				logical, _, err = evidencePayloadBytesTx(tx, lineageAgent)
				return err
			}); err != nil {
				b.Fatal(err)
			}
			times := make([]time.Duration, b.N)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range times {
				start := time.Now()
				out, err := s.RecallPreview(ctx, lineageAgent, "profile lineage", 8)
				if err != nil || len(out) == 0 {
					b.Fatalf("preview = %+v, %v", out, err)
				}
				times[i] = time.Since(start)
			}
			b.StopTimer()
			slices.Sort(times)
			b.ReportMetric(float64(times[(95*len(times)-1)/100].Nanoseconds()), "p95-ns")
			b.ReportMetric(float64(logical), "evidence-B")
			b.ReportMetric(float64(info.Size()), "database-B")
			b.ReportMetric(float64(len(archive.Evidence)), "snapshots")
			b.ReportMetric(float64(fixture.depth), "chain-depth")
			b.ReportMetric(float64(fixture.forks), "additional-heads")
		})
	}
}

func lineageProjectionFor(facts []Fact, id string) LineageProjection {
	for _, fact := range facts {
		if fact.ID == id {
			return fact.LineageProjection
		}
	}
	return LineageProjection{}
}

func TestLineageL03RefusesStaleSelectedConfidence(t *testing.T) {
	s := openLineageStore(t, t.TempDir(), false)
	old := putLineageFact(t, s, "reviewed confidence")
	current := old
	current.Confidence = "unverified"
	if err := s.UpdateFact(lineageAgent, current); err != nil {
		t.Fatal(err)
	}
	before := revisionDBSnapshot(t, s)
	if got, err := s.ReviseFactsWithOptions(context.Background(), lineageAgent, "changed revision", WriteOptions{}, old); got.ID != "" || !errors.Is(err, ErrFactChanged) {
		t.Fatalf("revision accepted changed selected version: %+v, %v", got, err)
	}
	if !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatal("stale selection changed the store")
	}
}

func TestLineageL13MaintenanceDoesNotRecreateExpiredTombstoneEvidence(t *testing.T) {
	s := openLineageStore(t, t.TempDir(), false)
	source := putLineageFact(t, s, "pinned historical source")
	if err := s.SetPinned(lineageAgent, true, source); err != nil {
		t.Fatal(err)
	}
	head := reviseLineageFact(t, s, "pinned replacement", source)
	if err := s.SetPinned(lineageAgent, true, head); err != nil {
		t.Fatal(err)
	}
	if err := s.Retire(lineageAgent, head); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return lineageTime.Add(evidenceOrphanGrace + time.Hour) }
	for range 2 {
		if err := s.Consolidate(context.Background(), lineageAgent, defaultTestCfg()); err != nil {
			t.Fatal(err)
		}
		for _, f := range []Fact{head, source} {
			if _, ok := s.loadFact(lineageAgent, f.ID); !ok {
				t.Fatal("pinned canonical tombstone was removed")
			}
			if _, err := s.InspectLineage(context.Background(), lineageAgent, f.ID, NewEvidenceSnapshot(f).ContentHash); !errors.Is(err, ErrFactNotFound) {
				t.Fatalf("maintenance resurrected expired evidence: %v", err)
			}
		}
	}
}

func TestLineageL19LegacyQuotaPreservesSourcesAndOtherMaintenance(t *testing.T) {
	s := openLineageStore(t, t.TempDir(), false)
	legacy := putLineageFact(t, s, strings.Repeat("\x00", 200_000))
	head := putLineageFact(t, s, "historical head")
	legacy.SupersededBy = head.ID
	legacy.Weight = 0
	if err := s.UpdateFact(lineageAgent, legacy); err != nil {
		t.Fatal(err)
	}
	unrelated := putLineageFact(t, s, "unrelated low weight")
	unrelated.Weight = 0
	unrelated.AccessedAt = lineageTime.Add(-201 * 24 * time.Hour)
	if err := s.UpdateFact(lineageAgent, unrelated); err != nil {
		t.Fatal(err)
	}
	err := s.Consolidate(context.Background(), lineageAgent, defaultTestCfg())
	if !errors.Is(err, ErrEvidenceQuota) {
		t.Fatalf("maintenance did not report legacy backpressure: %v", err)
	}
	if _, ok := s.loadFact(lineageAgent, legacy.ID); !ok {
		t.Fatal("uncaptured historical evidence was pruned")
	}
	if _, ok := s.loadFact(lineageAgent, unrelated.ID); ok {
		t.Fatal("legacy backpressure blocked unrelated pruning")
	}
}

func TestLineageL16RejectsCorruptDurableHistory(t *testing.T) {
	for _, mode := range []string{"unversioned-reference", "cycle", "fabricated-owner"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s := openLineageStore(t, t.TempDir(), false)
			source := putLineageFact(t, s, "corrupt backup earlier")
			head := reviseLineageFact(t, s, "corrupt backup later", source)
			err := s.db.Update(func(tx *bolt.Tx) error {
				headKey := NewEvidenceSnapshot(head).key()
				if mode == "fabricated-owner" {
					owners, _ := evidenceNamespace(tx, bucketEvidenceOwners, lineageAgent, false)
					return owners.Bucket([]byte(headKey)).Put([]byte(lineageOwnerPrefix+"fabricated"), []byte{1})
				}
				records, _ := evidenceNamespace(tx, bucketLineageRecords, lineageAgent, false)
				record, _, err := lineageRecordAt(records, lineageAgent, headKey)
				if err != nil {
					return err
				}
				if mode == "cycle" {
					record.Replaces[0] = record.Head
				} else {
					record.Replaces[0].ContentHash = ""
				}
				raw, err := json.Marshal(record)
				if err != nil {
					return err
				}
				return records.Put([]byte(headKey), raw)
			})
			if err != nil {
				t.Fatal(err)
			}
			var image bytes.Buffer
			if err := s.Backup(ctx, &image); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), "corrupt.db")
			if err := RestoreLineageBackup(ctx, &image, dst); err == nil {
				t.Fatal("corrupted authority passed complete restore")
			}
			if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("corrupt restore created a destination: %v", err)
			}
		})
	}
}

func TestLineageL05PinnedOwnedEvidenceMaintenanceIsReadOnly(t *testing.T) {
	s := openLineageStore(t, t.TempDir(), false)
	source := putLineageFact(t, s, "pinned source")
	if err := s.SetPinned(lineageAgent, true, source); err != nil {
		t.Fatal(err)
	}
	head := reviseLineageFact(t, s, "pinned derived evidence", source)
	if err := s.SetPinned(lineageAgent, true, head); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.db.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Consolidate(context.Background(), lineageAgent, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(s.db.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("no-op maintenance rewrote pinned facts or their retained evidence")
	}
}
