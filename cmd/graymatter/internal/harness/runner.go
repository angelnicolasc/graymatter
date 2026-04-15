package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/oklog/ulid/v2"
	bolt "go.etcd.io/bbolt"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

var bucketHarness = []byte("harness_sessions")

// HarnessSession is the canonical run-metadata record persisted in the
// harness_sessions bbolt bucket. One record per agent run.
type HarnessSession struct {
	ID         string            `json:"id"`
	AgentID    string            `json:"agent_id"`
	AgentFile  string            `json:"agent_file"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`
	Status     string            `json:"status"` // running|done|failed|killed
	PID        int               `json:"pid,omitempty"`
	LogFile    string            `json:"log_file,omitempty"`
	Inputs     map[string]string `json:"inputs,omitempty"`
	LastCPID   string            `json:"last_checkpoint_id,omitempty"`
	Attempts   int               `json:"attempts"`
	ErrorMsg   string            `json:"error_msg,omitempty"`
}

// RunConfig is the complete set of parameters for a single agent run.
type RunConfig struct {
	// AgentFile is the path to the SKILL.md-format agent definition file.
	AgentFile string

	// Inputs is the map of variable substitutions (e.g. {"task": "follow up Maria"}).
	Inputs map[string]string

	// DataDir is the GrayMatter data directory. Default: ".graymatter"
	DataDir string

	// MaxRetries is the maximum number of LLM call attempts. Default: 3
	MaxRetries int

	// ResumeID is a harness session ID or "latest". When non-empty, the prior
	// message history is loaded from the latest checkpoint and prepended.
	ResumeID string

	// Stdout receives the final agent reply. Default: os.Stdout
	Stdout io.Writer

	// Stderr receives diagnostic messages (retry counts, etc). Default: os.Stderr
	Stderr io.Writer

	// APIKey overrides ANTHROPIC_API_KEY. If empty, env var is used.
	APIKey string

	// llmDoer replaces the real Anthropic API call. Injected by tests only.
	// Production code leaves this nil.
	llmDoer func(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error)
}

// RunResult is returned by Run on success.
type RunResult struct {
	SessionID  string
	Messages   []session.Message
	FinalReply string
	Attempts   int
}

