package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

const subjectNS = "subjects-test"

func subjectString(s string) *string { return &s }

func putSubject(t *testing.T, s *Store, subject, text string) WriteReceipt {
	t.Helper()
	r, err := s.PutManyReturningReceipts(context.Background(), subjectNS, []WriteFact{{Text: text, Options: WriteOptions{Subject: &subject}}})
	if err != nil {
		t.Fatal(err)
	}
	return r[0]
}

func TestSubjectNormalizationAndStaticBatchValidation(t *testing.T) {
	for _, pair := range [][2]string{{"  CAFE\u0301\u2003POLICY \n", "café policy"}, {"日本\t語", "日本 語"}, {"ＡＢＣ", "ａｂｃ"}} {
		got, err := NormalizeSubject(pair[0])
		if err != nil || got != pair[1] {
			t.Fatalf("%q => %q %v", pair[0], got, err)
		}
	}
	for _, bad := range []string{"", "\t\n", strings.Repeat("a", 257), string([]byte{0xff})} {
		if _, err := NormalizeSubject(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if a, _ := NormalizeSubject("café"); a == "cafe" {
		t.Fatal("accent folded")
	}
	calls := 0
	s := openAtomicRevisionStore(t, StoreConfig{Embedder: &utilityEmbedder{fn: func(string) []float32 { calls++; return nil }}})
	before := revisionDBSnapshot(t, s)
	_, err := s.PutManyReturningFacts(context.Background(), subjectNS, []WriteFact{
		{Text: "one", Options: WriteOptions{Subject: subjectString("CAFÉ")}},
		{Text: "two", Options: WriteOptions{Subject: subjectString("cafe\u0301")}},
	})
	if err == nil || calls != 0 || !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatalf("duplicate subject reached effects: %v calls=%d", err, calls)
	}
}

func TestSubjectReplacePreservesVersionCitationAndRetiresOld(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{CandidateRetrieval: true})
	ctx := context.Background()
	source, quote, confidence := "file:policy", "exact\r\nquote á", "unverified"
	subject := "Retention"
	old, err := s.PutWithOptionsReturningFact(ctx, subjectNS, "policy old", WriteOptions{Subject: &subject, Source: &source, Quote: &quote, Confidence: &confidence})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPinned(subjectNS, true, old); err != nil {
		t.Fatal(err)
	}
	newer := putSubject(t, s, " retention ", "policy current")
	if newer.Fact.Pinned || newer.Fact.Confidence != confidence || newer.Fact.Evidence != nil || len(newer.Replaced) != 1 || newer.Replaced[0].ID != old.ID || newer.Replaced[0].Text != old.Text || newer.Subjects[0].Revisions != 1 {
		t.Fatalf("wrong replacement receipt %+v", newer)
	}
	rows, err := s.List(subjectNS)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rows {
		if f.ID == old.ID && f.SupersededBy != newer.Fact.ID {
			t.Fatalf("old still live %+v", f)
		}
	}
	citation, err := s.FetchCitation(ctx, subjectNS, old.ID, NewEvidenceSnapshot(old).ContentHash)
	if err != nil || citation.Evidence.Quote != quote {
		t.Fatalf("old exact evidence %+v %v", citation, err)
	}
	if _, err := s.FetchCitation(ctx, subjectNS, newer.Fact.ID, NewEvidenceSnapshot(newer.Fact).ContentHash); err == nil {
		t.Fatal("new version inherited quote")
	}
	got, err := s.SearchScoped(ctx, subjectNS, []string{"policy"}, 8, false, RecallOptions{})
	if err != nil || len(got.Facts) != 1 || got.Facts[0].ID != newer.Fact.ID || got.Facts[0].Subjects[0].Revisions != 1 || got.Facts[0].ShortID == "" {
		t.Fatalf("search %+v %v", got, err)
	}
	raw, err := newer.Fact.marshal()
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, leaked := fields["subjects"]; leaked {
		t.Fatalf("projection leaked into row %s %v", raw, err)
	}
	f := newer.Fact
	f.Subjects = nil
	if NewEvidenceSnapshot(f).ContentHash != NewEvidenceSnapshot(newer.Fact).ContentHash {
		t.Fatal("subject altered PR-D hash")
	}
}

