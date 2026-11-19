package memory

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

const (
	aliasLifecycleAgent    = "alias-lifecycle"
	aliasLifecycleHalfLife = 30 * 24 * time.Hour
	aliasLifecycleAge      = 201 * 24 * time.Hour
)

func putAliasLifecycleFact(t *testing.T, s *Store, text, kind, source string, weight float64) Fact {
	t.Helper()
	f, err := s.putReturningFactKind(context.Background(), aliasLifecycleAgent, text, kind, source)
	if err != nil {
		t.Fatal(err)
	}
	f.Weight = weight
	if err := s.UpdateFact(f.AgentID, f); err != nil {
		t.Fatal(err)
	}
	return f
}

func readAliasLifecycleFact(t *testing.T, s *Store, f Fact) Fact {
	t.Helper()
	current, ok := s.loadFact(f.AgentID, f.ID)
	if !ok {
		t.Fatalf("missing fact %s (%s)", f.ID, f.Text)
	}
	return current
}

func requireAliasLifecycleWeight(t *testing.T, f Fact, want float64) {
	t.Helper()
	if math.Abs(f.Weight-want) > 1e-12 {
		t.Fatalf("%s: weight = %g, want %g", f.Text, f.Weight, want)
	}
}

func requireAliasLifecycleAccess(t *testing.T, f Fact, count int, at time.Time) {
	t.Helper()
	if f.AccessCount != count || !f.AccessedAt.Equal(at) {
		t.Fatalf("%s: count/time = %d/%s, want %d/%s", f.Text, f.AccessCount, f.AccessedAt, count, at)
	}
}

func consolidateAliasLifecycle(t *testing.T, s *Store) {
	t.Helper()
	cfg := &testConsolidateCfg{threshold: 100, halfLife: aliasLifecycleHalfLife}
	if err := s.Consolidate(context.Background(), aliasLifecycleAgent, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestAliasLifecycleB201ClassStatePinMatrix(t *testing.T) {
	classes := []struct {
		name, kind, source string
	}{
		{"authored", KindAlias, ""},
		{"usage", KindAlias, AliasSourceUsage},
		{"unknown-source", KindAlias, "future-source"},
		{"ordinary", KindFact, ""},
	}
	now := aliasTestTime.Add(aliasLifecycleAge)
	lambda := math.Ln2 / aliasLifecycleHalfLife.Hours()
	decayed := math.Pow(0.5, float64(aliasLifecycleAge)/float64(aliasLifecycleHalfLife))
	if decayed >= 0.01 {
		t.Fatalf("fixture age does not reach the prune threshold: %g", decayed)
	}
	for _, class := range classes {
		for _, state := range []string{"live", "forgotten", "replaced"} {
			for _, pinned := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/pinned=%t", class.name, state, pinned), func(t *testing.T) {
					s := openAliasActivityStore(t, aliasActivityConfig(t, false))
					f := putAliasLifecycleFact(t, s, "alias: broker = acquiring", class.kind, class.source, 0.6)
					if pinned {
						if err := s.SetPinned(f.AgentID, true, f); err != nil {
							t.Fatal(err)
						}
						f = readAliasLifecycleFact(t, s, f)
					}
					var supersededBy string
					switch state {
					case "forgotten":
						if err := s.Retire(f.AgentID, f); err != nil {
							t.Fatal(err)
						}
						supersededBy = SupersededByAgent
					case "replaced":
						var err error
						supersededBy, err = s.Revise(context.Background(), f.AgentID, "alias: broker = underwriting", f)
						if err != nil {
							t.Fatal(err)
						}
						if supersededBy == "" || supersededBy == SupersededByAgent {
							t.Fatalf("invalid replacement ID %q", supersededBy)
						}
					}

					protected := pinned || (class.name == "authored" && state == "live")
					wantWeight := decayed
					if protected {
						wantWeight = f.Weight
					}
					afterDecay, err := s.decayFact(f.AgentID, f.ID, now, lambda)
					if err != nil {
						t.Fatal(err)
					}
					requireAliasLifecycleWeight(t, afterDecay, wantWeight)
					if afterDecay.SupersededBy != supersededBy || afterDecay.Pinned != pinned ||
						afterDecay.Kind != class.kind || afterDecay.AliasSource != class.source {
						t.Fatalf("decay changed fact classification: %+v", afterDecay)
					}
					if err := s.pruneFact(f.AgentID, f.ID, now, lambda); err != nil {
						t.Fatal(err)
					}
					afterPrune, exists := s.loadFact(f.AgentID, f.ID)
					if exists != protected {
						t.Fatalf("exists after prune = %t, want %t (weight %g)", exists, protected, afterDecay.Weight)
					}
					if exists {
						requireAliasLifecycleWeight(t, afterPrune, wantWeight)
						if afterPrune.SupersededBy != supersededBy || afterPrune.Pinned != pinned {
							t.Fatalf("prune changed protected fact state: %+v", afterPrune)
						}
					}
				})
			}
		}
	}
}