// Run executes the agent described by cfg.AgentFile.
//
// It injects prior memory into the system prompt, calls the Anthropic API with
// exponential-backoff retries, checkpoints after each successful response, and
// persists observations to the memory store.
//
// On total failure the run state is written to <DataDir>/failed/<sessionID>.json
// and an error is returned. Run never calls panic() or os.Exit().
func Run(ctx context.Context, cfg RunConfig) (*RunResult, error) {
	// Apply defaults.
	if cfg.DataDir == "" {
		cfg.DataDir = ".graymatter"
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}

	// Parse agent file.
	def, err := ParseAgentFile(cfg.AgentFile, cfg.Inputs)
	if err != nil {
		return nil, fmt.Errorf("parse agent file: %w", err)
	}

	// Open memory + bbolt store.
	gmCfg := graymatter.DefaultConfig()
	gmCfg.DataDir = cfg.DataDir
	if cfg.APIKey != "" {
		gmCfg.AnthropicAPIKey = cfg.APIKey
	}
	gm, err := graymatter.NewWithConfig(gmCfg)
	if err != nil {
		return nil, fmt.Errorf("open memory store: %w", err)
	}
	defer func() { _ = gm.Close() }()

	db := gm.Advanced().DB()

	// Ensure harness_sessions bucket exists.
	if err := initHarnessBucket(db); err != nil {
		return nil, fmt.Errorf("init harness bucket: %w", err)
	}

	// Allocate a new session ID for this run.
	sessionID := ulid.Make().String()

	// Load prior checkpoint message history when resuming.
	var priorMessages []session.Message
	if cfg.ResumeID != "" {
		cp, cpErr := session.Resume(db, def.Name)
		if cpErr == nil && cp != nil {
			priorMessages = cp.Messages
			fmt.Fprintf(cfg.Stderr, "Resuming from checkpoint %s...\n", cp.ID)
		}
	}

	// Create and persist the harness session record.
	hs := HarnessSession{
		ID:        sessionID,
		AgentID:   def.Name,
		AgentFile: cfg.AgentFile,
		StartedAt: time.Now().UTC(),
		Status:    "running",
		Inputs:    cfg.Inputs,
	}
	if err := saveHarnessSession(db, hs); err != nil {
		return nil, fmt.Errorf("save harness session: %w", err)
	}

	// Recall relevant memory and inject into system prompt.
	memFacts, _ := gm.Recall(ctx, def.Name, def.Task)
	systemContent := def.SystemPrompt
	if len(memFacts) > 0 {
		systemContent += "\n\n## Memory\n" + strings.Join(memFacts, "\n")
	}

	// Reconstruct message history (prior turns) and append the new user task.
	apiMessages := messagesFromCheckpoint(priorMessages)
	var sessionMessages []session.Message
	for _, m := range priorMessages {
		sessionMessages = append(sessionMessages, m)
	}
	if def.Task != "" {
		apiMessages = append(apiMessages, anthropic.NewUserMessage(anthropic.NewTextBlock(def.Task)))
		sessionMessages = append(sessionMessages, session.Message{Role: "user", Content: def.Task})
	}

	// Resolve API key.
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}

	var finalReply string
	var lastErr error

	for attempt := 1; attempt <= cfg.MaxRetries; attempt++ {
		hs.Attempts = attempt
		_ = saveHarnessSession(db, hs) // best-effort progress update

		params := anthropic.MessageNewParams{
			Model:     anthropic.Model(def.Model),
			MaxTokens: 4096,
			System: []anthropic.TextBlockParam{
				{Text: systemContent},
			},
			Messages: apiMessages,
		}

		var msg *anthropic.Message
		if cfg.llmDoer != nil {
			msg, err = cfg.llmDoer(ctx, params)
		} else {
			var client anthropic.Client
			if apiKey != "" {
				client = anthropic.NewClient(option.WithAPIKey(apiKey))
			} else {
				client = anthropic.NewClient()
			}
			msg, err = client.Messages.New(ctx, params)
		}

		if err != nil {
			lastErr = err
			fmt.Fprintf(cfg.Stderr, "graymatter run: attempt %d/%d failed: %v\n", attempt, cfg.MaxRetries, err)
			if attempt < cfg.MaxRetries {
				time.Sleep(backoffDuration(attempt))
			}
			continue
		}

		// Extract text from response.
		finalReply = extractText(msg)

		// Append assistant turn to both message slices.
		apiMessages = append(apiMessages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(finalReply)))
		sessionMessages = append(sessionMessages, session.Message{Role: "assistant", Content: finalReply})

		// Store observation in memory (best-effort).
		if finalReply != "" {
			_ = gm.Remember(ctx, def.Name, finalReply)
		}

		// Checkpoint: persist full message history to bbolt.
		cp := session.Checkpoint{
			AgentID: def.Name,
			State: map[string]any{
				"session_id": sessionID,
				"attempt":    attempt,
				"agent_file": cfg.AgentFile,
			},
			Messages: sessionMessages,
			Metadata: map[string]string{"harness_session_id": sessionID},
		}
		saved, saveErr := session.Save(db, cp)
		if saveErr == nil {
			hs.LastCPID = saved.ID
		}

		// Finalise harness session record.
		now := time.Now().UTC()
		hs.FinishedAt = &now
		hs.Status = "done"
		hs.Attempts = attempt
		_ = saveHarnessSession(db, hs)

		fmt.Fprintln(cfg.Stdout, finalReply)

		return &RunResult{
			SessionID:  sessionID,
			Messages:   sessionMessages,
			FinalReply: finalReply,
			Attempts:   attempt,
		}, nil
	}

	// All retries exhausted — write failure record and return error.
	now := time.Now().UTC()
	hs.FinishedAt = &now
	hs.Status = "failed"
	hs.ErrorMsg = lastErr.Error()
	_ = saveHarnessSession(db, hs)
	writeFailedRun(cfg.DataDir, sessionID, hs, lastErr)

	return nil, fmt.Errorf("run %q failed after %d attempt(s): %w", def.Name, cfg.MaxRetries, lastErr)
}

