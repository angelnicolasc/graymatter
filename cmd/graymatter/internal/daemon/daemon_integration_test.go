package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

var (
	testBinaryOnce sync.Once
	testBinaryDir  string
	testBinaryPath string
	testBinaryErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	// Shutdown acknowledges before the daemon necessarily exits, so Windows
	// may still hold the executable here. This does not remove that race: it
	// defers cleanup until after all package tests and makes it best-effort
	// instead of a mandatory t.TempDir cleanup assertion.
	if testBinaryDir != "" {
		_ = os.RemoveAll(testBinaryDir)
	}
	os.Exit(code)
}

// buildBinary lazily compiles one graymatter binary for the package. Keeping
// it outside t.TempDir prevents a transient Windows image lock from failing
// an otherwise successful test during that test's mandatory cleanup.
func buildBinary(t *testing.T) string {
	t.Helper()
	testBinaryOnce.Do(func() {
		testBinaryDir, testBinaryErr = os.MkdirTemp("", "graymatter-daemon-test-")
		if testBinaryErr != nil {
			return
		}
		testBinaryPath = filepath.Join(testBinaryDir, "graymatter")
		if runtime.GOOS == "windows" {
			testBinaryPath += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", testBinaryPath,
			"github.com/angelnicolasc/graymatter/cmd/graymatter")
		if combined, err := cmd.CombinedOutput(); err != nil {
			testBinaryErr = fmt.Errorf("go build graymatter: %w\n%s", err, combined)
		}
	})
	if testBinaryErr != nil {
		t.Fatal(testBinaryErr)
	}
	return testBinaryPath
}

// withBuiltDaemon points spawn-on-connect at a freshly built binary for the
// duration of the test.
func withBuiltDaemon(t *testing.T) {
	t.Helper()
	bin := buildBinary(t)
	prev := resolveExecutable
	resolveExecutable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { resolveExecutable = prev })
}

// TestConnect_SpawnsDaemonAndWrites is the end-to-end issue #8 acceptance
// test: with no daemon running, Connect must start one and a write must
// round-trip through it.
func TestConnect_SpawnsDaemonAndWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	withBuiltDaemon(t)
	dir := t.TempDir()
	ctx := context.Background()

	c, err := Connect(dir)
	if err != nil {
		t.Fatalf("Connect (should auto-spawn): %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Remember(ctx, "agent-a", "first fact through the daemon"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	facts, err := c.List("agent-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}

	// A daemon must be reachable now without spawning.
	if pid := ReadPIDFile(dir); pid == 0 {
		t.Error("expected a daemon pid file after spawn")
	}

	if err := c.Shutdown(); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestConcurrentClients_ThroughDaemon mirrors the real #4 / #9 scenario:
// several independent clients (think TUI + MCP server + a CLI command) all
// writing to the same data dir at once, which the single-writer bbolt lock
// would otherwise forbid.
func TestConcurrentClients_ThroughDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	withBuiltDaemon(t)
	dir := t.TempDir()
	ctx := context.Background()

	// First connection spawns the daemon; keep it open so the daemon stays
	// up for the whole test.
	lead, err := Connect(dir)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = lead.Close() }()

	const clients = 4
	const writes = 20
	var wg sync.WaitGroup
	errCh := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c, err := Connect(dir) // existing daemon: no second spawn
			if err != nil {
				errCh <- fmt.Errorf("client %d connect: %w", id, err)
				return
			}
			defer func() { _ = c.Close() }()
			for j := 0; j < writes; j++ {
				if err := c.Remember(ctx, "swarm", fmt.Sprintf("c%d-%d", id, j)); err != nil {
					errCh <- fmt.Errorf("client %d remember %d: %w", id, j, err)
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

	facts, err := lead.List("swarm")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != clients*writes {
		t.Fatalf("got %d facts, want %d (lost writes = lock contention)", len(facts), clients*writes)
	}

	_ = lead.Shutdown()
}

// TestIdleExit verifies a client-spawned daemon reaps itself after the idle
// window once every client disconnects.
func TestIdleExit(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon; skipped in -short")
	}
	dir := t.TempDir()

	// Run the daemon directly with a tiny idle window in a goroutine.
	done := make(chan error, 1)
	go func() {
		done <- Run(RunOptions{
			DataDir:  dir,
			IdleExit: 1 * time.Second,
			Logf:     func(string, ...any) {},
		})
	}()

	// Wait for it to come up.
	deadline := time.Now().Add(5 * time.Second)
	var c *Client
	for time.Now().Before(deadline) {
		if cl, err := rpc.Dial(rpc.DialOptions{DataDir: dir}); err == nil {
			c = &Client{Client: cl, dataDir: dir}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c == nil {
		t.Fatal("daemon did not come up")
	}
	// Disconnect so the idle clock can start.
	_ = c.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon exited with error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not idle-exit within 15s")
	}
}
