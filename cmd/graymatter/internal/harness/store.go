package harness

import (
	"context"
	"fmt"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

// Store is the persistence surface Run needs. It is deliberately the subset
// of operations a run performs, so the runner has exactly one code path
// regardless of whether the store is owned by the daemon (production) or
// opened in-process (tests, --no-daemon).
//
// The daemon client in package daemon satisfies this interface directly;
// LocalStore provides the in-process implementation.
type Store interface {
	Remember(ctx context.Context, agentID, text string) error
	RecallDefault(ctx context.Context, agentID, query string) ([]string, error)
	CheckpointSave(cp session.Checkpoint) (session.Checkpoint, error)
	CheckpointResume(agentID string) (*session.Checkpoint, error)
	SessionSave(hs HarnessSession) error
	TokenRecord(agent, model string, input, output, cacheRead, cacheWrite uint64) error
	Close() error
}

// LocalStore is the in-process Store backed by a graymatter.Memory and its
// bbolt handle. Used by tests and the --no-daemon path; the daemon owns the
// store in normal operation.
type LocalStore struct {
	mem *graymatter.Memory
}

// OpenLocalStore opens an in-process store at dataDir for a run. apiKey, when
// non-empty, overrides ANTHROPIC_API_KEY for embedding/consolidation.
func OpenLocalStore(dataDir, apiKey string) (*LocalStore, error) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dataDir
	if apiKey != "" {
		cfg.AnthropicAPIKey = apiKey
	}
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("open memory store: %w", err)
	}
	db := mem.Advanced().DB()
	if err := initHarnessBucket(db); err != nil {
		_ = mem.Close()
		return nil, fmt.Errorf("init harness bucket: %w", err)
	}
	_ = initTokenUsageBucket(db) // best-effort; accounting never breaks a run
	return &LocalStore{mem: mem}, nil
}

func (s *LocalStore) Remember(ctx context.Context, agentID, text string) error {
	return s.mem.Remember(ctx, agentID, text)
}

func (s *LocalStore) RecallDefault(ctx context.Context, agentID, query string) ([]string, error) {
	return s.mem.Recall(ctx, agentID, query)
}

func (s *LocalStore) CheckpointSave(cp session.Checkpoint) (session.Checkpoint, error) {
	return session.Save(s.mem.Advanced().DB(), cp)
}

func (s *LocalStore) CheckpointResume(agentID string) (*session.Checkpoint, error) {
	return session.Resume(s.mem.Advanced().DB(), agentID)
}

func (s *LocalStore) SessionSave(hs HarnessSession) error {
	return saveHarnessSession(s.mem.Advanced().DB(), hs)
}

func (s *LocalStore) TokenRecord(agent, model string, input, output, cacheRead, cacheWrite uint64) error {
	return RecordTokenUsage(s.mem.Advanced().DB(), agent, model, input, output, cacheRead, cacheWrite)
}

func (s *LocalStore) Close() error { return s.mem.Close() }
