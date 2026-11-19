package memory

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	bolt "go.etcd.io/bbolt"
)

var aliasTestTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func openAliasActivityStore(t *testing.T, cfg StoreConfig) *Store {
	t.Helper()
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return aliasTestTime }
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func putActivityAlias(t *testing.T, s *Store, agent, term string, equivalents ...string) Fact {
	t.Helper()
	f, err := s.PutAlias(context.Background(), agent, term, equivalents)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func putActivityFact(t *testing.T, s *Store, agent, text string) Fact {
	t.Helper()
	f, err := s.PutReturningFact(context.Background(), agent, text)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func requireAliasActivity(t *testing.T, s *Store, f Fact, count int, at time.Time) {
	t.Helper()
	got, ok := s.loadFact(f.AgentID, f.ID)
	if !ok || got.AccessCount != count || !got.AccessedAt.Equal(at) {
		t.Fatalf("alias %s/%s exists=%v, count/time = %d/%s, want %d/%s", f.AgentID, f.ID, ok, got.AccessCount, got.AccessedAt, count, at)
	}
	if got.Weight != f.Weight || got.Pinned != f.Pinned {
		t.Fatalf("alias activity changed weight or pin: got %+v, before %+v", got, f)
	}
}

func aliasActivityConfig(t *testing.T, indexed bool) StoreConfig {
	return StoreConfig{DataDir: t.TempDir(), Embedder: embedding.NewKeyword(), CandidateRetrieval: indexed}
}

func TestAliasActivityRecallExplainAllBatchAndPreview(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			ctx := context.Background()
			s := openAliasActivityStore(t, aliasActivityConfig(t, indexed))
			content := putActivityFact(t, s, "agent", "acquiring handles merchant payments")
			putActivityFact(t, s, "agent", "nightly cluster backups")
			alias := putActivityAlias(t, s, "agent", "broker", "acquiring")
			now := aliasTestTime.Add(time.Hour)
			s.now = func() time.Time { return now }

			if indexed {
				pipeline, err := s.runRecallPipelineIndexed(ctx, "agent", "broker", 1)
				if err != nil || pipeline == nil || pipeline.kwRank[content.ID] == 0 {
					t.Fatalf("indexed path failed to rank the alias match: %+v, %v", pipeline, err)
				}
			}
			txID := inspectionTxID(t, s)
			preview, err := s.RecallPreview(ctx, "agent", "broker", 1)
			if err != nil || len(preview) != 1 || preview[0].Text != content.Text || preview[0].Ranks.KeywordRank == 0 {
				t.Fatalf("preview = %+v, %v", preview, err)
			}
			requireAliasActivity(t, s, alias, 0, aliasTestTime)
			if inspectionTxID(t, s) != txID {
				t.Fatal("preview opened a durable write transaction")
			}

			got, err := s.Recall(ctx, "agent", "broker", 1)
			if err != nil || !slices.Equal(got, []string{content.Text}) {
				t.Fatalf("recall = %v, %v", got, err)
			}
			requireAliasActivity(t, s, alias, 1, now)

			explained, err := s.RecallExplain(ctx, "agent", "broker", 1)
			if err != nil || len(explained) != 1 || explained[0].Text != preview[0].Text || explained[0].Ranks != preview[0].Ranks {
				t.Fatalf("explain = %+v, %v, preview = %+v", explained, err, preview)
			}
			requireAliasActivity(t, s, alias, 2, now)

			got, err = s.RecallAll(ctx, "agent", "broker", 1)
			if err != nil || !slices.Equal(got, []string{content.Text}) {
				t.Fatalf("all = %v, %v", got, err)
			}
			requireAliasActivity(t, s, alias, 3, now)

			batch, err := s.BatchRecall(ctx, "agent", []string{"broker", "Broker", "broker"}, 1)
			if err != nil || len(batch) != 3 {
				t.Fatalf("batch = %+v, %v", batch, err)
			}
			for _, row := range batch {
				if row.Err != nil || !slices.Equal(row.Facts, []string{content.Text}) {
					t.Fatalf("batch row = %+v", row)
				}
			}
			requireAliasActivity(t, s, alias, 4, now)
			current, _ := s.loadFact("agent", content.ID)
			if current.AccessCount != 6 {
				t.Fatalf("normal content count = %d, want 6", current.AccessCount)
			}
		})
	}
}

