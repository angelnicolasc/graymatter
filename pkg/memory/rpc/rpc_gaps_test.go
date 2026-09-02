package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/rpc/jsonrpc"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Coverage-gap tests for the RPC surface's error paths and the discovery
// file helpers. The happy paths are pinned elsewhere; here every backend
// failure must reach the caller through the wire intact, and every filesystem
// misconfiguration must fail loudly instead of silently mis-serving.

// failingBackend refuses everything: the stand-in for a store whose owner
// process went away mid-request. Each handler's error branch is exercised
// through the real wire by TestBackendErrorSurface.
type failingBackend struct{}

func (failingBackend) Put(context.Context, string, string) error { return errors.New("backend down") }
func (failingBackend) PutShared(context.Context, string) error {
	return errors.New("backend down")
}
func (failingBackend) Recall(context.Context, string, string, int) ([]string, error) {
	return nil, errors.New("backend down")
}

func (failingBackend) RecallDetailed(context.Context, string, string, int) ([]string, string, error) {
	return nil, "", errors.New("backend down")
}

func (failingBackend) PutAlias(context.Context, string, string, []string) (memory.Fact, error) {
	return memory.Fact{}, errors.New("backend down")
}
func (failingBackend) RecallShared(context.Context, string, int) ([]string, error) {
	return nil, errors.New("backend down")
}
func (failingBackend) RecallAll(context.Context, string, string, int) ([]string, error) {
	return nil, errors.New("backend down")
}
func (failingBackend) RecallExplain(context.Context, string, string, int) ([]memory.RecallReceipt, error) {
	return nil, errors.New("backend down")
}
func (failingBackend) List(string) ([]memory.Fact, error) {
	return nil, errors.New("backend down")
}
func (failingBackend) ListAgents() ([]string, error) { return nil, errors.New("backend down") }
func (failingBackend) Stats(string) (memory.MemoryStats, error) {
	return memory.MemoryStats{}, errors.New("backend down")
}
func (failingBackend) Delete(string, string) error { return errors.New("backend down") }
func (failingBackend) UpdateFact(string, memory.Fact) error {
	return errors.New("backend down")
}
func (failingBackend) Consolidate(context.Context, string, memory.ConsolidateConfig) error {
	return errors.New("backend down")
}
func (failingBackend) PendingVectorCount() int { return 7 }

type rpcTestConfig struct{}

func (rpcTestConfig) GetAnthropicAPIKey() string        { return "" }
func (rpcTestConfig) GetConsolidateLLM() string         { return "" }
func (rpcTestConfig) GetConsolidateModel() string       { return "" }
func (rpcTestConfig) GetConsolidateThreshold() int      { return 100 }
func (rpcTestConfig) GetDecayHalfLife() time.Duration   { return time.Hour }
func (rpcTestConfig) GetOllamaURL() string              { return "" }
func (rpcTestConfig) GetOllamaConsolidateModel() string { return "" }

