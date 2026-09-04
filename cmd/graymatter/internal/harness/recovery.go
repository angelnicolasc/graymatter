package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	bolt "go.etcd.io/bbolt"
)

// ListSessions returns all HarnessSession records for dataDir, sorted newest
// first. It opens the bbolt database read-only so it is safe to call while a
// background agent holds the write lock.
func ListSessions(dataDir string) ([]HarnessSession, error) {
	db, err := openReadOnly(dataDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	return listHarnessSessions(db)
}

// ListSessionsDB returns all HarnessSession records from an already-open db
// handle. Use this from processes that already hold the write lock (like the
// TUI) — on Windows, bbolt refuses a second open, even read-only.
func ListSessionsDB(db *bolt.DB) ([]HarnessSession, error) {
	if db == nil {
		return nil, fmt.Errorf("nil db")
	}
	return listHarnessSessions(db)
}

// KillSession sends a termination signal to the background process recorded
// in the HarnessSession for sessionID, then marks its status as "killed".
// It opens its own write handle on the store; processes that already hold
// one (the daemon) must use KillSessionDB instead.
//
// Returns an error if:
//   - the session does not exist
//   - the session is not in "running" status
//   - no PID is recorded (non-background run)
//   - the OS signal fails
func KillSession(sessionID, dataDir string) error {
	db, err := openDB(dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return KillSessionDB(db, sessionID)
}

// KillSessionDB is KillSession against an already-open db handle.
func KillSessionDB(db *bolt.DB, sessionID string) error {
	if err := initHarnessBucket(db); err != nil {
		return fmt.Errorf("init harness bucket: %w", err)
	}

	hs, err := loadHarnessSession(db, sessionID)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	if hs.Status != "running" {
		return fmt.Errorf("session %q is not running (status: %s)", sessionID, hs.Status)
	}
	if hs.PID == 0 {
		return fmt.Errorf("session %q has no PID — it was not started in background mode", sessionID)
	}
	if err := confirmOurProcess(db, sessionID, hs.PID, hs.PIDStart); err != nil {
		return err
	}

	if err := killPID(hs.PID); err != nil {
		return fmt.Errorf("kill session %q (pid %d): %w", sessionID, hs.PID, err)
	}

	// Mark as killed.
	now := time.Now().UTC()
	hs.Status = "killed"
	hs.FinishedAt = &now
	return saveHarnessSession(db, *hs)
}

// confirmOurProcess checks that pid is one graymatter actually spawned for
// sessionID, by matching its PID file and platform-derived process-start identity.
//
// Without this, the kill target is whatever number happens to sit in a session
// record — and session records can be written through the authenticated RPC
// surface (SessionSave). Save a record with someone else's PID and status
// "running", call SessionKill, and the daemon terminates that process on your
// behalf. The PID file narrows the primitive to processes this tool started.
// The start identity also rejects stale records after the OS recycles a PID.
//
// It is not a defence against a process already running as the same user: that
// process can write both the record and the file. It is a defence against the
// RPC surface being a kill primitive on its own, which is a different thing.
func confirmOurProcess(db *bolt.DB, sessionID string, pid int, recordedStart int64) error {
	if pid == os.Getpid() {
		return fmt.Errorf("session %q names this process (pid %d); refusing to kill it", sessionID, pid)
	}
	if recordedStart == 0 {
		return fmt.Errorf(
			"session %q predates process-identity hardening and has no process start time; "+
				"refusing to kill pid %d: stop it manually only after verifying its identity",
			sessionID, pid)
	}
	if recordedStart < 0 {
		return fmt.Errorf(
			"session %q could not record a process start time; refusing to kill pid %d: "+
				"stop it manually only after verifying its identity",
			sessionID, pid)
	}

	// gray.db sits at the root of the data dir, which is where run/ lives too.
	dataDir := filepath.Dir(db.Path())
	pidPath := PIDFilePath(dataDir, sessionID)

	onDisk, err := ReadPIDFile(pidPath)
	if err != nil {
		return fmt.Errorf(
			"session %q: no PID file at %s, so pid %d is recorded only in the database; "+
				"refusing to kill a process graymatter cannot confirm it started: %w",
			sessionID, pidPath, pid, err)
	}
	if onDisk != pid {
		return fmt.Errorf(
			"session %q: the database says pid %d but %s says %d; refusing to kill either",
			sessionID, pid, pidPath, onDisk)
	}
	liveStart, err := processStartTime(pid)
	if err != nil {
		return fmt.Errorf(
			"session %q: cannot verify the start time of pid %d; refusing to kill it: %w",
			sessionID, pid, err)
	}
	if liveStart != recordedStart {
		return fmt.Errorf(
			"session %q: pid %d was recycled (recorded start time %d, live start time %d); refusing to kill it",
			sessionID, pid, recordedStart, liveStart)
	}
	return nil
}

// SaveSessionDB persists a HarnessSession record against an already-open db
// handle, creating the harness bucket if needed. Used by the daemon host
// service; in-process callers go through Run's own bookkeeping.
func SaveSessionDB(db *bolt.DB, hs HarnessSession) error {
	if err := initHarnessBucket(db); err != nil {
		return fmt.Errorf("init harness bucket: %w", err)
	}
	return saveHarnessSession(db, hs)
}

// ResolveSessionIDDB is ResolveSessionID against an already-open db handle.
func ResolveSessionIDDB(db *bolt.DB, agentID, sessionID string) (string, error) {
	return resolveSessionID(db, agentID, sessionID)
}

func (s *LocalStore) CheckpointLoad(agentID, checkpointID string) (*session.Checkpoint, error) {
	return session.Load(s.mem.Advanced().DB(), agentID, checkpointID)
}

func (s *LocalStore) SessionsList() ([]HarnessSession, error) {
	return listHarnessSessions(s.mem.Advanced().DB())
}

func (s *LocalStore) SessionResolve(agentID, sessionID string) (string, error) {
	return resolveSessionID(s.mem.Advanced().DB(), agentID, sessionID)
}

// Resume looks up the HarnessSession for sessionID (or "latest" for the most
// recent session), reads its AgentFile and Inputs, and returns a RunConfig
// ready to pass to Run. The caller is responsible for calling Run.
//
// Library callers may use this entry point after a restart. Run's CLI path uses
// its already-open Store to resolve the target HarnessSession.LastCPID.
func Resume(_ context.Context, sessionID, dataDir string) (*RunConfig, error) {
	db, err := openReadOnly(dataDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	sessions, err := listHarnessSessions(db)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	var target *HarnessSession
	if sessionID == "latest" {
		if len(sessions) == 0 {
			return nil, fmt.Errorf("no sessions found in %q", dataDir)
		}
		target = &sessions[0]
	} else {
		for i := range sessions {
			if sessions[i].ID == sessionID {
				target = &sessions[i]
				break
			}
		}
		if target == nil {
			return nil, fmt.Errorf("session %q not found", sessionID)
		}
	}

	return &RunConfig{
		AgentFile: target.AgentFile,
		Inputs:    target.Inputs,
		DataDir:   dataDir,
		ResumeID:  target.ID,
	}, nil
}

// StreamLogs writes the contents of the session log file to out, then returns.
// It is used by "graymatter sessions logs <id>".
func StreamLogs(sessionID, dataDir string, out interface{ Write([]byte) (int, error) }) error {
	db, err := openReadOnly(dataDir)
	if err != nil {
		return err
	}
	hs, loadErr := loadHarnessSession(db, sessionID)
	_ = db.Close()
	if loadErr != nil {
		return fmt.Errorf("load session: %w", loadErr)
	}
	if hs.LogFile == "" {
		return fmt.Errorf("session %q was not started in background mode (no log file)", sessionID)
	}
	data, err := ReadSessionLog(dataDir, hs.LogFile)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}

// ResolveSessionID resolves "latest" to the most recent session ID for agentID,
// or validates that a concrete ID exists. Returns the concrete session ID.
func ResolveSessionID(dataDir, agentID, sessionID string) (string, error) {
	db, err := openReadOnly(dataDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	return resolveSessionID(db, agentID, sessionID)
}

// resolveSessionID is the unexported core used by Run and CLI commands.
func resolveSessionID(db *bolt.DB, agentID, sessionID string) (string, error) {
	sessions, err := listHarnessSessions(db)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}

	if sessionID == "latest" {
		// Find the most recent session for this agentID.
		for _, s := range sessions { // already sorted newest-first
			if s.AgentID == agentID || agentID == "" {
				return s.ID, nil
			}
		}
		return "", fmt.Errorf("no sessions found for agent %q", agentID)
	}

	// Validate the concrete ID exists.
	for _, s := range sessions {
		if s.ID == sessionID {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("session %q not found", sessionID)
}

// PIDFilePath returns the canonical path for the PID file of sessionID.
func PIDFilePath(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "run", sessionID+".pid")
}

// LogFilePath returns the canonical path for the log file of sessionID.
func LogFilePath(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "logs", sessionID+".log")
}

// ReadSessionLog returns the contents of a session's log file, but only if
// that file really lives under <dataDir>/logs.
//
// The path is read back out of bbolt, where it was put by whichever process
// launched the session. spawnBackground always writes a path inside logs/, so
// requiring one here costs nothing — but without the check, anything that can
// write a session record (the authenticated RPC surface, or a hand-edited
// database) turns `graymatter sessions logs` into an arbitrary-file reader
// running with the user's own permissions.
//
// Containment is checked on cleaned absolute paths. Symlinks are not resolved:
// planting one inside logs/ already requires write access to the data dir.
func ReadSessionLog(dataDir, logFile string) ([]byte, error) {
	if logFile == "" {
		return nil, fmt.Errorf("session has no log file")
	}

	logsDir, err := filepath.Abs(filepath.Join(dataDir, "logs"))
	if err != nil {
		return nil, fmt.Errorf("resolve logs dir: %w", err)
	}
	abs, err := filepath.Abs(logFile)
	if err != nil {
		return nil, fmt.Errorf("resolve log path %q: %w", logFile, err)
	}
	if abs == logsDir || !strings.HasPrefix(abs, logsDir+string(os.PathSeparator)) {
		return nil, fmt.Errorf(
			"log path %q is outside %s; refusing to read it", logFile, logsDir)
	}

	data, err := os.ReadFile(abs) //nolint:gosec // contained above
	if err != nil {
		return nil, fmt.Errorf("read log file %q: %w", logFile, err)
	}
	return data, nil
}

// ReadPIDFile reads the PID from a PID file written by spawnBackground.
func ReadPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse PID from %q: %w", path, err)
	}
	return pid, nil
}

// SortSessionsNewestFirst sorts sessions by StartedAt descending in place.
func SortSessionsNewestFirst(sessions []HarnessSession) {
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt.After(sessions[j].StartedAt)
	})
}

// openDB opens the gray.db with write access, creating it if needed.
func openDB(dataDir string) (*bolt.DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dbPath := filepath.Join(dataDir, "gray.db")
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open gray.db: %w", err)
	}
	return db, nil
}

// openReadOnly opens gray.db in read-only mode.
// Safe to call while another process holds the write lock.
func openReadOnly(dataDir string) (*bolt.DB, error) {
	dbPath := filepath.Join(dataDir, "gray.db")
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{
		ReadOnly: true,
		Timeout:  2 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("open gray.db (read-only): %w", err)
	}
	return db, nil
}
