package main

import (
	"errors"
	netrpc "net/rpc"
	"strings"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	bolt "go.etcd.io/bbolt"
)

type disconnectedCheckpointStore struct{ cliStore }

func (disconnectedCheckpointStore) CheckpointResume(string) (*session.Checkpoint, error) {
	return nil, netrpc.ErrShutdown
}

func (disconnectedCheckpointStore) Close() error { return nil }

func TestCheckpointResumeDirectAndReconnect(t *testing.T) {
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

	save := func(cp session.Checkpoint) session.Checkpoint {
		t.Helper()
		saved, err := direct.CheckpointSave(cp)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	useful := save(session.Checkpoint{
		AgentID: "agent", CreatedAt: time.Unix(10, 0).UTC(), State: map[string]any{"step": "finish"},
	})
	for _, event := range []string{"pre-compact", "session-end"} {
		marker := sessionCheckpointFor("agent", hookEventPayload{SessionID: "editor"}, event)
		marker.CreatedAt = time.Unix(11, 0).UTC()
		saved := save(marker)
		if loaded, err := direct.CheckpointLoad("agent", saved.ID); err != nil || loaded == nil || loaded.Metadata["event"] != event {
			t.Fatalf("Load marker %q = %+v, %v", event, loaded, err)
		}
	}
	save(sessionCheckpointFor("only-markers", hookEventPayload{SessionID: "editor"}, "session-end"))

	reconnects := 0
	wrapper := newReconnectingStoreAt(disconnectedCheckpointStore{}, func() (cliStore, error) {
		reconnects++
		return direct, nil
	})
	for _, store := range []cliStore{direct, wrapper} {
		got, err := store.CheckpointResume("agent")
		if err != nil || got == nil || got.ID != useful.ID {
			t.Fatalf("%T resume = %+v, %v; want %s", store, got, err, useful.ID)
		}
		if got, err := store.CheckpointResume("only-markers"); got != nil || !errors.Is(err, session.ErrNoCheckpoint) {
			t.Fatalf("%T marker-only resume = %+v, %v; want typed absence", store, got, err)
		}
	}
	if err := direct.store.DB().Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("sessions")).Bucket([]byte("agent")).Put([]byte("broken"), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	for _, store := range []cliStore{direct, wrapper} {
		got, err := store.CheckpointResume("agent")
		if got != nil || err == nil || errors.Is(err, session.ErrNoCheckpoint) || !strings.Contains(err.Error(), "decode checkpoint") {
			t.Fatalf("%T corrupt resume = %+v, %v; want decode error", store, got, err)
		}
	}
	if reconnects != 1 {
		t.Fatalf("reconnects = %d; only the disconnected read should reconnect", reconnects)
	}
}
