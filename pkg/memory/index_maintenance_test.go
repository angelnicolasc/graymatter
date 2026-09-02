package memory

import (
	"context"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// The index is only safe to trust because every write path maintains it and
// every disagreement rebuilds. These pin both halves.

// A store that has never been written through a maintaining path — a database
// that predates the index, which is every database in the field — must still
// answer, and answer correctly. The first recall builds the index and the
// answer is the scan's answer.
func TestIndexBackfillsAnExistingStore(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Write with the index off at the config level; maintenance is
	// unconditional, so to simulate a pre-index database the index buckets are
	// dropped afterwards.
	s, err := Open(StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 8760 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, txt := range []string{
		"the retention window for invoices is 90 days",
		"invoices go to the billing team",
		"contractor verification happens during onboarding",
	} {
		if err := s.Put(ctx, "back", txt); err != nil {
			t.Fatal(err)
		}
	}
	want, _, err := s.RecallDetailed(ctx, "back", "how long are invoices retained?", 8)
	if err != nil {
		t.Fatal(err)
	}
	dropIndex(t, s, "back")
	_ = s.Close()

	s, err = Open(StoreConfig{
		DataDir:            dir,
		Embedder:           embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife:      8760 * time.Hour,
		CandidateRetrieval: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	got, _, err := s.RecallDetailed(ctx, "back", "how long are invoices retained?", 8)
	if err != nil {
		t.Fatal(err)
	}
	assertSameTexts(t, "backfill", want, got)
}

// A write that bypasses maintenance is a code-level mistake, not a runtime
// event — but if one ever ships, the store must degrade to slow, never to
// wrong. The count mismatch is what catches it.
func TestIndexRebuildsWhenAWriteBypassedMaintenance(t *testing.T) {
	ctx := context.Background()
	s, err := Open(StoreConfig{
		DataDir:            t.TempDir(),
		Embedder:           embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife:      8760 * time.Hour,
		CandidateRetrieval: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	for _, txt := range []string{
		"the deploy freeze starts on friday",
		"deploys need two approvals",
	} {
		if err := s.Put(ctx, "bypass", txt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Recall(ctx, "bypass", "deploy", 8); err != nil {
		t.Fatal(err)
	}

	// Smuggle a fact straight into the facts bucket, the way a forgotten
	// write path would.
	smuggled := newFact("bypass", "the deploy window is two hours on friday", nil, s.now())
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.Bucket(bucketFacts).CreateBucketIfNotExists([]byte("bypass"))
		if err != nil {
			return err
		}
		data, err := smuggled.marshal()
		if err != nil {
			return err
		}
		return b.Put([]byte(smuggled.ID), data)
	}); err != nil {
		t.Fatal(err)
	}
	// The process already verified this agent, so force the check the way a
	// fresh process would see it.
	s.idxForgetCounted("bypass")

	got, err := s.Recall(ctx, "bypass", "deploy window", 8)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range got {
		if g == smuggled.Text {
			found = true
		}
	}
	if !found {
		t.Fatalf("the smuggled fact never came back; the index did not rebuild:\n%v", got)
	}
}

// StemKeywords changes what a token is, so an index built under one fold
// cannot answer under the other. Reopening with the other fold must rebuild
// rather than answer from postings that mean something else.
func TestIndexRebuildsWhenTheTokenisationFoldChanges(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	open := func(stem bool) *Store {
		s, err := Open(StoreConfig{
			DataDir:            dir,
			Embedder:           embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
			DecayHalfLife:      8760 * time.Hour,
			StemKeywords:       stem,
			CandidateRetrieval: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	s := open(false)
	for _, txt := range []string{
		"backup retention is ninety days",
		"the pager rotation was stretched to two weeks",
		"invoices go to the billing team",
	} {
		if err := s.Put(ctx, "fold", txt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Recall(ctx, "fold", "backups", 8); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// Same store, stemming on. The reference is the scan under the same fold.
	s = open(true)
	got, _, err := s.RecallDetailed(ctx, "fold", "backups", 8)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.CandidateRetrieval = false
	want, _, err := s.RecallDetailed(ctx, "fold", "backups", 8)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	assertSameTexts(t, "fold change", want, got)
}

// A read-only store cannot rebuild. It must fall back to the scan and answer,
// because a degraded open that refuses to answer is worse than a slow one.
func TestIndexFallsBackOnAReadOnlyStore(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 8760 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, txt := range []string{"the archive index rebuilds monthly", "archive requests take a day"} {
		if err := s.Put(ctx, "ro", txt); err != nil {
			t.Fatal(err)
		}
	}
	want, _, err := s.RecallDetailed(ctx, "ro", "archive", 8)
	if err != nil {
		t.Fatal(err)
	}
	dropIndex(t, s, "ro")
	_ = s.Close()

	ro, err := Open(StoreConfig{
		DataDir:            dir,
		Embedder:           embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife:      8760 * time.Hour,
		CandidateRetrieval: true,
		ReadOnly:           true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	got, _, err := ro.RecallDetailed(ctx, "ro", "archive", 8)
	if err != nil {
		t.Fatalf("read-only store refused to answer: %v", err)
	}
	assertSameTexts(t, "read-only fallback", want, got)
}

// Revising and forgetting have to take facts out of the postings, or a
// tombstoned sentence keeps contributing document frequencies and every idf
// in the store drifts.
func TestIndexDropsRetiredAndDeletedFacts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(StoreConfig{
		DataDir:            t.TempDir(),
		Embedder:           embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife:      8760 * time.Hour,
		CandidateRetrieval: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	old, err := s.putReturningFact(ctx, "churn", "the escalation contact is Dana Reyes")
	if err != nil {
		t.Fatal(err)
	}
	doomed, err := s.putReturningFact(ctx, "churn", "the escalation pager rotates weekly")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revise(ctx, "churn", "the escalation contact is Priya Nair", old); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("churn", doomed.ID); err != nil {
		t.Fatal(err)
	}

	if df := dfOf(t, s, "churn", "escalation"); df != 1 {
		t.Errorf("df(escalation) = %d, want 1: one live fact carries it", df)
	}
	if df := dfOf(t, s, "churn", "reyes"); df != 0 {
		t.Errorf("df(reyes) = %d, want 0: the fact was retired", df)
	}
	if df := dfOf(t, s, "churn", "rotates"); df != 0 {
		t.Errorf("df(rotates) = %d, want 0: the fact was deleted", df)
	}

	got, _, err := s.RecallDetailed(ctx, "churn", "who is the escalation contact?", 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if g == old.Text || g == doomed.Text {
			t.Errorf("a retired or deleted fact came back: %q", g)
		}
	}
}

func dfOf(t *testing.T, s *Store, agentID, term string) int {
	t.Helper()
	n := 0
	if err := s.db.View(func(tx *bolt.Tx) error {
		n = idxDF(idxBucketRO(tx, bucketIdxTerms, agentID), term)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func dropIndex(t *testing.T, s *Store, agentID string) {
	t.Helper()
	if err := s.db.Update(func(tx *bolt.Tx) error {
		for _, root := range [][]byte{bucketIdxTerms, bucketIdxRecency} {
			if parent := tx.Bucket(root); parent != nil && parent.Bucket([]byte(agentID)) != nil {
				if err := parent.DeleteBucket([]byte(agentID)); err != nil {
					return err
				}
			}
		}
		if mb := tx.Bucket(bucketIdxMeta); mb != nil {
			return mb.Delete([]byte(agentID))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.idxForgetCounted(agentID)
}

func assertSameTexts(t *testing.T, what string, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: %d facts vs %d\n  want %v\n  got  %v", what, len(want), len(got), want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("%s: rank %d\n  want %q\n  got  %q", what, i, want[i], got[i])
		}
	}
}

// The spine cache's invalidation, on the mutation that does not change the
// fact count.
//
// A revision writes a replacement and tombstones the original: two facts
// before, two facts after. A cache keyed on the fact count would serve the
// pre-revision spine forever — the retired fact still live in the ranking, its
// replacement absent from it — and every test above would still pass, because
// they all open a fresh store. This is the case that only shows up in a
// long-lived process, which is to say in the daemon, which is to say in
// production.
func TestSpineCacheInvalidatesOnRevision(t *testing.T) {
	ctx := context.Background()
	s, err := Open(StoreConfig{
		DataDir:            t.TempDir(),
		Embedder:           embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife:      8760 * time.Hour,
		CandidateRetrieval: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	old, err := s.putReturningFact(ctx, "spine", "the escalation contact is Dana Reyes")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "spine", "escalation pages the duty engineer"); err != nil {
		t.Fatal(err)
	}

	// Warm the cache on the same handle the revision will land on.
	before, err := s.Recall(ctx, "spine", "who is the escalation contact?", 8)
	if err != nil {
		t.Fatal(err)
	}
	if !containsText(before, old.Text) {
		t.Fatalf("setup: the original fact should be retrievable, got %v", before)
	}

	if _, err := s.Revise(ctx, "spine", "the escalation contact is Priya Nair", old); err != nil {
		t.Fatal(err)
	}

	after, err := s.Recall(ctx, "spine", "who is the escalation contact?", 8)
	if err != nil {
		t.Fatal(err)
	}
	if containsText(after, old.Text) {
		t.Errorf("the retired fact came back after the revision; the spine cache did not invalidate:\n%v", after)
	}
	if !containsText(after, "the escalation contact is Priya Nair") {
		t.Errorf("the replacement is missing; the spine cache is serving a pre-revision picture:\n%v", after)
	}
}

// Deleting a fact moves the count, but the cache has to notice on the same
// handle rather than at the next open.
func TestSpineCacheInvalidatesOnDelete(t *testing.T) {
	ctx := context.Background()
	s, err := Open(StoreConfig{
		DataDir:            t.TempDir(),
		Embedder:           embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife:      8760 * time.Hour,
		CandidateRetrieval: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	doomed, err := s.putReturningFact(ctx, "spine", "the archive room needs badge access")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recall(ctx, "spine", "archive room", 8); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("spine", doomed.ID); err != nil {
		t.Fatal(err)
	}
	after, err := s.Recall(ctx, "spine", "archive room", 8)
	if err != nil {
		t.Fatal(err)
	}
	if containsText(after, doomed.Text) {
		t.Errorf("the deleted fact came back:\n%v", after)
	}
}

func containsText(got []string, want string) bool {
	for _, g := range got {
		if g == want {
			return true
		}
	}
	return false
}
