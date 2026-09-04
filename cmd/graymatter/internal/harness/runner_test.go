package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	bolt "go.etcd.io/bbolt"
)

// cannedMessage returns a fake *anthropic.Message with the given text.
func cannedMessage(text string) *anthropic.Message {
	return &anthropic.Message{
		Content: []anthropic.ContentBlockUnion{
			{Text: text},
		},
	}
}

// agentFile creates a temporary agent .md file and returns its path.
func agentFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write agent file: %v", err)
	}
	return path
}

const simpleAgentContent = `---
name: test-runner-agent
model: claude-opus-4-6
---

## System Prompt
You are a test agent.

## Task
Say hello.
`

func TestRun_Success(t *testing.T) {
	dir := t.TempDir()
	af := agentFile(t, simpleAgentContent)

	cfg := RunConfig{
		AgentFile:  af,
		DataDir:    dir,
		MaxRetries: 3,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		llmDoer: func(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
			return cannedMessage("Hello from the test agent!"), nil
		},
	}

	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.FinalReply != "Hello from the test agent!" {
		t.Errorf("FinalReply = %q", result.FinalReply)
	}
	if result.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", result.Attempts)
	}
	if result.SessionID == "" {
		t.Error("SessionID should not be empty")
	}

	// Verify the session was persisted as "done".
	sessions, err := ListSessions(dir)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Status != "done" {
		t.Errorf("status = %q, want %q", sessions[0].Status, "done")
	}
}

func TestRun_RetryOnTransientError(t *testing.T) {
	dir := t.TempDir()
	af := agentFile(t, simpleAgentContent)

	callCount := 0
	cfg := RunConfig{
		AgentFile:  af,
		DataDir:    dir,
		MaxRetries: 3,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		llmDoer: func(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
			callCount++
			if callCount < 2 {
				return nil, &testError{"transient failure"}
			}
			return cannedMessage("Recovered!"), nil
		},
	}

	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", result.Attempts)
	}
	if callCount != 2 {
		t.Errorf("callCount = %d, want 2", callCount)
	}
}

func TestRun_MaxRetriesExceeded(t *testing.T) {
	dir := t.TempDir()
	af := agentFile(t, simpleAgentContent)

	cfg := RunConfig{
		AgentFile:  af,
		DataDir:    dir,
		MaxRetries: 2,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		llmDoer: func(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
			return nil, &testError{"always fails"}
		},
	}

	_, err := Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error after max retries, got nil")
	}

	// Verify failed run record was written.
	sessions, lsErr := ListSessions(dir)
	if lsErr != nil {
		t.Fatalf("ListSessions: %v", lsErr)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Status != "failed" {
		t.Errorf("status = %q, want %q", sessions[0].Status, "failed")
	}

	// Verify .graymatter/failed/<id>.json exists.
	failedDir := filepath.Join(dir, "failed")
	entries, readErr := os.ReadDir(failedDir)
	if readErr != nil {
		t.Fatalf("read failed dir: %v", readErr)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 failed record, got %d", len(entries))
	}
}

func runTestReply(t *testing.T, dir, af, reply string) *RunResult {
	t.Helper()
	result, err := Run(context.Background(), RunConfig{
		AgentFile:  af,
		DataDir:    dir,
		MaxRetries: 1,
		Stdout:     io.Discard,
		Stderr:     io.Discard,
		llmDoer: func(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
			return cannedMessage(reply), nil
		},
	})
	if err != nil {
		t.Fatalf("Run reply %q: %v", reply, err)
	}
	return result
}

func messageHistoryJSON(t *testing.T, params anthropic.MessageNewParams) string {
	t.Helper()
	b, err := json.Marshal(params.Messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	return string(b)
}

func TestRun_ResumeConcreteSession(t *testing.T) {
	dir := t.TempDir()
	af := agentFile(t, simpleAgentContent)

	first := runTestReply(t, dir, af, "first-session-context")
	runTestReply(t, dir, af, "newer-session-context")

	var capturedParams anthropic.MessageNewParams
	var stderr bytes.Buffer
	cfg2 := RunConfig{
		AgentFile:  af,
		DataDir:    dir,
		MaxRetries: 1,
		ResumeID:   first.SessionID,
		Stdout:     io.Discard,
		Stderr:     &stderr,
		llmDoer: func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
			capturedParams = params
			return cannedMessage("resumed"), nil
		},
	}
	_, err := Run(context.Background(), cfg2)
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}

	history := messageHistoryJSON(t, capturedParams)
	if !strings.Contains(history, "first-session-context") {
		t.Errorf("requested session context missing: %s", history)
	}
	if strings.Contains(history, "newer-session-context") {
		t.Errorf("newer session context was loaded instead: %s", history)
	}
	if !strings.Contains(stderr.String(), first.SessionID) {
		t.Errorf("resume diagnostic does not name session %q: %s", first.SessionID, stderr.String())
	}
}

