package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	bolt "go.etcd.io/bbolt"
)

// Deprecated: ErrConsolidateLLMUnsupported is no longer returned. Ollama is
// implemented as a consolidation summariser since v0.14.0; the sentinel is
// kept for API compatibility with callers that matched on it. A configured
// but unreachable Ollama now surfaces as the underlying network error via
// OnConsolidateError.
var ErrConsolidateLLMUnsupported = errors.New(
	"consolidation LLM \"ollama\" is implemented since v0.14.0; this sentinel " +
		"is retained only for API compatibility and is never returned")

// ErrInvalidProposal marks an LLM response that arrived intact but is not a
// usable consolidation proposal — malformed JSON, missing summary, or an
// empty consumes list. The proposal is discarded and the store is untouched;
// OnConsolidateError carries this sentinel so callers can tell a bad model
// output apart from a transport failure.
var ErrInvalidProposal = errors.New("invalid consolidation proposal")

// ConsolidateConfig is the subset of configuration used by consolidation.
// Defined as an interface to avoid a circular import with the root package.
type ConsolidateConfig interface {
	GetAnthropicAPIKey() string
	GetConsolidateLLM() string
	GetConsolidateModel() string
	GetConsolidateThreshold() int
	GetDecayHalfLife() time.Duration
	// Ollama settings for the local consolidation summariser (W3). The
	// embedder already carries GRAYMATTER_OLLAMA_URL; consolidation reuses it
	// and adds its own model knob.
	GetOllamaURL() string
	GetOllamaConsolidateModel() string
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
		// Invariant I-1 (ADR-010): pinned facts are exempt from decay. The
		// user declared them permanent; a dormant period must not collect
		// them, and the decay write would only churn the store.
		if facts[i].Pinned {
			continue
		}
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
	//
	// Propose/apply discipline (ADR-011): the LLM only ever proposes; the
	// application is deterministic. A proposal that fails to arrive or fails
	// validation is discarded with a hook and the store is untouched — decay
	// and pruning already ran, so a broken summariser degrades to the exact
	// behaviour of ConsolidateLLM="".
	if len(facts) > cfg.GetConsolidateThreshold() && cfg.GetConsolidateLLM() != "" {
		batch := summarisationBatch(facts)

		prop, err := summariseFacts(ctx, batch, cfg)
		switch {
		case err != nil:
			// Report rather than swallow. Summarisation failing is not fatal —
			// the facts are still there — but a caller who configured an LLM
			// and gets nothing back has no other way to find out. Unreachable
			// Ollama and discarded proposals both arrive here; neither may
			// touch the store.
			if s.cfg.OnConsolidateError != nil {
				s.cfg.OnConsolidateError(agentID, err)
			}
		case prop != nil:
			applied, applyErr := s.applyProposal(ctx, agentID, batch, prop)
			if applied > 0 {
				if cErr := recordConsolidation(s.db, applied); cErr != nil && s.cfg.OnConsolidateError != nil {
					s.cfg.OnConsolidateError(agentID, fmt.Errorf("record consolidation counters: %w", cErr))
				}
			}
			if applyErr != nil && s.cfg.OnConsolidateError != nil {
				s.cfg.OnConsolidateError(agentID, applyErr)
			}
		}
		// prop == nil && err == nil: summariser produced nothing to apply by
		// configuration (unknown provider name) — silence stays correct.
	}

	// Step 3: prune dead facts. Pinned facts are exempt (invariant I-1):
	// pruning is the only thing that ever removes a fact, so a pin that
	// didn't stop pruning would be a promise the store breaks.
	facts, err = s.List(agentID)
	if err != nil {
		return err
	}
	for _, f := range facts {
		if f.Pinned {
			continue
		}
		if f.Weight < 0.01 {
			// Best-effort; weight-zero facts will simply be ignored in future recalls.
			_ = s.Delete(agentID, f.ID)
		}
	}

	// Step 4: run entity extraction on surviving facts and upsert into graph.
	//
	// A text-signature watermark (A7) makes the pass incremental: a fact is
	// extracted only when its text changed since the last successful pass.
	// Extraction is idempotent, so the old re-extract-everything behaviour was
	// correct but O(facts) per cycle for no information.
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
			// Superseded facts are retired: their content lives on in the
			// fact (or summary) that replaced them. Extracting a tombstone
			// would re-grow graph nodes for information that is no longer
			// retrievable — the graph must mirror recallable memory, and
			// since tombstones stopped being deleted this pass sees them.
			if f.IsSuperseded() {
				continue
			}
			sig := textSignature(f.Text)
			if prev, ok := s.extractedSignature(agentID, f.ID); ok && prev == sig {
				continue
			}
			clean := true
			if typedOK && writeOK {
				refs, links, extractErr := typed.ExtractTyped(f.Text)
				if extractErr != nil {
					continue
				}
				for _, ref := range refs {
					if upsertErr := graph.UpsertNode(ref.ID, ref.Label, ref.EntityType); upsertErr != nil {
						clean = false
						if s.cfg.OnConsolidateError != nil {
							s.cfg.OnConsolidateError(agentID, fmt.Errorf("upsert node %s: %w", ref.ID, upsertErr))
						}
					}
				}
				for i := 0; i < len(links); i++ {
					if linkErr := writer.LinkEdges([]EntityLink{links[i]}, f.ID); linkErr != nil {
						clean = false
						if s.cfg.OnConsolidateError != nil {
							s.cfg.OnConsolidateError(agentID, fmt.Errorf("link edge: %w", linkErr))
						}
					}
				}
				if clean {
					_ = s.markExtracted(agentID, f.ID, sig)
				}
				continue
			}

			ids, extractErr := extractor.ExtractIDs(f.Text)
			if extractErr != nil {
				continue
			}
			for _, id := range ids {
				if upsertErr := graph.UpsertNode(id, id, "concept"); upsertErr != nil {
					clean = false
					if s.cfg.OnConsolidateError != nil {
						s.cfg.OnConsolidateError(agentID, fmt.Errorf("upsert node %s: %w", id, upsertErr))
					}
				}
			}
			if clean {
				_ = s.markExtracted(agentID, f.ID, sig)
			}
		}
	}

	// Step 5: decay graph weights. Optional capability - existing
	// GraphAccessor implementations keep compiling unchanged.
	//
	// The graph mirrors recallable memory; without this pass its weights
	// only ever grew (upsert takes max), so a hub entity from a finished
	// project stayed prominent forever. Same recompute-from-staleness rule
	// as Step 1, for the same reason: multiplying re-applied whole elapsed
	// periods per cycle and compounded forgetting.
	s.mu.RLock()
	graph = s.graph
	s.mu.RUnlock()
	if decayer, ok := graph.(GraphDecayer); ok {
		if err := decayer.DecayGraph(halfLife); err != nil {
			if s.cfg.OnConsolidateError != nil {
				s.cfg.OnConsolidateError(agentID, fmt.Errorf("graph decay: %w", err))
			}
		}
	}

	return nil
}

