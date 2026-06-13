//go:build windows

package rpc

import (
	"fmt"
	"net"
	"os"
)

// Listen creates a listener for the daemon, writes a discovery file with the
// address and auth token, and returns the listener plus a cleanup func to
// remove the discovery file. On Windows, native Unix domain sockets are not
// portably available from the standard library, so we bind TCP loopback on a
// kernel-assigned port. The loopback interface is reachable by every local
// process, which is why the auth token in the 0600 discovery file is
// mandatory here — connections that fail the token preamble are dropped
// before any RPC dispatch (see Server.authenticate).
func Listen(dataDir, token string) (net.Listener, func(), error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("rpc: create data dir: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("rpc: listen tcp 127.0.0.1: %w", err)
	}

	addr := "tcp://" + ln.Addr().String()
	if err := writeDiscovery(dataDir, addr, token); err != nil {
		_ = ln.Close()
		return nil, nil, fmt.Errorf("rpc: write discovery: %w", err)
	}

	cleanup := func() {
		removeDiscovery(dataDir)
	}
	return ln, cleanup, nil
}
