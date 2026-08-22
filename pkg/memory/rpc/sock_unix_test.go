//go:build unix

package rpc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSocketPath_PrefersTheDataDir keeps the common case intact: a socket
// beside the store, where `ls .graymatter` shows it.
func TestSocketPath_PrefersTheDataDir(t *testing.T) {
	dir := t.TempDir()
	got, err := socketPath(dir)
	if err != nil {
		t.Fatalf("socketPath: %v", err)
	}
	if want := filepath.Join(dir, "graymatter.sock"); got != want {
		t.Errorf("socketPath = %q, want %q", got, want)
	}
}

// longDataDir returns a data dir path whose in-dir socket would exceed
// sun_path, forcing the fallback.
func longDataDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	deep := base
	for len(filepath.Join(deep, "graymatter.sock")) <= maxUnixSocketPath {
		deep = filepath.Join(deep, strings.Repeat("d", 20))
	}
	return deep
}

// TestSocketPath_FallbackLivesInAPrivateDir is the H-16 regression test. The
// fallback used to be a bare file in os.TempDir() with a fully predictable
// name, so any local user could bind there first — denying the daemon its
// socket, or standing in for it long enough to be handed a client's token.
func TestSocketPath_FallbackLivesInAPrivateDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	got, err := socketPath(longDataDir(t))
	if err != nil {
		t.Fatalf("socketPath: %v", err)
	}
	if len(got) > maxUnixSocketPath {
		t.Errorf("fallback path is %d bytes, over the %d sun_path budget: %s",
			len(got), maxUnixSocketPath, got)
	}

	parent := filepath.Dir(got)
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("stat fallback dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("fallback dir mode = %#o, want no group or other bits", perm)
	}
	if want := "graymatter-" + strconv.Itoa(os.Getuid()); filepath.Base(parent) != want {
		t.Errorf("fallback dir = %q, want it named %q", filepath.Base(parent), want)
	}
}

// TestSocketPath_RefusesAWorldWritableRuntimeDir covers the squat: MkdirAll is
// a no-op when the directory already exists, including when someone else made
// it first with permissions of their choosing.
func TestSocketPath_RefusesAWorldWritableRuntimeDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", base)

	squatted := filepath.Join(base, "graymatter-"+strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(squatted, 0o777); err != nil {
		t.Fatalf("mkdir squatted dir: %v", err)
	}
	// Mkdir applies the umask, so set the mode explicitly.
	if err := os.Chmod(squatted, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := socketPath(longDataDir(t)); err == nil {
		t.Fatal("socketPath accepted a world-writable runtime dir")
	} else if !strings.Contains(err.Error(), "mode") {
		t.Errorf("error = %v, want it to name the permissions problem", err)
	}
}

// TestSocketPath_RefusesASymlinkedRuntimeDir — a symlink is the other way to
// redirect the socket somewhere the attacker controls.
func TestSocketPath_RefusesASymlinkedRuntimeDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", base)

	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "graymatter-"+strconv.Itoa(os.Getuid()))
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := socketPath(longDataDir(t)); err == nil {
		t.Error("socketPath accepted a symlinked runtime dir")
	}
}

// TestSocketPath_FallbackIsStable — the discovery file records the path, but a
// path that changed between calls would still be a bug worth catching.
func TestSocketPath_FallbackIsStable(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dataDir := longDataDir(t)

	first, err := socketPath(dataDir)
	if err != nil {
		t.Fatalf("socketPath: %v", err)
	}
	second, err := socketPath(dataDir)
	if err != nil {
		t.Fatalf("socketPath: %v", err)
	}
	if first != second {
		t.Errorf("fallback path is not stable: %q then %q", first, second)
	}
}
