package main

import (
	"context"
	"errors"
	"io"
	netrpc "net/rpc"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type utilityReplyStore struct {
	cliStore
	store    *memory.Store
	failure  error
	probeErr error
	writes   int
}

func (s *utilityReplyStore) Ready() error { return s.probeErr }
func (s *utilityReplyStore) Close() error { return nil }
func (s *utilityReplyStore) SearchScoped(ctx context.Context, agent string, q []string, k int, all bool, opts memory.RecallOptions) (memory.ScopedSearchResult, error) {
	return s.store.SearchScoped(ctx, agent, q, k, all, opts)
}
func (s *utilityReplyStore) PutManyReturningFacts(ctx context.Context, agent string, facts []memory.WriteFact) ([]memory.Fact, error) {
	s.writes++
	written, err := s.store.PutManyReturningFacts(ctx, agent, facts)
	if err != nil {
		return nil, err
	}
	if s.failure != nil {
		return nil, s.failure
	}
	return written, nil
}

func TestUtilityReconnectLostReplyDoesNotReplay(t *testing.T) {
	for _, failure := range []error{io.EOF, netrpc.ErrShutdown} {
		t.Run(failure.Error(), func(t *testing.T) {
			cfg := memory.StoreConfig{DataDir: t.TempDir(), StrictWrite: true, Embedder: embedding.NewKeyword()}
			store, err := memory.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			old := &utilityReplyStore{store: store, failure: failure}
			fresh := &utilityReplyStore{store: store}
			reopens := 0
			r := newReconnectingStoreAt(old, func() (cliStore, error) { reopens++; return fresh, nil })
			facts, err := r.PutManyReturningFacts(context.Background(), "a", []memory.WriteFact{{Text: "first durable row"}, {Text: "second durable row"}})
			if !errors.Is(err, memory.ErrMutationOutcomeUnknown) || len(facts) != 0 || old.writes != 1 || fresh.writes != 0 || reopens != 0 {
				t.Fatalf("facts=%+v err=%v writes=%d/%d reopens=%d", facts, err, old.writes, fresh.writes, reopens)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := memory.Open(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			rows, err := reopened.List("a")
			if err != nil || len(rows) != 2 {
				t.Fatalf("reopened batch=%+v %v", rows, err)
			}
		})
	}
}

func TestUtilityReconnectPreflightAndValidationBeforeWrite(t *testing.T) {
	blocked := errors.New("health rejected")
	old := &utilityReplyStore{probeErr: blocked}
	r := newReconnectingStoreAt(old, func() (cliStore, error) { t.Fatal("semantic preflight reconnected"); return nil, nil })
	if facts, err := r.PutManyReturningFacts(context.Background(), "a", []memory.WriteFact{{Text: "valid"}}); !errors.Is(err, blocked) || len(facts) != 0 || old.writes != 0 {
		t.Fatalf("preflight facts=%+v err=%v writes=%d", facts, err, old.writes)
	}
	if _, err := r.PutManyReturningFacts(context.Background(), "a", []memory.WriteFact{{Text: " "}}); err == nil || errors.Is(err, blocked) || old.writes != 0 {
		t.Fatalf("validation err=%v writes=%d", err, old.writes)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.PutManyReturningFacts(ctx, "a", []memory.WriteFact{{Text: "valid"}}); !errors.Is(err, context.Canceled) || old.writes != 0 {
		t.Fatalf("canceled err=%v writes=%d", err, old.writes)
	}
}

type utilityAsyncStore struct {
	graymatter.AdvancedStore
	memory.UtilityStore
	launches int
	observed []memory.Fact
}

func (s *utilityAsyncStore) LaunchAsyncConsolidate(agent string, _ memory.ConsolidateConfig) {
	s.launches++
	s.observed, _ = s.AdvancedStore.List(agent)
}

func TestUtilityDirectBatchLaunchesOnceAfterCommit(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.AsyncConsolidate = true
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	store := &utilityAsyncStore{AdvancedStore: mem.Advanced(), UtilityStore: mem.Advanced().(memory.UtilityStore)}
	d := &directStore{mem: mem, store: store}
	ctx := context.Background()
	facts, err := d.PutManyReturningFacts(ctx, "a", []memory.WriteFact{{Text: "one"}, {Text: "two"}, {Text: "three"}})
	if err != nil || len(facts) != 3 || store.launches != 1 || len(store.observed) != 3 {
		t.Fatalf("batch=%+v err=%v launches=%d observed=%+v", facts, err, store.launches, store.observed)
	}
	if _, err := d.PutManyReturningFacts(ctx, "a", []memory.WriteFact{{Text: "four"}, {Text: " "}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := d.PutManyReturningFacts(canceled, "a", []memory.WriteFact{{Text: "five"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation err=%v", err)
	}
	rows, _ := mem.Advanced().List("a")
	if store.launches != 1 || len(rows) != 3 {
		t.Fatalf("failed batch changed rows/maintenance: %d %+v", store.launches, rows)
	}
}
