package memory

import (
	"context"
	"strings"
	"testing"
)

// Alias vocabulary and weak-match feedback, against the specification both
// were written to. The two invariants it demands and this file pins: the alias widens
// what a query can reach without ever appearing as a result, and the feedback
// block is additive text — the facts and their order are identical with it on
// or off.

func aliasStore(t *testing.T) (*Store, func()) {
	t.Helper()
	s, _, done := newExplainStore(t)
	ctx := context.Background()
	for _, txt := range []string{
		"merchant acquiring moved to FinRoute",
		"payments route through FinRoute since March",
		"billing reconciliation runs daily at 02:00 UTC",
		"the compliance queue flags transactions over $25,000",
	} {
		if err := s.Put(ctx, "alias-agent", txt); err != nil {
			t.Fatal(err)
		}
	}
	return s, done
}

func TestAliasBridgesVocabularyGap(t *testing.T) {
	s, done := aliasStore(t)
	defer done()
	ctx := context.Background()

	// The query's word ("paying") and the store's word ("payments") differ
	// lexically and no stemmer is on: keyword gives the fact nothing, so with
	// only recency to rank by the payments fact cannot lead the result.
	got, err := s.Recall(ctx, "alias-agent", "who handles the paying process?", 5)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got[0], "payments route through FinRoute") {
		t.Fatalf("payments fact led the result without alias: %v", got)
	}

	if _, err := s.PutAlias(ctx, "alias-agent", "paying", []string{"payments"}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Recall(ctx, "alias-agent", "who handles the paying process?", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got[0], "payments route through FinRoute") {
		t.Fatalf("alias did not bridge the vocabulary gap, got %v", got)
	}
}

func TestAliasFactsAreNeverInjected(t *testing.T) {
	s, done := aliasStore(t)
	defer done()
	ctx := context.Background()

	if _, err := s.PutAlias(ctx, "alias-agent", "payments", []string{"acquiring"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Recall(ctx, "alias-agent", "what handles payments and acquiring and billing?", 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if strings.HasPrefix(f, "alias:") {
			t.Fatalf("alias fact leaked into the result set: %q", f)
		}
	}
}

func TestSupersededAliasStopsExpanding(t *testing.T) {
	s, done := aliasStore(t)
	defer done()
	ctx := context.Background()

	f, err := s.PutAlias(ctx, "alias-agent", "paying", []string{"payments"})
	if err != nil {
		t.Fatal(err)
	}
	// The ordinary machinery applies: revising the alias fact retires the old
	// mapping and installs the new one.
	if _, err := s.Revise(ctx, "alias-agent", "alias: paying = charges", f); err != nil {
		t.Fatal(err)
	}

	got, err := s.Recall(ctx, "alias-agent", "who handles the paying process?", 5)
	if err != nil {
		t.Fatal(err)
	}
	// Two regressions to guard: the revised alias leaking back into the
	// result set as an injectable content fact, and the old mapping still
	// expanding (which would keep leading the payments fact).
	for _, f := range got {
		if strings.HasPrefix(f, "alias:") {
			t.Fatalf("revised alias leaked into the result set: %v", got)
		}
	}
	if strings.HasPrefix(got[0], "payments route through FinRoute") {
		t.Fatalf("superseded alias still expands: %v", got)
	}
}

func TestFeedbackAdditiveRanking(t *testing.T) {
	s, done := aliasStore(t)
	defer done()
	ctx := context.Background()
	if _, err := s.PutAlias(ctx, "alias-agent", "payments", []string{"acquiring"}); err != nil {
		t.Fatal(err)
	}
	queries := []string{
		"how often does billing reconciliation run?",
		"who runs the acquiring side?",
		"what is the transaction flagging threshold?",
		"a question about something entirely absent from this store",
	}
	for _, q := range queries {
		plain, err := s.Recall(ctx, "alias-agent", q, 8)
		if err != nil {
			t.Fatal(err)
		}
		detailed, _, err := s.RecallDetailed(ctx, "alias-agent", q, 8)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(plain, "|") != strings.Join(detailed, "|") {
			t.Errorf("query %q: RecallDetailed changed the facts:\n  plain     %v\n  detailed  %v", q, plain, detailed)
		}
	}
}

func TestFeedbackFiresOnVocabularyGap(t *testing.T) {
	s, done := aliasStore(t)
	defer done()
	ctx := context.Background()

	// Zero coverage: no query term exists in the store. The block must fire
	// and the fallback must carry the returned facts' own vocabulary.
	_, feedback, err := s.RecallDetailed(ctx, "alias-agent", "what is the frobnication quota for widgets?", 8)
	if err != nil {
		t.Fatal(err)
	}
	if feedback == "" {
		t.Fatal("weak-match feedback did not fire on zero coverage")
	}
	if !strings.Contains(feedback, "weak match:") {
		t.Errorf("feedback missing the diagnostic line: %q", feedback)
	}
	if !strings.Contains(feedback, "nearby vocabulary:") {
		t.Errorf("feedback missing the vocabulary line: %q", feedback)
	}
	if !strings.Contains(feedback, "memory_alias") {
		t.Errorf("feedback missing the suggested action: %q", feedback)
	}
}

func TestFeedbackQuietOnStrongMatch(t *testing.T) {
	s, done := aliasStore(t)
	defer done()
	ctx := context.Background()

	// Every content term exists in the store and the top fact overlaps
	// heavily: both halves of the OR trigger stay quiet. Note
	// the trigger counts natural-language filler ("how", "does") as missing
	// terms — that is what the specification asks for, and the fire-rate
	// criterion of the evaluation corpus is what measures whether the OR is
	// too eager.
	_, feedback, err := s.RecallDetailed(ctx, "alias-agent", "billing reconciliation runs daily", 8)
	if err != nil {
		t.Fatal(err)
	}
	if feedback != "" {
		t.Errorf("feedback fired on a strong match: %q", feedback)
	}
}

func TestFeedbackVocabularyNamesStoreTerms(t *testing.T) {
	s, done := aliasStore(t)
	defer done()
	ctx := context.Background()

	// One seed term exists ("reconciliation"), the rest of the query does not:
	// the neighbourhood should surface the store's own nearby vocabulary.
	_, feedback, err := s.RecallDetailed(ctx, "alias-agent", "reconciliation schedule details", 8)
	if err != nil {
		t.Fatal(err)
	}
	if feedback == "" {
		t.Fatal("feedback did not fire for a one-seed query")
	}
	if !strings.Contains(feedback, "reconciliation") && !strings.Contains(feedback, "billing") {
		t.Errorf("vocabulary line carries none of the store's terms: %q", feedback)
	}
}
