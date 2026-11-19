package main

import (
	"context"
	"errors"
	"io"
	netrpc "net/rpc"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

type subjectLegacyStore struct {
	cliStore
	writes int
}

func (*subjectLegacyStore) Ready() error                                     { return nil }
func (s *subjectLegacyStore) Remember(context.Context, string, string) error { s.writes++; return nil }
func (s *subjectLegacyStore) PutReturningFact(context.Context, string, string) (memory.Fact, error) {
	s.writes++
	return memory.Fact{ID: "lost-options"}, nil
}

func TestSubjectOptionalIdentityRejectsUnsupportedIntentBeforeLegacyWrite(t *testing.T) {
	backend := &subjectLegacyStore{}
	r := newReconnectingStoreAt(backend, func() (cliStore, error) { t.Fatal("unsupported intent reconnected"); return nil, nil })
	subject, source, quote := "topic", "document", "literal"
	replaces, err := memory.NewReplacementIDs([]string{"id"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		opts memory.WriteOptions
		want error
	}{
		{memory.WriteOptions{Subject: &subject}, memory.ErrSubjectUnsupported},
		{memory.WriteOptions{Replaces: replaces}, memory.ErrSubjectUnsupported},
		{memory.WriteOptions{Source: &source}, memory.ErrCitationUnsupported},
		{memory.WriteOptions{Quote: &quote}, memory.ErrCitationUnsupported},
	} {
		if fact, err := r.PutWithOptionalIdentity(context.Background(), "a", "assertion", tc.opts); !errors.Is(err, tc.want) || fact.ID != "" {
			t.Fatalf("unsupported result=%+v %v", fact, err)
		}
	}
	if backend.writes != 0 {
		t.Fatalf("legacy writes=%d", backend.writes)
	}
}

type subjectReplyStore struct{ *utilityReplyStore }

func (*subjectReplyStore) SupportsSubjectHeads() bool { return true }
func (s *subjectReplyStore) PutManyReturningReceipts(ctx context.Context, namespace string, inputs []memory.WriteFact) ([]memory.WriteReceipt, error) {
	s.writes++
	result, err := s.store.PutManyReturningReceipts(ctx, namespace, inputs)
	if err != nil {
		return result, err
	}
	if s.failure != nil {
		return nil, s.failure
	}
	return result, nil
}
func (s *subjectReplyStore) ResolveFactTargets(ctx context.Context, namespace, target, targetType string, includeRetired bool) ([]memory.Fact, error) {
	return s.store.ResolveFactTargets(ctx, namespace, target, targetType, includeRetired)
}
func (s *subjectReplyStore) SuggestSimilarFacts(ctx context.Context, namespace, text string) (memory.SuggestionResult, error) {
	return s.store.SuggestSimilarFacts(ctx, namespace, text)
}

func TestSubjectReconnectLostReplyDoesNotReplay(t *testing.T) {
	for _, failure := range []error{io.EOF, netrpc.ErrShutdown} {
		t.Run(failure.Error(), func(t *testing.T) {
			store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), StrictWrite: true})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			subject := "retention"
			if _, err := store.PutManyReturningReceipts(context.Background(), "a", []memory.WriteFact{{Text: "old", Options: memory.WriteOptions{Subject: &subject}}}); err != nil {
				t.Fatal(err)
			}
			old := &subjectReplyStore{&utilityReplyStore{store: store, failure: failure}}
			fresh := &subjectReplyStore{&utilityReplyStore{store: store}}
			reopens := 0
			r := newReconnectingStoreAt(old, func() (cliStore, error) { reopens++; return fresh, nil })
			receipts, err := r.PutManyReturningReceipts(context.Background(), "a", []memory.WriteFact{{Text: "new", Options: memory.WriteOptions{Subject: &subject}}})
			if !errors.Is(err, memory.ErrMutationOutcomeUnknown) || len(receipts) != 0 || old.writes != 1 || fresh.writes != 0 || reopens != 0 {
				t.Fatalf("receipts=%+v err=%v writes=%d/%d reopens=%d", receipts, err, old.writes, fresh.writes, reopens)
			}
			rows, err := store.List("a")
			if err != nil || len(rows) != 2 {
				t.Fatalf("rows=%+v %v", rows, err)
			}
			live := 0
			for _, row := range rows {
				if !row.IsSuperseded() {
					live++
					if row.Text != "new" {
						t.Fatalf("wrong head: %+v", row)
					}
				}
			}
			if live != 1 {
				t.Fatalf("live=%d", live)
			}
		})
	}
}

type subjectAsyncStore struct {
	graymatter.AdvancedStore
	rpc.SubjectBackend
	launches int
	observed []memory.Fact
}

func (s *subjectAsyncStore) LaunchAsyncConsolidate(namespace string, _ memory.ConsolidateConfig) {
	s.launches++
	s.observed, _ = s.AdvancedStore.List(namespace)
}

func TestSubjectDirectMaintenanceRunsAfterAtomicReplacement(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.AsyncConsolidate = true
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	store := &subjectAsyncStore{AdvancedStore: mem.Advanced(), SubjectBackend: mem.Advanced().(rpc.SubjectBackend)}
	d := &directStore{mem: mem, store: store}
	subject := "policy"
	for _, value := range []string{"old", "new"} {
		if _, err := d.PutManyReturningReceipts(context.Background(), "a", []memory.WriteFact{{Text: value, Options: memory.WriteOptions{Subject: &subject}}}); err != nil {
			t.Fatal(err)
		}
	}
	if store.launches != 2 || len(store.observed) != 2 {
		t.Fatalf("launches=%d rows=%+v", store.launches, store.observed)
	}
	live := 0
	for _, row := range store.observed {
		if !row.IsSuperseded() {
			live++
			if row.Text != "new" {
				t.Fatalf("maintenance saw old head: %+v", row)
			}
		}
	}
	if live != 1 {
		t.Fatalf("live=%d", live)
	}
	replaces, err := memory.NewReplacementIDs([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.PutManyReturningReceipts(context.Background(), "a", []memory.WriteFact{{Text: "bad", Options: memory.WriteOptions{Replaces: replaces}}}); err == nil {
		t.Fatal("empty replaces accepted")
	}
	if store.launches != 2 {
		t.Fatal("maintenance ran after rejected write")
	}
}