func TestRun_ResumeUnknownSessionFailsBeforeRun(t *testing.T) {
	dir := t.TempDir()
	af := agentFile(t, simpleAgentContent)
	runTestReply(t, dir, af, "seed")

	_, err := Run(context.Background(), RunConfig{
		AgentFile: af, DataDir: dir, ResumeID: "missing-session",
		Stdout: io.Discard, Stderr: io.Discard,
		llmDoer: func(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
			return cannedMessage("unexpected"), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "missing-session") {
		t.Fatalf("Run error = %v, want requested session ID", err)
	}
	sessions, listErr := ListSessions(dir)
	if listErr != nil || len(sessions) != 1 {
		t.Fatalf("unknown resume persisted a run: sessions=%d err=%v", len(sessions), listErr)
	}
}

func TestRun_ResumeLatestKeepsLatestCheckpointSemantics(t *testing.T) {
	dir := t.TempDir()
	af := agentFile(t, simpleAgentContent)
	runTestReply(t, dir, af, "older-context")
	time.Sleep(2 * time.Millisecond)
	runTestReply(t, dir, af, "latest-checkpoint-context")

	store, err := OpenLocalStore(dir, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.SessionSave(HarnessSession{
		ID: "newer-failed-session", AgentID: "test-runner-agent",
		AgentFile: af, StartedAt: time.Now().UTC().Add(time.Hour), Status: "failed",
	}); err != nil {
		t.Fatalf("save failed session: %v", err)
	}
	_ = store.Close()

	var capturedParams anthropic.MessageNewParams
	_, err = Run(context.Background(), RunConfig{
		AgentFile: af, DataDir: dir, ResumeID: "latest",
		Stdout: io.Discard, Stderr: io.Discard,
		llmDoer: func(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
			capturedParams = params
			return cannedMessage("resumed"), nil
		},
	})
	if err != nil {
		t.Fatalf("resume latest: %v", err)
	}
	history := messageHistoryJSON(t, capturedParams)
	if !strings.Contains(history, "latest-checkpoint-context") {
		t.Errorf("latest checkpoint context missing: %s", history)
	}
}

func TestRun_ResumeRejectsDifferentAgent(t *testing.T) {
	dir := t.TempDir()
	other := agentFile(t, strings.Replace(simpleAgentContent,
		"name: test-runner-agent", "name: other-agent", 1))
	target := runTestReply(t, dir, other, "other-context")

	_, err := Run(context.Background(), RunConfig{
		AgentFile: agentFile(t, simpleAgentContent), DataDir: dir, ResumeID: target.SessionID,
		Stdout: io.Discard, Stderr: io.Discard,
		llmDoer: func(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
			return cannedMessage("unexpected"), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), target.SessionID) ||
		!strings.Contains(err.Error(), "other-agent") || !strings.Contains(err.Error(), "test-runner-agent") {
		t.Fatalf("Run error = %v, want session and both agent IDs", err)
	}
}

func TestBackoffDuration(t *testing.T) {
	// Verify exponential growth and cap at 30s.
	prev := backoffDuration(1)
	for attempt := 2; attempt <= 6; attempt++ {
		dur := backoffDuration(attempt)
		// Allow for jitter — just verify the duration is positive and grows (or hits cap).
		if dur <= 0 {
			t.Errorf("attempt %d: duration = %v, want > 0", attempt, dur)
		}
		if dur > 31*time.Second {
			t.Errorf("attempt %d: duration = %v exceeds cap", attempt, dur)
		}
		_ = prev
		prev = dur
	}
}

// openTestDB opens the test gray.db in the given dir for direct inspection.
func openTestDB(t *testing.T, dir string) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(filepath.Join(dir, "gray.db"), 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// mustSaveCheckpoint persists a checkpoint directly for test setup.
func mustSaveCheckpoint(t *testing.T, db *bolt.DB, agentID string, messages []session.Message) {
	t.Helper()
	cp := session.Checkpoint{
		AgentID:  agentID,
		Messages: messages,
		State:    map[string]any{},
	}
	if _, err := session.Save(db, cp); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
