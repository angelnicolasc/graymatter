package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ErrConsolidateLLMUnsupported is returned when ConsolidateLLM names a
// provider that can embed but cannot yet summarise. Today that is "ollama":
// it is a supported embedding backend, and configuring it as the consolidation
// LLM is accepted by config but does nothing.
//
// It used to do nothing quietly. A store configured this way would run decay
// and pruning forever and never summarise, with no error and no log line to
// explain why memory kept growing. It now reaches OnConsolidateError like any
// other consolidation failure.
var ErrConsolidateLLMUnsupported = errors.New(
	"consolidation LLM \"ollama\" is not implemented: Ollama works as an embedding " +
		"backend but cannot summarise yet; set ConsolidateLLM to \"anthropic\" or \"\"")

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
//
// The threshold is read twice on the way through, with deliberately different
// comparisons, and the difference is load-bearing rather than an oversight:
// this function fires at >= N, while the LLM summarisation step inside
// Consolidate fires at > N. At exactly N facts a cycle therefore runs decay
// and pruning but does not summarise. Decay and pruning are cheap, local and
// worth doing at the boundary; summarisation costs an API call and destroys
// the batch it replaces, so it waits until the store is unambiguously over
// the line. See docs/decisions/001-decay-half-life.md.
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

	// Step 1: decay all facts. Accumulate errors rather than silently dropping.
	//
	// Weight is recomputed from staleness, not multiplied into. Multiplying
	// re-applied the entire elapsed period on every run — nothing recorded
	// that a fact had already been decayed — so weight halved once per
	// consolidation cycle rather than once per half-life. Five cycles in the
	// same millisecond took a one-half-life-stale fact from 0.5 to 0.03, and
	// with AsyncConsolidate on, a busy agent could prune a month-old fact in
	// minutes. The half-life was per run, not per 30 days.
	//
	// min() rather than plain assignment, for two reasons: decay must never
	// hand weight back, and a fact whose weight was deliberately zeroed —
	// a supersede tombstone (ADR-007) — must stay collectable by pruning
	// instead of being resurrected by its own recent access time.
	var decayErrs []error
	nowT := s.now()
	for i := range facts {
		hours := nowT.Sub(facts[i].AccessedAt).Hours()
		facts[i].Weight = math.Min(facts[i].Weight, math.Exp(-lambda*hours))
		if err := s.UpdateFact(agentID, facts[i]); err != nil {
			decayErrs = append(decayErrs, fmt.Errorf("decay fact %s: %w", facts[i].ID, err))
		}
	}
	if len(decayErrs) > 0 {
		return errors.Join(decayErrs...)
	}

	// Step 2: LLM summarisation when enabled and threshold exceeded.
	// Strictly greater, unlike MaybeConsolidate's >= — see the note there.
	if len(facts) > cfg.GetConsolidateThreshold() && cfg.GetConsolidateLLM() != "" {
		sort.Slice(facts, func(i, j int) bool { return facts[i].Weight < facts[j].Weight })
		batch := facts[:len(facts)/2]

		summary, err := summariseFacts(ctx, batch, cfg)
		if err != nil && s.cfg.OnConsolidateError != nil {
			// Report rather than swallow. Summarisation failing is not fatal —
			// the facts are still there and decay still runs — but a caller
			// who configured an LLM and is getting no summaries has no other
			// way to find out. ErrConsolidateLLMUnsupported arrives here too,
			// which is how a store configured for Ollama summarisation learns
			// that it is not implemented instead of quietly doing nothing.
			s.cfg.OnConsolidateError(agentID, err)
		}
		if err == nil && summary != "" {
			// Only delete the batch if Put of the summary succeeds — never lose data.
			if putErr := s.Put(ctx, agentID, summary); putErr == nil {
				for _, f := range batch {
					// Best-effort deletes; facts will be pruned by weight decay if delete fails.
					_ = s.Delete(agentID, f.ID)
				}
			}
		}
	}

	// Step 3: prune dead facts.
	facts, err = s.List(agentID)
	if err != nil {
		return err
	}
	for _, f := range facts {
		if f.Weight < 0.01 {
			// Best-effort; weight-zero facts will simply be ignored in future recalls.
			_ = s.Delete(agentID, f.ID)
		}
	}

	// Step 4: run entity extraction on all surviving facts and upsert into graph.
	//
	// Two paths, chosen by capability:
	//   - TypedEntityExtractor + EdgeWriter: nodes keep the extractor's label
	//     and type, and co-mentioned pairs become edges. This is what makes
	//     recall enrichment traversable (issue #24).
	//   - Legacy: ID-only upserts, no edges — preserved verbatim so older
	//     extractor implementations behave exactly as before.
	s.mu.RLock()
	extractor := s.extractor
	graph := s.graph
	s.mu.RUnlock()
	if extractor != nil && graph != nil {
		typed, typedOK := extractor.(TypedEntityExtractor)
		writer, writeOK := graph.(EdgeWriter)

		facts, _ = s.List(agentID)
		for _, f := range facts {
			if typedOK && writeOK {
				refs, links, extractErr := typed.ExtractTyped(f.Text)
				if extractErr != nil {
					continue
				}
				for _, ref := range refs {
					if upsertErr := graph.UpsertNode(ref.ID, ref.Label, ref.EntityType); upsertErr != nil {
						if s.cfg.OnConsolidateError != nil {
							s.cfg.OnConsolidateError(agentID, fmt.Errorf("upsert node %s: %w", ref.ID, upsertErr))
						}
					}
				}
				for i := 0; i < len(links); i++ {
					if linkErr := writer.LinkEdges(links[i].From, links[i].To, links[i].Relation); linkErr != nil {
						if s.cfg.OnConsolidateError != nil {
							s.cfg.OnConsolidateError(agentID, fmt.Errorf("link edge: %w", linkErr))
						}
					}
				}
				continue
			}

			ids, extractErr := extractor.ExtractIDs(f.Text)
			if extractErr != nil {
				continue
			}
			for _, id := range ids {
				if upsertErr := graph.UpsertNode(id, id, "fact"); upsertErr != nil {
					if s.cfg.OnConsolidateError != nil {
						s.cfg.OnConsolidateError(agentID, fmt.Errorf("upsert node %s: %w", id, upsertErr))
					}
				}
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
		return "", ErrConsolidateLLMUnsupported
	default:
		// Consolidation LLM disabled. Decay and pruning still run; only
		// summarisation is skipped, and that is the configured behaviour
		// rather than a failure.
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
