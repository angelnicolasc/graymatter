package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/lineageview"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func (s *Server) handleMemorySearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	agentID, ok := getString(args, "agent_id")
	if !ok || agentID == "" {
		return toolError("agent_id is required")
	}
	query, ok := getString(args, "query")
	if !ok || query == "" {
		return toolError("query is required")
	}
	topK := getInt(args, "top_k", 0) // 0 = store default
	explain := getBool(args, "explain")
	opts, err := recallOptions(args)
	if err != nil {
		return toolError(err.Error())
	}

	if explain {
		return s.handleMemorySearchExplain(ctx, agentID, query, topK, opts)
	}

	// RecallDetailed over Recall: same facts, same order — the second return
	// is the additive weak-match vocabulary block (empty when the match is
	// strong), which is what lets the agent reformulate once, informed,
	// instead of guessing N times.
	var facts []string
	var feedback string
	var retrieval *memory.RetrievalMetadata
	var notices []memory.ReviewNotice
	if useConfidenceRecall(s.backend, opts) {
		backend, ok := s.backend.(confidenceRecaller)
		if !ok {
			return toolError(memory.ErrConfidenceUnsupported.Error())
		}
		result, callErr := backend.RecallWithOptions(ctx, agentID, query, topK, opts)
		facts, feedback, retrieval, err = result.Facts, result.Feedback, result.Retrieval, callErr
		notices = memory.MergeReviewNotices(result.ReviewNotices)
	} else {
		facts, feedback, err = s.backend.RecallDetailed(ctx, agentID, query, topK)
	}
	if err != nil {
		return toolError(fmt.Sprintf("recall error: %v", err))
	}

	if topK > 0 && topK < len(facts) {
		facts = facts[:topK]
	}

	if len(facts) == 0 {
		notice := fmt.Sprintf("No memories found for agent %q matching %q.", agentID, query)
		if feedback != "" {
			notice += "\n\n" + feedback
		}
		if retrieval != nil {
			notice += "\n\n" + retrieval.Text()
		}
		return toolStructured(searchResult{AgentID: agentID, Query: query, Count: 0, Facts: []string{}, Feedback: feedback, Retrieval: retrieval}, notice)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d relevant memories for agent %q:\n\n", len(facts), agentID))
	for i, f := range facts {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, f))
	}
	if feedback != "" {
		sb.WriteString("\n" + feedback + "\n")
	}
	if retrieval != nil {
		sb.WriteString("\n" + retrieval.Text() + "\n")
	}
	writeReviewNotices(&sb, notices)
	return toolStructured(searchResult{AgentID: agentID, Query: query, Count: len(facts), Facts: facts, Feedback: feedback, Retrieval: retrieval, ReviewNotices: notices}, sb.String())
}

// handleMemoryAlias is the memory_alias entry point: it teaches the store's
// vocabulary so later searches bridging the two sides reach the right facts.
func (s *Server) handleMemoryAlias(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	agentID, ok := getString(args, "agent_id")
	if !ok || agentID == "" {
		return toolError("agent_id is required")
	}
	term, ok := getString(args, "term")
	if !ok || strings.TrimSpace(term) == "" {
		return toolError("term is required")
	}
	equivalents := getStringSlice(args, "equivalents")
	if len(equivalents) == 0 {
		return toolError("equivalents is required and must hold at least one non-empty value")
	}
	if err := s.backend.PutAlias(ctx, agentID, term, equivalents); err != nil {
		return toolError(fmt.Sprintf("alias error: %v", err))
	}
	out := aliasResult{AgentID: agentID, Term: term, Equivalents: equivalents, Stored: true}
	return toolStructured(out, fmt.Sprintf("Alias stored for agent %q: %q = %s. Future searches mentioning either side now reach the facts the other side matches.",
		agentID, term, strings.Join(equivalents, ", ")))
}

