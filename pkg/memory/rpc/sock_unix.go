//go:build unix

package rpc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// Listen creates a listener for the daemon, writes a discovery file with the
// address and auth token, and returns the listener plus a cleanup func that
// removes both the discovery file and the socket. On Unix this binds a Unix
// domain socket at $DataDir/graymatter.sock, chmod 0600 so other local users
// cannot connect even where the data dir itself is world-readable.
func Listen(dataDir, token string) (net.Listener, func(), error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("rpc: create data dir: %w", err)
	}
	sockPath := filepath.Join(dataDir, "graymatter.sock")

	// Remove any stale socket from a previous crashed daemon. Best-effort:
	// if the file is held by a live daemon, the subsequent Listen will
	// fail with EADDRINUSE and we surface that.
	_ = os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, nil, fmt.Errorf("rpc: listen unix %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, nil, fmt.Errorf("rpc: chmod socket: %w", err)
	}

	addr := "unix://" + sockPath
	if err := writeDiscovery(dataDir, addr, token); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, nil, fmt.Errorf("rpc: write discovery: %w", err)
	}

	cleanup := func() {
		removeDiscovery(dataDir)
		_ = os.Remove(sockPath)
	}
	return ln, cleanup, nil
}