func TestSubjectLegacyMergeAndDeclaredReplacementAmbiguity(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	a := putSubject(t, s, "A", "alpha").Fact
	b := putSubject(t, s, "B", "beta").Fact
	before := revisionDBSnapshot(t, s)
	_, err := s.PutManyReturningReceipts(context.Background(), subjectNS, []WriteFact{{Text: "new merge", Options: WriteOptions{Replaces: []string{a.ID, b.ID}}}})
	if !errors.Is(err, ErrSubjectAmbiguous) || !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatalf("undeclared merge %v", err)
	}
	merged, err := s.ReviseFactsWithOptions(context.Background(), subjectNS, "legacy merge", WriteOptions{}, a, b)
	if err != nil || len(merged.Subjects) != 2 {
		t.Fatalf("legacy merge %+v %v", merged, err)
	}
	before = revisionDBSnapshot(t, s)
	_, err = s.PutManyReturningReceipts(context.Background(), subjectNS, []WriteFact{{Text: "only alpha", Options: WriteOptions{Subject: subjectString("A")}}})
	if !errors.Is(err, ErrSubjectAmbiguous) || !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatalf("shared head split %v", err)
	}
	next, err := s.PutManyReturningReceipts(context.Background(), subjectNS, []WriteFact{{Text: "declared combined", Options: WriteOptions{Replaces: []string{merged.ID}}}})
	if err != nil || len(next[0].Subjects) != 2 || next[0].Subjects[0].Name != "A" || next[0].Subjects[1].Name != "B" || next[0].Subjects[1].Revisions != 2 {
		t.Fatalf("lost associations %+v %v", next, err)
	}
}

func TestSubjectConcurrentWritersResolveInsideTransaction(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	initial := putSubject(t, s, "policy", "initial").Fact
	start := make(chan struct{})
	committed := make(chan WriteReceipt, 4)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			text := fmt.Sprintf("value %d", i)
			receipts, err := s.PutManyReturningReceipts(context.Background(), subjectNS, []WriteFact{{Text: text, Options: WriteOptions{Subject: subjectString("policy")}}})
			if err != nil {
				t.Error(err)
				return
			}
			if len(receipts) != 1 || receipts[0].Fact.Text != text {
				t.Errorf("wrong confirmed receipt %+v", receipts)
				return
			}
			committed <- receipts[0]
		}(i)
	}
	close(start)
	wg.Wait()
	close(committed)
	known := map[string]Fact{initial.ID: initial}
	byPredecessor := map[string]WriteReceipt{}
	for receipt := range committed {
		if receipt.Fact.ID == "" || known[receipt.Fact.ID].ID != "" {
			t.Fatalf("duplicate confirmed identity %+v", receipt)
		}
		if len(receipt.Replaced) != 1 || len(receipt.Subjects) != 1 {
			t.Fatalf("missing replacement notice %+v", receipt)
		}
		predecessor := receipt.Replaced[0].ID
		if byPredecessor[predecessor].Fact.ID != "" {
			t.Fatalf("two committed writes claim predecessor %s", predecessor)
		}
		byPredecessor[predecessor] = receipt
		known[receipt.Fact.ID] = receipt.Fact
	}
	if len(known) != 5 || len(byPredecessor) != 4 {
		t.Fatalf("incomplete confirmed writes known=%d receipts=%d", len(known), len(byPredecessor))
	}
	current := initial
	visited := map[string]bool{initial.ID: true}
	for revision := uint64(1); revision <= 4; revision++ {
		receipt, exists := byPredecessor[current.ID]
		if !exists || receipt.Replaced[0].Text != current.Text || receipt.Subjects[0].Revisions != revision || visited[receipt.Fact.ID] {
			t.Fatalf("broken or cyclic committed chain after %+v: %+v", current, receipt)
		}
		visited[receipt.Fact.ID] = true
		current = receipt.Fact
	}
	if _, exists := byPredecessor[current.ID]; exists {
		t.Fatal("head has an unexpected successor")
	}
	rows, err := s.List(subjectNS)
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, fact := range rows {
		if known[fact.ID].Text != fact.Text {
			t.Fatalf("row differs from confirmed receipt %+v", fact)
		}
		if !fact.IsSuperseded() {
			live++
			if fact.ID != current.ID || len(fact.Subjects) != 1 || fact.Subjects[0].Revisions != 4 {
				t.Fatalf("lost committed revision %+v", fact)
			}
		} else if byPredecessor[fact.ID].Fact.ID != fact.SupersededBy {
			t.Fatalf("retired row disagrees with notice %+v", fact)
		}
	}
	if live != 1 || len(rows) != 5 {
		t.Fatalf("rows=%d live=%d", len(rows), live)
	}
}

func TestSubjectBatchRollsBackAfterFirstReplacement(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{CandidateRetrieval: true})
	putSubject(t, s, "A", "old alpha")
	putSubject(t, s, "B", "old beta")
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return replaceRevisionValueWithBucket(idxBucketRO(tx, bucketIdxTerms, subjectNS), []byte("poisonword"))
	}); err != nil {
		t.Fatal(err)
	}
	before := revisionDBSnapshot(t, s)
	_, err := s.PutManyReturningFacts(context.Background(), subjectNS, []WriteFact{{Text: "new alpha", Options: WriteOptions{Subject: subjectString("A")}}, {Text: "poisonword", Options: WriteOptions{Subject: subjectString("B")}}})
	if err == nil || !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatalf("batch retained partial revision: %v", err)
	}
}

