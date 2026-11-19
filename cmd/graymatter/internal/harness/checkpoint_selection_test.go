package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/anthropics/anthropic-sdk-go"
	bolt "go.etcd.io/bbolt"
)

type resumeContractStore struct {
	Store
	checkpoint   *session.Checkpoint
	resumeError  error
	resumedAgent string
	sessionSaves int
	recalls      int
}

func (s *resumeContractStore) CheckpointResume(agentID string) (*session.Checkpoint, error) {
	s.resumedAgent = agentID
	return s.checkpoint, s.resumeError
}

func (s *resumeContractStore) SessionSave(HarnessSession) error {
	s.sessionSaves++
	return nil
}

func (s *resumeContractStore) RecallDefault(context.Context, string, string) ([]string, error) {
	s.recalls++
	return nil, nil
}

func (*resumeContractStore) Remember(context.Context, string, string) error { return nil }

func (*resumeContractStore) CheckpointSave(cp session.Checkpoint) (session.Checkpoint, error) {
	return cp, nil
}

func (*resumeContractStore) TokenRecord(string, string, uint64, uint64, uint64, uint64) error {
	return nil
}

func TestRunResumeLatestFailureStopsBeforeWork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"corrupt JSON", &json.SyntaxError{}},
		{"storage", bolt.ErrDatabaseNotOpen},
		{"transport", io.ErrUnexpectedEOF},
		{"untyped absence", errors.New(session.ErrNoCheckpoint.Error())},
		{"nil checkpoint without error", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &resumeContractStore{resumeError: tc.cause}
			modelCalls := 0
			res, err := Run(context.Background(), RunConfig{
				AgentFile: agentFile(t, simpleAgentContent), DataDir: t.TempDir(),
				Store: store, ResumeID: "latest", MaxRetries: 1, Stdout: io.Discard, Stderr: io.Discard,
				llmDoer: func(context.Context, anthropic.MessageNewParams) (*anthropic.Message, error) {
					modelCalls++
					return cannedMessage("unexpected"), nil
				},
			})
			if res != nil || err == nil || !strings.Contains(err.Error(), "resume session") {
				t.Fatalf("Run = %+v, %v; want explicit resume failure", res, err)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("Run error = %v; want %v preserved", err, tc.cause)
			}
			if modelCalls != 0 || store.recalls != 0 || store.sessionSaves != 0 {
				t.Fatalf("failed resume started work: model=%d recall=%d session writes=%d", modelCalls, store.recalls, store.sessionSaves)
			}
		})
	}
}

func TestRunResumeLatestRestoresMessagesOnly(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cp          *session.Checkpoint
		cause       error
		wantHistory bool
	}{
		{"typed absence starts fresh", nil, fmt.Errorf("store: %w", session.ErrNoCheckpoint), false},
		{"state-only MCP checkpoint", &session.Checkpoint{ID: "state", State: map[string]any{"goal": "private-state-only"}}, nil, false},
		{"conversation checkpoint", &session.Checkpoint{
			ID: "history", State: map[string]any{"goal": "private-state-only"},
			Messages: []session.Message{{Role: "user", Content: "prior-user"}, {Role: "assistant", Content: "prior-reply"}},
		}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &resumeContractStore{checkpoint: tc.cp, resumeError: tc.cause}
			var request anthropic.MessageNewParams
			modelCalls := 0
			_, err := Run(context.Background(), RunConfig{
				AgentFile: agentFile(t, simpleAgentContent), DataDir: t.TempDir(),
				Store: store, ResumeID: "latest", MaxRetries: 1, Stdout: io.Discard, Stderr: io.Discard,
				llmDoer: func(_ context.Context, p anthropic.MessageNewParams) (*anthropic.Message, error) {
					request = p
					modelCalls++
					return cannedMessage("reply"), nil
				},
			})
			if err != nil || modelCalls != 1 || store.resumedAgent != "test-runner-agent" {
				t.Fatalf("Run error=%v model=%d namespace=%q", err, modelCalls, store.resumedAgent)
			}
			history := messageHistoryJSON(t, request)
			if strings.Contains(history, "private-state-only") || !strings.Contains(history, "Say hello.") {
				t.Fatalf("unexpected prompt history: %s", history)
			}
			for _, turn := range []string{"prior-user", "prior-reply"} {
				if strings.Contains(history, turn) != tc.wantHistory {
					t.Fatalf("prompt history missing expected status for %q: %s", turn, history)
				}
			}
		})
	}
}
