package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

func TestDirectBackendCheckpointPeekPending(t *testing.T) {
	_, mem := newTestServer(t)
	b := NewDirectBackend(mem, nil)
	cp, err := b.CheckpointSave(session.Checkpoint{AgentID: "a", State: map[string]any{"step": "private unfinished state"}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(mem.Advanced().DB().Path())
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.CheckpointPeekPending("a")
	if err != nil || got == nil || got.ID != cp.ID {
		t.Fatalf("pointer: %+v, %v", got, err)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "private unfinished state") || strings.Contains(string(wire), "state") {
		t.Fatalf("pointer copied task state: %s", wire)
	}
	after, err := os.ReadFile(mem.Advanced().DB().Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("pointer backend modified database")
	}
	if got, err := NewDirectBackend(nil, nil).CheckpointPeekPending("a"); got != nil || err == nil {
		t.Fatalf("uninitialised: %+v, %v", got, err)
	}
}
