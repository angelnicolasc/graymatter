package memory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

type reviewLineageEvidence struct {
	entry  storedEvidence
	exists bool
	owners map[string]bool
}

func readReviewLineageEvidence(t *testing.T, s *Store, snapshot EvidenceSnapshot) reviewLineageEvidence {
	t.Helper()
	state := reviewLineageEvidence{owners: make(map[string]bool)}
	if err := s.db.View(func(tx *bolt.Tx) error {
		key := []byte(snapshot.key())
		snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, snapshot.Namespace, false)
		if err != nil {
			return err
		}
		if snapshots == nil || snapshots.Get(key) == nil {
			return nil
		}
		state.entry, err = evidenceEntry(snapshots, string(key))
		if err != nil {
			return err
		}
		state.exists = true
		owners, err := evidenceNamespace(tx, bucketEvidenceOwners, snapshot.Namespace, false)
		if err != nil || owners == nil {
			return err
		}
		if rooted := owners.Bucket(key); rooted != nil {
			return rooted.ForEach(func(owner, _ []byte) error {
				state.owners[string(owner)] = true
				return nil
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

func collectReviewLineage(t *testing.T, s *Store, namespace string, now time.Time) {
	t.Helper()
	if err := s.sweepLineage(namespace, now); err != nil {
		t.Fatal(err)
	}
}

func TestReviewLineageL12CollectionRevalidatesJournalReference(t *testing.T) {
	for _, order := range []string{"journal before collection", "collection before journal", "canonical prune before journal"} {
		t.Run(order, func(t *testing.T) {
			ctx := context.Background()
			s := openReviewJournalTestStore(t, t.TempDir(), false)
			now := journalTestTime
			s.now = func() time.Time { return now }
			source := putReviewJournalFact(t, s, "project", "retained source")
			snapshot := NewEvidenceSnapshot(source)
			in := reviewFixture("ollama", "fixture", "earlier", source)
			in.Findings = []ReviewFinding{{Kind: ReviewKindSummary, SourceIDs: []string{source.ID}, Summary: "fixture proposal"}}
			earlier := commitReviewFixture(t, s, in)[0]
			for _, action := range []string{"reject", "purge"} {
				if err := s.ReviewApply(ctx, source.AgentID, earlier.ID, ReviewAction{Action: action}); err != nil {
					t.Fatal(err)
				}
			}
			state := readReviewLineageEvidence(t, s, snapshot)
			if !state.exists || state.entry.OrphanSince == nil || !state.entry.OrphanSince.Equal(now) {
				t.Fatalf("expected first orphan observation: %+v", state)
			}
			now = now.Add(evidenceOrphanGrace)

			// Both contenders start with the same eligible orphan. The
			// barrier chooses the write transaction order independently.
			start := make(chan struct{})
			result := make(chan error, 1)
			if order == "journal before collection" {
				go func() {
					<-start
					result <- s.sweepLineage(source.AgentID, now)
				}()
				in.PromptVersion = "current"
				records, err := s.ReviewCommit(ctx, in)
				close(start)
				collectErr := <-result
				if err != nil || collectErr != nil || len(records) != 1 {
					t.Fatalf("ordered writes: %d records, journal %v, collection %v", len(records), err, collectErr)
				}
				assertReviewLineageRoot(t, s, snapshot, records[0].ID)
				return
			}

			in.PromptVersion = "current"
			var records []ReviewRecord
			go func() {
				<-start
				var err error
				records, err = s.ReviewCommit(ctx, in)
				result <- err
			}()
			defer func() {
				if start != nil {
					close(start)
					<-result
				}
			}()
			if order == "canonical prune before journal" {
				if err := s.Consolidate(ctx, source.AgentID, &testConsolidateCfg{halfLife: time.Hour}); err != nil {
					t.Fatal(err)
				}
			} else {
				collectReviewLineage(t, s, source.AgentID, now)
			}
			if got := readReviewLineageEvidence(t, s, snapshot); got.exists {
				t.Fatalf("eligible orphan still exists: %+v", got)
			}
			close(start)
			err := <-result
			start = nil
			if order == "canonical prune before journal" {
				if !errors.Is(err, ErrFactChanged) || len(records) != 0 {
					t.Fatalf("journal accepted a removed source: %+v, %v", records, err)
				}
				if got := readReviewLineageEvidence(t, s, snapshot); got.exists {
					t.Fatalf("failed journal write recreated evidence: %+v", got)
				}
				return
			}
			if err != nil || len(records) != 1 {
				t.Fatalf("journal could not capture the still-live source: %+v, %v", records, err)
			}
			assertReviewLineageRoot(t, s, snapshot, records[0].ID)
		})
	}
}

func assertReviewLineageRoot(t *testing.T, s *Store, snapshot EvidenceSnapshot, recordID string) {
	t.Helper()
	got := readReviewLineageEvidence(t, s, snapshot)
	if !got.exists || got.entry.Snapshot != snapshot || got.entry.OrphanSince != nil ||
		!got.owners[reviewEvidenceOwnerPrefix+recordID] {
		t.Fatalf("journal evidence is unrooted or changed: %+v", got)
	}
}

func TestReviewLineageL14SharedEvidenceSurvivesCanonicalPrune(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openReviewJournalTestStore(t, dir, false)
	now := journalTestTime
	s.now = func() time.Time { return now }
	const namespace = "project"
	a := putReviewJournalFact(t, s, namespace, "original assertion")
	b := putReviewJournalFact(t, s, namespace, "conflicting assertion")
	in := reviewFixture("ollama", "fixture", "conflict", a, b)
	in.Findings = []ReviewFinding{{Kind: ReviewKindConflict, SourceIDs: []string{a.ID, b.ID}, Note: "contradiction"}}
	conflict := commitReviewFixture(t, s, in)[0]
	aSnapshot := NewEvidenceSnapshot(a)
	usage := func() reviewUsage {
		t.Helper()
		var result reviewUsage
		if err := s.db.View(func(tx *bolt.Tx) error {
			journal, err := reviewBucketsTx(tx, namespace, false)
			if err != nil {
				return err
			}
			result, err = reviewQuotaTx(tx, namespace, journal.records)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return result
	}
	journalBudget := usage()

	first, err := s.ReviseFactsWithOptions(ctx, namespace, "first revision", WriteOptions{}, a)
	if err != nil {
		t.Fatal(err)
	}
	last, err := s.ReviseFactsWithOptions(ctx, namespace, "second revision", WriteOptions{}, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPinned(namespace, true, last); err != nil {
		t.Fatal(err)
	}
	firstSnapshot, lastSnapshot := NewEvidenceSnapshot(first), NewEvidenceSnapshot(last)
	shared := readReviewLineageEvidence(t, s, aSnapshot)
	if shared.entry.Snapshot != aSnapshot || !shared.owners[reviewEvidenceOwnerPrefix+conflict.ID] ||
		!shared.owners[lineageOwnerPrefix+firstSnapshot.key()] || len(shared.owners) != 2 {
		t.Fatalf("review and revision did not share one snapshot: %+v", shared)
	}
	if got := usage(); got != journalBudget {
		t.Fatalf("lineage-only and shared evidence changed journal usage: %+v, want %+v", got, journalBudget)
	}

	now = now.Add(evidenceOrphanGrace + time.Second)
	if err := s.Consolidate(ctx, namespace, &testConsolidateCfg{halfLife: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		for _, id := range []string{a.ID, b.ID, first.ID} {
			if _, err := factInTx(tx, namespace, id); !errors.Is(err, ErrFactChanged) {
				t.Fatalf("canonical row %s survived pruning: %v", id, err)
			}
		}
		pinned, err := factInTx(tx, namespace, last.ID)
		if err != nil || !pinned.Pinned || pinned.IsSuperseded() {
			t.Fatalf("maintenance altered the pinned head: %+v, %v", pinned, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []EvidenceSnapshot{aSnapshot, firstSnapshot, lastSnapshot} {
		if state := readReviewLineageEvidence(t, s, snapshot); !state.exists || state.entry.OrphanSince != nil {
			t.Fatalf("pinned chain did not retain %q: %+v", snapshot.Text, state)
		}
	}

	if err := s.Delete(namespace, last.ID); err != nil {
		t.Fatal(err)
	}
	collectReviewLineage(t, s, namespace, now)
	for _, snapshot := range []EvidenceSnapshot{firstSnapshot, lastSnapshot} {
		got := readReviewLineageEvidence(t, s, snapshot)
		if !got.exists || got.entry.OrphanSince == nil || !got.entry.OrphanSince.Equal(now) || !got.entry.LineageOnly {
			t.Fatalf("lineage orphan did not begin grace for %q: %+v", snapshot.Text, got)
		}
	}
	assertReviewLineageRoot(t, s, aSnapshot, conflict.ID)
	if got := usage(); got != journalBudget {
		t.Fatalf("lineage grace changed journal usage: %+v, want %+v", got, journalBudget)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openReviewJournalTestStore(t, dir, false)
	s.now = func() time.Time { return now }
	now = now.Add(evidenceOrphanGrace)
	collectReviewLineage(t, s, namespace, now)
	for _, snapshot := range []EvidenceSnapshot{firstSnapshot, lastSnapshot} {
		if got := readReviewLineageEvidence(t, s, snapshot); got.exists {
			t.Fatalf("eligible lineage orphan survived for %q: %+v", snapshot.Text, got)
		}
	}
	assertReviewLineageRoot(t, s, aSnapshot, conflict.ID)
	record := inspectReviewFixture(t, s, namespace, conflict.ID)
	if record.Resolution != "unresolved" || len(record.Sources) != 2 {
		t.Fatalf("open conflict lost its references after reopen: %+v", record)
	}
	var originalPresent bool
	for _, source := range record.Sources {
		if source.Snapshot == aSnapshot && source.State == "missing" {
			originalPresent = true
		}
	}
	if !originalPresent {
		t.Fatalf("pruned source was unavailable as original evidence: %+v", record.Sources)
	}

	if err := s.ReviewApply(ctx, namespace, conflict.ID, ReviewAction{Action: "resolve", Decision: "reviewed", References: []string{"fixture"}}); err != nil {
		t.Fatal(err)
	}
	assertReviewLineageRoot(t, s, aSnapshot, conflict.ID)
	if err := s.ReviewApply(ctx, namespace, conflict.ID, ReviewAction{Action: "purge"}); err != nil {
		t.Fatal(err)
	}
	released := readReviewLineageEvidence(t, s, aSnapshot)
	if !released.exists || released.entry.OrphanSince == nil || !released.entry.OrphanSince.Equal(now) {
		t.Fatalf("last journal owner did not start grace: %+v", released)
	}
	now = now.Add(evidenceOrphanGrace)
	collectReviewLineage(t, s, namespace, now)
	if got := readReviewLineageEvidence(t, s, aSnapshot); got.exists {
		t.Fatalf("expired journal orphan survived: %+v", got)
	}
}

func TestReviewLineageL12JournalRerootRestartsTransitiveGrace(t *testing.T) {
	ctx := context.Background()
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	now := journalTestTime
	s.now = func() time.Time { return now }
	const namespace = "project"
	source := putReviewJournalFact(t, s, namespace, "original source")
	head, err := s.ReviseFactsWithOptions(ctx, namespace, "revised head", WriteOptions{}, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(namespace, head.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := NewEvidenceSnapshot(source)
	got := readReviewLineageEvidence(t, s, snapshot)
	if got.entry.OrphanSince == nil || !got.entry.OrphanSince.Equal(now) {
		t.Fatalf("deleting the last head did not start transitive grace: %+v", got)
	}

	// Model an older writer restoring the canonical row without restoring
	// the explicit owner edges already released by the new writer.
	now = now.Add(evidenceOrphanGrace - time.Second)
	legacy := head
	legacy.LineageProjection = LineageProjection{}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return lineageRowBucket(tx, namespace).Put([]byte(head.ID), raw)
	}); err != nil {
		t.Fatal(err)
	}
	in := reviewFixture("ollama", "fixture", "reroot", head)
	in.Findings = []ReviewFinding{{Kind: ReviewKindSummary, SourceIDs: []string{head.ID}, Summary: "proposed review"}}
	record := commitReviewFixture(t, s, in)[0]
	if err := s.Delete(namespace, head.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	collectReviewLineage(t, s, namespace, now)
	got = readReviewLineageEvidence(t, s, snapshot)
	if !got.exists || got.entry.Snapshot != snapshot || got.entry.OrphanSince != nil || len(got.owners) != 0 {
		t.Fatalf("journal's transitive root did not clear old grace: %+v", got)
	}

	for _, action := range []string{"reject", "purge"} {
		if err := s.ReviewApply(ctx, namespace, record.ID, ReviewAction{Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	got = readReviewLineageEvidence(t, s, snapshot)
	if got.entry.OrphanSince == nil || !got.entry.OrphanSince.Equal(now) {
		t.Fatalf("loss of the renewed journal root did not restart grace: %+v", got)
	}
	now = now.Add(evidenceOrphanGrace - time.Nanosecond)
	collectReviewLineage(t, s, namespace, now)
	if got := readReviewLineageEvidence(t, s, snapshot); !got.exists {
		t.Fatal("renewed grace ended early")
	}
	now = now.Add(time.Nanosecond)
	collectReviewLineage(t, s, namespace, now)
	if got := readReviewLineageEvidence(t, s, snapshot); got.exists {
		t.Fatal("expired transitive orphan survived")
	}
}

func TestReviewLineageL16BackupRequiresMatchingJournalOwners(t *testing.T) {
	ctx := context.Background()
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	a := putReviewJournalFact(t, s, "project", "source")
	b := putReviewJournalFact(t, s, a.AgentID, "additional observation")
	in := reviewFixture("ollama", "fixture", "backup", a, b)
	in.Findings = []ReviewFinding{{Kind: ReviewKindSummary, SourceIDs: []string{a.ID}, Summary: "candidate"}}
	record := commitReviewFixture(t, s, in)[0]
	if err := s.db.View(func(tx *bolt.Tx) error {
		return validateReviewEvidenceBackupTx(ctx, tx)
	}); err != nil {
		t.Fatal(err)
	}

	for _, corruption := range []string{"missing owner", "unclaimed owner", "missing evidence"} {
		t.Run(corruption, func(t *testing.T) {
			rollback := errors.New("rollback test corruption")
			err := s.db.Update(func(tx *bolt.Tx) error {
				key := []byte(NewEvidenceSnapshot(b).key())
				owners, err := evidenceNamespace(tx, bucketEvidenceOwners, a.AgentID, false)
				if err != nil {
					return err
				}
				switch corruption {
				case "missing owner":
					if err := owners.Bucket(key).Delete([]byte(reviewEvidenceOwnerPrefix + record.ID)); err != nil {
						return err
					}
				case "unclaimed owner":
					if err := owners.Bucket(key).Put([]byte(reviewEvidenceOwnerPrefix+"unclaimed"), []byte{1}); err != nil {
						return err
					}
				case "missing evidence":
					journal, err := reviewBucketsTx(tx, a.AgentID, false)
					if err != nil {
						return err
					}
					entry, err := getStoredReview(journal.records, record.ID)
					if err != nil {
						return err
					}
					entry.Evidence = []string{NewEvidenceSnapshot(a).key()}
					raw, err := json.Marshal(entry)
					if err != nil {
						return err
					}
					if err := journal.records.Put([]byte(record.ID), raw); err != nil {
						return err
					}
				}
				if err := validateReviewEvidenceBackupTx(ctx, tx); err == nil {
					return errors.New("validation accepted a broken journal index")
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatalf("backup validation: %v", err)
			}
		})
	}
}

func TestReviewLineageL19SharedQuotaCountsSnapshotOnce(t *testing.T) {
	ctx := context.Background()
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	a := putReviewJournalFact(t, s, "project", "reviewed assertion")
	in := reviewFixture("ollama", "fixture", "quota", a)
	in.Findings = []ReviewFinding{{Kind: ReviewKindSummary, SourceIDs: []string{a.ID}, Summary: "candidate"}}
	record := commitReviewFixture(t, s, in)[0]
	head, err := s.ReviseFactsWithOptions(ctx, a.AgentID, "corrected assertion", WriteOptions{}, a)
	if err != nil {
		t.Fatal(err)
	}
	other := putReviewJournalFact(t, s, "other", "separate project")
	otherIn := reviewFixture("ollama", "fixture", "quota", other)
	otherIn.Findings = []ReviewFinding{{Kind: ReviewKindSummary, SourceIDs: []string{other.ID}, Summary: "other candidate"}}
	otherRecord := commitReviewFixture(t, s, otherIn)[0]

	snapshotBytes := func(f Fact) int64 {
		t.Helper()
		snapshot := NewEvidenceSnapshot(f)
		raw, err := json.Marshal(storedEvidence{Snapshot: snapshot})
		if err != nil {
			t.Fatal(err)
		}
		return int64(len(snapshot.key()) + len(raw))
	}
	ownerBytes := func(f Fact, owner string) int64 {
		return int64(len(NewEvidenceSnapshot(f).key()) + len(owner) + 1)
	}
	aSnapshot, headSnapshot := NewEvidenceSnapshot(a), NewEvidenceSnapshot(head)
	headKey := headSnapshot.key()
	lineageOwner := lineageOwnerPrefix + headKey
	raw, err := json.Marshal(lineageRecord{
		Head: lineageRef(headSnapshot), Replaces: []LineageReference{lineageRef(aSnapshot)},
	})
	if err != nil {
		t.Fatal(err)
	}
	namespaceBytes := snapshotBytes(a) + snapshotBytes(head) +
		ownerBytes(a, reviewEvidenceOwnerPrefix+record.ID) + ownerBytes(a, lineageOwner) + ownerBytes(head, lineageOwner) +
		int64(len(headKey)+len(raw)+len(head.ID)+len(headKey))
	storeBytes := namespaceBytes + snapshotBytes(other) + ownerBytes(other, reviewEvidenceOwnerPrefix+otherRecord.ID)

	if err := s.db.View(func(tx *bolt.Tx) error {
		gotNS, gotStore, err := evidencePayloadBytesTx(tx, a.AgentID)
		if err != nil {
			return err
		}
		if gotNS != namespaceBytes || gotStore != storeBytes {
			t.Fatalf("logical payload = %d namespace, %d store; want %d, %d", gotNS, gotStore, namespaceBytes, storeBytes)
		}
		for _, test := range []struct {
			owner string
			want  int64
		}{
			{lineageOwner, 0},
			{reviewEvidenceOwnerPrefix + record.ID, 0},
			{reviewEvidenceOwnerPrefix + "new", ownerBytes(a, reviewEvidenceOwnerPrefix+"new")},
		} {
			if got, err := evidenceRootAddedBytesTx(tx, aSnapshot, test.owner); err != nil || got != test.want {
				t.Fatalf("new logical bytes for %q = %d, %v; want %d", test.owner, got, err, test.want)
			}
		}
		if err := checkEvidenceQuotaTx(tx, a.AgentID, evidenceTransactionBytes); err != nil {
			return err
		}
		if err := checkEvidenceQuotaTx(tx, a.AgentID, evidenceTransactionBytes+1); !errors.Is(err, ErrEvidenceQuota) {
			t.Fatalf("transaction quota accepted an excess byte: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