func TestAliasLifecycleB202AuthoredSurvivalAt201Days(t *testing.T) {
	s := openAliasActivityStore(t, aliasActivityConfig(t, true))
	live := putAliasLifecycleFact(t, s, "alias: broker = acquiring", KindAlias, "", 0.009)
	retired := putAliasLifecycleFact(t, s, "alias: banker = underwriting", KindAlias, "", 0.009)
	if err := s.Retire(retired.AgentID, retired); err != nil {
		t.Fatal(err)
	}
	retired = readAliasLifecycleFact(t, s, retired)
	if err := s.SetPinned(retired.AgentID, true, retired); err == nil {
		t.Fatal("pinning a retired alias unexpectedly succeeded")
	}
	if got := readAliasLifecycleFact(t, s, retired); got.Pinned || got.SupersededBy != SupersededByAgent {
		t.Fatalf("pinning attempt changed retired alias: %+v", got)
	}

	now := aliasTestTime.Add(aliasLifecycleAge)
	s.now = func() time.Time { return now }
	consolidateAliasLifecycle(t, s)
	requireAliasLifecycleWeight(t, readAliasLifecycleFact(t, s, live), 0.009)
	if got, ok := s.loadFact(retired.AgentID, retired.ID); ok {
		t.Fatalf("retired authored alias survived ordinary pruning: %+v", got)
	}

	content := putActivityFact(t, s, aliasLifecycleAgent, "acquiring handles merchant payments")
	putActivityFact(t, s, aliasLifecycleAgent, "nightly cluster backups")
	result, err := s.RecallExplain(context.Background(), aliasLifecycleAgent, "broker", 1)
	if err != nil || len(result) != 1 || result[0].Provenance.FactID != content.ID || result[0].Ranks.KeywordRank == 0 {
		t.Fatalf("201-day authored alias did not expand recall: %+v, %v", result, err)
	}
	current := readAliasLifecycleFact(t, s, live)
	requireAliasLifecycleAccess(t, current, 1, now)
	requireAliasLifecycleWeight(t, current, 0.009)
}

