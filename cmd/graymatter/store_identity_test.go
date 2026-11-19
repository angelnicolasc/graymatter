package main

import (
	"context"
	"errors"
	"io"
	netrpc "net/rpc"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type identityReplyStore struct {
	cliStore
	store    *memory.Store
	failure  error
	probeErr error
	writes   int
}

func (s *identityReplyStore) Ready() error { return s.probeErr }
func (*identityReplyStore) Close() error   { return nil }

func (s *identityReplyStore) PutReturningFact(ctx context.Context, agent, text string) (memory.Fact, error) {
	return s.PutWithOptionsReturningFact(ctx, agent, text, memory.WriteOptions{})
}

func (s *identityReplyStore) PutWithOptionsReturningFact(ctx context.Context, agent, text string, opts memory.WriteOptions) (memory.Fact, error) {
	s.writes++
	fact, err := s.store.PutWithOptionsReturningFact(ctx, agent, text, opts)
	if err != nil {
		return fact, err
	}
	if s.failure != nil {
		return memory.Fact{}, s.failure
	}
	return fact, nil
}

func (s *identityReplyStore) ReviseWithOptions(context.Context, string, string, string, memory.WriteOptions) (memory.Fact, error) {
	panic("identity add called the revision path")
}

func (s *identityReplyStore) ReviseFactsWithOptions(context.Context, string, string, memory.WriteOptions, ...memory.Fact) (memory.Fact, error) {
	panic("identity add called the revision path")
}

func TestR4IdentityReconnectLostReplyIsNeverReplayed(t *testing.T) {
	for _, failure := range []error{io.EOF, netrpc.ErrShutdown} {
		for _, mode := range []string{"strict-returning", "optional-identity", "explicit-confidence"} {
			t.Run(mode+"/"+failure.Error(), func(t *testing.T) {
				dir := t.TempDir()
				config := memory.StoreConfig{DataDir: dir, StrictWrite: true, Embedder: embedding.NewKeyword()}
				store, err := memory.Open(config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				old := &identityReplyStore{store: store, failure: failure}
				fresh := &identityReplyStore{store: store}
				reopens := 0
				r := newReconnectingStoreAt(old, func() (cliStore, error) {
					reopens++
					return fresh, nil
				})
				const namespace = "identity-namespace"
				const text = "write before the reply was lost"
				var receipt memory.Fact
				ctx := context.Background()
				switch mode {
				case "strict-returning":
					receipt, err = r.PutReturningFact(ctx, namespace, text)
				case "optional-identity":
					receipt, err = r.PutWithOptionalIdentity(ctx, namespace, text, memory.WriteOptions{})
				case "explicit-confidence":
					label := "verified"
					receipt, err = r.PutWithOptionalIdentity(ctx, namespace, text, memory.WriteOptions{Confidence: &label})
				}
				if !errors.Is(err, memory.ErrMutationOutcomeUnknown) || receipt.ID != "" || old.writes != 1 || fresh.writes != 0 || reopens != 0 {
					t.Fatalf("uncertain write replayed or returned a false receipt: %+v / %v, old=%d new=%d reopens=%d",
						receipt, err, old.writes, fresh.writes, reopens)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := memory.Open(config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reopened.Close() })
				facts, err := reopened.List(namespace)
				if err != nil || len(facts) != 1 || facts[0].Text != text {
					t.Fatalf("reopen: %+v / %v", facts, err)
				}
			})
		}
	}
}

func TestR4IdentityReconnectPreflightPrecedesDispatch(t *testing.T) {
	ctx := context.Background()
	blocked := errors.New("health probe rejected this connection")
	old := &identityReplyStore{probeErr: blocked}
	r := newReconnectingStoreAt(old, func() (cliStore, error) {
		t.Fatal("semantic health rejection attempted reconnect")
		return nil, nil
	})
	if f, err := r.PutWithOptionalIdentity(ctx, "a", "untouched", memory.WriteOptions{}); !errors.Is(err, blocked) || f.ID != "" || old.writes != 0 {
		t.Fatalf("failed preflight dispatched a write: %+v / %v", f, err)
	}
	invalid := ""
	if f, err := r.PutWithOptionalIdentity(ctx, "a", "untouched", memory.WriteOptions{Confidence: &invalid}); err == nil || errors.Is(err, blocked) || f.ID != "" || old.writes != 0 {
		t.Fatalf("invalid options were not rejected first: %+v / %v", f, err)
	}

	dir := t.TempDir()
	store, err := memory.Open(memory.StoreConfig{DataDir: dir, StrictWrite: true, Embedder: embedding.NewKeyword()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dead := &identityReplyStore{probeErr: netrpc.ErrShutdown}
	fresh := &identityReplyStore{store: store}
	r = newReconnectingStoreAt(dead, func() (cliStore, error) { return fresh, nil })
	fact, err := r.PutWithOptionalIdentity(ctx, "a", "confirmed after safe preflight", memory.WriteOptions{})
	facts, readErr := store.List("a")
	if err != nil || readErr != nil || dead.writes != 0 || fresh.writes != 1 || len(facts) != 1 || fact.ID == "" || fact.ID != facts[0].ID {
		t.Fatalf("read-only preflight did not recover before one confirmed dispatch: %+v / %v, rows=%+v / %v", fact, err, facts, readErr)
	}
}