// GraphDecayer is an optional GraphAccessor capability: lifecycle-driven
// weight decay over graph state, invoked once per consolidation cycle.
// Implementations must be idempotent - recomputing from staleness rather
// than multiplying into weight - so running more often never forgets faster.
type GraphDecayer interface {
	DecayGraph(halfLife time.Duration) error
}

// textSignature fingerprints a fact's text for the extraction watermark.
func textSignature(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", sum)
}

// summarisationBatch selects the facts the LLM summariser may consume: the
// weakest half of the unpinned facts, weakest first. Pinned facts are never
// eligible (invariant I-1, ADR-010) — consolidation must not consume what the
// user declared permanent, and pinned facts are precisely the rarely-accessed
// ones the weight sort would surface first.
func summarisationBatch(facts []Fact) []Fact {
	live := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if !f.Pinned {
			live = append(live, f)
		}
	}
	if len(live) == 0 {
		return nil
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Weight < live[j].Weight })
	return live[:len(live)/2]
}

// consolidationProposal is the structured output a consolidation summariser
// must produce (ADR-011): the summary paragraph, the batch fact IDs the
// summary fully accounts for, and any contradictions noticed along the way.
// The LLM proposes this; applyProposal applies it deterministically.
type consolidationProposal struct {
	Summary        string   `json:"summary"`
	Consumes       []string `json:"consumes"`
	Contradictions []string `json:"contradictions,omitempty"`
}

