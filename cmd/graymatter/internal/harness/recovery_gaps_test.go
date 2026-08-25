package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Recovery-surface gaps: streaming a session's log back out and resolving
// session IDs against edge cases. The happy paths of Resume/List are covered
// elsewhere; these pin what happens when records reference files that do not
// exist, were never captured, or live outside the logs directory.

func seedSessionWithLog(t *testing.T, dataDir, logFile string) {
	t.Helper()
	db := killTestDB(t, dataDir)
	hs := HarnessSession{ID: "sess-log", AgentID: "agent", AgentFile: "a.md", Status: "done", LogFile: logFile}
	if err := SaveSessionDB(db, hs); err != nil {
		t.Fatal(err)
	}
	// The writer must release its lock: StreamLogs opens read-only, and a
	// held bbolt lock makes even read-only opens time out.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStreamLogs_WritesTheCapturedLog(t *testing.T) {
	dir := t.TempDir()
	// Production stores the absolute canonical path from LogFilePath.
	logPath := LogFilePath(dir, "sess-log")
	seedSessionWithLog(t, dir, logPath)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := StreamLogs("sess-log", dir, &buf); err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	if buf.String() != "line one\nline two\n" {
		t.Errorf("streamed %q", buf.String())
	}
}

func TestStreamLogs_SessionWithoutLogFileErrors(t *testing.T) {
	dir := t.TempDir()
	db := killTestDB(t, dir)
	if err := SaveSessionDB(db, HarnessSession{ID: "foreground", AgentID: "a", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	var buf bytes.Buffer
	err := StreamLogs("foreground", dir, &buf)
	if err == nil || !strings.Contains(err.Error(), "no log file") {
		t.Errorf("err = %v, want explicit foreground-session error", err)
	}
}

func TestStreamLogs_UnknownSessionErrors(t *testing.T) {
	dir := t.TempDir()
	db := killTestDB(t, dir)
	_ = db.Close()

	var buf bytes.Buffer
	err := StreamLogs("ghost", dir, &buf)
	if err == nil || !strings.Contains(err.Error(), "load session") {
		t.Errorf("err = %v, want load failure", err)
	}
}

func TestStreamLogs_LogOutsideLogsDirIsRefused(t *testing.T) {
	dir := t.TempDir()
	db := killTestDB(t, dir)
	outside := filepath.Join(dir, "secrets.txt")
	if err := os.WriteFile(outside, []byte("sensitive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionDB(db, HarnessSession{
		ID: "evil", AgentID: "a", Status: "done",
		LogFile: filepath.Join(dir, "secrets.txt"),
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	var buf bytes.Buffer
	err := StreamLogs("evil", dir, &buf)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("err = %v, want containment refusal", err)
	}
}

func TestStreamLogs_MissingLogFileOnDiskErrors(t *testing.T) {
	dir := t.TempDir()
	logPath := LogFilePath(dir, "sess-log")
	seedSessionWithLog(t, dir, logPath)
	var buf bytes.Buffer
	err := StreamLogs("sess-log", dir, &buf)
	if err == nil || !strings.Contains(err.Error(), "read log file") {
		t.Errorf("err = %v, want missing-file error naming the log", err)
	}
}

func TestResolveSessionIDDB_EdgeCases(t *testing.T) {
	dir := t.TempDir()
	db := killTestDB(t, dir)
	sessions := []HarnessSession{
		{ID: "s2", AgentID: "beta", Status: "done"},
		{ID: "s1", AgentID: "alpha", Status: "done"},
	}
	SortSessionsNewestFirst(sessions)
	for _, hs := range sessions {
		if err := SaveSessionDB(db, hs); err != nil {
			t.Fatal(err)
		}
	}

	// Concrete ID that exists resolves regardless of agent.
	id, err := ResolveSessionIDDB(db, "", "s1")
	if err != nil || id != "s1" {
		t.Errorf("concrete resolve = %q, %v", id, err)
	}
	// latest scoped to an agent with no sessions errors with the agent named.
	_, err = ResolveSessionIDDB(db, "gamma", "latest")
	if err == nil || !strings.Contains(err.Error(), "gamma") {
		t.Errorf("err = %v, want agent-scoped miss", err)
	}
	// Concrete unknown ID errors.
	_, err = ResolveSessionIDDB(db, "alpha", "nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want not-found", err)
	}
}

func TestSortSessionsNewestFirst_OrdersByStartedAtDesc(t *testing.T) {
	base := time.Now()
	in := []HarnessSession{
		{ID: "old", StartedAt: base.Add(-2 * time.Hour)},
		{ID: "newest", StartedAt: base},
		{ID: "mid", StartedAt: base.Add(-time.Hour)},
	}
	SortSessionsNewestFirst(in)
	if in[0].ID != "newest" || in[2].ID != "old" {
		t.Errorf("order = %s,%s,%s", in[0].ID, in[1].ID, in[2].ID)
	}
}
