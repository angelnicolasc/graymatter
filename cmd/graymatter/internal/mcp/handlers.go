package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	bolt "go.etcd.io/bbolt"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

var bucketAudit = []byte("kg_audit")

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
	topK := getInt(args, "top_k", s.mem.Config().TopK)

	facts, err := s.mem.Recall(ctx, agentID, query)
	if err != nil {
		return toolError(fmt.Sprintf("recall error: %v", err))
	}

	if topK < len(facts) {
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

	if err := s.mem.Remember(ctx, agentID, text); err != nil {
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

	store := s.mem.Advanced()
	if store == nil {
		return toolError("memory store not initialised")
	}

	cp := session.Checkpoint{
		AgentID:   agentID,
		CreatedAt: time.Now().UTC(),
		State:     state,
		Metadata:  map[string]string{"source": "mcp"},
	}
	saved, err := session.Save(store.DB(), cp)
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

	store := s.mem.Advanced()
	if store == nil {
		return toolError("memory store not initialised")
	}

	cp, err := session.Resume(store.DB(), agentID)
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
	text, ok := getString(args, "text")
	if !ok || text == "" {
		return toolError("text is required")
	}
	target, _ := getString(args, "target")

	store := s.mem.Advanced()
	if store == nil {
		return toolError("memory store not initialised")
	}

	var oldText string
	var resultMsg string

	switch action {
	case "add":
		if err := s.mem.Remember(ctx, agentID, text); err != nil {
			return toolError(fmt.Sprintf("add failed: %v", err))
		}
		resultMsg = fmt.Sprintf("Added fact for agent %q.", agentID)

	case "update":
		if target == "" {
			return toolError("target (fact to supersede) is required for update")
		}
		facts, err := store.List(agentID)
		if err != nil {
			return toolError(fmt.Sprintf("list facts: %v", err))
		}
		for _, f := range facts {
			if f.Text == target {
				oldText = f.Text
				f.Weight = 0
				_ = store.UpdateFact(agentID, f)
				break
			}
		}
		if oldText == "" {
			return toolError(fmt.Sprintf("target fact not found: %q", target))
		}
		if err := s.mem.Remember(ctx, agentID, text); err != nil {
			return toolError(fmt.Sprintf("add updated fact: %v", err))
		}
		resultMsg = fmt.Sprintf("Updated fact for agent %q.", agentID)

	case "forget":
		if target == "" {
			return toolError("target (fact to forget) is required for forget")
		}
		facts, err := store.List(agentID)
		if err != nil {
			return toolError(fmt.Sprintf("list facts: %v", err))
		}
		for _, f := range facts {
			if f.Text == target {
				oldText = f.Text
				f.Weight = 0
				_ = store.UpdateFact(agentID, f)
				break
			}
		}
		if oldText == "" {
			return toolError(fmt.Sprintf("target fact not found: %q", target))
		}
		resultMsg = fmt.Sprintf("Fact suppressed for agent %q.", agentID)

	case "link":
		if target == "" {
			return toolError("target (node ID) is required for link")
		}
		if s.kgLinker == nil {
			return toolError("knowledge graph not available in this server instance")
		}
		fromID := strings.ToLower(strings.TrimSpace(text))
		toID := strings.ToLower(strings.TrimSpace(target))
		if err := s.kgLinker.LinkNodes(fromID, toID, "agent_link"); err != nil {
			return toolError(fmt.Sprintf("link nodes: %v", err))
		}
		resultMsg = fmt.Sprintf("Linked %q → %q.", fromID, toID)

	default:
		return toolError(fmt.Sprintf("unknown action %q", action))
	}

	writeAuditEntry(store.DB(), auditEntry{
		Timestamp: time.Now().UTC(),
		Action:    action,
		Agent:     agentID,
		OldText:   oldText,
		NewText:   text,
		Source:    "agent_self",
	})

	return toolText(resultMsg)
}

type auditEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Agent     string    `json:"agent"`
	OldText   string    `json:"old_text,omitempty"`
	NewText   string    `json:"new_text"`
	Source    string    `json:"source"`
}

func writeAuditEntry(db *bolt.DB, entry auditEntry) {
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	key := []byte(entry.Timestamp.Format(time.RFC3339Nano) + "_" + entry.Action + "_" + entry.Agent)
	_ = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketAudit)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}
