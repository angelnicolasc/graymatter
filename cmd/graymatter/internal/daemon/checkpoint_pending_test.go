package daemon

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
	bolt "go.etcd.io/bbolt"
)

// Exported receiver/method are deliberately old: no pending-pointer RPC.
type OldPendingHost struct{ resumes int }

func (h *OldPendingHost) CheckpointResume(_ *CheckpointResumeRequest, _ *CheckpointResumeResponse) error {
	h.resumes++
	return nil
}

func pendingRPCClient(t *testing.T, host any) *Client {
	t.Helper()
	dir := t.TempDir()
	ln, cleanup, err := rpc.Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	srv := rpc.NewServer(nil, nil)
	srv.RegisterExtra(HostServiceName, host)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	conn, err := rpc.Dial(rpc.DialOptions{DataDir: dir, PingOnDial: true})
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{Client: conn}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCheckpointPeekPendingRPCReadOnly(t *testing.T) {
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://pending-test")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	h := newDirectHost(t)
	c := pendingRPCClient(t, h)
	if got, err := c.CheckpointPeekPending("missing"); err != nil || got != nil {
		t.Fatalf("missing: %+v, %v", got, err)
	}
	saved, err := c.CheckpointSave(session.Checkpoint{AgentID: "a", CreatedAt: time.Unix(10, 0).UTC(), State: map[string]any{"step": "finish review"}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(h.db.Path())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		got, err := c.CheckpointPeekPending("a")
		if err != nil || got == nil || got.ID != saved.ID || !got.CreatedAt.Equal(saved.CreatedAt) {
			t.Fatalf("pending: %+v, %v", got, err)
		}
	}
	after, err := os.ReadFile(h.db.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("pointer RPC mutated database")
	}
	if _, err := c.CheckpointSave(session.Checkpoint{AgentID: "a", CreatedAt: time.Unix(11, 0).UTC(), State: map[string]any{"status": "complete"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.CheckpointPeekPending("a"); err != nil || got != nil {
		t.Fatalf("completed: %+v, %v", got, err)
	}
	if err := h.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("sessions")).Bucket([]byte("a")).Put([]byte("broken"), []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.CheckpointPeekPending("a"); got != nil || err == nil || !strings.Contains(err.Error(), "decode checkpoint") {
		t.Fatalf("corrupt: %+v, %v", got, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := c.CheckpointPeekPending("a"); got != nil || err == nil || strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("transport error misclassified: %+v, %v", got, err)
	}
}

func TestCheckpointPeekPendingOldDaemonVisibleError(t *testing.T) {
	h := &OldPendingHost{}
	c := pendingRPCClient(t, h)
	got, err := c.CheckpointPeekPending("a")
	if got != nil || err == nil || !strings.Contains(err.Error(), "upgrade and restart") {
		t.Fatalf("old daemon: %+v, %v", got, err)
	}
	if h.resumes != 0 {
		t.Fatal("pending pointer fell back to Resume")
	}
}
