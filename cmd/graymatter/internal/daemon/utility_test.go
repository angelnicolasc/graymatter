package daemon

import (
	"context"
	"errors"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type utilityAsyncBackend struct {
	graymatter.AdvancedStore
	memory.UtilityStore
	launches int
	observed []memory.Fact
}

func (b *utilityAsyncBackend) LaunchAsyncConsolidate(agent string, _ memory.ConsolidateConfig) {
	b.launches++
	b.observed, _ = b.AdvancedStore.List(agent)
}

func TestUtilityBackendBatchLaunchesOnceAfterCommit(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.AsyncConsolidate = true
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	store := &utilityAsyncBackend{AdvancedStore: mem.Advanced(), UtilityStore: mem.Advanced().(memory.UtilityStore)}
	b := rememberBackend{mem: mem, AdvancedStore: store}
	ctx := context.Background()
	facts, err := b.PutManyReturningFacts(ctx, "a", []memory.WriteFact{{Text: "one"}, {Text: "two"}, {Text: "three"}})
	if err != nil || len(facts) != 3 || store.launches != 1 || len(store.observed) != 3 {
		t.Fatalf("batch=%+v err=%v launches=%d observed=%+v", facts, err, store.launches, store.observed)
	}
	if _, err := b.PutManyReturningFacts(ctx, "a", []memory.WriteFact{{Text: "four"}, {Text: " "}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.PutManyReturningFacts(canceled, "a", []memory.WriteFact{{Text: "five"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation err=%v", err)
	}
	rows, _ := mem.Advanced().List("a")
	if store.launches != 1 || len(rows) != 3 {
		t.Fatalf("failed batch changed rows/maintenance: %d %+v", store.launches, rows)
	}
}
