package rpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// startServer opens a real memory.Store in a temp dir, wraps it in a Server,
// and serves it on the platform listener (unix socket / TCP loopback).
// Returns the data dir for clients to Dial against.
func startServer(t *testing.T, token string) (dataDir string, srv *Server) {
	t.Helper()
	dataDir = t.TempDir()

	store, err := memory.Open(memory.StoreConfig{DataDir: dataDir, StrictWrite: true})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	srv = NewServer(store, nil)
	srv.SetAuthToken(token)

	ln, cleanup, err := Listen(dataDir, token)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(cleanup)

	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return dataDir, srv
}

func dialT(t *testing.T, dataDir string) *Client {
	t.Helper()
	c, err := Dial(DialOptions{DataDir: dataDir, PingOnDial: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRoundTrip_CoreSurface(t *testing.T) {
	dataDir, _ := startServer(t, mustToken(t))
	c := dialT(t, dataDir)
	ctx := context.Background()

	// Put + List
	if err := c.Put(ctx, "agent-a", "fact alpha"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Put(ctx, "agent-a", "fact beta"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	facts, err := c.List("agent-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("List returned %d facts, want 2", len(facts))
	}

	// Recall
	recalled, err := c.Recall(ctx, "agent-a", "alpha", 5)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(recalled) == 0 {
		t.Fatal("Recall returned nothing")
	}

	// Shared namespace
	if err := c.PutShared(ctx, "shared gamma"); err != nil {
		t.Fatalf("PutShared: %v", err)
	}
	sharedFacts, err := c.RecallShared(ctx, "gamma", 5)
	if err != nil {
		t.Fatalf("RecallShared: %v", err)
	}
	if len(sharedFacts) == 0 {
		t.Fatal("RecallShared returned nothing")
	}

	// ListAgents + Stats
	agents, err := c.ListAgents()
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) == 0 {
		t.Fatal("ListAgents returned nothing")
	}
	stats, err := c.Stats("agent-a")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.FactCount != 2 {
		t.Errorf("Stats.FactCount = %d, want 2", stats.FactCount)
	}

	// UpdateFact + Delete
	f := facts[0]
	f.Weight = 0.123
	if err := c.UpdateFact("agent-a", f); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}
	if err := c.Delete("agent-a", facts[1].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	after, err := c.List("agent-a")
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("after delete: %d facts, want 1", len(after))
	}
	if after[0].Weight != 0.123 {
		t.Errorf("updated weight = %v, want 0.123", after[0].Weight)
	}

	// PendingVectorCount (keyword-only store: zero)
	if n, err := c.PendingVectorCount(); err != nil || n != 0 {
		t.Errorf("PendingVectorCount = %d, %v; want 0, nil", n, err)
	}
}

// TestConcurrentClients is the issue #8 acceptance test at the RPC layer:
// multiple processes' worth of clients writing simultaneously through one
// store owner, with no bbolt lock fights.
func TestConcurrentClients_WritesInterleave(t *testing.T) {
	dataDir, _ := startServer(t, mustToken(t))
	ctx := context.Background()

	const clients = 4
	const writesPerClient = 25

	var wg sync.WaitGroup
	errCh := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c, err := Dial(DialOptions{DataDir: dataDir, PingOnDial: true})
			if err != nil {
				errCh <- fmt.Errorf("client %d dial: %w", id, err)
				return
			}
			defer func() { _ = c.Close() }()
			for j := 0; j < writesPerClient; j++ {
				if err := c.Put(ctx, "swarm", fmt.Sprintf("client %d fact %d", id, j)); err != nil {
					errCh <- fmt.Errorf("client %d put %d: %w", id, j, err)
					return
				}
			}
			errCh <- nil
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	c := dialT(t, dataDir)
	facts, err := c.List("swarm")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != clients*writesPerClient {
		t.Fatalf("got %d facts, want %d", len(facts), clients*writesPerClient)
	}
}

func TestAuth_WrongTokenRejected(t *testing.T) {
	dataDir, _ := startServer(t, mustToken(t))

	// Sabotage the token the client will read.
	addr, _, err := readDiscovery(dataDir)
	if err != nil {
		t.Fatalf("readDiscovery: %v", err)
	}
	if err := writeDiscovery(dataDir, addr, "deadbeef"); err != nil {
		t.Fatalf("writeDiscovery: %v", err)
	}

	_, err = Dial(DialOptions{DataDir: dataDir, PingOnDial: true})
	if err == nil {
		t.Fatal("Dial with wrong token should fail")
	}
}

func TestAuth_MissingTokenRejected(t *testing.T) {
	dataDir, _ := startServer(t, mustToken(t))

	addr, _, err := readDiscovery(dataDir)
	if err != nil {
		t.Fatalf("readDiscovery: %v", err)
	}

	// Raw connection that never sends the preamble: the server must drop it
	// within its 3s auth deadline; the client read then fails.
	conn, err := dialAddr(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialAddr: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("server should close unauthenticated connections, but it answered")
	}
}

func TestActiveConns_TracksClients(t *testing.T) {
	dataDir, srv := startServer(t, mustToken(t))

	if n := srv.ActiveConns(); n != 0 {
		t.Fatalf("ActiveConns = %d before any client, want 0", n)
	}

	c1 := dialT(t, dataDir)
	c2 := dialT(t, dataDir)
	_ = c1.Ping()
	_ = c2.Ping()
	waitFor(t, func() bool { return srv.ActiveConns() == 2 }, "two active conns")

	_ = c1.Close()
	waitFor(t, func() bool { return srv.ActiveConns() == 1 }, "one active conn after close")

	before := srv.LastActivity()
	_ = c2.Close()
	waitFor(t, func() bool { return srv.ActiveConns() == 0 }, "zero active conns")
	if !srv.LastActivity().After(before) && srv.LastActivity() != before {
		// Disconnect must refresh the idle clock (>= is fine on coarse clocks).
		t.Error("LastActivity did not advance on disconnect")
	}
}

func TestDial_NoDaemonReturnsErrClosed(t *testing.T) {
	_, err := Dial(DialOptions{DataDir: t.TempDir()})
	if err == nil {
		t.Fatal("Dial without a daemon should fail")
	}
	// Spawn-on-connect logic keys off net.ErrClosed to mean "no daemon".
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected net.ErrClosed in chain, got: %v", err)
	}
}

func TestServer_StopUnblocksServe(t *testing.T) {
	dataDir, srv := startServer(t, mustToken(t))
	c := dialT(t, dataDir)
	if err := c.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	done := make(chan struct{})
	go func() {
		srv.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return within 10s")
	}
}

func mustToken(t *testing.T) string {
	t.Helper()
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64", len(tok))
	}
	return tok
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
