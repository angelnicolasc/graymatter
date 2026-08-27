package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

	facts, err := s.backend.Recall(ctx, agentID, query, topK)
	if err != nil {
		return toolError(fmt.Sprintf("recall error: %v", err))
	}

	if topK > 0 && topK < len(facts) {
		facts = facts[:topK]
	}

	if len(facts) == 0 {
		notice := fmt.Sprintf("No memories found for agent %q matching %q.", agentID, query)
		return toolStructured(searchResult{AgentID: agentID, Query: query, Count: 0, Facts: []string{}}, notice)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d relevant memories for agent %q:\n\n", len(facts), agentID))
	for i, f := range facts {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, f))
	}
	return toolStructured(searchResult{AgentID: agentID, Query: query, Count: len(facts), Facts: facts}, sb.String())
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
	// The schema names this parameter `agent`, but the other four tools use
	// `agent_id` and models generalize across a toolset — an alias costs one
	// line and saves the silent "agent is required" failure. Canonical stays
	// `agent`; docs/AGENTS.md documents both as accepted.
	agentID, ok := getString(args, "agent")
	if !ok || agentID == "" {
		agentID, _ = getString(args, "agent_id")
	}
	if agentID == "" {
		return toolError("agent is required")
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
		before, err := s.backend.List(agentID)
		if err != nil {
			return toolError(fmt.Sprintf("list facts: %v", err))
		}
		victim, ok := findByText(before, target)
		if !ok {
			return toolError(fmt.Sprintf("target fact not found: %q", target))
		}
		oldText = victim.Text

		// Write the correction before retiring what it corrects. The previous
		// order zeroed the old fact's weight first, so a failing Remember left
		// the agent with a retired fact and no replacement.
		if err := s.backend.Remember(ctx, agentID, text); err != nil {
			return toolError(fmt.Sprintf("add updated fact: %v", err))
		}

		// Point the tombstone at the replacement so the correction can be
		// followed later, rather than only showing that something was retired.
		replacementID := newFactID(s.backend, agentID, before)
		if replacementID == "" {
			replacementID = memory.SupersededByAgent
		}
		if err := s.supersedeFact(agentID, victim, replacementID); err != nil {
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

// newFactID returns the ID of the fact added since the `before` snapshot was
// taken. Matching on identity rather than text keeps it correct when the new
// fact repeats wording that is already stored. Returns "" if the new fact
// cannot be identified — the caller falls back to a generic marker rather than
// leaving the correction untombstoned.
func newFactID(backend Backend, agentID string, before []memory.Fact) string {
	after, err := backend.List(agentID)
	if err != nil {
		return ""
	}
	known := make(map[string]bool, len(before))
	for _, f := range before {
		known[f.ID] = true
	}
	for _, f := range after {
		if !known[f.ID] {
			return f.ID
		}
	}
	return ""
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
