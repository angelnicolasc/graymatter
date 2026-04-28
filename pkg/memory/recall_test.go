package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestTokenize_Basic verifies lowercase conversion and stopword removal.
func TestTokenize_Basic(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"The quick brown fox", []string{"quick", "brown", "fox"}},
		{"is it in on at", []string{}}, // all stopwords
		{"Paris is the capital of France", []string{"paris", "capital", "france"}},
		{"", []string{}},
		{"A B C", []string{}}, // single-char tokens filtered out
		{"hello123 world", []string{"hello123", "world"}},
	}

	for _, tc := range cases {
		got := tokenize(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tc.input, got, tc.want)
			continue
		}
		for i, w := range tc.want {
			if got[i] != w {
				t.Errorf("tokenize(%q)[%d] = %q, want %q", tc.input, i, got[i], w)
			}
		}
	}
}

// TestTokenize_StopWordSet verifies the package-level set is properly initialized.
func TestTokenize_StopWordSet(t *testing.T) {
	if len(stopWordSet) == 0 {
		t.Error("stopWordSet must not be empty")
	}
	for _, w := range []string{"the", "is", "in", "on", "at", "a", "an"} {
		if !stopWordSet[w] {
			t.Errorf("expected %q in stopWordSet", w)
		}
	}
}

// TestKeywordScore_RanksByRelevance verifies that a fact sharing more query
// terms ranks higher than one sharing fewer.
func TestKeywordScore_RanksByRelevance(t *testing.T) {
	facts := []Fact{
		{ID: "f1", Text: "Paris is the capital of France and a beautiful city"},
		{ID: "f2", Text: "London is the capital of England"},
		{ID: "f3", Text: "Tokyo is a city in Japan"},
	}
	query := "Paris capital France"
	scores := keywordScore(query, facts)

	if scores["f1"] <= scores["f2"] {
		t.Errorf("f1 should rank higher than f2 for query %q; f1=%.4f f2=%.4f", query, scores["f1"], scores["f2"])
	}
	if scores["f2"] <= scores["f3"] {
		t.Errorf("f2 should rank higher than f3 for query %q; f2=%.4f f3=%.4f", query, scores["f2"], scores["f3"])
	}
}

// TestKeywordScore_EmptyQuery returns nil, not an error.
func TestKeywordScore_EmptyQuery(t *testing.T) {
	facts := []Fact{{ID: "f1", Text: "some fact"}}
	scores := keywordScore("", facts)
	if len(scores) != 0 {
		t.Errorf("expected empty scores for empty query, got %v", scores)
	}
}

// TestKeywordScore_EmptyFacts returns empty map.
func TestKeywordScore_EmptyFacts(t *testing.T) {
	scores := keywordScore("query", []Fact{})
	if len(scores) != 0 {
		t.Errorf("expected empty scores for empty facts, got %v", scores)
	}
}

// TestRecall_ReturnsTopK verifies that Recall returns at most topK results.
func TestRecall_ReturnsTopK(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	for i := 0; i < 20; i++ {
		_ = s.Put(ctx, "topk-agent", strings.Repeat("fact ", i+1)+"about Paris")
	}

	results, err := s.Recall(ctx, "topk-agent", "Paris", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) > 5 {
		t.Errorf("Recall returned %d results, want ≤5", len(results))
	}
}

// TestRecall_EmptyStore returns no error and no results.
func TestRecall_EmptyStore(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	results, err := s.Recall(context.Background(), "nobody", "anything", 5)
	if err != nil {
		t.Fatalf("Recall on empty store: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results on empty store, got %d", len(results))
	}
}

