//go:build unix

package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// maxUnixSocketPath is a conservative cap on the length of a Unix domain
// socket path. The kernel struct sockaddr_un.sun_path is 104 bytes on
// macOS/BSD and 108 on Linux (including the NUL terminator); 100 is safely
// under both. Paths longer than this fail bind() with EINVAL.
const maxUnixSocketPath = 100

// runtimeDirPerm is the mode the fallback directory must have: owner only.
const runtimeDirPerm = 0o700

// secureDiscoveryFile tightens the discovery file's access after it lands on
// its final name. On Unix this is a no-op: the 0600 mode bits written by
// os.WriteFile are enforced by the kernel, and the OS permission model is
// the primary gate for the whole data dir.
func secureDiscoveryFile(_ string) error { return nil }

// socketPath chooses where to bind the daemon socket. It prefers a socket
// inside dataDir (nice for `ls .graymatter`), but deeply nested project
// paths can blow past sun_path — so when the in-dir path is too long it
// falls back to a private directory under the user's runtime dir, named from
// a hash of the absolute data dir. The discovery file records whichever path
// we pick, so clients never need to recompute it.
//
// The fallback used to be a bare file in os.TempDir() with a fully predictable
// name. On a shared machine any local user could compute it and bind there
// first: the daemon then fails to start (denial of service), or, if a stale
// discovery file still points at that path, a client connects to the impostor
// and hands over its auth token. Owning the containing directory closes both.
func socketPath(dataDir string) (string, error) {
	inDir := filepath.Join(dataDir, "graymatter.sock")
	if len(inDir) <= maxUnixSocketPath {
		return inDir, nil
	}

	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	sum := sha256.Sum256([]byte(abs))

	dir, err := runtimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".sock"), nil
}

// runtimeDir returns a directory that only this user can enter, creating it if
// necessary, and refuses to hand back one that someone else owns or that is
// open to the world.
//
// XDG_RUNTIME_DIR is preferred where the session manager provides one: it is
// already per-user, already 0700, and cleaned up at logout. Otherwise a
// per-UID directory under the temp dir serves the same purpose.
func runtimeDir() (string, error) {
	uid := os.Getuid()

	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "graymatter-"+strconv.Itoa(uid))

	if err := os.MkdirAll(dir, runtimeDirPerm); err != nil {
		return "", fmt.Errorf("rpc: create runtime dir %s: %w", dir, err)
	}
	// MkdirAll is a no-op when the directory already exists, including when
	// someone else created it first with permissions of their choosing. That
	// is exactly the squatting case, so check rather than assume.
	if err := checkPrivateDir(dir, uid); err != nil {
		return "", err
	}
	return dir, nil
}

// checkPrivateDir verifies dir is a directory, owned by uid, and not
// accessible to group or other.
func checkPrivateDir(dir string, uid int) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("rpc: stat runtime dir %s: %w", dir, err)
	}
	// Symlink first: Lstat on a link reports IsDir() false, so checking that
	// ahead of this would answer "not a directory" for what is really someone
	// redirecting the socket somewhere they control.
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("rpc: runtime dir %s is a symlink; refusing to use it", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("rpc: runtime path %s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf(
			"rpc: runtime dir %s has mode %#o; it must not be readable or writable by others", dir, perm)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// No ownership information available. The mode check above already
		// rules out the open cases, so carry on rather than refuse to run.
		return nil
	}
	if int(st.Uid) != uid {
		return fmt.Errorf(
			"rpc: runtime dir %s is owned by uid %d, not %d; refusing to use it", dir, st.Uid, uid)
	}
	return nil
}

// Listen creates a listener for the daemon, writes a discovery file with the
// address and auth token, and returns the listener plus a cleanup func that
// removes both the discovery file and the socket. The socket is chmod 0600 so
// other local users cannot connect even where the data dir is world-readable.
func Listen(dataDir, token string) (net.Listener, func(), error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("rpc: create data dir: %w", err)
	}
	sockPath, err := socketPath(dataDir)
	if err != nil {
		return nil, nil, err
	}

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
