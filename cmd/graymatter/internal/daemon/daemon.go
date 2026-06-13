package daemon

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

// DefaultIdleExit is how long a client-spawned daemon survives with no
// connected clients and no traffic before exiting on its own. One-shot CLI
// bursts (remember → recall → export) reuse the warm daemon; an abandoned
// one reaps itself.
const DefaultIdleExit = 2 * time.Minute

// RunOptions configures a daemon run.
type RunOptions struct {
	// DataDir is the .graymatter directory to own. Made absolute internally
	// so the daemon is immune to client cwd differences.
	DataDir string

	// IdleExit shuts the daemon down after this long with zero connected
	// clients and no RPC traffic. 0 disables idle-exit (for service-manager
	// setups where systemd/launchd owns the lifecycle).
	IdleExit time.Duration

	// Logf receives lifecycle log lines. Defaults to a stderr printer.
	Logf func(format string, v ...any)
}

// Run starts the daemon in the foreground and blocks until shutdown
// (signal, Shutdown RPC, idle-exit, or listener error).
//
// Ordering is load-bearing for the concurrent-start race: the bbolt lock is
// acquired FIRST (strict write — a daemon that cannot own the store must
// die loudly, not degrade to read-only), and only the lock winner writes
// the discovery file. A second daemon racing here fails its open within the
// lock timeout and exits; the spawning client just retries the dial and
// reaches the winner.
func Run(opts RunOptions) error {
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, v ...any) { fmt.Fprintf(os.Stderr, format+"\n", v...) }
	}

	absDir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return fmt.Errorf("daemon: resolve data dir: %w", err)
	}

	cfg := graymatter.DefaultConfig()
	cfg.DataDir = absDir
	cfg.StrictWrite = true

	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	defer func() { _ = mem.Close() }()

	adv := mem.Advanced()
	db := adv.DB()

	var graph *kg.Graph
	var adapter *kg.GraphAdapter
	if g, err := kg.Open(db); err == nil {
		graph = g
		adapter = kg.NewGraphAdapter(g)
	}

	token, err := rpc.GenerateToken()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	srv := rpc.NewServer(rememberBackend{AdvancedStore: adv, mem: mem}, cfg)
	srv.SetAuthToken(token)
	srv.RegisterExtra(HostServiceName, &Host{
		mem:     mem,
		db:      db,
		graph:   graph,
		adapter: adapter,
		stop:    srv.Stop,
	})

	ln, cleanup, err := rpc.Listen(absDir, token)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	defer cleanup()

	if err := writePIDFile(absDir); err != nil {
		logf("daemon: write pid file: %v (continuing)", err)
	}
	defer removePIDFile(absDir)

	// Signal-driven shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		if _, ok := <-sigCh; ok {
			logf("daemon: signal received, shutting down")
			srv.Stop()
		}
	}()

	// Idle-exit loop.
	if opts.IdleExit > 0 {
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for range t.C {
				if srv.ActiveConns() == 0 && time.Since(srv.LastActivity()) >= opts.IdleExit {
					logf("daemon: idle for %s with no clients, exiting", opts.IdleExit)
					srv.Stop()
					return
				}
			}
		}()
	}

	addr, _ := rpc.DiscoveryAddr(absDir)
	logf("graymatter daemon ready: dir=%s addr=%s pid=%d idle-exit=%s",
		absDir, addr, os.Getpid(), idleExitLabel(opts.IdleExit))

	err = srv.Serve(ln)
	logf("daemon: stopped")
	return err
}

func idleExitLabel(d time.Duration) string {
	if d <= 0 {
		return "disabled"
	}
	return d.String()
}

func writePIDFile(dataDir string) error {
	return os.WriteFile(rpc.PIDFilePath(dataDir), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

func removePIDFile(dataDir string) {
	_ = os.Remove(rpc.PIDFilePath(dataDir))
}

// ReadPIDFile returns the daemon PID recorded in dataDir, or 0 if absent.
func ReadPIDFile(dataDir string) int {
	data, err := os.ReadFile(rpc.PIDFilePath(dataDir))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}
