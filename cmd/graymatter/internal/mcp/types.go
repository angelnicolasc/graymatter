package mcp

import "github.com/angelnicolasc/graymatter/pkg/memory"

// Result payloads mirrored as structuredContent alongside the human-readable
// text content (issue #76, ADR-013). Per the MCP spec, a tool returning
// structured content SHOULD also return functionally equivalent unstructured
// content — every handler keeps its exact pre-existing text as the fallback,
// so text-parsing clients are unaffected. Each type below generates the tool's
// outputSchema via mcp-go's WithOutputSchema[T]; the JSON tags are the wire
// contract and the schema property names, so they may not be renamed without
// a major-version conversation.

// batchResult is what memory_search returns when the caller passes `queries`.
// Merged comes first on purpose: a caller that just wants context to read can
// take it and ignore the per-query breakdown, and a fact that answered three
// of the questions appears once rather than three times.
type batchResult struct {
	AgentID  string   `json:"agent_id"`
	Count    int      `json:"count"`
	Merged   []string `json:"merged"`
	PerQuery []struct {
		Query string   `json:"query"`
		Facts []string `json:"facts"`
		Error string   `json:"error,omitempty"`
	} `json:"per_query"`
}

type searchResult struct {
	AgentID string   `json:"agent_id"`
	Query   string   `json:"query"`
	Count   int      `json:"count"`
	Facts   []string `json:"facts"`
	// Feedback carries the weak-match vocabulary block (v0.18.0): additive
	// text emitted when the query's vocabulary barely overlaps the store's.
	// Omitted when the match is strong, so the bare shape is unchanged.
	Feedback string `json:"feedback,omitempty"`
	// Explained carries the per-fact receipts when memory_search was called
	// with explain=true (v0.17.0). Omitted otherwise, so the schema stays
	// additive: the property is optional and the bare shape is unchanged.
	// The receipt type is pkg/memory's RecallReceipt verbatim — one struct,
	// so the MCP wire shape and `graymatter recall --explain --json` cannot
	// drift apart.
	Explained []memory.RecallReceipt `json:"explained,omitempty"`
}

// aliasResult is what memory_alias returns: the stored vocabulary mapping.
type aliasResult struct {
	AgentID     string   `json:"agent_id"`
	Term        string   `json:"term"`
	Equivalents []string `json:"equivalents"`
	Stored      bool     `json:"stored"`
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
