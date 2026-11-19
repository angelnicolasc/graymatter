package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	bolt "go.etcd.io/bbolt"
)

var journalTestTime = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

func openReviewJournalTestStore(t *testing.T, dir string, readOnly bool) *Store {
	t.Helper()
	s, err := Open(StoreConfig{DataDir: dir, Embedder: embedding.NewKeyword(), ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return journalTestTime }
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func putReviewJournalFact(t *testing.T, s *Store, namespace, text string) Fact {
	t.Helper()
	f, err := s.PutReturningFact(context.Background(), namespace, text)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func reviewFixture(provider, model, prompt string, facts ...Fact) ReviewSubmission {
	in := ReviewSubmission{Namespace: facts[0].AgentID, Provider: provider, Model: model, PromptVersion: prompt}
	for _, f := range facts {
		in.Snapshots = append(in.Snapshots, NewEvidenceSnapshot(f))
	}
	return in
}

func commitReviewFixture(t *testing.T, s *Store, in ReviewSubmission) []ReviewRecord {
	t.Helper()
	result, err := s.ReviewCommit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func inspectReviewFixture(t *testing.T, s *Store, namespace, id string) ReviewRecord {
	t.Helper()
	record, err := s.ReviewInspect(context.Background(), namespace, id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func journalTransactionID(t *testing.T, s *Store) int {
	t.Helper()
	var id int
	if err := s.db.View(func(tx *bolt.Tx) error { id = tx.ID(); return nil }); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestReviewJournalC103RejectsWholeInvalidResponse(t *testing.T) {
	ctx := context.Background()
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	a := putReviewJournalFact(t, s, "project", "first observation")
	b := putReviewJournalFact(t, s, "project", "second observation")
	foreign := putReviewJournalFact(t, s, "other", "different project")
	valid := ReviewFinding{Kind: ReviewKindSummary, Summary: "candidate", SourceIDs: []string{a.ID}}
	for _, bad := range []ReviewFinding{
		{Kind: ReviewKindConflict, SourceIDs: []string{a.ID}, Note: "one source"},
		{Kind: ReviewKindConflict, SourceIDs: []string{a.ID, a.ID}, Note: "duplicate"},
		{Kind: ReviewKindConflict, SourceIDs: []string{a.ID, foreign.ID}, Note: "foreign"},
		{Kind: ReviewKindConflict, SourceIDs: []string{a.ID, "unobserved"}, Note: "unseen"},
		{Kind: ReviewKindConflict, SourceIDs: []string{a.ID, b.ID}, Note: strings.Repeat("x", ReviewMaxNoteBytes+1)},
		{Kind: ReviewKindUntyped, SourceIDs: []string{a.ID}, Note: "invalid typed reference"},
		valid,
	} {
		in := reviewFixture("anthropic", "model", "v1", a, b)
		in.Findings = []ReviewFinding{valid, bad}
		before := journalTransactionID(t, s)
		if result, err := s.ReviewCommit(ctx, in); !errors.Is(err, ErrReviewInvalid) || result != nil {
			t.Fatalf("invalid response returned %+v, %v", result, err)
		}
		if journalTransactionID(t, s) != before {
			t.Fatal("invalid response committed a transaction")
		}
	}
	for _, fake := range []Fact{foreign, {ID: "invented", AgentID: a.AgentID, Text: "unknown", CreatedAt: a.CreatedAt}} {
		in := reviewFixture("anthropic", "model", "v1", a, fake)
		in.Findings = []ReviewFinding{valid}
		if result, err := s.ReviewCommit(ctx, in); err == nil || result != nil {
			t.Fatalf("invalid batch returned %+v, %v", result, err)
		}
	}
	list, err := s.ReviewList(ctx, a.AgentID)
	if err != nil || len(list) != 0 {
		t.Fatalf("partial journal = %+v, %v", list, err)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketEvidenceSnapshots) != nil || tx.Bucket(bucketReviewJournal) != nil {
			t.Fatal("invalid submission created journal buckets")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewJournalC104C105C110SourceStateAndResolution(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openReviewJournalTestStore(t, dir, false)
	a := putReviewJournalFact(t, s, "project", "Original policy requires human review.")
	b := putReviewJournalFact(t, s, "project", "Original policy permits automated review.")
	c := putReviewJournalFact(t, s, "project", "Policy changed after launch.")
	extra := putReviewJournalFact(t, s, "project", "Unrelated observed evidence.")
	in := reviewFixture("ollama", "local", "v1", extra, a, c, b)
	in.Findings = []ReviewFinding{
		{Kind: ReviewKindConflict, SourceIDs: []string{c.ID, a.ID, b.ID}, Note: "conflict"},
		{Kind: ReviewKindSummary, SourceIDs: []string{a.ID}, Summary: "untrusted candidate"},
		{Kind: ReviewKindUntyped, Note: "untyped observation"},
	}
	records := commitReviewFixture(t, s, in)
	conflict, summary, untyped := records[0].ID, records[1].ID, records[2].ID
	if records[0].Resolution != "unresolved" || records[1].Status != "pending" ||
		records[2].Kind != ReviewKindUntyped || len(records[2].Sources) != 4 {
		t.Fatalf("initial dispositions: %+v", records)
	}
	if notices, err := s.ReviewNotices(ctx, a.AgentID, []string{a.ID}); err != nil ||
		len(notices) != 1 || notices[0].ID != conflict || !slices.Equal(notices[0].SourceIDs, []string{a.ID}) {
		t.Fatalf("notice = %+v, %v", notices, err)
	}
	if notices, err := s.ReviewNotices(ctx, a.AgentID, []string{extra.ID}); err != nil || len(notices) != 0 {
		t.Fatalf("untyped observation attributed to extra source: %+v, %v", notices, err)
	}
	if err := s.Retire(a.AgentID, a); err != nil {
		t.Fatal(err)
	}
	got := inspectReviewFixture(t, s, a.AgentID, conflict)
	if got.SourceState != "retired" || got.Resolution != "unresolved" {
		t.Fatalf("after retirement: %+v", got)
	}
	if candidate := inspectReviewFixture(t, s, a.AgentID, summary); candidate.Status != "stale" {
		t.Fatalf("summary after retirement = %q", candidate.Status)
	}
	c.Text = "Changed after capture."
	if err := s.UpdateFact(c.AgentID, c); err != nil {
		t.Fatal(err)
	}
	if state := inspectReviewFixture(t, s, a.AgentID, conflict).SourceState; state != "changed" {
		t.Fatalf("aggregate state after edit = %q", state)
	}
	if err := s.Delete(b.AgentID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	ro := openReviewJournalTestStore(t, dir, true)
	before := journalTransactionID(t, ro)
	got = inspectReviewFixture(t, ro, a.AgentID, conflict)
	if got.SourceState != "missing" || got.Resolution != "unresolved" ||
		!slices.Equal(got.BatchIDs, []string{extra.ID, a.ID, c.ID, b.ID}) ||
		len(got.AdditionalEvidence) != 1 || got.AdditionalEvidence[0] != NewEvidenceSnapshot(extra) {
		t.Fatalf("reopened conflict: %+v", got)
	}
	states := map[string]string{a.ID: "retired", b.ID: "missing", c.ID: "changed"}
	original := map[string]EvidenceSnapshot{}
	for _, snapshot := range in.Snapshots {
		original[snapshot.FactID] = snapshot
	}
	for _, source := range got.Sources {
		if source.State != states[source.Snapshot.FactID] || source.Snapshot != original[source.Snapshot.FactID] {
			t.Fatalf("reopened source did not retain the original snapshot: %+v", source)
		}
	}
	list, err := ro.ReviewList(ctx, a.AgentID)
	if err != nil || len(list) != 3 || len(list[0].Sources) != 0 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	overview, err := ro.ReviewOverview(ctx)
	if err != nil || overview.Unresolved != 2 || overview.Stale != 1 || overview.LogicalBytes == 0 {
		t.Fatalf("overview = %+v, %v", overview, err)
	}
	action := ReviewAction{Action: "resolve", Decision: "Current policy verified", References: []string{"policy-123"}}
	if err := ro.ReviewApply(ctx, a.AgentID, conflict, action); !errors.Is(err, ErrStoreReadOnly) {
		t.Fatalf("read-only resolution = %v", err)
	}
	if journalTransactionID(t, ro) != before {
		t.Fatal("read-only review advanced the database transaction")
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}

	writer := openReviewJournalTestStore(t, dir, false)
	if err := writer.ReviewApply(ctx, a.AgentID, conflict, action); err != nil {
		t.Fatal(err)
	}
	if err := writer.ReviewApply(ctx, a.AgentID, untyped, ReviewAction{Action: "dismiss", Decision: "insufficient support", References: []string{"operator-check"}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	final := openReviewJournalTestStore(t, dir, true)
	got = inspectReviewFixture(t, final, a.AgentID, conflict)
	if got.Resolution != "resolved" || got.SourceState != "missing" ||
		got.Decision != action.Decision || !slices.Equal(got.References, action.References) {
		t.Fatalf("resolution after reopen: %+v", got)
	}
	if got := inspectReviewFixture(t, final, a.AgentID, untyped); got.Resolution != "dismissed" {
		t.Fatalf("dismissal after reopen: %+v", got)
	}
	if notices, err := final.ReviewNotices(ctx, a.AgentID, []string{c.ID}); err != nil || len(notices) != 0 {
		t.Fatalf("closed conflict still exposed notices: %+v, %v", notices, err)
	}
}

func TestReviewJournalC106RejectsStaleResponseAtomically(t *testing.T) {
	ctx := context.Background()
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	a := putReviewJournalFact(t, s, "project", "first")
	b := putReviewJournalFact(t, s, "project", "second")
	in := reviewFixture("ollama", "model", "v1", a, b)
	in.Findings = []ReviewFinding{{Kind: ReviewKindConflict, SourceIDs: []string{a.ID, b.ID}, Note: "potential conflict"}}
	b.Text = "different revision after the request"
	if err := s.UpdateFact(b.AgentID, b); err != nil {
		t.Fatal(err)
	}
	before := journalTransactionID(t, s)
	if result, err := s.ReviewCommit(ctx, in); !errors.Is(err, ErrFactChanged) || result != nil {
		t.Fatalf("stale response = %+v, %v", result, err)
	}
	if journalTransactionID(t, s) != before {
		t.Fatal("stale response committed a transaction")
	}
	facts, err := s.List(a.AgentID)
	if err != nil || len(facts) != 2 {
		t.Fatalf("stale response touched canonical facts: %+v, %v", facts, err)
	}
	for _, fact := range facts {
		if fact.IsSuperseded() {
			t.Fatalf("stale response retired %s", fact.ID)
		}
	}
}

func TestReviewJournalC107DedupeConcurrentAndScoped(t *testing.T) {
	ctx := context.Background()
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	a := putReviewJournalFact(t, s, "project", "first")
	b := putReviewJournalFact(t, s, "project", "second")
	makeSubmission := func(provider, model string, first, second Fact) ReviewSubmission {
		in := reviewFixture(provider, model, "v1", first, second)
		in.Findings = []ReviewFinding{
			{Kind: ReviewKindConflict, SourceIDs: []string{second.ID, first.ID}, Note: "conflict"},
			{Kind: ReviewKindSummary, SourceIDs: []string{first.ID, second.ID}, Summary: "summary"},
		}
		return in
	}
	type result struct {
		records []ReviewRecord
		err     error
	}
	out := make(chan result, 2)
	for _, in := range []ReviewSubmission{
		makeSubmission("anthropic", "a", a, b), makeSubmission("ollama", "b", b, a),
	} {
		go func(in ReviewSubmission) {
			records, err := s.ReviewCommit(ctx, in)
			out <- result{records, err}
		}(in)
	}
	x, y := <-out, <-out
	if x.err != nil || y.err != nil {
		t.Fatalf("concurrent submissions: %v, %v", x.err, y.err)
	}
	if x.records[0].ID != y.records[0].ID || x.records[1].ID == y.records[1].ID {
		t.Fatalf("dedupe mismatch: %+v, %+v", x.records, y.records)
	}
	got := inspectReviewFixture(t, s, a.AgentID, x.records[0].ID)
	if got.Observations != 2 || len(got.History) != 2 {
		t.Fatalf("concurrent observations = %+v", got)
	}
	if got.History[0].BatchHash == got.History[1].BatchHash {
		t.Fatal("batch hash did not encode request order")
	}
	if _, err := s.ReviewInspect(ctx, "other", got.ID); !errors.Is(err, ErrReviewNotFound) {
		t.Fatalf("cross-namespace inspect = %v", err)
	}
	if notices, err := s.ReviewNotices(ctx, "other", []string{a.ID, b.ID}); err != nil || len(notices) != 0 {
		t.Fatalf("cross-namespace notices = %+v, %v", notices, err)
	}
	if evidenceHash("test", "a", "bc") == evidenceHash("test", "ab", "c") ||
		evidenceIdentityKey("a", "bc", "d") == evidenceIdentityKey("ab", "c", "d") {
		t.Fatal("evidence hash framing is ambiguous")
	}
}

func TestReviewJournalC108RecordByteBoundaryAndDecision(t *testing.T) {
	ctx := context.Background()
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	a := putReviewJournalFact(t, s, "project", strings.Repeat("x", ReviewMaxSourceBytes))
	makeSubmission := func(version, summary string) ReviewSubmission {
		in := reviewFixture("ollama", "model", version, a)
		in.Findings = []ReviewFinding{{Kind: ReviewKindSummary, SourceIDs: []string{a.ID}, Summary: summary}}
		return in
	}
	small := commitReviewFixture(t, s, makeSubmission("v0", "x"))[0]
	raw, err := json.Marshal(small)
	if err != nil {
		t.Fatal(err)
	}
	limit := ReviewMaxRecordBytes - reviewActionBudget
	base := limit - len(raw) + 1
	var boundary ReviewRecord
	for i, adjustment := range []int{-1, 0, 1} {
		in := makeSubmission(fmt.Sprintf("v%d", i+1), strings.Repeat("x", base+adjustment))
		result, err := s.ReviewCommit(ctx, in)
		if adjustment > 0 {
			if !errors.Is(err, ErrReviewQuota) || result != nil {
				t.Fatalf("N+1 bytes = %+v, %v", result, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%+d boundary = %v", adjustment, err)
		}
		boundary = result[0]
		actual, err := json.Marshal(boundary)
		if err != nil || len(actual) != limit+adjustment {
			t.Fatalf("record bytes = %d, want %d, error %v", len(actual), limit+adjustment, err)
		}
	}
	if err := s.ReviewApply(ctx, a.AgentID, boundary.ID, ReviewAction{
		Action: "reject", Decision: strings.Repeat("x", ReviewMaxNoteBytes),
	}); err != nil {
		t.Fatalf("open record at byte boundary could not close: %v", err)
	}
	record := inspectReviewFixture(t, s, a.AgentID, boundary.ID)
	if record.Status != "rejected" || len(record.Decision) != ReviewMaxNoteBytes {
		t.Fatalf("boundary decision = %+v", record)
	}
	list, err := s.ReviewList(ctx, a.AgentID)
	if err != nil || len(list) != 3 {
		t.Fatalf("over-limit write was not rolled back: %d, %v", len(list), err)
	}
}

func TestReviewJournalC102DistinctUntypedObservations(t *testing.T) {
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	a := putReviewJournalFact(t, s, "project", "observed evidence")
	in := reviewFixture("ollama", "model", "v1", a)
	in.Findings = []ReviewFinding{
		{Kind: ReviewKindUntyped, Note: "first hypothesis"},
		{Kind: ReviewKindUntyped, Note: "second hypothesis"},
	}
	first := commitReviewFixture(t, s, in)
	second := commitReviewFixture(t, s, in)
	if len(first) != 2 || first[0].ID == first[1].ID ||
		first[0].ID != second[0].ID || first[1].ID != second[1].ID ||
		second[0].Observations != 2 || second[1].Observations != 2 {
		t.Fatalf("untyped dedupe: %+v, %+v", first, second)
	}
}

func TestReviewJournalC109CountQuotaAndPurge(t *testing.T) {
	ctx := context.Background()
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	a := putReviewJournalFact(t, s, "project", "evidence")
	submission := func(i int) ReviewSubmission {
		in := reviewFixture("ollama", "model", fmt.Sprintf("v%d", i), a)
		in.Findings = []ReviewFinding{{Kind: ReviewKindSummary, SourceIDs: []string{a.ID}, Summary: "candidate"}}
		return in
	}
	first := commitReviewFixture(t, s, submission(0))[0]
	for i := 1; i < ReviewMaxRecords; i++ {
		commitReviewFixture(t, s, submission(i))
	}
	if err := s.ReviewApply(ctx, a.AgentID, first.ID, ReviewAction{Action: "purge"}); !errors.Is(err, ErrReviewTransition) {
		t.Fatalf("open purge = %v", err)
	}
	if err := s.ReviewApply(ctx, a.AgentID, first.ID, ReviewAction{Action: "reject"}); err != nil {
		t.Fatal(err)
	}
	if result, err := s.ReviewCommit(ctx, submission(ReviewMaxRecords)); !errors.Is(err, ErrReviewQuota) || result != nil {
		t.Fatalf("rejection freed a record slot: %+v, %v", result, err)
	}
	if err := s.ReviewApply(ctx, a.AgentID, first.ID, ReviewAction{Action: "purge"}); err != nil {
		t.Fatal(err)
	}
	replacement := commitReviewFixture(t, s, submission(ReviewMaxRecords))[0]
	if len(replacement.Sources) != 1 || replacement.Sources[0].Snapshot != NewEvidenceSnapshot(a) {
		t.Fatal("purging one record removed shared evidence")
	}
}

func TestReviewJournalC109HistoryAndSnapshotGrace(t *testing.T) {
	ctx := context.Background()
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	a := putReviewJournalFact(t, s, "project", "first source")
	b := putReviewJournalFact(t, s, "project", "second source")
	shared := putReviewJournalFact(t, s, "project", "shared historical source")
	makeConflict := func(extra ...Fact) ReviewSubmission {
		in := reviewFixture("ollama", "model", "conflict", append([]Fact{a, b}, extra...)...)
		in.Findings = []ReviewFinding{{Kind: ReviewKindConflict, SourceIDs: []string{a.ID, b.ID}, Note: "contradiction"}}
		return in
	}
	makeSummary := func(prompt string, fact Fact) ReviewSubmission {
		in := reviewFixture("ollama", "model", prompt, fact)
		in.Findings = []ReviewFinding{{Kind: ReviewKindSummary, SourceIDs: []string{fact.ID}, Summary: "candidate"}}
		return in
	}
	first := commitReviewFixture(t, s, makeConflict())[0]
	closed := commitReviewFixture(t, s, makeSummary("secondary", shared))[0]
	commitReviewFixture(t, s, makeConflict(shared))
	var latest ReviewRecord
	for i := 0; i < reviewHistoryLimit; i++ {
		extra := putReviewJournalFact(t, s, "project", fmt.Sprintf("new observed source %d", i))
		latest = commitReviewFixture(t, s, makeConflict(extra))[0]
	}
	if latest.ID != first.ID || latest.Observations != uint64(2+reviewHistoryLimit) ||
		len(latest.History) != reviewHistoryLimit || len(latest.AdditionalEvidence) != reviewHistoryLimit ||
		!slices.Equal(latest.BatchIDs, []string{a.ID, b.ID}) {
		t.Fatalf("retained observation history: %+v", latest)
	}
	key := NewEvidenceSnapshot(shared).key()
	check := func(wantOwner, wantOrphan, wantPresent bool) {
		t.Helper()
		if err := s.db.View(func(tx *bolt.Tx) error {
			bucket, err := evidenceNamespace(tx, bucketEvidenceSnapshots, a.AgentID, false)
			if err != nil {
				return err
			}
			owners, err := evidenceNamespace(tx, bucketEvidenceOwners, a.AgentID, false)
			if err != nil {
				return err
			}
			raw := bucket.Get([]byte(key))
			if (raw != nil) != wantPresent || (owners.Bucket([]byte(key)) != nil) != wantOwner {
				t.Fatalf("unexpected evidence presence or owner for historical source: %t, %t", raw != nil, owners.Bucket([]byte(key)) != nil)
			}
			if wantPresent {
				entry, err := evidenceEntry(bucket, key)
				if err != nil {
					return err
				}
				if (entry.OrphanSince != nil) != wantOrphan || (wantOrphan && !entry.OrphanSince.Equal(journalTestTime)) {
					t.Fatalf("wrong orphan grace marker: %+v", entry)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check(true, false, true)
	if err := s.ReviewApply(ctx, a.AgentID, closed.ID, ReviewAction{Action: "reject"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReviewApply(ctx, a.AgentID, closed.ID, ReviewAction{Action: "purge"}); err != nil {
		t.Fatal(err)
	}
	check(false, true, true)
	s.now = func() time.Time { return journalTestTime.Add(evidenceOrphanGrace + time.Second) }
	trigger := commitReviewFixture(t, s, makeSummary("gc trigger", a))[0]
	if err := s.ReviewApply(ctx, a.AgentID, trigger.ID, ReviewAction{Action: "reject"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReviewApply(ctx, a.AgentID, trigger.ID, ReviewAction{Action: "purge"}); err != nil {
		t.Fatal(err)
	}
	check(false, false, false)
	if got := inspectReviewFixture(t, s, a.AgentID, first.ID); got.Resolution != "unresolved" || len(got.Sources) != 2 {
		t.Fatalf("closed purge touched an open conflict: %+v", got)
	}
}

func TestReviewJournalC105PreservesObservedTimestampPerBatch(t *testing.T) {
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	a := putReviewJournalFact(t, s, "project", "same content across timestamp correction")
	submit := func(provider string, fact Fact) ReviewRecord {
		in := reviewFixture(provider, "model", "v1", fact)
		in.Findings = []ReviewFinding{{Kind: ReviewKindSummary, SourceIDs: []string{fact.ID}, Summary: "candidate"}}
		return commitReviewFixture(t, s, in)[0]
	}
	first := submit("ollama", a)
	original := a.CreatedAt
	a.CreatedAt = original.Add(time.Hour)
	if err := s.UpdateFact(a.AgentID, a); err != nil {
		t.Fatal(err)
	}
	second := submit("anthropic", a)
	repeat := submit("ollama", a)
	if first.ID == second.ID || first.ID != repeat.ID ||
		first.Sources[0].Snapshot.ContentHash != second.Sources[0].Snapshot.ContentHash ||
		!repeat.Sources[0].Snapshot.CreatedAt.Equal(original) ||
		!second.Sources[0].Snapshot.CreatedAt.Equal(a.CreatedAt) ||
		len(repeat.History) != 2 || !repeat.History[1].BatchCreatedAt[0].Equal(a.CreatedAt) ||
		repeat.History[0].BatchHash == repeat.History[1].BatchHash {
		t.Fatalf("observed timestamps were not preserved across shared evidence: %+v, %+v", second, repeat)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, a.AgentID, false)
		if err != nil {
			return err
		}
		if snapshots.Stats().KeyN != 1 {
			t.Fatal("metadata-only rewrite forked the content identity")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewJournalC109ByteQuotaKeepsDecisionCapacity(t *testing.T) {
	ctx := context.Background()
	s := openReviewJournalTestStore(t, t.TempDir(), false)
	a := putReviewJournalFact(t, s, "project", "first")
	b := putReviewJournalFact(t, s, "project", "second")
	in := reviewFixture("ollama", "model", "v1", a, b)
	in.Findings = []ReviewFinding{{Kind: ReviewKindConflict, SourceIDs: []string{a.ID, b.ID}, Note: "possible contradiction"}}
	open := commitReviewFixture(t, s, in)[0]

	// Seed retained evidence directly to reach the exact logical quota
	// without hundreds of unrelated canonical writes in this control.
	if err := s.db.Update(func(tx *bolt.Tx) error {
		journal, err := reviewBucketsTx(tx, a.AgentID, false)
		if err != nil {
			return err
		}
		usage, err := reviewQuotaTx(tx, a.AgentID, journal.records)
		if err != nil {
			return err
		}
		snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, a.AgentID, true)
		if err != nil {
			return err
		}
		remaining := ReviewMaxJournalBytes - usage.bytes - usage.reserved
		for i := 0; remaining > 0; i++ {
			fact := Fact{ID: fmt.Sprintf("quota-source-%06d", i), AgentID: a.AgentID, CreatedAt: journalTestTime}
			empty := NewEvidenceSnapshot(fact)
			raw, err := json.Marshal(storedEvidence{Snapshot: empty})
			if err != nil {
				return err
			}
			overhead := len(empty.key()) + len(raw)
			size := min(ReviewMaxSourceBytes, remaining-overhead)
			if remaining-overhead-size > 0 && remaining-overhead-size < overhead {
				size -= overhead
			}
			if size < 0 {
				t.Fatalf("cannot fill exact logical quota, %d bytes remain", remaining)
			}
			fact.Text = strings.Repeat("x", size)
			snapshot := NewEvidenceSnapshot(fact)
			if err := putEvidenceEntry(snapshots, snapshot.key(), storedEvidence{Snapshot: snapshot}); err != nil {
				return err
			}
			remaining -= overhead + size
		}
		usage, err = reviewQuotaTx(tx, a.AgentID, journal.records)
		if err != nil || usage.bytes+usage.reserved != ReviewMaxJournalBytes {
			t.Fatalf("logical byte fixture = %+v, %v", usage, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	in.PromptVersion = "v2"
	if records, err := s.ReviewCommit(ctx, in); !errors.Is(err, ErrReviewQuota) || records != nil {
		t.Fatalf("at-capacity write = %+v, %v", records, err)
	}
	if got := inspectReviewFixture(t, s, a.AgentID, open.ID); got.Resolution != "unresolved" {
		t.Fatalf("quota failure altered open conflict: %+v", got)
	}
	if err := s.ReviewApply(ctx, a.AgentID, open.ID, ReviewAction{
		Action: "resolve", Decision: strings.Repeat("a", ReviewMaxNoteBytes), References: []string{"ticket-42"},
	}); err != nil {
		t.Fatalf("resolution at capacity failed: %v", err)
	}
	if got := inspectReviewFixture(t, s, a.AgentID, open.ID); got.Resolution != "resolved" {
		t.Fatalf("explicit resolution = %+v", got)
	}
}

func TestReviewJournalC105AdditionalEvidenceUsesFirstRetainedObservation(t *testing.T) {
	for _, shared := range []bool{true, false} {
		t.Run(fmt.Sprintf("shared=%t", shared), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s := openReviewJournalTestStore(t, dir, false)
			const namespace = "project"
			a := putReviewJournalFact(t, s, namespace, "require review")
			b := putReviewJournalFact(t, s, namespace, "allow automation")
			initial := putReviewJournalFact(t, s, namespace, "initial context")
			later := putReviewJournalFact(t, s, namespace, "later context")
			initialSnapshot := NewEvidenceSnapshot(initial)
			original := NewEvidenceSnapshot(later)

			var seedID string
			if shared {
				in := reviewFixture("ollama", "local", "seed", later)
				in.Findings = []ReviewFinding{{Kind: ReviewKindSummary, SourceIDs: []string{later.ID}, Summary: "candidate"}}
				seedID = commitReviewFixture(t, s, in)[0].ID
			}
			observe := func(extra ...Fact) ReviewRecord {
				t.Helper()
				in := reviewFixture("ollama", "local", "conflict", append([]Fact{a, b}, extra...)...)
				in.Findings = []ReviewFinding{{Kind: ReviewKindConflict, SourceIDs: []string{a.ID, b.ID}, Note: "disagreement"}}
				return commitReviewFixture(t, s, in)[0]
			}
			first := observe(initial)
			rewriteDate := func(f *Fact, at time.Time) {
				t.Helper()
				f.CreatedAt = at
				if err := s.UpdateFact(namespace, *f); err != nil {
					t.Fatal(err)
				}
			}
			t1 := original.CreatedAt.Add(2 * time.Hour)
			t2 := original.CreatedAt.Add(time.Hour)
			rewriteDate(&later, t1)
			rewriteDate(&initial, initial.CreatedAt.Add(3*time.Hour))
			checkEvidence := func(record ReviewRecord, laterAt time.Time) {
				t.Helper()
				want := []EvidenceSnapshot{initialSnapshot}
				if !laterAt.IsZero() {
					snapshot := NewEvidenceSnapshot(later)
					snapshot.CreatedAt = laterAt
					want = append(want, snapshot)
				}
				if record.ID != first.ID || !slices.Equal(record.AdditionalEvidence, want) {
					t.Fatalf("record %q additional evidence = %+v, want record %q evidence %+v", record.ID, record.AdditionalEvidence, first.ID, want)
				}
			}
			checkHistory := func(record ReviewRecord, want ...time.Time) {
				t.Helper()
				var observed []time.Time
				for _, item := range record.History {
					if i := slices.Index(item.BatchIDs, later.ID); i >= 0 {
						if item.BatchVersions[i] != original.ContentHash {
							t.Fatalf("observation changed the content identity: %+v", item)
						}
						observed = append(observed, item.BatchCreatedAt[i])
					}
				}
				if !slices.EqualFunc(observed, want, time.Time.Equal) {
					t.Fatalf("observation dates = %v, want %v", observed, want)
				}
			}
			receipt := observe(initial, later)
			checkHistory(receipt, t1)
			checkEvidence(receipt, t1)
			checkEvidence(inspectReviewFixture(t, s, namespace, first.ID), t1)

			rewriteDate(&later, t2)
			receipt = observe(later, initial)
			checkHistory(receipt, t1, t2)
			checkEvidence(receipt, t1)

			for i := 0; i < reviewHistoryLimit-1; i++ {
				receipt = observe(initial)
			}
			checkHistory(receipt, t2)
			checkEvidence(receipt, t2)
			if shared {
				seed := inspectReviewFixture(t, s, namespace, seedID)
				if len(seed.Sources) != 1 || seed.Sources[0].Snapshot != original {
					t.Fatalf("original owner lost its timestamp: %+v", seed.Sources)
				}
				for _, action := range []string{"reject", "purge"} {
					if err := s.ReviewApply(ctx, namespace, seedID, ReviewAction{Action: action}); err != nil {
						t.Fatal(err)
					}
				}
			}

			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openReviewJournalTestStore(t, dir, true)
			reopened := inspectReviewFixture(t, s, namespace, first.ID)
			checkHistory(reopened, t2)
			checkEvidence(reopened, t2)
			if err := s.db.View(func(tx *bolt.Tx) error {
				snapshots, err := evidenceNamespace(tx, bucketEvidenceSnapshots, namespace, false)
				if err != nil {
					return err
				}
				entry, err := evidenceEntry(snapshots, original.key())
				if err != nil {
					return err
				}
				want := original
				if !shared {
					want.CreatedAt = t1
				}
				if entry.Snapshot != want || entry.OrphanSince != nil {
					t.Fatalf("retained shared snapshot = %+v, want rooted snapshot %+v", entry, want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openReviewJournalTestStore(t, dir, false)
			receipt = observe(initial)
			checkHistory(receipt)
			checkEvidence(receipt, time.Time{})
		})
	}
}