// backoffDuration returns the wait duration before retry attempt n (1-based).
// Formula: base * 2^(n-1) + ±25% jitter, final value capped at 30s. Base = 1s.
func backoffDuration(attempt int) time.Duration {
	const (
		base   = float64(time.Second)
		capDur = 30 * float64(time.Second)
		jitter = 0.25
	)
	exp := math.Pow(2, float64(attempt-1))
	dur := base * exp
	// Apply ±25% jitter before capping so the cap is a hard ceiling.
	j := (rand.Float64()*2 - 1) * jitter * dur
	result := dur + j
	if result > capDur {
		result = capDur
	}
	if result < 0 {
		result = 0
	}
	return time.Duration(result)
}

// extractText returns the first non-empty text block from an Anthropic response.
func extractText(msg *anthropic.Message) string {
	if msg == nil {
		return ""
	}
	for _, block := range msg.Content {
		if block.Text != "" {
			return block.Text
		}
	}
	return ""
}

// messagesFromCheckpoint converts session.Message slice to Anthropic MessageParam slice.
func messagesFromCheckpoint(prior []session.Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(prior))
	for _, m := range prior {
		switch m.Role {
		case "user":
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case "assistant":
			out = append(out, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		}
	}
	return out
}

// writeFailedRun writes a JSON failure record to <dataDir>/failed/<sessionID>.json.
// Any OS error is logged to stderr and suppressed — this is best-effort auditing.
func writeFailedRun(dataDir, sessionID string, hs HarnessSession, runErr error) {
	dir := filepath.Join(dataDir, "failed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "graymatter: mkdir failed dir: %v\n", err)
		return
	}
	type record struct {
		HarnessSession
		Error string `json:"error"`
	}
	r := record{HarnessSession: hs}
	if runErr != nil {
		r.Error = runErr.Error()
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(dir, sessionID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "graymatter: write failed run record: %v\n", err)
	}
}

// --- bbolt helpers ---

func initHarnessBucket(db *bolt.DB) error {
	return db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketHarness)
		return err
	})
}

func saveHarnessSession(db *bolt.DB, hs HarnessSession) error {
	data, err := json.Marshal(hs)
	if err != nil {
		return fmt.Errorf("marshal harness session: %w", err)
	}
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketHarness)
		if b == nil {
			return fmt.Errorf("harness_sessions bucket missing")
		}
		return b.Put([]byte(hs.ID), data)
	})
}

func loadHarnessSession(db *bolt.DB, sessionID string) (*HarnessSession, error) {
	var hs HarnessSession
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketHarness)
		if b == nil {
			return fmt.Errorf("harness_sessions bucket missing")
		}
		data := b.Get([]byte(sessionID))
		if data == nil {
			return fmt.Errorf("session %q not found", sessionID)
		}
		return json.Unmarshal(data, &hs)
	})
	if err != nil {
		return nil, err
	}
	return &hs, nil
}

func listHarnessSessions(db *bolt.DB) ([]HarnessSession, error) {
	var sessions []HarnessSession
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketHarness)
		if b == nil {
			return nil // bucket not yet created — no sessions
		}
		return b.ForEach(func(_, v []byte) error {
			var hs HarnessSession
			if err := json.Unmarshal(v, &hs); err != nil {
				return nil // skip corrupt entries
			}
			sessions = append(sessions, hs)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	// Sort newest first.
	for i := 1; i < len(sessions); i++ {
		for j := i; j > 0 && sessions[j].StartedAt.After(sessions[j-1].StartedAt); j-- {
			sessions[j], sessions[j-1] = sessions[j-1], sessions[j]
		}
	}
	return sessions, nil
}