func startFailingServer(t *testing.T) (*Server, *Client) {
	t.Helper()
	token := mustToken(t)
	srv := NewServer(failingBackend{}, rpcTestConfig{})
	srv.SetAuthToken(token)

	// One dataDir serves both sides: Listen writes its real discovery file
	// there (unix socket on POSIX, TCP loopback on Windows) and the client
	// dials through the ordinary public path.
	dataDir := t.TempDir()
	ln, cleanup, err := Listen(dataDir, token)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(cleanup)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	c, err := Dial(DialOptions{DataDir: dataDir, PingOnDial: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return srv, c
}

func TestBackendErrorSurface_ReachesEveryCaller(t *testing.T) {
	_, c := startFailingServer(t)
	ctx := context.Background()

	if _, err := c.Recall(ctx, "a", "q", 5); err == nil {
		t.Error("Recall: backend error was swallowed")
	}
	if _, err := c.RecallShared(ctx, "q", 5); err == nil {
		t.Error("RecallShared: backend error was swallowed")
	}
	if _, err := c.RecallAll(ctx, "a", "q", 5); err == nil {
		t.Error("RecallAll: backend error was swallowed")
	}
	if _, err := c.List("a"); err == nil {
		t.Error("List: backend error was swallowed")
	}
	if _, err := c.ListAgents(); err == nil {
		t.Error("ListAgents: backend error was swallowed")
	}
	if _, err := c.Stats("a"); err == nil {
		t.Error("Stats: backend error was swallowed")
	}
	if err := c.Put(ctx, "a", "t"); err == nil {
		t.Error("Put: backend error was swallowed")
	}
	if err := c.PutShared(ctx, "t"); err == nil {
		t.Error("PutShared: backend error was swallowed")
	}
	if err := c.Delete("a", "id"); err == nil {
		t.Error("Delete: backend error was swallowed")
	}
	if err := c.UpdateFact("a", memory.Fact{}); err == nil {
		t.Error("UpdateFact: backend error was swallowed")
	}
	if err := c.Consolidate(ctx, "a"); err == nil {
		t.Error("Consolidate: backend error was swallowed")
	}
	// PendingVectorCount carries a count, not an error: the fake's marker
	// value proves the response body made the round trip on a healthy path.
	if n, err := c.PendingVectorCount(); err != nil || n != 7 {
		t.Errorf("PendingVectorCount = %d, %v; want 7, nil", n, err)
	}
}

func TestRecallAll_MergesAgentAndSharedOverTheWire(t *testing.T) {
	dataDir, _ := startServer(t, mustToken(t))
	c := dialT(t, dataDir)
	ctx := context.Background()

	// Seed through the wire: the server owns the store's write lock.
	if err := c.Put(ctx, "merge-agent", "agent scoped fact about deploy"); err != nil {
		t.Fatal(err)
	}
	if err := c.PutShared(ctx, "shared fact about deploy"); err != nil {
		t.Fatal(err)
	}

	got, err := c.RecallAll(ctx, "merge-agent", "deploy", 8)
	if err != nil {
		t.Fatalf("RecallAll: %v", err)
	}
	if len(got) < 2 {
		t.Errorf("RecallAll returned %d facts, want agent + shared merged", len(got))
	}
}

func TestConsolidate_WithoutConfiguredPolicyFails(t *testing.T) {
	// startServer builds the server with a nil consolidation policy: the
	// daemon always configures one, so hitting this branch means a wiring
	// mistake that must be visible instead of a silent no-op consolidation.
	dataDir, _ := startServer(t, mustToken(t))
	c := dialT(t, dataDir)
	err := c.Consolidate(context.Background(), "any")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err = %v, want consolidation-not-configured", err)
	}
}

type echoService struct{}

func (echoService) Echo(args *string, reply *string) error {
	*reply = "echo:" + *args
	return nil
}

func TestRegisterExtra_ServiceIsReachableThroughThePreamble(t *testing.T) {
	token := mustToken(t)
	srv := NewServer(failingBackend{}, nil)
	srv.SetAuthToken(token)
	srv.RegisterExtra("GrayMatterEcho", echoService{})

	dataDir := t.TempDir()
	ln, cleanup, err := Listen(dataDir, token)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer cleanup()
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	addr, _, err := readDiscovery(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialAddr(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "%s\n", token); err != nil {
		t.Fatal(err)
	}
	rc := jsonrpc.NewClient(conn)
	defer rc.Close()
	var out string
	arg := "ping"
	if err := rc.Call("GrayMatterEcho.Echo", &arg, &out); err != nil {
		t.Fatalf("extra service call: %v", err)
	}
	if out != "echo:ping" {
		t.Errorf("reply = %q, want echoed argument", out)
	}
}

func TestServe_BrokenExtraRegistrationFailsFast(t *testing.T) {
	srv := NewServer(failingBackend{}, nil)
	srv.RegisterExtra("Broken", struct{}{}) // no exported methods: net/rpc rejects it

	ln, cleanup, err := Listen(t.TempDir(), mustToken(t))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := srv.Serve(ln); err == nil || !strings.Contains(err.Error(), "RegisterName") {
		t.Errorf("Serve err = %v, want registration failure naming RegisterName", err)
	}
}

func TestServe_AcceptFailureSurfacesWhenStopWasNotCalled(t *testing.T) {
	srv := NewServer(failingBackend{}, nil)
	token := mustToken(t)
	ln, cleanup, err := Listen(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	time.Sleep(50 * time.Millisecond) // let Serve block in Accept
	// Closing the listener behind Serve's back — not via Stop — is an
	// abnormal shutdown, and its error must not be mistaken for graceful.
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("abnormal listener close produced a nil error from Serve")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve never returned after the listener died")
	}
	srv.Stop()
}

func TestAuth_OversizedPreambleDropped(t *testing.T) {
	dataDir, _ := startServer(t, mustToken(t))
	addr, _, err := readDiscovery(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialAddr(addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// More bytes than maxTokenLine without ever sending the newline: the
	// reader must give up instead of buffering forever.
	fmt.Fprint(conn, strings.Repeat("a", maxTokenLine+64))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	for {
		if _, err := conn.Read(buf); err != nil {
			return // dropped, as required
		}
	}
}

func TestErrIsClosedConn_Table(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"io.EOF", fmt.Errorf("read: %w", io.EOF), true},
		{"net closed", fmt.Errorf("accept: %w", net.ErrClosed), true},
		{"anything else", errors.New("boom"), false},
	}
	for _, tc := range tests {
		if got := errIsClosedConn(tc.err); got != tc.want {
			t.Errorf("%s: errIsClosedConn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDiscoveryAddr_RoundTripAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := DiscoveryAddr(dir); !errors.Is(err, net.ErrClosed) {
		t.Errorf("missing discovery: err = %v, want net.ErrClosed in chain", err)
	}
	if err := writeDiscovery(dir, "tcp://127.0.0.1:9", "tok"); err != nil {
		t.Fatal(err)
	}
	addr, err := DiscoveryAddr(dir)
	if err != nil || addr != "tcp://127.0.0.1:9" {
		t.Errorf("DiscoveryAddr = %q, %v; want the recorded address", addr, err)
	}
}

func TestParseAddr_Table(t *testing.T) {
	network, target, err := parseAddr("unix:///run/gm.sock")
	if err != nil || network != "unix" || target != "/run/gm.sock" {
		t.Errorf("unix: %s %s %v", network, target, err)
	}
	network, target, err = parseAddr("tcp://127.0.0.1:1234")
	if err != nil || network != "tcp" || target != "127.0.0.1:1234" {
		t.Errorf("tcp: %s %s %v", network, target, err)
	}
	if _, _, err := parseAddr("pipe://whatever"); err == nil {
		t.Error("unknown scheme accepted")
	}
}

func TestWriteDiscovery_FailureModesAreLoud(t *testing.T) {
	t.Run("dataDir is a regular file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "occupied")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeDiscovery(file, "tcp://x", "tok"); err == nil {
			t.Error("MkdirAll against a file succeeded")
		}
	})

	t.Run("tmp name is taken by a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, discoveryFile+".tmp"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeDiscovery(dir, "tcp://x", "tok"); err == nil {
			t.Error("writing through a directory-shaped tmp path succeeded")
		}
	})

	t.Run("final name is taken by a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, discoveryFile), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeDiscovery(dir, "tcp://x", "tok"); err == nil {
			t.Error("renaming onto a directory succeeded")
		}
	})
}