func TestAliasActivityOnlyCreditsNewTokens(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			s := openAliasActivityStore(t, aliasActivityConfig(t, indexed))
			putActivityFact(t, s, "agent", "acquiring gateway")
			first := putActivityAlias(t, s, "agent", "broker", "acquiring")
			second := putActivityAlias(t, s, "agent", "broker", "acquiring")
			if second.ID < first.ID {
				first, second = second, first
			}
			// The older alias shares acquiring, but is the sole source of gateway.
			s.now = func() time.Time { return aliasTestTime.Add(-time.Hour) }
			extra := putActivityAlias(t, s, "agent", "broker", "acquiring", "gateway")
			now := aliasTestTime.Add(time.Hour)
			s.now = func() time.Time { return now }
			if _, err := s.Recall(context.Background(), "agent", "broker acquiring gateway", 1); err != nil {
				t.Fatal(err)
			}
			requireAliasActivity(t, s, first, 0, first.AccessedAt)
			requireAliasActivity(t, s, second, 0, second.AccessedAt)
			requireAliasActivity(t, s, extra, 0, extra.AccessedAt)

			if _, err := s.Recall(context.Background(), "agent", "broker", 1); err != nil {
				t.Fatal(err)
			}
			requireAliasActivity(t, s, first, 1, now)
			requireAliasActivity(t, s, second, 0, second.AccessedAt)
			requireAliasActivity(t, s, extra, 1, now)
		})
	}
}

func TestAliasActivityRecallAllNamespaceAndIdentity(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			ctx := context.Background()
			s := openAliasActivityStore(t, aliasActivityConfig(t, indexed))
			content := putActivityFact(t, s, "agent", "acquiring system")
			sharedContent := putActivityFact(t, s, SharedAgentID, content.Text)
			a := putActivityAlias(t, s, "agent", "broker", "acquiring")
			b, err := s.putReturningFactPrepared(ctx, SharedAgentID, a.Text, KindAlias, "", WriteOptions{}, func(_ *bolt.Tx, f *Fact) error {
				f.ID = a.ID
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			now := aliasTestTime.Add(time.Hour)
			s.now = func() time.Time { return now }
			got, err := s.RecallAll(ctx, "agent", "broker", 1)
			if err != nil || !slices.Equal(got, []string{content.Text}) {
				t.Fatalf("all = %v, %v", got, err)
			}
			requireAliasActivity(t, s, a, 1, now)
			requireAliasActivity(t, s, b, 1, now)
			shared, _ := s.loadFact(SharedAgentID, sharedContent.ID)
			if shared.AccessCount != 0 {
				t.Fatal("namespace merge touched discarded duplicate content")
			}
			if _, err := s.RecallAll(ctx, SharedAgentID, "broker", 1); err != nil {
				t.Fatal(err)
			}
			requireAliasActivity(t, s, a, 1, now)
			requireAliasActivity(t, s, b, 2, now)
		})
	}
}

func TestAliasActivityEmptyResultsAndReadOnlyReopen(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			ctx := context.Background()
			cfg := aliasActivityConfig(t, indexed)
			s := openAliasActivityStore(t, cfg)
			a := putActivityAlias(t, s, "agent", "broker", "acquiring")
			now := aliasTestTime.Add(time.Hour)
			s.now = func() time.Time { return now }
			txID := inspectionTxID(t, s)
			if got, err := s.RecallPreview(ctx, "agent", "broker", 1); err != nil || len(got) != 0 {
				t.Fatalf("preview = %v, %v", got, err)
			}
			requireAliasActivity(t, s, a, 0, aliasTestTime)
			if inspectionTxID(t, s) != txID {
				t.Fatal("alias-only preview changed durable state")
			}
			if got, err := s.Recall(ctx, "agent", "broker", 1); err != nil || len(got) != 0 {
				t.Fatalf("recall = %v, %v", got, err)
			}
			if got, err := s.RecallExplain(ctx, "agent", "broker", 1); err != nil || len(got) != 0 {
				t.Fatalf("explain = %v, %v", got, err)
			}
			if got, err := s.RecallAll(ctx, "agent", "broker", 1); err != nil || len(got) != 0 {
				t.Fatalf("all = %v, %v", got, err)
			}
			rows, err := s.BatchRecall(ctx, "agent", []string{"broker", "broker"}, 1)
			if err != nil || len(rows) != 2 || rows[0].Err != nil || rows[1].Err != nil || len(rows[0].Facts)+len(rows[1].Facts) != 0 {
				t.Fatalf("batch = %+v, %v", rows, err)
			}
			requireAliasActivity(t, s, a, 4, now)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			dbPath := filepath.Join(cfg.DataDir, "gray.db")
			before, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ReadOnly = true
			ro := openAliasActivityStore(t, cfg)
			ro.now = func() time.Time { return now.Add(time.Hour) }
			requireAliasActivity(t, ro, a, 4, now)
			if _, err := ro.Recall(ctx, "agent", "broker", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := ro.RecallExplain(ctx, "agent", "broker", 1); err != nil {
				t.Fatal(err)
			}
			requireAliasActivity(t, ro, a, 4, now)
			if err := ro.Close(); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(dbPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("read-only recall changed store bytes: %v", err)
			}
		})
	}
}