// TestRecall_RelevantBeforeIrrelevant verifies that keyword-relevant facts
// rank ahead of unrelated ones.
func TestRecall_RelevantBeforeIrrelevant(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	_ = s.Put(ctx, "rank-agent", "Maria Rodriguez is VP Sales at Acme Corp.")
	_ = s.Put(ctx, "rank-agent", "The sky is blue and the grass is green.")
	_ = s.Put(ctx, "rank-agent", "Maria Rodriguez sent an email about the Q3 deal.")

	results, err := s.Recall(ctx, "rank-agent", "Maria Rodriguez", 3)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results, got none")
	}
	// The first result must mention Maria.
	if !strings.Contains(results[0], "Maria") {
		t.Errorf("top result should mention Maria; got %q", results[0])
	}
}

// TestRecall_UpdatesAccessMetadata verifies AccessCount and AccessedAt are
// updated after a successful Recall.
func TestRecall_UpdatesAccessMetadata(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	_ = s.Put(ctx, "meta-agent", "Unique trackable fact about elephants.")

	before, _ := s.List("meta-agent")
	if len(before) == 0 {
		t.Fatal("expected 1 fact")
	}
	initialCount := before[0].AccessCount
	initialAccess := before[0].AccessedAt

	// Small sleep to ensure time advances.
	time.Sleep(5 * time.Millisecond)

	_, err := s.Recall(ctx, "meta-agent", "elephants", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	// Recall updates access metadata in a background goroutine. Poll with a
	// deadline rather than relying on a fixed sleep — under -race on slow CI
	// runners a fixed wait becomes flaky.
	var after []Fact
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		after, _ = s.List("meta-agent")
		if len(after) > 0 && after[0].AccessCount > initialCount && after[0].AccessedAt.After(initialAccess) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(after) == 0 {
		t.Fatal("fact disappeared after recall")
	}
	if after[0].AccessCount <= initialCount {
		t.Errorf("AccessCount not incremented within 2s: before=%d after=%d", initialCount, after[0].AccessCount)
	}
	if !after[0].AccessedAt.After(initialAccess) {
		t.Errorf("AccessedAt not advanced within 2s: before=%v after=%v", initialAccess, after[0].AccessedAt)
	}
}

// TestRecall_NoDuplicates verifies that no text appears more than once in results.
func TestRecall_NoDuplicates(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()

	ctx := context.Background()
	_ = s.Put(ctx, "dedup-agent", "Paris is the capital of France.")
	_ = s.Put(ctx, "dedup-agent", "The Eiffel Tower is in Paris.")

	results, err := s.Recall(ctx, "dedup-agent", "Paris France", 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	seen := make(map[string]bool, len(results))
	for _, r := range results {
		if seen[r] {
			t.Errorf("duplicate result in Recall: %q", r)
		}
		seen[r] = true
	}
}

// BenchmarkRecall measures Recall latency across different fact set sizes.
func BenchmarkRecall_100(b *testing.B)  { benchmarkRecall(b, 100) }
func BenchmarkRecall_1000(b *testing.B) { benchmarkRecall(b, 1000) }

func benchmarkRecall(b *testing.B, n int) {
	b.Helper()
	dir := b.TempDir()
	s, err := Open(StoreConfig{DataDir: dir, Embedder: nil, DecayHalfLife: 720 * time.Hour})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	for i := 0; i < n; i++ {
		_ = s.Put(ctx, "bench-agent", strings.Repeat("bench fact ", 5))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Recall(ctx, "bench-agent", "bench fact query", 8)
	}
}

// BenchmarkTokenize measures tokenize throughput.
func BenchmarkTokenize(b *testing.B) {
	text := "The quick brown fox jumps over the lazy dog and Paris is the capital of France"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tokenize(text)
	}
}

// BenchmarkKeywordScore measures scoring throughput with 1000 facts.
func BenchmarkKeywordScore(b *testing.B) {
	facts := make([]Fact, 1000)
	for i := range facts {
		facts[i] = Fact{ID: strings.Repeat("x", i+1), Text: "Paris capital France Eiffel Tower"}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = keywordScore("Paris France capital", facts)
	}
}
