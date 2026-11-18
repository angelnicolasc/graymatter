package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	bolt "go.etcd.io/bbolt"
)

func TestCheckpointResumeMarkersMCPWire(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.ConsolidateLLM = ""
	cfg.AsyncConsolidate = false
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	backend := NewDirectBackend(mem, nil)
	s := New(backend, "test")

	saved, err := backend.CheckpointSave(session.Checkpoint{
		AgentID: "agent", CreatedAt: time.Unix(10, 0).UTC(), State: map[string]any{"step": "continue"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"agent", "only-markers"} {
		_, err := backend.CheckpointSave(session.Checkpoint{
			AgentID: agent, CreatedAt: time.Unix(11, 0).UTC(),
			State:    map[string]any{"source": "hooks", "event": "session-end", "session_id": "editor"},
			Metadata: map[string]string{"source": "hooks", "event": "session-end", "session_id": "editor"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	resume := callToolJSONRPC(t, s, 1, "checkpoint_resume", map[string]any{"agent_id": "agent"}).Result
	var checkpoint checkpointResumeResult
	if resume.IsError || json.Unmarshal(resume.StructuredContent, &checkpoint) != nil ||
		checkpoint.ID != saved.ID || checkpoint.State["step"] != "continue" || !checkpointWireTextContains(resume.Content, saved.ID) {
		t.Fatalf("resume did not restore the useful checkpoint in both outputs: %+v", resume)
	}

	for _, agent := range []string{"only-markers", "missing"} {
		ordinary := callToolJSONRPC(t, s, 2, "checkpoint_resume", map[string]any{"agent_id": agent}).Result
		if !ordinary.IsError || len(ordinary.StructuredContent) != 0 || !checkpointWireTextContains(ordinary.Content, "no checkpoint found") {
			t.Fatalf("%s: expected the historical prose-only absence error, got %+v", agent, ordinary)
		}
		empty := callToolJSONRPC(t, s, 3, "checkpoint_resume", map[string]any{"agent_id": agent, "on_missing": "empty"}).Result
		var payload map[string]any
		if empty.IsError || json.Unmarshal(empty.StructuredContent, &payload) != nil ||
			payload["found"] != false || payload["agent_id"] != agent || !checkpointWireTextContains(empty.Content, "No checkpoint saved") {
			t.Fatalf("%s: expected structured and textual absence, got %+v", agent, empty)
		}
	}

	if err := mem.Advanced().DB().Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("sessions")).Bucket([]byte("only-markers")).Put([]byte("broken"), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	corrupt := callToolJSONRPC(t, s, 4, "checkpoint_resume", map[string]any{"agent_id": "only-markers", "on_missing": "empty"}).Result
	if !corrupt.IsError || len(corrupt.StructuredContent) != 0 || !checkpointWireTextContains(corrupt.Content, "decode checkpoint") {
		t.Fatalf("expected corruption to remain a prose-only error, got %+v", corrupt)
	}
}

func checkpointWireTextContains(content []json.RawMessage, value string) bool {
	return len(content) == 1 && strings.Contains(string(content[0]), value)
}
