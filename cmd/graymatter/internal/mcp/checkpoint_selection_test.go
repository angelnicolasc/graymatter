package mcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	bolt "go.etcd.io/bbolt"
)

func TestCheckpointResumeMarkersMCPWire(t *testing.T) {
	core, mem := utilityContractServer(t)
	backend := NewDirectBackend(mem, nil)
	full := New(backend, "test", WithFullCatalog())
	save := func(agent, id string, created int64, state map[string]any, metadata map[string]string) session.Checkpoint {
		t.Helper()
		checkpoint, err := backend.CheckpointSave(session.Checkpoint{
			ID: id, AgentID: agent, CreatedAt: time.Unix(created, 0).UTC(), State: state, Metadata: metadata,
		})
		if err != nil {
			t.Fatal(err)
		}
		return checkpoint
	}
	marker := func(agent, event string) session.Checkpoint {
		t.Helper()
		return save(agent, "marker", 11,
			map[string]any{"source": "hooks", "event": event, "session_id": "editor"},
			map[string]string{"source": "hooks", "event": event, "session_id": "editor"})
	}

	pending := save("pending", "pending", 10, map[string]any{"step": "continue"}, nil)
	latest := marker("pending", "session-end")
	preCompact := marker("only-pre-compact", "pre-compact")
	sessionEnd := marker("only-session-end", "session-end")
	completed := save("completed", "completed", 12, map[string]any{"status": "complete"}, nil)
	marker("completed", "pre-compact")
	markerWithWork := save("extra-state", "extra-state", 12,
		map[string]any{"source": "hooks", "event": "pre-compact", "session_id": "editor", "step": "continue"},
		map[string]string{"source": "hooks", "event": "pre-compact", "session_id": "editor"})
	// Ties use the same List order as the published binary.
	save("tie", "z-last-key", 12, map[string]any{"step": "second"}, nil)
	save("tie", "a-first-key", 12, map[string]any{"step": "first"}, nil)
	ties, err := backend.CheckpointList("tie")
	if err != nil || len(ties) != 2 {
		t.Fatalf("tie fixture: %+v / %v", ties, err)
	}

	for _, test := range []struct {
		agent string
		full  session.Checkpoint
		core  session.Checkpoint
	}{
		{"pending", latest, pending},
		{"only-pre-compact", preCompact, session.Checkpoint{}},
		{"only-session-end", sessionEnd, session.Checkpoint{}},
		{"completed", completed, session.Checkpoint{}},
		{"extra-state", markerWithWork, markerWithWork},
		{"tie", ties[0], ties[0]},
	} {
		t.Run(test.agent, func(t *testing.T) {
			for _, onMissing := range []string{"", "error", "empty"} {
				args := map[string]any{"agent_id": test.agent}
				if onMissing != "" {
					args["on_missing"] = onMissing
				}
				legacy := callToolJSONRPC(t, full, 1, "checkpoint_resume", args).Result
				var restored checkpointResumeResult
				if legacy.IsError || json.Unmarshal(legacy.StructuredContent, &restored) != nil ||
					restored.ID != test.full.ID || restored.CreatedAt != test.full.CreatedAt.Format(time.RFC3339) ||
					!reflect.DeepEqual(restored.State, test.full.State) || !checkpointWireTextContains(legacy.Content, test.full.ID) {
					t.Fatalf("full restored %+v, want %+v: %+v", restored, test.full, legacy)
				}
				resume := callToolJSONRPC(t, core, 2, "checkpoint_resume", args).Result
				if test.core.ID == "" {
					if resume.IsError || len(resume.StructuredContent) != 0 || !checkpointWireTextContains(resume.Content, "No pending checkpoint.") {
						t.Fatalf("core should report no pending checkpoint: %+v", resume)
					}
				} else {
					var restored checkpointResumeResult
					if resume.IsError || json.Unmarshal(resume.StructuredContent, &restored) != nil ||
						restored.ID != test.core.ID || !reflect.DeepEqual(restored.State, test.core.State) {
						t.Fatalf("core restored %+v, want %+v: %+v", restored, test.core, resume)
					}
				}
			}
		})
	}

	ordinary := callToolJSONRPC(t, full, 3, "checkpoint_resume", map[string]any{"agent_id": "missing"}).Result
	if !ordinary.IsError || len(ordinary.StructuredContent) != 0 || !checkpointWireTextContains(ordinary.Content, "no checkpoint found") {
		t.Fatalf("expected the historical prose-only absence error, got %+v", ordinary)
	}
	empty := callToolJSONRPC(t, full, 4, "checkpoint_resume", map[string]any{"agent_id": "missing", "on_missing": "empty"}).Result
	var payload map[string]any
	if empty.IsError || json.Unmarshal(empty.StructuredContent, &payload) != nil ||
		payload["found"] != false || payload["agent_id"] != "missing" || !checkpointWireTextContains(empty.Content, "No checkpoint saved") {
		t.Fatalf("expected structured and textual absence, got %+v", empty)
	}

	if err := mem.Advanced().DB().Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("sessions")).Bucket([]byte("pending")).Put([]byte("broken"), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	for name, srv := range map[string]*Server{"core": core, "full": full} {
		corrupt := callToolJSONRPC(t, srv, 5, "checkpoint_resume", map[string]any{"agent_id": "pending", "on_missing": "empty"}).Result
		if !corrupt.IsError || len(corrupt.StructuredContent) != 0 || !checkpointWireTextContains(corrupt.Content, "decode checkpoint") {
			t.Fatalf("%s: expected corruption to remain a prose-only error, got %+v", name, corrupt)
		}
	}
}

func checkpointWireTextContains(content []json.RawMessage, value string) bool {
	return len(content) == 1 && strings.Contains(string(content[0]), value)
}