// handleMemorySearchExplain is the explain=true branch of memory_search: the
// same ranking as the plain path, with one receipt per fact. The plain path's
// text contract is untouched; this branch has its own prose shape, and the
// structured payload rides the same searchResult type under the optional
// `explained` key so the declared output schema covers both.
func (s *Server) handleMemorySearchExplain(ctx context.Context, agentID, query string, topK int, options ...memory.RecallOptions) (*mcp.CallToolResult, error) {
	var opts memory.RecallOptions
	if len(options) > 0 {
		opts = options[0]
	}
	var receipts []memory.RecallReceipt
	var retrieval *memory.RetrievalMetadata
	var err error
	if useConfidenceRecall(s.backend, opts) {
		backend, ok := s.backend.(confidenceRecaller)
		if !ok {
			return toolError(memory.ErrConfidenceUnsupported.Error())
		}
		result, callErr := backend.RecallExplainWithOptions(ctx, agentID, query, topK, opts)
		receipts, retrieval, err = result.Receipts, result.Retrieval, callErr
	} else {
		receipts, err = s.backend.RecallExplain(ctx, agentID, query, topK)
	}
	if err != nil {
		return toolError(fmt.Sprintf("recall error: %v", err))
	}

	if len(receipts) == 0 {
		notice := fmt.Sprintf("No memories found for agent %q matching %q.", agentID, query)
		if retrieval != nil {
			notice += "\n\n" + retrieval.Text()
		}
		return toolStructured(searchResult{AgentID: agentID, Query: query, Count: 0, Facts: []string{}, Retrieval: retrieval}, notice)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d relevant memories for agent %q (with receipts):\n\n", len(receipts), agentID))
	var notices []memory.ReviewNotice
	for i, r := range receipts {
		notices = memory.MergeReviewNotices(notices, r.ReviewNotices)
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, r.Text))
		sb.WriteString(fmt.Sprintf("   score %.4f = vector %d / keyword %d / recency %d (k %.0f) · weight %.3f · age %.1fd · written %s\n",
			r.Ranks.FusedScore, r.Ranks.VectorRank, r.Ranks.KeywordRank, r.Ranks.RecencyRank, r.Ranks.K,
			r.Weight, r.AgeDays, r.Provenance.WrittenAt.Format("2006-01-02")))
		sb.WriteString(fmt.Sprintf("   fact_id %s\n", r.Provenance.FactID))
		if r.Ranking != nil {
			sb.WriteString(fmt.Sprintf("   final score %.8f = base %.8f * factor %g; effective confidence %s, weight %g, policy %s\n", r.Ranking.FinalScore, r.Ranking.BaseScore, r.Ranking.Factor, r.Ranking.EffectiveConfidence, r.Ranking.ConfidenceWeight, r.Ranking.Policy))
		}
		for _, line := range lineageview.Lines(r.Provenance.LineageProjection) {
			fmt.Fprintf(&sb, "   %s\n", line)
		}
		if len(r.Provenance.Supersedes) > 0 {
			fmt.Fprintf(&sb, "   Supersedes IDs (compatibility field) · %s\n", strings.Join(r.Provenance.Supersedes, ", "))
		}
	}
	if retrieval != nil {
		sb.WriteString("\n" + retrieval.Text() + "\n")
	}
	writeReviewNotices(&sb, notices)
	return toolStructured(searchResult{
		AgentID: agentID,
		Query:   query,
		Count:   len(receipts),
		// Explain mode carries the receipts under `explained`; the bare
		// facts array stays present-but-empty so the payload always
		// conforms to the declared output schema (null would not).
		Facts:     []string{},
		Explained: receipts,
		Retrieval: retrieval,
	}, sb.String())
}

func (s *Server) handleMemoryAdd(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	agentID, ok := getString(args, "agent_id")
	if !ok || agentID == "" {
		return toolError("agent_id is required")
	}
	text, ok := getString(args, "text")
	if !ok || text == "" {
		return toolError("text is required")
	}

	opts, err := writeOptions(args)
	if err != nil {
		return toolError(err.Error())
	}
	out := addResult{AgentID: agentID, Stored: true}
	notice := fmt.Sprintf("Memory stored for agent %q.", agentID)
	fact, err := s.putWithIdentity(ctx, agentID, text, opts)
	if err != nil {
		return committedWriteError("remember", fact, err)
	}
	out.FactID, out.Confidence = fact.ID, fact.Confidence
	notice += writeIdentityNotice(fact.ID, fact.Confidence)
	return toolStructured(out, notice)
}

