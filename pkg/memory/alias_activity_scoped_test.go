package memory

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestScopedBatchAliasActivityPreservesPolicyAndCanonicalTouches(t *testing.T) {
	cfg := aliasActivityConfig(t, true)
	cfg.ConfidenceWeight = 0.4
	s := openAliasActivityStore(t, cfg)
	ctx := context.Background()
	content := putActivityFact(t, s, "agent", "acquiring system")
	sharedContent := putActivityFact(t, s, SharedAgentID, content.Text)
	a := putActivityAlias(t, s, "agent", "broker", "acquiring")
	b := putActivityAlias(t, s, SharedAgentID, "broker", "acquiring")
	now := aliasTestTime.Add(time.Hour)
	s.now = func() time.Time { return now }

	var batch ScopedBatchRecaller = s
	queries := []string{"broker", "Broker", "broker"}
	rows, err := batch.BatchRecallScoped(ctx, "agent", queries, 0, true, nil)
	if err != nil || len(rows) != len(queries) {
		t.Fatalf("legacy batch = %+v, %v", rows, err)
	}
	for i, row := range rows {
		if row.Query != queries[i] || row.Err != nil || len(row.Facts) != 0 || row.Retrieval != nil {
			t.Fatalf("legacy zero-top-k or omitted-policy result changed: %+v", row)
		}
	}
	requireAliasActivity(t, s, a, 1, now)
	requireAliasActivity(t, s, b, 1, now)

	zero := 0.0
	rows, err = batch.BatchRecallScoped(ctx, "agent", queries, 1, true, &RecallOptions{ConfidenceWeight: &zero})
	if err != nil || len(rows) != len(queries) {
		t.Fatalf("option-aware batch = %+v, %v", rows, err)
	}
	for i, row := range rows {
		if row.Query != queries[i] || row.Err != nil || !slices.Equal(row.Facts, []string{content.Text}) || row.Retrieval == nil || row.Retrieval.ConfidenceWeight != 0 {
			t.Fatalf("option-aware result lost scope, order, or explicit zero: %+v", row)
		}
	}
	requireAliasActivity(t, s, a, 2, now)
	requireAliasActivity(t, s, b, 2, now)
	agent, _ := s.loadFact("agent", content.ID)
	shared, _ := s.loadFact(SharedAgentID, sharedContent.ID)
	if agent.AccessCount != len(queries) || shared.AccessCount != 0 {
		t.Fatalf("canonical batch touch count changed: agent=%d, shared=%d", agent.AccessCount, shared.AccessCount)
	}
}

func TestAliasActivityEmptyRecallCancellation(t *testing.T) {
	for _, path := range []struct {
		name    string
		indexed bool
	}{{"scan", false}, {"indexed", true}} {
		for _, method := range []struct {
			name   string
			recall func(context.Context, *Store) error
		}{
			{"recall", func(ctx context.Context, s *Store) error {
				_, err := s.Recall(ctx, "agent", "broker", 1)
				return err
			}},
			{"explain", func(ctx context.Context, s *Store) error {
				_, err := s.RecallExplain(ctx, "agent", "broker", 1)
				return err
			}},
		} {
			t.Run(path.name+"/"+method.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				cfg := aliasActivityConfig(t, path.indexed)
				calls := 0
				cfg.OnRecall = func(string, string, int, time.Duration) {
					calls++
					cancel()
				}
				s := openAliasActivityStore(t, cfg)
				a := putActivityAlias(t, s, "agent", "broker", "acquiring")
				s.now = func() time.Time { return aliasTestTime.Add(time.Hour) }

				if err := method.recall(ctx, s); !errors.Is(err, context.Canceled) {
					t.Fatalf("empty recall error = %v, want cancellation", err)
				}
				if calls != 1 {
					t.Fatalf("empty recall hook called %d times, want 1", calls)
				}
				requireAliasActivity(t, s, a, 0, aliasTestTime)
			})
		}
	}
}

func TestAliasActivityLegacyBlankSkipsKGEnrichment(t *testing.T) {
	s := openAliasActivityStore(t, aliasActivityConfig(t, true))
	blank := putActivityFact(t, s, "legacy", " \t ")
	content := putActivityFact(t, s, "ordinary", "acquiring system")
	graph := &confidenceHintGraph{labels: []string{"graph neighbor"}}
	extractor := &inspectionExtractor{}
	s.SetKG(graph, extractor)

	got, err := s.Recall(context.Background(), "legacy", "unmatched query", 1)
	if err != nil || !slices.Equal(got, []string{blank.Text}) || extractor.calls != 0 || graph.calls != 0 {
		t.Fatalf("legacy blank recall = %q, %v, extractor/graph calls = %d/%d", got, err, extractor.calls, graph.calls)
	}
	got, err = s.Recall(context.Background(), "ordinary", "acquiring", 1)
	if err != nil || !slices.Equal(got, []string{content.Text, "graph neighbor"}) || extractor.calls != 1 || graph.calls != 1 {
		t.Fatalf("ordinary recall = %q, %v, extractor/graph calls = %d/%d", got, err, extractor.calls, graph.calls)
	}
}
