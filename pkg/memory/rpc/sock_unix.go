//go:build unix

package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// maxUnixSocketPath is a conservative cap on the length of a Unix domain
// socket path. The kernel struct sockaddr_un.sun_path is 104 bytes on
// macOS/BSD and 108 on Linux (including the NUL terminator); 100 is safely
// under both. Paths longer than this fail bind() with EINVAL.
const maxUnixSocketPath = 100

// socketPath chooses where to bind the daemon socket. It prefers a socket
// inside dataDir (nice for `ls .graymatter`), but deeply nested project
// paths can blow past sun_path — so when the in-dir path is too long it
// falls back to a short, stable path under the system temp dir, derived
// from a hash of the absolute data dir. The discovery file records whichever
// path we pick, so clients never need to recompute it.
func socketPath(dataDir string) string {
	inDir := filepath.Join(dataDir, "graymatter.sock")
	if len(inDir) <= maxUnixSocketPath {
		return inDir
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(os.TempDir(), "graymatter-"+hex.EncodeToString(sum[:8])+".sock")
}

// Listen creates a listener for the daemon, writes a discovery file with the
// address and auth token, and returns the listener plus a cleanup func that
// removes both the discovery file and the socket. The socket is chmod 0600 so
// other local users cannot connect even where the data dir is world-readable.
func Listen(dataDir, token string) (net.Listener, func(), error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("rpc: create data dir: %w", err)
	}
	sockPath := socketPath(dataDir)

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