// parseProposal validates a raw model response into a proposal. Fenced code
// blocks are stripped because models emit them even when told not to; after
// that the payload must be a strict JSON object with a non-empty summary and
// at least one consumed ID — a proposal consuming nothing would grow memory
// without retiring anything, which is not a consolidation.
func parseProposal(raw string) (*consolidationProposal, error) {
	var p consolidationProposal
	if err := json.Unmarshal([]byte(stripCodeFence(raw)), &p); err != nil {
		return nil, fmt.Errorf("%w: response is not a JSON object: %v", ErrInvalidProposal, err)
	}
	if strings.TrimSpace(p.Summary) == "" || len(p.Consumes) == 0 {
		return nil, fmt.Errorf("%w: empty summary or empty consumes", ErrInvalidProposal)
	}
	p.Summary = strings.TrimSpace(p.Summary)
	return &p, nil
}

// stripCodeFence removes one surrounding markdown code fence, if present.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	for {
		switch {
		case strings.HasPrefix(t, "```"):
			t = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(t, "```"), "json"))
		case strings.HasSuffix(t, "```"):
			t = strings.TrimSpace(strings.TrimSuffix(t, "```"))
		default:
			return t
		}
	}
}

// applyProposal puts the proposed summary and tombstones exactly the consumed
// batch facts toward it — ADR-007 receipts with propose/apply discipline:
//
//   - The summary enters first. If its Put fails, nothing else changes and
//     the batch stays live; data is never lost ahead of its replacement.
//   - Consumed IDs are clamped to the batch: the model may only consume what
//     it was shown. Hallucinated or duplicate IDs are ignored deterministically.
//   - Tombstones keep their post-decay weight and decay on like any other
//     fact. They leave Recall immediately (IsSuperseded), remain listed,
//     exportable and auditable, and pruning collects them on the ordinary
//     schedule. Zeroing their weight instead would let step 3 of this very
//     cycle delete each receipt milliseconds after writing it.
//   - The receipt points at the exact summary fact this cycle wrote. Looking
//     it up by text could select an identical fact from a concurrent writer
//     and falsify the audit trail.
//
// It returns how many facts were actually tombstoned; per-fact write failures
// are joined into the returned error without stopping the remaining ones.
func (s *Store) applyProposal(ctx context.Context, agentID string, batch []Fact, prop *consolidationProposal) (int, error) {
	// Defence in depth: parseProposal validates Ollama output, but the
	// Anthropic path wraps plain text into a proposal directly. No caller
	// reaches here with an unusable proposal any more — keeping the check at
	// the mutation boundary is what makes that true for future paths too.
	if strings.TrimSpace(prop.Summary) == "" || len(prop.Consumes) == 0 {
		return 0, fmt.Errorf("%w: empty summary or empty consumes", ErrInvalidProposal)
	}
	byID := make(map[string]Fact, len(batch))
	for _, f := range batch {
		byID[f.ID] = f
	}
	consumed := make([]Fact, 0, len(prop.Consumes))
	for _, id := range prop.Consumes {
		f, ok := byID[id]
		if !ok {
			continue // not shown to the model: it cannot be consumed
		}
		delete(byID, id) // duplicate IDs consume once
		consumed = append(consumed, f)
	}

	summary, err := s.putReturningFact(ctx, agentID, prop.Summary)
	if err != nil {
		return 0, fmt.Errorf("put consolidation summary: %w", err)
	}
	summaryID := summary.ID

	var errs []error
	applied := 0
	for _, f := range consumed {
		tomb := f
		tomb.SupersededBy = summaryID
		if err := s.UpdateFact(agentID, tomb); err != nil {
			errs = append(errs, fmt.Errorf("tombstone consolidated fact %s: %w", f.ID, err))
			continue
		}
		applied++
	}
	return applied, errors.Join(errs...)
}