func TestAliasLifecycleB203UsageRenewal(t *testing.T) {
	t.Run("periodic-versus-inactive", func(t *testing.T) {
		s := openAliasActivityStore(t, aliasActivityConfig(t, false))
		active := putAliasLifecycleFact(t, s, "alias: broker = acquiring", KindAlias, AliasSourceUsage, 1)
		idle := putAliasLifecycleFact(t, s, "alias: biller = collections", KindAlias, AliasSourceUsage, 1)
		content := putActivityFact(t, s, aliasLifecycleAgent, "acquiring handles merchant payments")
		decoy := putActivityFact(t, s, aliasLifecycleAgent, "nightly cluster backups")
		if err := s.SetPinned(decoy.AgentID, true, decoy); err != nil {
			t.Fatal(err)
		}

		for i, days := range []int{30, 60, 90, 120, 150, 180, 201} {
			now := aliasTestTime.Add(time.Duration(days) * 24 * time.Hour)
			s.now = func() time.Time { return now }
			consolidateAliasLifecycle(t, s)
			before := readAliasLifecycleFact(t, s, active)
			if before.Weight <= 0 || before.Weight >= 1 {
				t.Fatalf("day %d: usage alias did not age between activations: %g", days, before.Weight)
			}
			got, err := s.RecallExplain(context.Background(), aliasLifecycleAgent, "broker", 1)
			if err != nil || len(got) != 1 || got[0].Provenance.FactID != content.ID || got[0].Ranks.KeywordRank == 0 {
				t.Fatalf("day %d: usage alias did not expand recall: %+v, %v", days, got, err)
			}
			after := readAliasLifecycleFact(t, s, active)
			requireAliasLifecycleWeight(t, after, 1)
			requireAliasLifecycleAccess(t, after, i+1, now)
		}
		if got, ok := s.loadFact(idle.AgentID, idle.ID); ok {
			t.Fatalf("unused usage alias survived 201 days: %+v", got)
		}
	})

	t.Run("below-prune-threshold", func(t *testing.T) {
		s := openAliasActivityStore(t, aliasActivityConfig(t, true))
		live := putAliasLifecycleFact(t, s, "alias: broker = acquiring", KindAlias, AliasSourceUsage, 0.009)
		retired := putAliasLifecycleFact(t, s, "alias: banker = acquiring", KindAlias, AliasSourceUsage, 0.009)
		pinned := putAliasLifecycleFact(t, s, "alias: patron = acquiring", KindAlias, AliasSourceUsage, 0.009)
		putActivityFact(t, s, aliasLifecycleAgent, "acquiring handles merchant payments")
		if err := s.Retire(retired.AgentID, retired); err != nil {
			t.Fatal(err)
		}
		if err := s.SetPinned(pinned.AgentID, true, pinned); err != nil {
			t.Fatal(err)
		}
		now := aliasTestTime.Add(time.Hour)
		s.now = func() time.Time { return now }
		ctx := context.Background()
		if _, err := s.RecallPreview(ctx, aliasLifecycleAgent, "broker", 1); err != nil {
			t.Fatal(err)
		}
		before := readAliasLifecycleFact(t, s, live)
		requireAliasLifecycleAccess(t, before, 0, aliasTestTime)
		requireAliasLifecycleWeight(t, before, 0.009)
		for _, query := range []string{"broker", "banker", "patron"} {
			if _, err := s.Recall(ctx, aliasLifecycleAgent, query, 1); err != nil {
				t.Fatal(err)
			}
		}

		after := readAliasLifecycleFact(t, s, live)
		requireAliasLifecycleWeight(t, after, 1)
		requireAliasLifecycleAccess(t, after, 1, now)
		tombstone := readAliasLifecycleFact(t, s, retired)
		requireAliasLifecycleWeight(t, tombstone, 0.009)
		requireAliasLifecycleAccess(t, tombstone, 0, aliasTestTime)
		if tombstone.SupersededBy != SupersededByAgent {
			t.Fatalf("recall changed the retired row: %+v", tombstone)
		}
		preserved := readAliasLifecycleFact(t, s, pinned)
		requireAliasLifecycleWeight(t, preserved, 0.009)
		requireAliasLifecycleAccess(t, preserved, 1, now)
		if !preserved.Pinned {
			t.Fatalf("recall cleared the pin: %+v", preserved)
		}
	})
}

