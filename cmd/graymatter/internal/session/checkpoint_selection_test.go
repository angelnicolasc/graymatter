package session

import (
	"errors"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func saveResumeCheckpoint(t *testing.T, db *bolt.DB, cp Checkpoint) Checkpoint {
	t.Helper()
	saved, err := Save(db, cp)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func resumeHookMarker(event, sessionID string) Checkpoint {
	return Checkpoint{
		AgentID: "agent",
		State: map[string]any{
			"source": "hooks", "event": event, "session_id": sessionID,
		},
		Metadata: map[string]string{
			"source": "hooks", "event": event, "session_id": sessionID,
		},
	}
}

func TestCheckpointResumeSkipsHistoricalHookMarkers(t *testing.T) {
	for _, event := range []string{"pre-compact", "session-end"} {
		t.Run(event, func(t *testing.T) {
			db := checkpointDB(t)
			useful := saveResumeCheckpoint(t, db, Checkpoint{
				AgentID: "agent", CreatedAt: time.Unix(10, 0).UTC(), State: map[string]any{"step": "finish review"},
			})
			marker := resumeHookMarker(event, "editor-session")
			marker.CreatedAt = time.Unix(11, 0).UTC()
			marker = saveResumeCheckpoint(t, db, marker)

			got, err := Resume(db, "agent")
			if err != nil || !reflect.DeepEqual(got, &useful) {
				t.Fatalf("Resume = %+v, %v; want %+v", got, err, useful)
			}
			list, err := List(db, "agent")
			if err != nil || !reflect.DeepEqual(list, []Checkpoint{marker, useful}) {
				t.Fatalf("List = %+v, %v; want both checkpoints", list, err)
			}
			for _, want := range list {
				loaded, err := Load(db, want.AgentID, want.ID)
				if err != nil || !reflect.DeepEqual(loaded, &want) {
					t.Fatalf("Load(%q) = %+v, %v; want %+v", want.ID, loaded, err, want)
				}
			}
		})
	}
}

func TestCheckpointResumeOnlyMarkersIsAbsence(t *testing.T) {
	db := checkpointDB(t)
	saveResumeCheckpoint(t, db, resumeHookMarker("pre-compact", "editor-session"))
	saveResumeCheckpoint(t, db, resumeHookMarker("session-end", ""))
	saveResumeCheckpoint(t, db, Checkpoint{AgentID: "other", State: map[string]any{"goal": "independent"}})

	for _, namespace := range []string{"agent", "missing"} {
		if got, err := Resume(db, namespace); got != nil || !errors.Is(err, ErrNoCheckpoint) {
			t.Fatalf("Resume(%q) = %+v, %v; want typed absence", namespace, got, err)
		}
	}
}

func TestCheckpointResumeKeepsListOrderAndNamespaceAfterReopen(t *testing.T) {
	db := checkpointDB(t)
	saveResumeCheckpoint(t, db, Checkpoint{ID: "old", AgentID: "agent", CreatedAt: time.Unix(10, 0).UTC()})
	between := resumeHookMarker("pre-compact", "older-editor-session")
	between.CreatedAt = time.Unix(11, 0).UTC()
	saveResumeCheckpoint(t, db, between)

	tiedAt := time.Unix(12, 0).UTC()
	for _, id := range []string{"first", "last"} {
		saveResumeCheckpoint(t, db, Checkpoint{ID: id, AgentID: "agent", CreatedAt: tiedAt})
	}
	tiedMarker := resumeHookMarker("session-end", "new-editor-session")
	tiedMarker.ID, tiedMarker.CreatedAt = "middle-marker", tiedAt
	saveResumeCheckpoint(t, db, tiedMarker)
	saveResumeCheckpoint(t, db, Checkpoint{ID: "other", AgentID: "other", CreatedAt: time.Unix(13, 0).UTC()})

	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	list, err := List(reopened, "agent")
	if err != nil {
		t.Fatal(err)
	}
	var firstTiedID string
	for _, cp := range list {
		if cp.ID == "first" || cp.ID == "last" {
			firstTiedID = cp.ID
			break
		}
	}
	got, err := Resume(reopened, "agent")
	if err != nil || got == nil || firstTiedID == "" || got.ID != firstTiedID {
		t.Fatalf("Resume = %+v, %v; want first useful checkpoint in List order %q", got, err, firstTiedID)
	}
	if got, err := Resume(reopened, "other"); err != nil || got == nil || got.ID != "other" {
		t.Fatalf("Resume for other namespace = %+v, %v", got, err)
	}
}

func TestCheckpointResumeKeepsUnrecognizedHookShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Checkpoint)
	}{
		{"legitimate empty checkpoint", func(cp *Checkpoint) { cp.State, cp.Metadata = nil, nil }},
		{"additional state", func(cp *Checkpoint) { cp.State["goal"] = "finish review" }},
		{"additional metadata", func(cp *Checkpoint) { cp.Metadata["note"] = "substantive" }},
		{"saved messages", func(cp *Checkpoint) { cp.Messages = []Message{{Role: "user", Content: "restore this"}} }},
		{"unknown event", func(cp *Checkpoint) { cp.State["event"], cp.Metadata["event"] = "future-event", "future-event" }},
		{"inconsistent event", func(cp *Checkpoint) { cp.Metadata["event"] = "session-end" }},
		{"inconsistent session", func(cp *Checkpoint) { cp.Metadata["session_id"] = "other" }},
		{"unrecognized session type", func(cp *Checkpoint) { cp.State["session_id"] = []any{} }},
		{"missing session field", func(cp *Checkpoint) {
			delete(cp.Metadata, "session_id")
			cp.Metadata["unknown"] = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := checkpointDB(t)
			cp := resumeHookMarker("pre-compact", "")
			tc.change(&cp)
			want := saveResumeCheckpoint(t, db, cp)
			got, err := Resume(db, "agent")
			if err != nil || got == nil || got.ID != want.ID {
				t.Fatalf("Resume = %+v, %v; want checkpoint %q", got, err, want.ID)
			}
		})
	}
}