// factIDByText returns the ID of the most recent fact with exactly this text,
// or "" when absent. List returns facts newest first, so the first match is
// the newest — scanning from the back would return the oldest duplicate,
// pointing consolidation receipts at a stale record.
func (s *Store) factIDByText(agentID, text string) string {
	facts, err := s.List(agentID)
	if err != nil {
		return ""
	}
	for _, f := range facts {
		if f.Text == text {
			return f.ID
		}
	}
	return ""
}

// extractedSignature returns the text signature recorded the last time this
// fact was successfully extracted into the knowledge graph.
func (s *Store) extractedSignature(agentID, factID string) (string, bool) {
	var sig string
	found := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketKGExtracted)
		if b == nil {
			return nil // read-only store against an old DB: treat as unextracted
		}
		if v := b.Get([]byte(agentID + "\x00" + factID)); v != nil {
			sig, found = string(v), true
		}
		return nil
	})
	return sig, found
}

// markExtracted records the text signature of a fact whose extraction fully
// succeeded. No-op on read-only stores (consolidation cannot run there
// anyway, and a failed write must not fail the pass).
func (s *Store) markExtracted(agentID, factID, sig string) error {
	if s.readOnly {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketKGExtracted)
		if b == nil {
			return fmt.Errorf("kg_extracted bucket missing")
		}
		return b.Put([]byte(agentID+"\x00"+factID), []byte(sig))
	})
}

func summariseFacts(ctx context.Context, facts []Fact, cfg ConsolidateConfig) (*consolidationProposal, error) {
	if len(facts) == 0 {
		return nil, nil
	}
	texts := make([]string, 0, len(facts))
	rows := make([]string, 0, len(facts))
	for _, f := range facts {
		texts = append(texts, "- "+f.Text)
		rows = append(rows, fmt.Sprintf("- %s: %s", f.ID, f.Text))
	}
	switch cfg.GetConsolidateLLM() {
	case "anthropic":
		prompt := fmt.Sprintf(
			"The following are memory facts for an AI agent. "+
				"Produce a single concise paragraph (≤5 sentences) that preserves all key information:\n\n%s",
			strings.Join(texts, "\n"),
		)
		summary, err := consolidateViaAnthropic(ctx, prompt, cfg)
		if err != nil {
			return nil, err
		}
		summary = strings.TrimSpace(summary)
		if summary == "" {
			// An empty model response must never become an empty fact that
			// eats its batch: discard with the same sentinel the Ollama path
			// validates with, so the hook fires and the store stays intact.
			return nil, fmt.Errorf("%w: anthropic returned empty content", ErrInvalidProposal)
		}
		// The Anthropic path predates structured proposals: whatever it
		// returns accounts for the whole batch, deterministically. The
		// apply step still tombstones with receipts — only the proposal's
		// granularity differs.
		ids := make([]string, 0, len(facts))
		for _, f := range facts {
			ids = append(ids, f.ID)
		}
		return &consolidationProposal{Summary: summary, Consumes: ids}, nil
	case "ollama":
		prompt := fmt.Sprintf(
			"The following are memory facts for an AI agent, each prefixed with its ID. "+
				"Summarise them into a single concise paragraph (≤5 sentences) that preserves all key "+
				"information, then list the IDs whose information the summary fully accounts for. "+
				"Respond with ONLY a JSON object:\n"+
				`{"summary":"<paragraph>","consumes":["<id>",...],"contradictions":["<optional note>",...]}`+
				"\n\n%s",
			strings.Join(rows, "\n"),
		)
		return consolidateViaOllama(ctx, prompt, cfg)
	default:
		// Unknown provider name. Summarisation is skipped; decay and pruning
		// still run, and that is the configured behaviour rather than a
		// failure.
		return nil, nil
	}
}

