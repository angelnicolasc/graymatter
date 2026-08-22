package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// killTestDB opens a bbolt file inside dataDir, the way the real code does, so
// KillSessionDB can find the run/ directory next to it.
func killTestDB(t *testing.T, dataDir string) *bolt.DB {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	db, err := bolt.Open(filepath.Join(dataDir, "gray.db"), 0o600,
		&bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func saveRunning(t *testing.T, db *bolt.DB, id string, pid int) {
	t.Helper()
	if err := SaveSessionDB(db, HarnessSession{
		ID:        id,
		AgentID:   "victim-agent",
		Status:    "running",
		PID:       pid,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}
}

// startSleeper launches a real, harmless child process and returns its PID.
// It is the stand-in for "some other process on this machine".
func startSleeper(t *testing.T) int {
	t.Helper()

	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd.exe", "/c", "ping -n 30 127.0.0.1 > NUL")
	} else {
		c = exec.Command("sleep", "30")
	}
	if err := c.Start(); err != nil {
		t.Skipf("cannot start a helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	})
	return c.Process.Pid
}

// TestKillSessionDB_RefusesPIDsWithoutAPIDFile is the H-17 regression test.
// Session records can be written through the authenticated RPC surface
// (SessionSave), so a record naming someone else's PID used to turn
// SessionKill into "terminate that process for me".
func TestKillSessionDB_RefusesPIDsWithoutAPIDFile(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), ".graymatter")
	db := killTestDB(t, dataDir)

	victim := startSleeper(t)
	saveRunning(t, db, "01PLANTED", victim)

	err := KillSessionDB(db, "01PLANTED")
	if err == nil {
		t.Fatal("KillSessionDB killed a PID that graymatter never spawned")
	}
	if !strings.Contains(err.Error(), "PID file") {
		t.Errorf("error = %v, want it to explain the missing PID file", err)
	}

	// And the process is still alive.
	if !processAlive(victim) {
		t.Errorf("the unrelated process (pid %d) was terminated anyway", victim)
	}
}

// TestKillSessionDB_RefusesMismatchedPIDFile covers the swap: a real session
// exists, but the database record was rewritten to point at something else.
func TestKillSessionDB_RefusesMismatchedPIDFile(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), ".graymatter")
	db := killTestDB(t, dataDir)

	victim := startSleeper(t)
	saveRunning(t, db, "01SWAPPED", victim)

	// graymatter's own record of what it spawned says a different PID.
	writePIDFile(t, dataDir, "01SWAPPED", victim+100000)

	err := KillSessionDB(db, "01SWAPPED")
	if err == nil {
		t.Fatal("KillSessionDB killed a PID the PID file disagreed with")
	}
	if !strings.Contains(err.Error(), "refusing to kill either") {
		t.Errorf("error = %v, want it to refuse both candidates", err)
	}
	if !processAlive(victim) {
		t.Errorf("the unrelated process (pid %d) was terminated anyway", victim)
	}
}

// TestKillSessionDB_RefusesToKillItself — the daemon writing its own PID into
// a session record should not be able to take itself down through this path.
func TestKillSessionDB_RefusesToKillItself(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), ".graymatter")
	db := killTestDB(t, dataDir)

	self := os.Getpid()
	saveRunning(t, db, "01SELF", self)
	writePIDFile(t, dataDir, "01SELF", self)

	err := KillSessionDB(db, "01SELF")
	if err == nil {
		t.Fatal("KillSessionDB agreed to kill the current process")
	}
	if !strings.Contains(err.Error(), "this process") {
		t.Errorf("error = %v, want it to say so plainly", err)
	}
}

// TestKillSessionDB_KillsRealSessions guards the happy path: the check must
// not break the command it protects.
func TestKillSessionDB_KillsRealSessions(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), ".graymatter")
	db := killTestDB(t, dataDir)

	pid := startSleeper(t)
	saveRunning(t, db, "01REAL", pid)
	writePIDFile(t, dataDir, "01REAL", pid)

	if err := KillSessionDB(db, "01REAL"); err != nil {
		t.Fatalf("KillSessionDB on a genuine session: %v", err)
	}

	sessions, err := ListSessionsDB(db)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	for _, s := range sessions {
		if s.ID == "01REAL" && s.Status != "killed" {
			t.Errorf("status = %q after kill, want killed", s.Status)
		}
	}
}

func writePIDFile(t *testing.T, dataDir, sessionID string, pid int) {
	t.Helper()
	path := PIDFilePath(dataDir, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
}

// processAlive reports whether pid still exists. Signal 0 is the portable-ish
// probe on Unix; on Windows FindProcess alone does not prove liveness, so this
// stays a best-effort check that never fails the test spuriously.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true // cannot probe cheaply; the assertions above carry the test
	}
	return p.Signal(nil) == nil
}
