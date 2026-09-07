package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
	bolt "go.etcd.io/bbolt"
)

func TestCheckpointResumeErrorRoundTrip(t *testing.T) {
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
	saved, err := c.CheckpointSave(session.Checkpoint{AgentID: "a", State: map[string]any{"step": "two"}})
	if err != nil {
		t.Fatal(err)
	}
	if cp, err := c.CheckpointResume("a"); err != nil || cp.ID != saved.ID || cp.State["step"] != "two" {
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
