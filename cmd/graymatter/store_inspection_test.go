package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func TestReadOnlyTUIBlocksAllMutationEntrypoints(t *testing.T) {
	s := &readOnlyTUIStore{} // nil backend: forwarding any write would panic
	ctx := context.Background()
	_, putErr := s.PutReturningFact(ctx, "a", "x")
	_, checkpointErr := s.CheckpointSave(session.Checkpoint{})
	_, curateErr := s.CurateFact(ctx, "a", "id", "pin", "", memory.Fact{})
	for _, err := range []error{
		s.Remember(ctx, "a", "x"), s.PutShared(ctx, "x"), putErr,
		s.PutAlias(ctx, "a", "x", []string{"y"}), s.Delete("a", "id"), s.UpdateFact("a", memory.Fact{}),
		s.Consolidate(ctx, "a"), checkpointErr, s.SessionKill("id"), s.SessionSave(harness.HarnessSession{}),
		s.KGLink("a", "b", "relation"), s.ExportGraphObsidian("path"), s.AuditWrite(audit.Entry{}),
		s.TokenRecord("a", "model", 1, 2, 3, 4), curateErr,
	} {
		if !errors.Is(err, memory.ErrStoreReadOnly) {
			t.Fatalf("write not blocked: %v", err)
		}
	}
	if _, ok := any(s).(interface {
		PutWithOptionsReturningFact(context.Context, string, string, memory.WriteOptions) (memory.Fact, error)
	}); ok {
		t.Fatal("optional write capability leaked")
	}
	if !s.IsReadOnly() {
		t.Fatal("session not marked read-only")
	}
}

type inspectionFake struct {
	cliStore
	writeCalls, previewCalls int
	dead                     bool
}

type lostInteractiveWrite struct {
	cliStore
	addCalls, stopCalls int
	err                 error
}

func (s *lostInteractiveWrite) Remember(context.Context, string, string) error {
	s.addCalls++
	return s.err
}
func (s *lostInteractiveWrite) SessionKill(string) error { s.stopCalls++; return s.err }
func (s *lostInteractiveWrite) Ready() error             { return nil }

func TestReconnectingInteractiveWritesDoNotReplay(t *testing.T) {
	for _, failure := range []error{io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, rpc.ErrCallTimeout} {
		t.Run(failure.Error(), func(t *testing.T) {
			backend := &lostInteractiveWrite{err: failure}
			reopens := 0
			s := newReconnectingStoreAt(backend, func() (cliStore, error) { reopens++; return backend, nil })
			for _, err := range []error{s.Remember(context.Background(), "a", "fact"), s.SessionKill("session")} {
				if !errors.Is(err, memory.ErrMutationOutcomeUnknown) {
					t.Fatalf("missing uncertainty: %v", err)
				}
			}
			if backend.addCalls != 1 || backend.stopCalls != 1 || reopens != 0 {
				t.Fatalf("replayed: adds=%d stops=%d reconnects=%d", backend.addCalls, backend.stopCalls, reopens)
			}
		})
	}
}

func (f *inspectionFake) CurateFact(context.Context, string, string, string, string, memory.Fact) (memory.Fact, error) {
	f.writeCalls++
	f.dead = true
	return memory.Fact{ID: "committed"}, io.EOF
}
func (f *inspectionFake) InspectRecall(context.Context, string, string, int) ([]memory.RecallReceipt, error) {
	f.previewCalls++
	return []memory.RecallReceipt{{Text: "preview"}}, nil
}
func (f *inspectionFake) ListFacts(context.Context, string, string, string, string, int) (memory.FactPage, error) {
	if f.dead {
		return memory.FactPage{}, io.EOF
	}
	return memory.FactPage{Total: 1}, nil
}
func (*inspectionFake) Close() error { return nil }
func (f *inspectionFake) Ready() error {
	if f.dead {
		return io.EOF
	}
	return nil
}

func TestInteractiveWriteReconnectsBeforeDispatch(t *testing.T) {
	dead, fresh := &fakeStore{}, &fakeStore{}
	dead.dead.Store(true)
	reopens := 0
	s := newReconnectingStoreAt(dead, func() (cliStore, error) { reopens++; return fresh, nil })
	if err := s.Remember(context.Background(), "a", "fact"); err != nil {
		t.Fatal(err)
	}
	if reopens != 1 || dead.calls.Load() != 1 || fresh.calls.Load() != 2 {
		t.Fatalf("expected failed probe, fresh probe and one write: reopens=%d old=%d new=%d", reopens, dead.calls.Load(), fresh.calls.Load())
	}
}

