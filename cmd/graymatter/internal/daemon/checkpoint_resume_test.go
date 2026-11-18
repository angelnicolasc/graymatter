package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
	bolt "go.etcd.io/bbolt"
)

func TestCheckpointResumeErrorRoundTrip(t *testing.T) {
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://checkpoint-test")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	h := newDirectHost(t)
	dir := t.TempDir()
	ln, cleanup, err := rpc.Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	srv := rpc.NewServer(nil, nil)
	srv.RegisterExtra(HostServiceName, h)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	conn, err := rpc.Dial(rpc.DialOptions{DataDir: dir, PingOnDial: true})
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{Client: conn}
	t.Cleanup(func() { _ = c.Close() })

	if cp, err := c.CheckpointResume("missing"); cp != nil || !errors.Is(err, session.ErrNoCheckpoint) {
		t.Fatalf("missing = %+v, %v; want ErrNoCheckpoint after RPC", cp, err)
	}
	// The request shape sent by older clients must still receive an error,
	// rather than mistaking the additive NotFound field for an empty success.
	var legacy CheckpointResumeResponse
	if err := c.hostCall("CheckpointResume", &struct{ AgentID string }{"missing"}, &legacy); err == nil {
		t.Fatal("legacy resume request returned success for missing checkpoint")
	}
	saved, err := c.CheckpointSave(session.Checkpoint{
		AgentID: "a", CreatedAt: time.Unix(10, 0).UTC(), State: map[string]any{"step": "two"},
	})
	if err != nil {
		t.Fatal(err)
	}
	marker := session.Checkpoint{
		AgentID: "only-markers", CreatedAt: time.Unix(11, 0).UTC(),
		State:    map[string]any{"source": "hooks", "event": "session-end", "session_id": "editor"},
		Metadata: map[string]string{"source": "hooks", "event": "session-end", "session_id": "editor"},
	}
	if _, err := c.CheckpointSave(marker); err != nil {
		t.Fatal(err)
	}
	if cp, err := c.CheckpointResume(marker.AgentID); cp != nil || !errors.Is(err, session.ErrNoCheckpoint) {
		t.Fatalf("marker-only = %+v, %v; want ErrNoCheckpoint after RPC", cp, err)
	}
	if err := c.hostCall("CheckpointResume", &struct{ AgentID string }{marker.AgentID}, &legacy); err == nil {
		t.Fatal("legacy resume request returned success for marker-only checkpoint history")
	}
	marker.AgentID = "a"
	savedMarker, err := c.CheckpointSave(marker)
	if err != nil {
		t.Fatal(err)
	}
	if cp, err := c.CheckpointLoad("a", savedMarker.ID); err != nil || cp == nil || cp.ID != savedMarker.ID {
		t.Fatalf("marker load = %+v, %v", cp, err)
	}
	if cps, err := c.CheckpointList("a"); err != nil || len(cps) != 2 || cps[0].ID != savedMarker.ID || cps[1].ID != saved.ID {
		t.Fatalf("checkpoint list = %+v, %v; want both entries", cps, err)
	}
	if cp, err := c.CheckpointResume("a"); err != nil || cp == nil || cp.ID != saved.ID || cp.State["step"] != "two" {
		t.Fatalf("saved = %+v, %v", cp, err)
	}
	if err := h.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("sessions")).Bucket([]byte("a")).Put([]byte("broken"), []byte("invalid JSON"))
	}); err != nil {
		t.Fatal(err)
	}
	if cp, err := c.CheckpointResume("a"); cp != nil || err == nil || errors.Is(err, session.ErrNoCheckpoint) || !strings.Contains(err.Error(), "decode checkpoint") {
		t.Fatalf("corrupt = %+v, %v; want decode error after RPC", cp, err)
	}
	if err := h.mem.Close(); err != nil {
		t.Fatal(err)
	}
	if cp, err := c.CheckpointResume("a"); cp != nil || err == nil || errors.Is(err, session.ErrNoCheckpoint) || !strings.Contains(err.Error(), bolt.ErrDatabaseNotOpen.Error()) {
		t.Fatalf("closed database = %+v, %v; want storage error after RPC", cp, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if cp, err := c.CheckpointResume("a"); cp != nil || err == nil || errors.Is(err, session.ErrNoCheckpoint) {
		t.Fatalf("closed connection = %+v, %v; want transport error", cp, err)
	}
}