func (s *Server) handleCheckpointSave(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	agentID, ok := getString(args, "agent_id")
	if !ok || agentID == "" {
		return toolError("agent_id is required")
	}

	var state map[string]any
	if stateStr, ok := getString(args, "state"); ok && stateStr != "" {
		if err := json.Unmarshal([]byte(stateStr), &state); err != nil {
			return toolError(fmt.Sprintf("state must be valid JSON: %v", err))
		}
	}

	cp := session.Checkpoint{
		AgentID:   agentID,
		CreatedAt: time.Now().UTC(),
		State:     state,
		Metadata:  map[string]string{"source": "mcp"},
	}
	saved, err := s.backend.CheckpointSave(cp)
	if err != nil {
		return toolError(fmt.Sprintf("checkpoint save error: %v", err))
	}

	return toolStructured(checkpointSaveResult{AgentID: agentID, CheckpointID: saved.ID, CreatedAt: saved.CreatedAt.Format(time.RFC3339)},
		fmt.Sprintf("Checkpoint %q saved for agent %q.", saved.ID, agentID))
}

func (s *Server) handleCheckpointResume(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	agentID, ok := getString(args, "agent_id")
	if !ok || agentID == "" {
		return toolError("agent_id is required")
	}

	// on_missing is an input enum in the schema; mcp-go can enforce it with the
	// server.WithInputSchemaValidation option, but this server does not enable
	// that option, so the handler validates it explicitly. Missing means the
	// historical behaviour; the types are checked before the value so a
	// non-string argument cannot slip through as the default.
	onMissing := "error"
	if raw, present := args["on_missing"]; present {
		value, ok := raw.(string)
		if !ok || (value != "error" && value != "empty") {
			return toolError(`on_missing must be "error" or "empty"`)
		}
		onMissing = value
	}

	cp, err := s.backend.CheckpointResume(agentID)
	if err != nil {
		if errors.Is(err, session.ErrNoCheckpoint) {
			if onMissing == "empty" {
				return toolStructured(checkpointResumeEmpty{Found: false, AgentID: agentID},
					fmt.Sprintf("No checkpoint saved for agent %q yet.", agentID))
			}
			return toolError(fmt.Sprintf("no checkpoint found for agent %q: %v", agentID, err))
		}
		// Storage, daemon, and corrupt-record failures stay prose-only in both
		// modes: they are not absence and must never masquerade as found:false.
		return toolError(fmt.Sprintf("checkpoint resume error: %v", err))
	}

	stateJSON, _ := json.MarshalIndent(cp.State, "", "  ")
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Checkpoint %q restored for agent %q.\n", cp.ID, agentID))
	sb.WriteString(fmt.Sprintf("Created: %s\n", cp.CreatedAt.Format(time.RFC3339)))
	if len(stateJSON) > 2 {
		sb.WriteString(fmt.Sprintf("State:\n%s\n", string(stateJSON)))
	}
	if len(cp.Messages) > 0 {
		sb.WriteString(fmt.Sprintf("Messages: %d turns\n", len(cp.Messages)))
	}
	return toolStructured(checkpointResumeResult{
		ID:           cp.ID,
		CreatedAt:    cp.CreatedAt.Format(time.RFC3339),
		State:        cp.State,
		MessageCount: len(cp.Messages),
	}, sb.String())
}

// reflectAgentID validates every supplied spelling before choosing a namespace.
// Trimming is only a blank check: valid namespace bytes must remain unchanged.
func reflectAgentID(args map[string]any) (string, error) {
	var canonical, alias string
	var present bool
	for _, key := range []string{"agent_id", "agent"} {
		raw, exists := args[key]
		if !exists {
			continue
		}
		present = true
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s must be a non-empty string, not only whitespace", key)
		}
		if key == "agent_id" {
			canonical = value
		} else {
			alias = value
		}
	}
	if !present {
		return "", errors.New("agent_id (or deprecated alias agent) is required")
	}
	if canonical != "" {
		return canonical, nil
	}
	return alias, nil
}

