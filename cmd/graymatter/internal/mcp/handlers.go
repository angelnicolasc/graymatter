package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
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

	if explain {
		return s.handleMemorySearchExplain(ctx, agentID, query, topK)
	}

	// RecallDetailed over Recall: same facts, same order — the second return
	// is the additive weak-match vocabulary block (empty when the match is
	// strong), which is what lets the agent reformulate once, informed,
	// instead of guessing N times.
	facts, feedback, err := s.backend.RecallDetailed(ctx, agentID, query, topK)
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
		return toolStructured(searchResult{AgentID: agentID, Query: query, Count: 0, Facts: []string{}, Feedback: feedback}, notice)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d relevant memories for agent %q:\n\n", len(facts), agentID))
	for i, f := range facts {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, f))
	}
	if feedback != "" {
		sb.WriteString("\n" + feedback + "\n")
	}
	return toolStructured(searchResult{AgentID: agentID, Query: query, Count: len(facts), Facts: facts, Feedback: feedback}, sb.String())
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
func (s *Server) handleMemorySearchExplain(ctx context.Context, agentID, query string, topK int) (*mcp.CallToolResult, error) {
	receipts, err := s.backend.RecallExplain(ctx, agentID, query, topK)
	if err != nil {
		return toolError(fmt.Sprintf("recall error: %v", err))
	}

	if len(receipts) == 0 {
		notice := fmt.Sprintf("No memories found for agent %q matching %q.", agentID, query)
		return toolStructured(searchResult{AgentID: agentID, Query: query, Count: 0, Facts: []string{}}, notice)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d relevant memories for agent %q (with receipts):\n\n", len(receipts), agentID))
	for i, r := range receipts {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, r.Text))
		sb.WriteString(fmt.Sprintf("   score %.4f = vector %d / keyword %d / recency %d (k %.0f) · weight %.3f · age %.1fd · written %s\n",
			r.Ranks.FusedScore, r.Ranks.VectorRank, r.Ranks.KeywordRank, r.Ranks.RecencyRank, r.Ranks.K,
			r.Weight, r.AgeDays, r.Provenance.WrittenAt.Format("2006-01-02")))
		sb.WriteString(fmt.Sprintf("   fact_id %s\n", r.Provenance.FactID))
		// The structured payload carries this under `explained`, but the agent
		// reads the prose. A value that replaced an earlier one is different
		// information from a value nobody ever questioned, and leaving it out
		// of the text is leaving it out.
		if n := len(r.Provenance.Supersedes); n > 0 {
			noun := "version"
			if n > 1 {
				noun = "versions"
			}
			sb.WriteString(fmt.Sprintf("   supersedes %d earlier %s: %s\n", n, noun,
				strings.Join(r.Provenance.Supersedes, ", ")))
		}
	}
	return toolStructured(searchResult{
		AgentID: agentID,
		Query:   query,
		Count:   len(receipts),
		// Explain mode carries the receipts under `explained`; the bare
		// facts array stays present-but-empty so the payload always
		// conforms to the declared output schema (null would not).
		Facts:     []string{},
		Explained: receipts,
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

	if err := s.backend.Remember(ctx, agentID, text); err != nil {
		return toolError(fmt.Sprintf("remember error: %v", err))
	}

	return toolStructured(addResult{AgentID: agentID, Stored: true}, fmt.Sprintf("Memory stored for agent %q.", agentID))
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

	cp, err := s.backend.CheckpointResume(agentID)
	if err != nil {
		// Typed not-found: structuredContent carries the machine-readable
		// error code, content keeps the historical prose, isError marks it.
		notice := fmt.Sprintf("no checkpoint found for agent %q: %v", agentID, err)
		res := mcp.NewToolResultStructured(checkpointResumeNotFound{Error: "not_found", AgentID: agentID}, notice)
		res.IsError = true
		return res, nil
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

func (s *Server) handleMemoryReflect(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	action, ok := getString(args, "action")
	if !ok || action == "" {
		return toolError("action is required")
	}
	// Since the canonical flip (issue #77 step 3, ADR-014) agent_id is the
	// canonical spelling and wins when both are set; `agent` remains accepted
	// as a deprecated alias so pre-flip callers keep working unchanged.
	agentID, ok := getString(args, "agent_id")
	if !ok || agentID == "" {
		agentID, _ = getString(args, "agent")
	}
	if agentID == "" {
		return toolError("agent_id is required")
	}
	// text and target are validated per-action below: forget works with
	// either one, so neither can be globally required (see PR #10).
	text, _ := getString(args, "text")
	target, _ := getString(args, "target")

	var oldText string
	var resultMsg string

	switch action {
	case "add":
		if text == "" {
			return toolError("text (the fact to add) is required for add")
		}
		if err := s.backend.Remember(ctx, agentID, text); err != nil {
			return toolError(fmt.Sprintf("add failed: %v", err))
		}
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
		victim, ok := findByText(facts, target)
		if !ok {
			return toolError(fmt.Sprintf("target fact not found: %q", target))
		}
		oldText = victim.Text

		// Write the correction before retiring what it corrects. The previous
		// order zeroed the old fact's weight first, so a failing Remember left
		// the agent with a retired fact and no replacement.
		writer, ok := s.backend.(interface {
			PutReturningFact(context.Context, string, string) (memory.Fact, error)
		})
		if !ok {
			return toolError("add updated fact: backend does not expose identity-preserving writes")
		}
		replacement, err := writer.PutReturningFact(ctx, agentID, text)
		if err != nil {
			return toolError(fmt.Sprintf("add updated fact: %v", err))
		}
		if replacement.ID == "" {
			return toolError("add updated fact: backend returned an empty fact identity")
		}

		// Point the tombstone at the replacement so the correction can be
		// followed later, rather than only showing that something was retired.
		if err := s.supersedeFact(agentID, victim, replacement.ID); err != nil {
			return toolError(fmt.Sprintf("supersede old fact: %v", err))
		}
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
		victim, ok := findByText(facts, wanted)
		if !ok {
			return toolError(fmt.Sprintf("target fact not found: %q", wanted))
		}
		oldText = victim.Text

		// Nothing replaces this one, so the tombstone records that an agent
		// decided to drop it.
		if err := s.supersedeFact(agentID, victim, memory.SupersededByAgent); err != nil {
			return toolError(fmt.Sprintf("suppress fact: %v", err))
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
		victim, ok := findByText(facts, wanted)
		if !ok {
			return toolError(fmt.Sprintf("target fact not found: %q", wanted))
		}
		// Pinning a retired fact would promise permanence for something that
		// is no longer live; unpinning one is harmless flag hygiene.
		if action == "pin" && victim.IsSuperseded() {
			return toolError("cannot pin a superseded fact")
		}
		victim.Pinned = action == "pin"
		if victim.Pinned {
			victim.PinnedAt = time.Now().UTC()
		} else {
			victim.PinnedAt = time.Time{}
		}
		if err := s.backend.UpdateFact(agentID, victim); err != nil {
			return toolError(fmt.Sprintf("%s failed: %v", action, err))
		}
		resultMsg = fmt.Sprintf("Fact %s for agent %q. Pinned facts are exempt from decay, pruning and summarisation.", action, agentID)

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

	return toolStructured(reflectResult{Action: action, Agent: agentID, OK: true}, resultMsg)
}

// findByText returns the first fact whose text matches exactly.
func findByText(facts []memory.Fact, text string) (memory.Fact, bool) {
	for _, f := range facts {
		if f.Text == text {
			return f, true
		}
	}
	return memory.Fact{}, false
}

// supersedeFact retires a fact: it is tombstoned so Recall skips it from
// the next query onward. Its weight is left exactly as decay left it —
// zeroing it here would drop it below the prune floor (<0.01), so the next
// consolidation cycle would collect the receipt milliseconds after writing
// it, destroying the audit trail ADR-007 keeps tombstones for. The fact
// itself is not deleted — storage is append-only, and an audit has to be
// able to see what was retired and why; ordinary decay collects the receipt
// in due course, on the same curve as every other fact.
func (s *Server) supersedeFact(agentID string, f memory.Fact, supersededBy string) error {
	f.SupersededBy = supersededBy
	return s.backend.UpdateFact(agentID, f)
}

// handleMemorySearchBatch answers several queries in one tool call.
//
// The fan-out lives here rather than in pkg/memory because the MCP server
// talks to the store through the Backend interface, which is either an
// in-process store or an RPC client to the daemon — both already safe for
// concurrent use, and both benefiting from the same fan-out. Putting it here
// means the daemon needs no new method and the wire protocol does not change.
//
// Per-query failures are reported per query. An agent asking six questions
// should get the five answers that worked rather than one error for all of
// them; a batch only fails outright if the whole store is unreachable, which
// each individual call would report anyway.
func (s *Server) handleMemorySearchBatch(ctx context.Context, agentID string, queries []string, topK int) (*mcp.CallToolResult, error) {
	type row struct {
		query string
		facts []string
		err   error
	}
	rows := make([]row, len(queries))

	limit := runtime.GOMAXPROCS(0)
	if limit > len(queries) {
		limit = len(queries)
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i].query = q
			facts, err := s.backend.Recall(ctx, agentID, q, topK)
			if err != nil {
				rows[i].err = err
				return
			}
			if topK > 0 && topK < len(facts) {
				facts = facts[:topK]
			}
			rows[i].facts = facts
		}(i, q)
	}
	wg.Wait()

	batch := make([]memory.BatchResult, 0, len(rows))
	for _, r := range rows {
		batch = append(batch, memory.BatchResult{Query: r.query, Facts: r.facts})
	}
	merged := memory.MergedFacts(batch)

	out := batchResult{AgentID: agentID, Count: len(merged), Merged: merged}
	for _, r := range rows {
		e := ""
		if r.err != nil {
			e = r.err.Error()
		}
		out.PerQuery = append(out.PerQuery, struct {
			Query string   `json:"query"`
			Facts []string `json:"facts"`
			Error string   `json:"error,omitempty"`
		}{Query: r.query, Facts: r.facts, Error: e})
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Answered %d queries for agent %q — %d distinct facts:\n\n", len(queries), agentID, len(merged))
	for i, f := range merged {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, f)
	}
	var failed []string
	for _, r := range rows {
		if r.err != nil {
			failed = append(failed, fmt.Sprintf("%q: %v", r.query, r.err))
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(&sb, "\n%d of %d queries failed:\n  %s\n", len(failed), len(queries), strings.Join(failed, "\n  "))
	}
	if len(merged) == 0 && len(failed) == 0 {
		return toolStructured(out, fmt.Sprintf("No memories found for agent %q matching any of the %d queries.", agentID, len(queries)))
	}
	return toolStructured(out, sb.String())
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
	return s.handleMemorySearchBatch(ctx, agentID, queries, getInt(args, "top_k", 0))
}
