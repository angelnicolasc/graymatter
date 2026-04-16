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

	if err := killPID(hs.PID); err != nil {
		return fmt.Errorf("kill session %q (pid %d): %w", sessionID, hs.PID, err)
	}

	// Mark as killed.
	now := time.Now().UTC()
	hs.Status = "killed"
	hs.FinishedAt = &now
	return saveHarnessSession(db, *hs)
}

// Resume looks up the HarnessSession for sessionID (or "latest" for the most
// recent session), reads its AgentFile and Inputs, and returns a RunConfig
// ready to pass to Run. The caller is responsible for calling Run.
//
// This is the primary entry point for resuming a session that was interrupted
// by a machine restart — no in-memory state is assumed.
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
	data, err := os.ReadFile(hs.LogFile)
	if err != nil {
		return fmt.Errorf("read log file %q: %w", hs.LogFile, err)
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
