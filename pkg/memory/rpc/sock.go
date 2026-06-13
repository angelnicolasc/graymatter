package rpc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// discoveryFile is the file in DataDir where the daemon writes its listener
// address and auth token, one per line:
//
//	line 1: unix:///path/to/graymatter.sock  (or tcp://127.0.0.1:port)
//	line 2: 64-hex-char auth token
//
// The file is written atomically with 0600 perms. On Unix the socket file
// itself is also permission-gated; on Windows (TCP loopback) the token is the
// actual access control — any local process can reach 127.0.0.1, but only
// ones that can read this file can authenticate.
const discoveryFile = "graymatter.addr"

// pidFile is the file in DataDir where the daemon writes its PID.
// Used by `graymatter daemon status|stop` and stale-daemon reaping.
const pidFile = "graymatter.pid"

// PIDFilePath returns the absolute path to the daemon's PID file in
// dataDir. Exported for tests and CLI introspection.
func PIDFilePath(dataDir string) string {
	return filepath.Join(dataDir, pidFile)
}

// DiscoveryFilePath returns the absolute path to the daemon's discovery
// file in dataDir.
func DiscoveryFilePath(dataDir string) string {
	return filepath.Join(dataDir, discoveryFile)
}

// GenerateToken returns a fresh 256-bit random token, hex-encoded.
func GenerateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rpc: generate token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// DiscoveryAddr returns the listener address recorded in dataDir's discovery
// file, without the token. For status displays and logs.
func DiscoveryAddr(dataDir string) (string, error) {
	addr, _, err := readDiscovery(dataDir)
	return addr, err
}

// readDiscovery reads the daemon's listener address and auth token from the
// discovery file in dataDir. Returns net.ErrClosed if the file does not
// exist (callers use errors.Is to distinguish "no daemon" from "broken
// daemon").
func readDiscovery(dataDir string) (addr, token string, err error) {
	path := DiscoveryFilePath(dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", net.ErrClosed
		}
		return "", "", err
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 3)
	addr = strings.TrimSpace(lines[0])
	if addr == "" {
		return "", "", fmt.Errorf("rpc: empty discovery file at %s", path)
	}
	if len(lines) > 1 {
		token = strings.TrimSpace(lines[1])
	}
	return addr, token, nil
}

// writeDiscovery atomically records the listener address and token to
// dataDir's discovery file with 0600 perms.
func writeDiscovery(dataDir, addr, token string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	path := DiscoveryFilePath(dataDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(addr+"\n"+token+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeDiscovery deletes the discovery file. Best-effort.
func removeDiscovery(dataDir string) {
	_ = os.Remove(DiscoveryFilePath(dataDir))
}

// dialAddr opens a connection to addr using the right transport.
// addr is the string form returned by Listen and recorded in the
// discovery file:
//
//	unix:///abs/path/to/sock     — Unix domain socket
//	tcp://127.0.0.1:54321        — TCP loopback (Windows)
func dialAddr(addr string, timeout time.Duration) (net.Conn, error) {
	scheme, target, err := parseAddr(addr)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: timeout}
	return d.Dial(scheme, target)
}

// parseAddr splits a discovery address into (network, target) suitable
// for net.Dial. Unknown schemes return an error.
func parseAddr(addr string) (network, target string, err error) {
	switch {
	case strings.HasPrefix(addr, "unix://"):
		return "unix", strings.TrimPrefix(addr, "unix://"), nil
	case strings.HasPrefix(addr, "tcp://"):
		return "tcp", strings.TrimPrefix(addr, "tcp://"), nil
	default:
		return "", "", fmt.Errorf("rpc: unknown discovery address scheme: %q", addr)
	}
}