func (s *Server) handleMemoryReflect(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	action, ok := getString(args, "action")
	if !ok || action == "" {
		return toolError("action is required")
	}
	// agent_id wins when both are valid; a malformed supplied spelling must
	// never silently redirect an operation to the other namespace.
	agentID, err := reflectAgentID(args)
	if err != nil {
		return toolError(err.Error())
	}
	// text and target are validated per-action below: forget works with
	// either one, so neither can be globally required (see PR #10).
	text, _ := getString(args, "text")
	target, _ := getString(args, "target")
	opts, err := writeOptions(args)
	if err != nil {
		return toolError(err.Error())
	}
	if opts.Confidence != nil && action != "add" && action != "update" {
		return toolError("confidence is supported only by reflect add/update")
	}

	var oldText string
	var resultMsg string
	out := reflectResult{Action: action, Agent: agentID, OK: true}

	switch action {
	case "add":
		if text == "" {
			return toolError("text (the fact to add) is required for add")
		}
		fact, err := s.putWithIdentity(ctx, agentID, text, opts)
		if err != nil {
			return committedWriteError("add", fact, err)
		}
		out.FactID, out.Confidence = fact.ID, fact.Confidence
		resultMsg = fmt.Sprintf("Added fact for agent %q.", agentID)

	case "update":
		if target == "" {
			return toolError("target (fact to supersede) is required for update")
		}
		if text == "" {
			return toolError("text (the corrected fact) is required for update")
		}
		facts, err := s.backend.List(agentID)
		if err != nil {
			return toolError(fmt.Sprintf("list facts: %v", err))
		}
		victims := findByText(facts, target, false)
		if len(victims) == 0 {
			return toolError(fmt.Sprintf("target fact not found: %q", target))
		}
		oldText = target
		reviser, ok := s.backend.(confidenceReviser)
		if !ok {
			return toolError("atomic revision is unavailable on this backend")
		}
		replacement, err := reviser.ReviseFactsWithOptions(ctx, agentID, text, opts, victims...)
		if err != nil {
			if replacement.ID != "" {
				return toolError(fmt.Sprintf("Update committed as fact_id %s (confidence %s). Follow-up error: %v",
					replacement.ID, memory.EffectiveConfidence(replacement.Confidence), err))
			}
			return committedWriteError("update", replacement, err)
		}
		if replacement.ID == "" {
			return toolError("atomic revision returned no committed fact")
		}
		out.FactID, out.Confidence = replacement.ID, replacement.Confidence
		resultMsg = fmt.Sprintf("Updated fact for agent %q.", agentID)

	case "forget":
		// The fact to forget may arrive in target or text — both are
		// documented as equivalent; target wins when both are set.
		wanted := target
		if wanted == "" {
			wanted = text
		}
		if wanted == "" {
			return toolError("the fact to forget is required: pass it in target (or text)")
		}
		facts, err := s.backend.List(agentID)
		if err != nil {
			return toolError(fmt.Sprintf("list facts: %v", err))
		}
		victims := findByText(facts, wanted, false)
		if len(victims) == 0 {
			return toolError(fmt.Sprintf("target fact not found: %q", wanted))
		}
		oldText = wanted
		if retirer, ok := s.backend.(interface {
			Retire(string, ...memory.Fact) error
		}); ok {
			if err := retirer.Retire(agentID, victims...); err != nil {
				return toolError(fmt.Sprintf("suppress fact: %v", err))
			}
			resultMsg = fmt.Sprintf("Fact suppressed for agent %q.", agentID)
			break
		}

		// Nothing replaces this one, so the tombstone records that an agent
		// decided to drop it.
		for _, victim := range victims {
			if err := s.supersedeFact(agentID, victim, memory.SupersededByAgent); err != nil {
				return toolError(fmt.Sprintf("suppress fact: %v", err))
			}
		}
		resultMsg = fmt.Sprintf("Fact suppressed for agent %q.", agentID)

	case "link":
		if target == "" {
			return toolError("target (node ID) is required for link")
		}
		if text == "" {
			return toolError("text (the source node ID) is required for link")
		}
		fromID := strings.ToLower(strings.TrimSpace(text))
		toID := strings.ToLower(strings.TrimSpace(target))
		if err := s.backend.KGLink(fromID, toID, "agent_link"); err != nil {
			return toolError(fmt.Sprintf("link nodes: %v", err))
		}
		resultMsg = fmt.Sprintf("Linked %q → %q.", fromID, toID)

	case "pin", "unpin":
		// Same argument convention as forget: target or text, target wins.
		wanted := target
		if wanted == "" {
			wanted = text
		}
		if wanted == "" {
			return toolError(fmt.Sprintf("the fact to %s is required: pass it in target (or text)", action))
		}
		facts, err := s.backend.List(agentID)
		if err != nil {
			return toolError(fmt.Sprintf("list facts: %v", err))
		}
		// Pin only live copies. Unpin also clears retired copies, preserving
		// the existing flag-cleanup behavior without changing their receipts.
		victims := findByText(facts, wanted, action == "unpin")
		if len(victims) == 0 {
			return toolError(fmt.Sprintf("target fact not found: %q", wanted))
		}
		if patcher, ok := s.backend.(interface {
			SetPinned(string, bool, ...memory.Fact) error
		}); ok {
			if err := patcher.SetPinned(agentID, action == "pin", victims...); err != nil {
				return toolError(fmt.Sprintf("%s failed: %v", action, err))
			}
			resultMsg = fmt.Sprintf("Fact %s for agent %q. Pinned facts are exempt from decay and pruning.", action, agentID)
			break
		}
		for _, victim := range victims {
			victim.Pinned = action == "pin"
			if victim.Pinned {
				victim.PinnedAt = time.Now().UTC()
			} else {
				victim.PinnedAt = time.Time{}
			}
			if err := s.backend.UpdateFact(agentID, victim); err != nil {
				return toolError(fmt.Sprintf("%s failed: %v", action, err))
			}
		}
		resultMsg = fmt.Sprintf("Fact %s for agent %q. Pinned facts are exempt from decay and pruning.", action, agentID)

	default:
		return toolError(fmt.Sprintf("unknown action %q", action))
	}

	_ = s.backend.AuditWrite(audit.Entry{
		Timestamp: time.Now().UTC(),
		Action:    action,
		Agent:     agentID,
		OldText:   oldText,
		NewText:   text,
		Source:    "agent_self",
	})

	return toolStructured(out, resultMsg+writeIdentityNotice(out.FactID, memory.EffectiveConfidence(out.Confidence)))
}