func TestAliasActivityDoesNotReplayStaleAliasState(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		for _, operation := range []string{"retire", "delete", "reclassify", "newer access"} {
			t.Run(fmt.Sprintf("indexed=%t/%s", indexed, operation), func(t *testing.T) {
				s := openAliasActivityStore(t, aliasActivityConfig(t, indexed))
				putActivityFact(t, s, "agent", "acquiring system")
				putActivityFact(t, s, "agent", "nightly backup")
				a := putActivityAlias(t, s, "agent", "broker", "acquiring")
				now := aliasTestTime.Add(time.Hour)
				s.now = func() time.Time { return now }
				ranked := make(chan struct{})
				resume := make(chan struct{})
				done := make(chan error, 1)
				s.debugRanking = func(string, []scored) {
					close(ranked)
					<-resume
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				go func() {
					_, err := s.Recall(ctx, "agent", "broker", 1)
					done <- err
				}()
				released := false
				defer func() {
					if !released {
						close(resume)
						<-done
					}
				}()
				select {
				case <-ranked:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				switch operation {
				case "retire":
					if err := s.Retire("agent", a); err != nil {
						t.Fatal(err)
					}
				case "delete":
					if err := s.Delete("agent", a.ID); err != nil {
						t.Fatal(err)
					}
				case "reclassify":
					a.Kind = KindFact
					if err := s.UpdateFact("agent", a); err != nil {
						t.Fatal(err)
					}
				case "newer access":
					a.AccessedAt = now.Add(time.Hour)
					a.AccessCount = 7
					a.Weight = 0.4
					a.Pinned = true
					if err := s.UpdateFact("agent", a); err != nil {
						t.Fatal(err)
					}
				}
				close(resume)
				released = true
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				current, exists := s.loadFact("agent", a.ID)
				switch operation {
				case "retire":
					if !exists || !current.IsSuperseded() || current.AccessCount != 0 {
						t.Fatalf("stale activation changed retired alias: %+v", current)
					}
				case "delete":
					if exists {
						t.Fatalf("stale activation resurrected alias: %+v", current)
					}
				case "reclassify":
					if !exists || current.IsAlias() || current.AccessCount != 0 {
						t.Fatalf("stale activation touched reclassified fact: %+v", current)
					}
				case "newer access":
					requireAliasActivity(t, s, a, 8, a.AccessedAt)
				}
			})
		}
	}
}

func TestAliasActivityRankedTouchSkipsNewAlias(t *testing.T) {
	s := openAliasActivityStore(t, aliasActivityConfig(t, false))
	f := putActivityFact(t, s, "agent", "ordinary content")
	updated := f
	updated.Kind = KindAlias
	if err := s.UpdateFact("agent", updated); err != nil {
		t.Fatal(err)
	}
	f.AccessedAt = f.AccessedAt.Add(time.Hour)
	s.touchFacts([]Fact{f})
	requireAliasActivity(t, s, updated, 0, updated.AccessedAt)
}

func TestAliasActivityVectorReconcileExcludesAliases(t *testing.T) {
	cfg := StoreConfig{DataDir: t.TempDir(), Embedder: &stubEmbedder{vec: []float32{1, 0, 0}}}
	first := openAliasActivityStore(t, cfg)
	content := putActivityFact(t, first, "agent", "acquiring")
	a := putActivityAlias(t, first, "agent", "broker", "acquiring")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Embedder = &stubEmbedder{vec: []float32{1, 0}}
	second := openAliasActivityStore(t, cfg)
	got, ok := second.loadFact("agent", content.ID)
	if !ok || len(got.Embedding) != 2 {
		t.Fatalf("content did not re-embed on dimension switch: %+v", got)
	}
	got, ok = second.loadFact("agent", a.ID)
	if !ok || len(got.Embedding) != 0 {
		t.Fatalf("dimension switch embedded the alias: %+v", got)
	}
	plantPendingMarker(t, second, "agent", a.ID)
	second.reconcileVectors()
	got, ok = second.loadFact("agent", a.ID)
	if !ok || len(got.Embedding) != 0 || second.PendingVectorCount() != 0 {
		t.Fatalf("reconcile did not discard stale alias vector intent: %+v", got)
	}
}