func TestSubjectOptionsSnapshotBeforeEmbedding(t *testing.T) {
	var mutate func()
	s := openAtomicRevisionStore(t, StoreConfig{Embedder: &utilityEmbedder{fn: func(string) []float32 {
		if mutate != nil {
			mutate()
		}
		return nil
	}}})
	first := putSubject(t, s, "policy", "old")
	ids := []string{first.Fact.ID}
	input := []WriteFact{{Text: "new", Options: WriteOptions{Replaces: ids}}}
	mutate = func() { ids[0] = "missing"; input[0].Text = "changed" }
	got, err := s.PutManyReturningReceipts(context.Background(), subjectNS, input)
	if err != nil || got[0].Fact.Text != "new" || got[0].Replaced[0].ID != first.Fact.ID {
		t.Fatalf("caller mutation affected write %+v %v", got, err)
	}
}

func TestSubjectSearchRejectsRevisionBetweenQueries(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	putSubject(t, s, "policy", "routing first")
	changed := false
	s.cfg.OnRecall = func(_, _ string, _ int, _ time.Duration) {
		if !changed {
			changed = true
			putSubject(t, s, "policy", "routing second")
		}
	}
	got, err := s.SearchScoped(context.Background(), subjectNS, []string{"routing", "routing"}, 8, false, RecallOptions{})
	if err == nil || !strings.Contains(err.Error(), "subject head changed") || len(got.Facts) != 0 {
		t.Fatalf("mixed heads escaped %+v %v", got, err)
	}
	rows, err := s.List(subjectNS)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rows {
		if f.AccessCount != 0 {
			t.Fatalf("failed search touched %+v", f)
		}
	}
}

func TestSubjectSuggestionsArePureScopedAndBounded(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	for range 5 {
		if err := s.Put(context.Background(), subjectNS, "alpha beta gamma delta"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put(context.Background(), SharedAgentID, "alpha beta gamma delta"); err != nil {
		t.Fatal(err)
	}
	before := revisionDBSnapshot(t, s)
	got, err := s.SuggestSimilarFacts(context.Background(), subjectNS, "ALPHA beta gamma delta")
	if err != nil || got.Total != 5 || len(got.Suggestions) != 3 || !reflect.DeepEqual(before, revisionDBSnapshot(t, s)) {
		t.Fatalf("impure suggestions %+v %v", got, err)
	}
	for i := 1; i < len(got.Suggestions); i++ {
		if got.Suggestions[i-1].ID >= got.Suggestions[i].ID {
			t.Fatal("unstable tie order")
		}
	}
	if r := similarFactSuggestions(subjectNS, "one two", []Fact{{ID: "short", AgentID: subjectNS, Text: "one two three"}}); r.Total != 0 {
		t.Fatal("short partial overlap suggested")
	}
	rows, err := s.PutManyReturningReceipts(context.Background(), subjectNS, []WriteFact{{Text: "alpha beta gamma delta"}, {Text: "alpha beta gamma delta"}})
	if err != nil || rows[0].SuggestionsTotal != 5 || rows[1].SuggestionsTotal != 5 {
		t.Fatalf("suggestions saw batch writes %+v %v", rows, err)
	}
}

func TestSubjectClosingNeverRevivesPreviousVersion(t *testing.T) {
	for _, action := range []string{"delete", "retire"} {
		t.Run(action, func(t *testing.T) {
			s := openAtomicRevisionStore(t, StoreConfig{})
			putSubject(t, s, "policy", "old")
			latest := putSubject(t, s, "policy", "current").Fact
			var err error
			if action == "delete" {
				err = s.Delete(subjectNS, latest.ID)
			} else {
				err = s.Retire(subjectNS, latest)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.SearchScoped(context.Background(), subjectNS, []string{"policy old current"}, 8, false, RecallOptions{})
			if err != nil || len(got.Facts) != 0 {
				t.Fatalf("resurrected %+v %v", got, err)
			}
			next := putSubject(t, s, "policy", "new explicit assertion")
			if len(next.Replaced) != 0 || next.Subjects[0].Revisions != 2 {
				t.Fatalf("closure lost %+v", next)
			}
		})
	}
}

func TestSubjectBackupRoundTripAndProjectionExcludedFromRows(t *testing.T) {
	s := openAtomicRevisionStore(t, StoreConfig{})
	putSubject(t, s, "policy", "old")
	latest := putSubject(t, s, "policy", "current")
	var image bytes.Buffer
	if err := s.Backup(context.Background(), &image); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restored.db")
	if err := RestoreLineageBackup(context.Background(), bytes.NewReader(image.Bytes()), path); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *bolt.Tx) error {
		idx, err := resolveSubjectIndexTx(tx, subjectNS)
		if err != nil {
			return err
		}
		if idx.Heads["policy"].Fact.ID != latest.Fact.ID || idx.Heads["policy"].Revisions != 1 {
			return errors.New("restored wrong head")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		raw := lineageRowBucket(tx, subjectNS).Get([]byte(latest.Fact.ID))
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value["subjects"] != nil {
			return errors.New("subjects stored in canonical row")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