// findByText selects every exact match from one List snapshot: acting on just
// one duplicate leaves the same belief live or with inconsistent pin state.
// Retired receipts are excluded unless unpin requests flag cleanup; later
// writes of the same text are separate facts, outside this operation's snapshot.
func findByText(facts []memory.Fact, text string, includeRetired bool) []memory.Fact {
	var matches []memory.Fact
	for _, f := range facts {
		if f.Text == text && (includeRetired || !f.IsSuperseded()) {
			matches = append(matches, f)
		}
	}
	return matches
}

// supersedeFact marks a fact as retired so Recall skips it immediately.
// It preserves the weight so the tombstone remains inspectable while the
// row exists. Ordinary pruning can remove it when it meets the same
// weight and pin criteria as any other fact.
func (s *Server) supersedeFact(agentID string, f memory.Fact, supersededBy string) error {
	f.SupersededBy = supersededBy
	return s.backend.UpdateFact(agentID, f)
}

// handleMemorySearchBatch answers several queries in one tool call.
//
// Current backends execute the whole batch as one store operation. Legacy
// backends can still answer each query separately.
func (s *Server) handleMemorySearchBatch(ctx context.Context, agentID string, queries []string, topK int, options ...memory.RecallOptions) (*mcp.CallToolResult, error) {
	var opts memory.RecallOptions
	if len(options) > 0 {
		opts = options[0]
	}
	if err := opts.Validate(); err != nil {
		return toolError(err.Error())
	}
	opts, useOptions, err := batchRecallOptions(s.backend, opts)
	if err != nil {
		return toolError(fmt.Sprintf("recall error: %v", err))
	}
	confidenceMetadata := useOptions
	if _, supports := s.backend.(confidenceRecaller); supports && reviewJournalSupported(s.backend) && !useOptions {
		zero := 0.0
		opts.ConfidenceWeight = &zero
		useOptions = true
	}
	var scopedOptions *memory.RecallOptions
	if useOptions {
		scopedOptions = &opts
	}
	batch, err := s.recallBatch(ctx, agentID, queries, topK, scopedOptions)
	if err != nil && len(batch) == 0 {
		if errors.Is(err, memory.ErrConfidenceUnsupported) {
			return toolError(memory.ErrConfidenceUnsupported.Error())
		}
		batch = make([]memory.BatchResult, len(queries))
		for i, query := range queries {
			batch[i] = memory.BatchResult{Query: query, Err: err}
		}
	}
	for i := range batch {
		if batch[i].Err != nil {
			batch[i].Facts = nil
		} else if topK > 0 && topK < len(batch[i].Facts) {
			batch[i].Facts = batch[i].Facts[:topK]
		}
	}
	merged := memory.MergedFacts(batch)

	out := batchResult{AgentID: agentID, Count: len(merged), Merged: merged}
	for _, r := range batch {
		if len(r.Facts) > 0 {
			out.ReviewNotices = memory.MergeReviewNotices(out.ReviewNotices, r.ReviewNotices)
		}
	}
	for _, r := range batch {
		if confidenceMetadata && r.Retrieval != nil && (out.Retrieval == nil || r.Retrieval.VectorFallback != "") {
			out.Retrieval = r.Retrieval
			if r.Retrieval.VectorFallback != "" {
				break
			}
		}
	}
	for _, r := range batch {
		e := ""
		if r.Err != nil {
			e = r.Err.Error()
		}
		out.PerQuery = append(out.PerQuery, struct {
			Query string   `json:"query"`
			Facts []string `json:"facts"`
			Error string   `json:"error,omitempty"`
		}{Query: r.Query, Facts: r.Facts, Error: e})
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Answered %d queries for agent %q — %d distinct facts:\n\n", len(queries), agentID, len(merged))
	for i, f := range merged {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, f)
	}
	var failed []string
	for _, r := range batch {
		if r.Err != nil {
			failed = append(failed, fmt.Sprintf("%q: %v", r.Query, r.Err))
		}
	}
	if len(failed) > 0 {
		if len(failed) == len(queries) {
			return toolError("All recall queries failed: " + strings.Join(failed, "; "))
		}
		fmt.Fprintf(&sb, "\n%d of %d queries failed:\n  %s\n", len(failed), len(queries), strings.Join(failed, "\n  "))
	}
	if len(merged) == 0 && len(failed) == 0 {
		notice := fmt.Sprintf("No memories found for agent %q matching any of the %d queries.", agentID, len(queries))
		if out.Retrieval != nil {
			notice += "\n\n" + out.Retrieval.Text()
		}
		return toolStructured(out, notice)
	}
	if out.Retrieval != nil {
		sb.WriteString("\n" + out.Retrieval.Text() + "\n")
	}
	writeReviewNotices(&sb, out.ReviewNotices)
	return toolStructured(out, sb.String())
}

