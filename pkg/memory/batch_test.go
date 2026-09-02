package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// The batch exists to save conversational turns, not to change answers. The
// first property is therefore the most important one: a batch of one must be
// indistinguishable from a plain Recall, and a batch of many must return
// exactly what those queries return one at a time. A batch that quietly ranked
// differently would be a second retrieval implementation, which is the thing
// explain_test.go already refuses to allow.

func batchStore(t *testing.T) (*Store, *explainClock, func()) {
	t.Helper()
	s, clock, done := newExplainStore(t)
	ctx := context.Background()
	for _, txt := range []string{
		"the webhook retry limit is now 8 attempts",
		"the export cap was raised to 50000 rows",
		"Kenji Mori took over on-call for billing",
		"the primary database lives in eu-central-1",
		"backups are retained for 91 days",
		"CI caches Go modules between runs",
		"the search team owns the relevance dashboard",
	} {
		clock.offset += time.Hour
		if err := s.Put(ctx, "explain-agent", txt); err != nil {
			t.Fatal(err)
		}
	}
	return s, clock, done
}

func TestBatchRecallMatchesRecallQueryForQuery(t *testing.T) {
	s, _, done := batchStore(t)
	defer done()
	ctx := context.Background()
	queries := []string{
		"how many webhook retries do we allow?",
		"what is the maximum export size?",
		"who is on call for billing?",
		"which region hosts the primary database?",
	}

	got, err := s.BatchRecall(ctx, "explain-agent", queries, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(queries) {
		t.Fatalf("got %d results for %d queries", len(got), len(queries))
	}
	for i, q := range queries {
		if got[i].Query != q {
			t.Errorf("result %d is for %q, want %q — order must match the input", i, got[i].Query, q)
		}
		want, err := s.Recall(ctx, "explain-agent", q, 5)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got[i].Facts, "|") != strings.Join(want, "|") {
			t.Errorf("query %q:\n  batch  %v\n  single %v", q, got[i].Facts, want)
		}
	}
}

func TestBatchOfOneIsAPlainRecall(t *testing.T) {
	s, _, done := batchStore(t)
	defer done()
	ctx := context.Background()
	const q = "how long do we keep backups?"

	single, err := s.Recall(ctx, "explain-agent", q, 5)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.BatchRecall(ctx, "explain-agent", []string{q}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 {
		t.Fatalf("got %d results, want 1", len(batch))
	}
	if strings.Join(batch[0].Facts, "|") != strings.Join(single, "|") {
		t.Errorf("batch of one differs:\n  batch  %v\n  single %v", batch[0].Facts, single)
	}
}

// Order must come from the input, not from which goroutine finished first.
func TestBatchOrderIsDeterministic(t *testing.T) {
	s, _, done := batchStore(t)
	defer done()
	ctx := context.Background()
	queries := make([]string, 0, 24)
	for i := 0; i < 24; i++ {
		queries = append(queries, fmt.Sprintf("query number %d about backups and webhooks", i))
	}
	for run := 0; run < 5; run++ {
		got, err := s.BatchRecall(ctx, "explain-agent", queries, 3)
		if err != nil {
			t.Fatal(err)
		}
		for i, q := range queries {
			if got[i].Query != q {
				t.Fatalf("run %d: position %d holds %q, want %q", run, i, got[i].Query, q)
			}
		}
	}
}

func TestBatchEmptyAndNil(t *testing.T) {
	s, _, done := batchStore(t)
	defer done()
	got, err := s.BatchRecall(context.Background(), "explain-agent", nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty batch returned %d results", len(got))
	}
}

// A fact answering several queries is one fact. Paying for it once is the
// entire point of merging.
func TestMergedFactsDeduplicates(t *testing.T) {
	results := []BatchResult{
		{Query: "a", Facts: []string{"shared fact", "only in a"}},
		{Query: "b", Facts: []string{"only in b", "shared fact"}},
		{Query: "c", Facts: []string{"shared fact"}},
	}
	merged := MergedFacts(results)
	if len(merged) != 3 {
		t.Fatalf("merged %d facts, want 3: %v", len(merged), merged)
	}
	seen := map[string]int{}
	for _, f := range merged {
		seen[f]++
	}
	for f, n := range seen {
		if n != 1 {
			t.Errorf("%q appears %d times in the merged block", f, n)
		}
	}
	// "shared fact" reached rank 1 in two lists; "only in a" reached rank 2.
	if merged[0] != "shared fact" {
		t.Errorf("merged[0] = %q, want the fact that ranked best across queries", merged[0])
	}
}

func TestMergedFactsPrefersBestRankThenAgreement(t *testing.T) {
	results := []BatchResult{
		{Query: "a", Facts: []string{"top of a", "second"}},
		{Query: "b", Facts: []string{"top of b", "second"}},
		{Query: "c", Facts: []string{"top of b"}},
	}
	merged := MergedFacts(results)
	// Both "top of a" and "top of b" hit rank 1; "top of b" satisfied two
	// queries, so agreement breaks the tie.
	if merged[0] != "top of b" {
		t.Errorf("merged = %v; want the rank-1 fact two queries agreed on first", merged)
	}
	if merged[len(merged)-1] != "second" {
		t.Errorf("merged = %v; want the rank-2 fact last", merged)
	}
}

// The store documents every public method as safe for concurrent use; the
// batch depends on that, so it is asserted here rather than assumed.
func TestBatchIsRaceFree(t *testing.T) {
	s, _, done := batchStore(t)
	defer done()
	ctx := context.Background()
	queries := []string{
		"how many webhook retries do we allow?",
		"what is the maximum export size?",
		"who is on call for billing?",
		"which region hosts the primary database?",
		"how long do we keep backups?",
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.BatchRecall(ctx, "explain-agent", queries, 5); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
