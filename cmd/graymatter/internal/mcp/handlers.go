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
		return toolText(fmt.Sprintf("No memories found for agent %q matching %q.", agentID, query))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d relevant memories for agent %q:\n\n", len(facts), agentID))
	for i, f := range facts {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, f))
	}
	return toolText(sb.String())
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

	return toolText(fmt.Sprintf("Memory stored for agent %q.", agentID))
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

	return toolText(fmt.Sprintf("Checkpoint %q saved for agent %q.", saved.ID, agentID))
}

func (s *Server) handleCheckpointResume(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	agentID, ok := getString(args, "agent_id")
	if !ok || agentID == "" {
		return toolError("agent_id is required")
	}

	cp, err := s.backend.CheckpointResume(agentID)
	if err != nil {
		return toolError(fmt.Sprintf("no checkpoint found for agent %q: %v", agentID, err))
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
	return toolText(sb.String())
}

func (s *Server) handleMemoryReflect(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	action, ok := getString(args, "action")
	if !ok || action == "" {
		return toolError("action is required")
	}
	agentID, ok := getString(args, "agent")
	if !ok || agentID == "" {
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
		facts, err := s.backend.List(agentID)
		if err != nil {
			return toolError(fmt.Sprintf("list facts: %v", err))
		}
		for _, f := range facts {
			if f.Text == target {
				oldText = f.Text
				f.Weight = 0
				_ = s.backend.UpdateFact(agentID, f)
				break
			}
		}
		if oldText == "" {
			return toolError(fmt.Sprintf("target fact not found: %q", target))
		}
		if err := s.backend.Remember(ctx, agentID, text); err != nil {
			return toolError(fmt.Sprintf("add updated fact: %v", err))
		}
		resultMsg = fmt.Sprintf("Updated fact for agent %q.", agentID)

	case "forget":
		// The fact to forget may arrive in target or text — both are
		// documented as equivalent; target wins when both are set.
		victim := target
		if victim == "" {
			victim = text
		}
		if victim == "" {
			return toolError("the fact to forget is required: pass it in target (or text)")
		}
		facts, err := s.backend.List(agentID)
		if err != nil {
			return toolError(fmt.Sprintf("list facts: %v", err))
		}
		for _, f := range facts {
			if f.Text == victim {
				oldText = f.Text
				f.Weight = 0
				_ = s.backend.UpdateFact(agentID, f)
				break
			}
		}
		if oldText == "" {
			return toolError(fmt.Sprintf("target fact not found: %q", victim))
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

	return toolText(resultMsg)
}
