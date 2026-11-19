package main

import (
	"bytes"
	netrpc "net/rpc"
	"os"
	"strings"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

type disconnectedPendingStore struct{ cliStore }

func (disconnectedPendingStore) CheckpointPeekPending(string) (*session.PendingCheckpoint, error) {
	return nil, netrpc.ErrShutdown
}
func (disconnectedPendingStore) Close() error { return nil }

type oldPendingStore struct {
	cliStore
	resumes int
}

func (s *oldPendingStore) CheckpointResume(string) (*session.Checkpoint, error) {
	s.resumes++
	return nil, nil
}
func (s *oldPendingStore) Close() error { return nil }

func TestCheckpointPeekPendingDirectAndReconnect(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.ConsolidateLLM = ""
	cfg.AsyncConsolidate = false
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	direct := &directStore{mem: mem, store: mem.Advanced()}
	t.Cleanup(func() { _ = direct.Close() })
	saved, err := direct.CheckpointSave(session.Checkpoint{AgentID: "a", CreatedAt: time.Unix(10, 0).UTC(), State: map[string]any{"step": "finish"}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(direct.store.DB().Path())
	if err != nil {
		t.Fatal(err)
	}
	redials := 0
	wrapper := newReconnectingStoreAt(disconnectedPendingStore{}, func() (cliStore, error) { redials++; return direct, nil })
	for _, reader := range []pendingCheckpointStore{direct, wrapper} {
		got, err := reader.CheckpointPeekPending("a")
		if err != nil || got == nil || got.ID != saved.ID {
			t.Fatalf("%T: %+v, %v", reader, got, err)
		}
	}
	if redials != 1 {
		t.Fatalf("redials = %d", redials)
	}
	after, err := os.ReadFile(direct.store.DB().Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("direct/reconnect pointer changed the store")
	}
}

func TestCheckpointPeekPendingMissingCapabilityDoesNotResume(t *testing.T) {
	old := &oldPendingStore{}
	wrapper := newReconnectingStoreAt(old, func() (cliStore, error) { t.Fatal("capability error must not reconnect"); return nil, nil })
	got, err := wrapper.CheckpointPeekPending("a")
	if got != nil || err == nil || !strings.Contains(err.Error(), "upgrade and restart") {
		t.Fatalf("missing capability: %+v, %v", got, err)
	}
	if old.resumes != 0 {
		t.Fatal("pending pointer used Resume")
	}
}