func (s *Server) recallBatch(ctx context.Context, agentID string, queries []string, topK int, opts *memory.RecallOptions) ([]memory.BatchResult, error) {
	if recaller, ok := s.backend.(memory.ScopedBatchRecaller); ok {
		rows, err := recaller.BatchRecallScoped(ctx, agentID, queries, topK, false, opts)
		if !errors.Is(err, memory.ErrBatchRecallUnsupported) {
			return rows, err
		}
	}

	backend, supports := s.backend.(confidenceRecaller)
	if opts != nil && !supports {
		return nil, memory.ErrConfidenceUnsupported
	}
	rows := make([]memory.BatchResult, len(queries))
	limit := min(runtime.GOMAXPROCS(0), len(queries))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, query := range queries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if opts != nil {
				result, err := backend.RecallWithOptions(ctx, agentID, query, topK, *opts)
				rows[i] = memory.BatchResult{Query: query, Facts: result.Facts, Retrieval: result.Retrieval, ReviewNotices: result.ReviewNotices, Err: err}
			} else {
				facts, err := s.backend.Recall(ctx, agentID, query, topK)
				rows[i] = memory.BatchResult{Query: query, Facts: facts, Err: err}
			}
		}()
	}
	wg.Wait()
	return rows, nil
}

// handleMemorySearchBatchTool is the memory_search_batch entry point: it reads
// the arguments and delegates to the shared fan-out.
func (s *Server) handleMemorySearchBatchTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	agentID, ok := getString(args, "agent_id")
	if !ok || agentID == "" {
		return toolError("agent_id is required")
	}
	queries := getStringSlice(args, "queries")
	if len(queries) == 0 {
		return toolError("queries is required and must hold at least one non-empty query")
	}
	opts, err := recallOptions(args)
	if err != nil {
		return toolError(err.Error())
	}
	return s.handleMemorySearchBatch(ctx, agentID, queries, getInt(args, "top_k", 0), opts)
}
