package session

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestPeekPendingSelectionAndClosure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    map[string]any
		messages []Message
		want     bool
	}{
		{"work", map[string]any{"step": "finish"}, nil, true},
		{"messages", nil, []Message{{Role: "user", Content: "unfinished task"}}, true},
		{"empty", nil, nil, false},
		{"complete", map[string]any{"status": "complete"}, nil, false},
		{"completed", map[string]any{"status": "completed"}, nil, false},
		{"done", map[string]any{"status": "done"}, nil, false},
		{"boolean closure", map[string]any{"completed": true}, nil, false},
		{"boolean false", map[string]any{"completed": false}, nil, true},
		{"free text", map[string]any{"step": "done with parsing; review remains"}, nil, true},
		{"exact status", map[string]any{"status": "COMPLETE"}, nil, true},
		{"string is not boolean", map[string]any{"completed": "true"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := checkpointDB(t)
			saveResumeCheckpoint(t, db, Checkpoint{AgentID: "a", CreatedAt: time.Unix(1, 0).UTC(), State: map[string]any{"step": "old task"}})
			latest := saveResumeCheckpoint(t, db, Checkpoint{AgentID: "a", CreatedAt: time.Unix(2, 0).UTC(), State: tc.state, Messages: tc.messages})
			marker := resumeHookMarker("session-end", "editor")
			marker.AgentID, marker.CreatedAt = "a", time.Unix(3, 0).UTC()
			saveResumeCheckpoint(t, db, marker)
			saveResumeCheckpoint(t, db, Checkpoint{AgentID: "other", CreatedAt: time.Unix(4, 0).UTC(), State: map[string]any{"step": "other task"}})
			got, err := PeekPending(db, "a")
			if err != nil {
				t.Fatal(err)
			}
			if !tc.want {
				if got != nil {
					t.Fatalf("latest state must not expose older work: %+v", got)
				}
				return
			}
			want := &PendingCheckpoint{ID: latest.ID, CreatedAt: latest.CreatedAt}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("pending = %+v, want %+v", got, want)
			}
			resumed, err := Resume(db, "a")
			if err != nil || resumed.ID != got.ID {
				t.Fatalf("pointer differs from resume: %+v, %v", resumed, err)
			}
		})
	}
}

func TestPeekPendingReadOnlyAfterReopen(t *testing.T) {
	db := checkpointDB(t)
	want := saveResumeCheckpoint(t, db, Checkpoint{AgentID: "a", CreatedAt: time.Unix(10, 0).UTC(), State: map[string]any{"step": "finish"}})
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"facts", "audit", "tokens"} {
			b, err := tx.CreateBucket([]byte(name))
			if err != nil {
				return err
			}
			if err := b.Put([]byte("unchanged"), []byte("access=7; tokens=11")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		got, err := PeekPending(ro, "a")
		if err != nil || got == nil || got.ID != want.ID {
			t.Fatalf("readonly pointer: %+v, %v", got, err)
		}
	}
	if got, err := PeekPending(ro, "missing"); err != nil || got != nil {
		t.Fatalf("missing: %+v, %v", got, err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("peek changed checkpoint, facts, audit, tokens or database pages")
	}
}

func TestPeekPendingFailsClosed(t *testing.T) {
	db := checkpointDB(t)
	saveResumeCheckpoint(t, db, Checkpoint{AgentID: "a", State: map[string]any{"step": "unfinished"}})
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSessions).Bucket([]byte("a")).Put([]byte("broken"), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := PeekPending(db, "a"); got != nil || err == nil || !strings.Contains(err.Error(), "decode checkpoint") {
		t.Fatalf("corrupt: %+v, %v", got, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := PeekPending(db, "a"); got != nil || !errors.Is(err, bolt.ErrDatabaseNotOpen) {
		t.Fatalf("closed: %+v, %v", got, err)
	}
	if got, err := PeekPending(nil, "a"); got != nil || err == nil {
		t.Fatalf("nil database: %+v, %v", got, err)
	}
	if got, err := PeekPending(checkpointDB(t), " "); got != nil || err == nil {
		t.Fatalf("empty identity: %+v, %v", got, err)
	}
}

func TestPeekPendingMarkersAndTies(t *testing.T) {
	db := checkpointDB(t)
	marker := resumeHookMarker("pre-compact", "editor")
	marker.CreatedAt = time.Unix(10, 0).UTC()
	saveResumeCheckpoint(t, db, marker)
	if got, err := PeekPending(db, "agent"); got != nil || err != nil {
		t.Fatalf("marker only: %+v, %v", got, err)
	}
	for _, id := range []string{"first", "last"} {
		saveResumeCheckpoint(t, db, Checkpoint{ID: id, AgentID: "agent", CreatedAt: marker.CreatedAt, State: map[string]any{"step": id}})
	}
	latest, err := Latest(db, "agent")
	if err != nil {
		t.Fatal(err)
	}
	got, err := PeekPending(db, "agent")
	if err != nil || got == nil || got.ID != latest.ID {
		t.Fatalf("tie order differs: %+v, %v, latest %+v", got, err, latest)
	}
}
