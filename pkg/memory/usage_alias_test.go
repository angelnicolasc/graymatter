package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Usage-alias learning. The guardrails below are the ones pre-registered
// before the code existed — k=2, one pending miss per agent, df filters,
// source marking, config gate. The property that makes the paradigm claim:
// after two sessions make the same lexical mistake, the store fixes it
// itself, with no agent action and no semantic decision server-side.

func learningStore(t *testing.T, learning bool) (*Store, *explainClock, func()) {
	t.Helper()
	s, clock, done := newExplainStore(t)
	s.cfg.UsageAliasLearning = learning
	ctx := context.Background()
	for _, txt := range []string{
		"payments route through FinRoute since March",
		"merchant acquiring moved to FinRoute",
		"billing reconciliation runs daily at 02:00 UTC",
	} {
		clock.offset += time.Hour
		if err := s.Put(ctx, "learn-agent", txt); err != nil {
			t.Fatal(err)
		}
	}
	return s, clock, done
}

func TestUsageAliasPromotedOnSecondObservation(t *testing.T) {
	s, clock, done := learningStore(t, true)
	defer done()
	ctx := context.Background()
	weak := "who handles the paying process?"
	strong := "payments route through FinRoute"

	// Session 1: the weak match leaves a pending miss; the strong match
	// records the evidence. One observation promotes nothing.
	if _, fb, err := s.RecallDetailed(ctx, "learn-agent", weak, 8); err != nil {
		t.Fatal(err)
	} else if fb == "" {
		t.Fatal("precondition: the reformulation probe must fire the weak-match trigger")
	}
	if _, fb, err := s.RecallDetailed(ctx, "learn-agent", strong, 8); err != nil {
		t.Fatal(err)
	} else if fb != "" {
		t.Fatalf("precondition: the reformulated query must be a strong match, got %q", fb)
	}
	if n := s.countUsageAliases("learn-agent"); n != 0 {
		t.Fatalf("promotion after a single observation: %d usage aliases", n)
	}

	// Session 2: same mistake, same fix — the pair reaches k=2 and promotes.
	clock.offset += time.Hour
	if _, _, err := s.RecallDetailed(ctx, "learn-agent", weak, 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecallDetailed(ctx, "learn-agent", strong, 8); err != nil {
		t.Fatal(err)
	}
	if n := s.countUsageAliases("learn-agent"); n != 1 {
		t.Fatalf("got %d usage aliases after two observations, want exactly 1", n)
	}
	terms := s.usageAliasTerms("learn-agent")
	if len(terms) != 1 || !strings.Contains(terms[0], "paying") || !strings.Contains(terms[0], "payments") {
		t.Fatalf("promoted alias is %v, want the paying=payments pair", terms)
	}

	// The flywheel closes mechanically: the weak query now leads with the
	// fact the strong vocabulary matches, with no agent in the loop.
	got, err := s.Recall(ctx, "learn-agent", weak, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got[0], "payments route through FinRoute") {
		t.Fatalf("promoted alias did not bridge the gap, got %v", got)
	}
}

func TestUsageAliasLearningDisabledByConfig(t *testing.T) {
	s, _, done := learningStore(t, false)
	defer done()
	ctx := context.Background()
	weak := "who handles the paying process?"
	strong := "payments route through FinRoute"

	for session := 0; session < 3; session++ {
		if _, _, err := s.RecallDetailed(ctx, "learn-agent", weak, 8); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.RecallDetailed(ctx, "learn-agent", strong, 8); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.countUsageAliases("learn-agent"); n != 0 {
		t.Fatalf("learning ran with the config gate off: %d usage aliases", n)
	}
}

func TestUsageAliasSourceMarking(t *testing.T) {
	s, _, done := learningStore(t, true)
	defer done()
	ctx := context.Background()

	// Agent-written: empty source (curation).
	if _, err := s.PutAlias(ctx, "learn-agent", "paying", []string{"payments"}); err != nil {
		t.Fatal(err)
	}
	// Store-promoted: "usage".
	if _, err := s.promoteUsageAlias(ctx, "learn-agent", "paying", "acquiring"); err != nil {
		t.Fatal(err)
	}
	// Same pair two ways must stay ONE alias: single-token expansion is
	// bidirectional, so promoting the reverse adds nothing but noise.
	seen := map[string]bool{}
	list, err := s.List("learn-agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range list {
		if f.IsAlias() {
			if f.AliasSource == AliasSourceUsage && seen["usage:"+f.Text] {
				t.Errorf("duplicate usage alias: %q", f.Text)
			}
			seen["usage:"+f.Text] = f.AliasSource == AliasSourceUsage
			if strings.HasPrefix(f.Text, "alias: paying = acquiring") && f.AliasSource != AliasSourceUsage {
				t.Errorf("promoted alias is not marked source=usage: %q", f.Text)
			}
			if strings.HasPrefix(f.Text, "alias: paying = payments") && f.AliasSource != "" {
				t.Errorf("agent alias carries a source marker: %q", f.Text)
			}
		}
	}
}

func TestUsageAliasReviseBecomesAgentAuthored(t *testing.T) {
	s, _, done := learningStore(t, true)
	defer done()
	ctx := context.Background()

	f, err := s.promoteUsageAlias(ctx, "learn-agent", "paying", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revise(ctx, "learn-agent", "alias: paying = card payments", f); err != nil {
		t.Fatal(err)
	}
	list, err := s.List("learn-agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range list {
		if got.IsSuperseded() {
			continue
		}
		if got.IsAlias() && strings.Contains(got.Text, "card payments") {
			if got.AliasSource == AliasSourceUsage {
				t.Errorf("agent-revised alias still marked usage: %q", got.Text)
			}
		}
	}
}

func TestPendingMissExpires(t *testing.T) {
	s, clock, done := learningStore(t, true)
	defer done()
	ctx := context.Background()
	weak := "who handles the paying process?"
	strong := "payments route through FinRoute"

	// The weak match is followed, past the TTL, by a strong match: the stale
	// pending must not pair with vocabulary it was never about.
	if _, _, err := s.RecallDetailed(ctx, "learn-agent", weak, 8); err != nil {
		t.Fatal(err)
	}
	clock.offset += 2 * pendingMissTTL
	if _, _, err := s.RecallDetailed(ctx, "learn-agent", strong, 8); err != nil {
		t.Fatal(err)
	}
	clock.offset += time.Hour
	if _, _, err := s.RecallDetailed(ctx, "learn-agent", weak, 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecallDetailed(ctx, "learn-agent", strong, 8); err != nil {
		t.Fatal(err)
	}
	// One in-TTL observation only: nothing promotes.
	if n := s.countUsageAliases("learn-agent"); n != 0 {
		t.Fatalf("stale pending promoted vocabulary: %d usage aliases", n)
	}
}

func TestUnknownWordFilterOnPromotion(t *testing.T) {
	s, clock, done := learningStore(t, true)
	defer done()
	ctx := context.Background()

	// The weak query's KNOWN term must never pair: an alias teaches "your
	// word for my word", and "billing" already is a store word.
	weak := "billing obligations of the paying process?"
	strong := "billing reconciliation runs daily"
	if _, _, err := s.RecallDetailed(ctx, "learn-agent", weak, 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecallDetailed(ctx, "learn-agent", strong, 8); err != nil {
		t.Fatal(err)
	}
	clock.offset += time.Hour
	if _, _, err := s.RecallDetailed(ctx, "learn-agent", weak, 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecallDetailed(ctx, "learn-agent", strong, 8); err != nil {
		t.Fatal(err)
	}
	for _, pair := range s.usageAliasTerms("learn-agent") {
		if strings.Contains(pair, "billing") {
			t.Errorf("known store word promoted as an alias side: %q", pair)
		}
	}
}
