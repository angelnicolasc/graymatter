package mcp

// Result payloads mirrored as structuredContent alongside the human-readable
// text content (issue #76, ADR-013). Per the MCP spec, a tool returning
// structured content SHOULD also return functionally equivalent unstructured
// content — every handler keeps its exact pre-existing text as the fallback,
// so text-parsing clients are unaffected. Each type below generates the tool's
// outputSchema via mcp-go's WithOutputSchema[T]; the JSON tags are the wire
// contract and the schema property names, so they may not be renamed without
// a major-version conversation.

type searchResult struct {
	AgentID string   `json:"agent_id"`
	Query   string   `json:"query"`
	Count   int      `json:"count"`
	Facts   []string `json:"facts"`
}

type addResult struct {
	AgentID string `json:"agent_id"`
	Stored  bool   `json:"stored"`
}

type checkpointSaveResult struct {
	AgentID      string `json:"agent_id"`
	CheckpointID string `json:"checkpoint_id"`
	CreatedAt    string `json:"created_at"`
}

type checkpointResumeResult struct {
	ID           string         `json:"id"`
	CreatedAt    string         `json:"created_at"`
	State        map[string]any `json:"state,omitempty"`
	MessageCount int            `json:"message_count,omitempty"`
}

// checkpointResumeNotFound is the typed error payload for resume with no
// checkpoint. It travels with isError=true and the same prose fallback the
// tool has always returned, so nothing that matched the old error breaks.
type checkpointResumeNotFound struct {
	Error   string `json:"error"`
	AgentID string `json:"agent_id"`
}

type reflectResult struct {
	Action string `json:"action"`
	Agent  string `json:"agent"`
	OK     bool   `json:"ok"`
}
