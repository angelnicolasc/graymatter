package memory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ConsolidateConfig is the subset of configuration used by consolidation.
// Defined as an interface to avoid a circular import with the root package.
type ConsolidateConfig interface {
	GetAnthropicAPIKey() string
	GetConsolidateLLM() string
	GetConsolidateModel() string
	GetConsolidateThreshold() int
	GetDecayHalfLife() time.Duration
}

// LaunchAsyncConsolidate starts MaybeConsolidate in a tracked, bounded goroutine.
// Non-blocking: if the semaphore is at capacity the trigger is silently dropped
// rather than blocking the caller.
func (s *Store) LaunchAsyncConsolidate(agentID string, cfg ConsolidateConfig) {
	select {
	case s.sema <- struct{}{}: // acquired slot
	default:
		return // at capacity; skip this consolidation cycle
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.sema }()
		if err := s.MaybeConsolidate(s.shutdownCtx, agentID, cfg); err != nil {
			if s.cfg.OnConsolidateError != nil {
				s.cfg.OnConsolidateError(agentID, err)
			}
		}
	}()
}

// MaybeConsolidate triggers consolidation only when the fact count for
// agentID meets or exceeds the threshold. Safe to call concurrently.
func (s *Store) MaybeConsolidate(ctx context.Context, agentID string, cfg ConsolidateConfig) error {
	facts, err := s.List(agentID)
	if err != nil {
		return err
	}
	if len(facts) < cfg.GetConsolidateThreshold() {
		return nil
	}
	return s.Consolidate(ctx, agentID, cfg)
}

// Consolidate runs the full consolidation pipeline for agentID:
//  1. Apply exponential decay weights to all facts.
//  2. If fact count > threshold, LLM-summarise the weakest batch.
//  3. Prune facts with weight < 0.01.
func (s *Store) Consolidate(ctx context.Context, agentID string, cfg ConsolidateConfig) error {
	facts, err := s.List(agentID)
	if err != nil {
		return err
	}
	if len(facts) == 0 {
		return nil
	}

	halfLife := cfg.GetDecayHalfLife()
	if halfLife == 0 {
		halfLife = 720 * time.Hour
	}
	lambda := math.Log(2) / halfLife.Hours()

	// Step 1: decay all facts.
	for i := range facts {
		hours := time.Since(facts[i].AccessedAt).Hours()
		facts[i].Weight *= math.Exp(-lambda * hours)
		_ = s.UpdateFact(agentID, facts[i])
	}

	// Step 2: LLM summarisation when enabled and threshold exceeded.
	if len(facts) > cfg.GetConsolidateThreshold() && cfg.GetConsolidateLLM() != "" {
		sort.Slice(facts, func(i, j int) bool { return facts[i].Weight < facts[j].Weight })
		batch := facts[:len(facts)/2]

		summary, err := summariseFacts(ctx, batch, cfg)
		if err == nil && summary != "" {
			for _, f := range batch {
				_ = s.Delete(agentID, f.ID)
			}
			_ = s.Put(ctx, agentID, summary)
		}
	}

	// Step 3: prune dead facts.
	facts, err = s.List(agentID)
	if err != nil {
		return err
	}
	for _, f := range facts {
		if f.Weight < 0.01 {
			_ = s.Delete(agentID, f.ID)
		}
	}

	// Step 4: run entity extraction on all surviving facts and upsert into graph.
	s.mu.RLock()
	extractor := s.extractor
	graph := s.graph
	s.mu.RUnlock()
	if extractor != nil && graph != nil {
		facts, _ = s.List(agentID)
		for _, f := range facts {
			ids, extractErr := extractor.ExtractIDs(f.Text)
			if extractErr != nil {
				continue
			}
			for _, id := range ids {
				_ = graph.UpsertNode(id, id, "fact")
			}
		}
	}

	return nil
}

func summariseFacts(ctx context.Context, facts []Fact, cfg ConsolidateConfig) (string, error) {
	if len(facts) == 0 {
		return "", nil
	}
	texts := make([]string, 0, len(facts))
	for _, f := range facts {
		texts = append(texts, "- "+f.Text)
	}
	prompt := fmt.Sprintf(
		"The following are memory facts for an AI agent. "+
			"Produce a single concise paragraph (≤5 sentences) that preserves all key information:\n\n%s",
		strings.Join(texts, "\n"),
	)
	switch cfg.GetConsolidateLLM() {
	case "anthropic":
		return consolidateViaAnthropic(ctx, prompt, cfg)
	case "ollama":
		// Ollama text generation not yet implemented; skip silently.
		return "", nil
	default:
		return "", nil
	}
}

func consolidateViaAnthropic(ctx context.Context, prompt string, cfg ConsolidateConfig) (string, error) {
	key := cfg.GetAnthropicAPIKey()
	if key == "" {
		return "", fmt.Errorf("ANTHROPIC_API_KEY not set")
	}
	client := anthropic.NewClient(option.WithAPIKey(key))
	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(cfg.GetConsolidateModel()),
		MaxTokens: 512,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", err
	}
	if len(msg.Content) == 0 {
		return "", nil
	}
	return msg.Content[0].Text, nil
}