// ollamaHTTPTimeout bounds one Ollama generate call. Package-level so tests
// can shorten it; production callers get a patient default because loading a
// cold model into memory routinely exceeds the first request's patience.
var ollamaHTTPTimeout = 120 * time.Second

// consolidateViaOllama proposes a consolidation through a local Ollama
// instance: zero accounts, zero cost, fully offline — the consolidation path
// that keeps the one-binary zero-infra promise end to end. The request asks
// Ollama for JSON at the sampler level ("format":"json"); the response is
// then validated as a strict proposal. One retry on transient failures
// (transport errors, 5xx, 408, 429); deterministic rejections (4xx) and
// invalid proposals fail immediately because retrying reproduces them.
func consolidateViaOllama(ctx context.Context, prompt string, cfg ConsolidateConfig) (*consolidationProposal, error) {
	base := strings.TrimRight(cfg.GetOllamaURL(), "/")
	if base == "" {
		base = "http://localhost:11434"
	}
	model := cfg.GetOllamaConsolidateModel()
	if model == "" {
		model = "llama3.2"
	}
	payload, err := json.Marshal(map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
		"format": "json",
	})
	if err != nil {
		return nil, fmt.Errorf("ollama: encode request: %w", err)
	}

	client := &http.Client{Timeout: ollamaHTTPTimeout}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/generate", bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("ollama: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("ollama: post %s/api/generate: %w", base, err)
			continue // one retry: cold-model loads routinely exceed the first call
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("ollama: read response: %w", readErr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("ollama: status %d: %s", resp.StatusCode, truncateForLog(body))
			if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests {
				continue
			}
			return nil, lastErr
		}
		var out struct {
			Response string `json:"response"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("ollama: decode envelope: %w", err)
		}
		return parseProposal(out.Response)
	}
	return nil, lastErr
}

func truncateForLog(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
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

const (
	metaKeyConsolidations    = "consolidations"
	metaKeyFactsConsolidated = "facts_consolidated"
)

// ReadConsolidationCounters reports lifetime consolidation activity recorded
// in the store's meta bucket: how many cycles applied at least one proposal,
// and how many batch facts were consumed across them. Exported because both
// surfaces that display it — the daemon host's StoreOverview and the direct
// in-process store — must read the same numbers without either owning the
// write logic.
func ReadConsolidationCounters(db *bolt.DB) (cycles, facts int) {
	_ = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(metaKeyConsolidations)); v != nil {
			cycles, _ = strconv.Atoi(string(v))
		}
		if v := b.Get([]byte(metaKeyFactsConsolidated)); v != nil {
			facts, _ = strconv.Atoi(string(v))
		}
		return nil
	})
	return cycles, facts
}

// ConsolidationCounters reads the counters from this store.
func (s *Store) ConsolidationCounters() (cycles, facts int) {
	return ReadConsolidationCounters(s.db)
}

// recordConsolidation bumps both counters once per applied proposal.
func recordConsolidation(db *bolt.DB, facts int) error {
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if b == nil {
			return fmt.Errorf("meta bucket missing")
		}
		bump := func(key string, by int) error {
			cur := 0
			if v := b.Get([]byte(key)); v != nil {
				n, err := strconv.Atoi(string(v))
				if err != nil {
					return fmt.Errorf("meta key %q: %w", key, err)
				}
				cur = n
			}
			return b.Put([]byte(key), []byte(strconv.Itoa(cur+by)))
		}
		if err := bump(metaKeyConsolidations, 1); err != nil {
			return err
		}
		return bump(metaKeyFactsConsolidated, facts)
	})
}