func TestAliasLifecycleB204ActivationInterleavings(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		for _, action := range []string{"touch-before-prune", "delete-before-touch", "retire-before-touch", "newer-access"} {
			t.Run(fmt.Sprintf("indexed=%t/%s", indexed, action), func(t *testing.T) {
				s := openAliasActivityStore(t, aliasActivityConfig(t, indexed))
				f := putAliasLifecycleFact(t, s, "alias: broker = acquiring", KindAlias, AliasSourceUsage, 0.02)
				putActivityFact(t, s, aliasLifecycleAgent, "acquiring handles merchant payments")
				now := aliasTestTime.Add(aliasLifecycleAge)
				s.now = func() time.Time { return now }

				decayed, err := s.decayFact(f.AgentID, f.ID, now, math.Ln2/aliasLifecycleHalfLife.Hours())
				if err != nil || decayed.Weight >= 0.01 {
					t.Fatalf("decay did not make the alias prune-eligible: %+v, %v", decayed, err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				ranked := make(chan struct{})
				resume := make(chan struct{})
				signal := sync.OnceFunc(func() { close(ranked) })
				release := sync.OnceFunc(func() { close(resume) })
				s.debugRanking = func(string, []scored) {
					signal()
					<-resume
				}
				done := make(chan error, 1)
				go func() {
					_, err := s.Recall(ctx, f.AgentID, "broker", 1)
					done <- err
				}()
				waited := false
				defer func() {
					release()
					if !waited {
						<-done
					}
				}()
				select {
				case <-ranked:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				switch action {
				case "delete-before-touch":
					err = s.Delete(f.AgentID, f.ID)
				case "retire-before-touch":
					err = s.Retire(f.AgentID, decayed)
				case "newer-access":
					newer := decayed
					newer.Weight = 1
					newer.AccessCount = 3
					newer.AccessedAt = now.Add(time.Hour)
					err = s.UpdateFact(f.AgentID, newer)
				}
				if err != nil {
					t.Fatal(err)
				}
				release()
				err = <-done
				waited = true
				if err != nil {
					t.Fatal(err)
				}

				switch action {
				case "touch-before-prune":
					if err := s.pruneFact(decayed.AgentID, decayed.ID, now, math.Ln2/aliasLifecycleHalfLife.Hours()); err != nil {
						t.Fatal(err)
					}
					got := readAliasLifecycleFact(t, s, f)
					requireAliasLifecycleWeight(t, got, 1)
					requireAliasLifecycleAccess(t, got, 1, now)
				case "delete-before-touch":
					if got, ok := s.loadFact(f.AgentID, f.ID); ok {
						t.Fatalf("stale activation recreated the deleted alias: %+v", got)
					}
				case "retire-before-touch":
					got := readAliasLifecycleFact(t, s, f)
					requireAliasLifecycleWeight(t, got, decayed.Weight)
					requireAliasLifecycleAccess(t, got, 0, aliasTestTime)
					if got.SupersededBy != SupersededByAgent {
						t.Fatalf("stale activation cleared the tombstone: %+v", got)
					}
				case "newer-access":
					got := readAliasLifecycleFact(t, s, f)
					requireAliasLifecycleWeight(t, got, 1)
					requireAliasLifecycleAccess(t, got, 4, now.Add(time.Hour))
				}
			})
		}
	}
}

func TestAliasLifecycleB204PruneRereadsCurrentProtection(t *testing.T) {
	for _, protection := range []string{"authored", "pinned"} {
		t.Run(protection, func(t *testing.T) {
			s := openAliasActivityStore(t, aliasActivityConfig(t, true))
			f := putAliasLifecycleFact(t, s, "alias: broker = acquiring", KindAlias, "future-source", 0.009)
			snapshot, err := s.decayFact(f.AgentID, f.ID, aliasTestTime.Add(aliasLifecycleAge), math.Ln2/aliasLifecycleHalfLife.Hours())
			if err != nil || snapshot.Weight >= 0.01 {
				t.Fatalf("expected a prune-eligible snapshot: %+v, %v", snapshot, err)
			}
			if protection == "authored" {
				f.AliasSource = ""
				err = s.UpdateFact(f.AgentID, f)
			} else {
				err = s.SetPinned(f.AgentID, true, snapshot)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.pruneFact(snapshot.AgentID, snapshot.ID, aliasTestTime.Add(aliasLifecycleAge), math.Ln2/aliasLifecycleHalfLife.Hours()); err != nil {
				t.Fatal(err)
			}
			got := readAliasLifecycleFact(t, s, f)
			if got.AliasSource != f.AliasSource || got.Pinned != (protection == "pinned") {
				t.Fatalf("pruning rewrote the row that gained protection: %+v", got)
			}
		})
	}
}

func TestAliasLifecycleB205UnknownAndOrdinaryAfterReopen(t *testing.T) {
	cfg := aliasActivityConfig(t, true)
	s := openAliasActivityStore(t, cfg)
	unknown := putAliasLifecycleFact(t, s, "alias: banker = acquiring", KindAlias, "future-source", 0.009)
	usage := putAliasLifecycleFact(t, s, "alias: broker = acquiring", KindAlias, AliasSourceUsage, 0.009)
	content := putAliasLifecycleFact(t, s, "acquiring handles merchant payments", KindFact, "", 0.8)
	now := aliasTestTime.Add(aliasLifecycleHalfLife)
	s.now = func() time.Time { return now }
	for _, query := range []string{"banker", "broker"} {
		if _, err := s.Recall(context.Background(), aliasLifecycleAgent, query, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openAliasActivityStore(t, cfg)
	for _, test := range []struct {
		fact   Fact
		weight float64
		count  int
	}{
		{unknown, 0.009, 1},
		{usage, 1, 1},
		{content, 0.8, 2},
	} {
		got := readAliasLifecycleFact(t, reopened, test.fact)
		requireAliasLifecycleAccess(t, got, test.count, now)
		requireAliasLifecycleWeight(t, got, test.weight)
		if got.AliasSource != test.fact.AliasSource {
			t.Fatalf("reopen changed alias source from %q to %q", test.fact.AliasSource, got.AliasSource)
		}
	}
	reopened.now = func() time.Time { return now.Add(aliasLifecycleHalfLife) }
	consolidateAliasLifecycle(t, reopened)
	if got, ok := reopened.loadFact(unknown.AgentID, unknown.ID); ok {
		t.Fatalf("unknown alias source inherited pruning immunity: %+v", got)
	}
	requireAliasLifecycleWeight(t, readAliasLifecycleFact(t, reopened, usage), 0.5)
	requireAliasLifecycleWeight(t, readAliasLifecycleFact(t, reopened, content), 0.5)
}