func TestReadOnlyTUIRecallUsesOnlyPreview(t *testing.T) {
	f := &inspectionFake{}
	s := &readOnlyTUIStore{cliStore: f}
	ctx := context.Background()
	if got, err := s.Recall(ctx, "a", "x", 8); err != nil || !reflect.DeepEqual(got, []string{"preview"}) {
		t.Fatalf("recall: %v %v", got, err)
	}
	if _, err := s.RecallExplain(ctx, "a", "x", 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecallDetailed(ctx, "a", "x", 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecallShared(ctx, "x", 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecallDefault(ctx, "a", "x"); err != nil {
		t.Fatal(err)
	}
	if f.previewCalls != 5 {
		t.Fatalf("preview calls=%d", f.previewCalls)
	}
	if _, err := s.RecallAll(ctx, "a", "x", 8); !errors.Is(err, memory.ErrInspectionUnsupported) {
		t.Fatal(err)
	}
}

type acknowledgedCurationRollback struct{ *inspectionFake }

func (*acknowledgedCurationRollback) CurateFact(context.Context, string, string, string, string, memory.Fact) (memory.Fact, error) {
	return memory.Fact{}, context.DeadlineExceeded
}

func TestReconnectingCurationPreservesAcknowledgedRollback(t *testing.T) {
	backend := &acknowledgedCurationRollback{inspectionFake: &inspectionFake{}}
	store := newReconnectingStoreAt(backend, func() (cliStore, error) {
		t.Fatal("acknowledged rollback triggered a retry")
		return nil, nil
	})
	fact, err := store.CurateFact(context.Background(), "a", "id", "revise", "text", memory.Fact{})
	if fact.ID != "" || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, memory.ErrMutationOutcomeUnknown) {
		t.Fatalf("acknowledged rollback was misreported: %+v %v", fact, err)
	}
}

func TestReconnectingCurationDoesNotReplayDroppedWrite(t *testing.T) {
	dead, fresh := &inspectionFake{}, &inspectionFake{}
	reopens := 0
	s := newReconnectingStoreAt(dead, func() (cliStore, error) { reopens++; return fresh, nil })
	f, err := s.CurateFact(context.Background(), "a", "id", "revise", "text", memory.Fact{})
	if !errors.Is(err, memory.ErrMutationOutcomeUnknown) || f.ID != "committed" {
		t.Fatalf("write ambiguity: %+v %v", f, err)
	}
	if reopens != 0 || dead.writeCalls != 1 || fresh.writeCalls != 0 {
		t.Fatal("ambiguous mutation replayed")
	}
	if page, err := s.ListFacts(context.Background(), "a", "all", "", "", 10); err != nil || page.Total != 1 || reopens != 1 {
		t.Fatalf("next read failed to reconnect: %+v %v reopens=%d", page, err, reopens)
	}
}

func TestOpenReadOnlyTUIMissingStoreCreatesNothing(t *testing.T) {
	previousDir, previousNoDaemon := dataDir, noDaemon
	t.Cleanup(func() { dataDir, noDaemon = previousDir, previousNoDaemon })
	t.Setenv("GRAYMATTER_NO_DAEMON", "")
	for _, direct := range []bool{false, true} {
		dataDir = filepath.Join(t.TempDir(), "missing", "store")
		noDaemon = direct
		if s, err := openTUIStore(true); err == nil {
			s.Close()
			t.Fatal("missing read-only store opened")
		}
		if _, err := os.Stat(filepath.Dir(dataDir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created parent directory: %v", err)
		}
	}
}

func TestReadOnlyTUIUsesDaemonWithoutChangingOtherClients(t *testing.T) {
	dir := t.TempDir()
	s, err := memory.Open(memory.StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	f, err := s.PutReturningFact(ctx, "a", "database fact")
	if err != nil {
		t.Fatal(err)
	}
	server := rpc.NewServer(s, nil)
	listener, cleanup, err := rpc.Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer server.Stop()
	go func() { _ = server.Serve(listener) }()
	previousDir, previousNoDaemon := dataDir, noDaemon
	dataDir, noDaemon = dir, false
	t.Cleanup(func() { dataDir, noDaemon = previousDir, previousNoDaemon })
	t.Setenv("GRAYMATTER_NO_DAEMON", "")
	readonly, err := openTUIStore(true)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	wrapped, ok := readonly.(*readOnlyTUIStore)
	if !ok {
		t.Fatal("missing session guard")
	}
	if _, ok := wrapped.cliStore.(*reconnectingStore); !ok {
		t.Fatalf("daemon was bypassed: %T", wrapped.cliStore)
	}
	before, _ := s.List("a")
	if _, err := readonly.(tuiStoreCapabilities).InspectRecall(ctx, "a", "database", 8); err != nil {
		t.Fatal(err)
	}
	after, _ := s.List("a")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("daemon preview mutated facts")
	}
	if _, err := readonly.(tuiStoreCapabilities).CurateFact(ctx, "a", f.ID, "pin", "", f); !errors.Is(err, memory.ErrStoreReadOnly) {
		t.Fatal(err)
	}
	if _, err := s.CurateFact(ctx, "a", f.ID, "pin", "", f); err != nil {
		t.Fatalf("another writer was made readonly: %v", err)
	}
}

func TestDirectInspectionAndGraphReadsAreNonMutating(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	d := &directStore{mem: mem, store: mem.Advanced()}
	if _, err := d.PutReturningFact(context.Background(), "a", "database policy"); err != nil {
		t.Fatal(err)
	}
	before, _ := d.List("a")
	if _, err := d.InspectRecall(context.Background(), "a", "database", 8); err != nil {
		t.Fatal(err)
	}
	if _, err := d.KGNodes(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.KGEdges(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.KGState(); err != nil {
		t.Fatal(err)
	}
	after, _ := d.List("a")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("direct inspection mutated facts")
	}
}
