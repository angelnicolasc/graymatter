// Package rpc provides a net/rpc + JSON transport that exposes the
// AdvancedStore surface of pkg/memory across processes.
//
// The package is intentionally narrow:
//
//   - Server wraps an in-process *memory.Store and serves it over a
//     local socket (Unix domain socket on POSIX, TCP loopback on Windows).
//   - Client dials the server and provides typed methods that mirror
//     AdvancedStore (minus DB(), which cannot cross a process boundary).
//
// The wire format is JSON via net/rpc/jsonrpc — stdlib only, zero new
// transitive dependencies.
//
// This is the foundation layer for the v0.6.0 daemon mode tracked in
// issue #8. It is deliberately consumer-agnostic: the TUI, MCP server,
// CLI subcommands, and plugin host migrate to this client in subsequent
// changes.
package rpc

import "github.com/angelnicolasc/graymatter/pkg/memory"

// ServiceName is the net/rpc service name registered by Server.
// Clients call methods as "{ServiceName}.{Method}".
const ServiceName = "GrayMatter"

// PutRequest is the wire form of AdvancedStore.Put.
type PutRequest struct {
	AgentID string
	Text    string
}

// PutResponse carries no data; presence indicates success.
type PutResponse struct{}

// PutSharedRequest is the wire form of AdvancedStore.PutShared.
type PutSharedRequest struct {
	Text string
}

// PutSharedResponse carries no data.
type PutSharedResponse struct{}

// RecallRequest is the wire form of AdvancedStore.Recall.
type RecallRequest struct {
	AgentID string
	Query   string
	TopK    int
}

// RecallResponse carries the recalled facts in ranked order.
type RecallResponse struct {
	Facts []string
}

// RecallSharedRequest is the wire form of AdvancedStore.RecallShared.
type RecallSharedRequest struct {
	Query string
	TopK  int
}

// RecallSharedResponse carries the recalled shared facts in ranked order.
type RecallSharedResponse struct {
	Facts []string
}

// RecallAllRequest is the wire form of Store.RecallAll (agent + shared
// namespaces merged and deduplicated).
type RecallAllRequest struct {
	AgentID string
	Query   string
	TopK    int
}

// RecallAllResponse carries the merged facts in ranked order.
type RecallAllResponse struct {
	Facts []string
}

// ListRequest is the wire form of AdvancedStore.List.
type ListRequest struct {
	AgentID string
}

// ListResponse carries every fact for an agent, newest first.
type ListResponse struct {
	Facts []memory.Fact
}

// ListAgentsRequest is the wire form of AdvancedStore.ListAgents.
// It carries no fields; the empty struct is required by net/rpc.
type ListAgentsRequest struct{}

// ListAgentsResponse carries every known agent ID.
type ListAgentsResponse struct {
	AgentIDs []string
}

// StatsRequest is the wire form of AdvancedStore.Stats.
type StatsRequest struct {
	AgentID string
}

// StatsResponse carries aggregate fact statistics.
type StatsResponse struct {
	Stats memory.MemoryStats
}

// DeleteRequest is the wire form of AdvancedStore.Delete.
type DeleteRequest struct {
	AgentID string
	FactID  string
}

// DeleteResponse carries no data.
type DeleteResponse struct{}

// UpdateFactRequest is the wire form of AdvancedStore.UpdateFact.
type UpdateFactRequest struct {
	AgentID string
	Fact    memory.Fact
}

// UpdateFactResponse carries no data.
type UpdateFactResponse struct{}

// ConsolidateRequest triggers server-side consolidation for an agent.
//
// Note: unlike AdvancedStore.Consolidate, the wire form does NOT carry a
// ConsolidateConfig. The daemon owns its consolidation policy and uses
// its own configuration. Clients cannot override it across the wire.
type ConsolidateRequest struct {
	AgentID string
}

// ConsolidateResponse carries no data.
type ConsolidateResponse struct{}

// PendingVectorCountRequest is the wire form of
// AdvancedStore.PendingVectorCount.
type PendingVectorCountRequest struct{}

// PendingVectorCountResponse carries the pending count.
type PendingVectorCountResponse struct {
	Count int
}

// PingRequest is a liveness probe used by Client.Dial to verify the server
// is reachable before the client returns. Carries no fields.
type PingRequest struct{}

// PingResponse echoes the server's protocol version.
type PingResponse struct {
	Protocol string
}

// Protocol identifies the wire-format version. Bump on breaking changes;
// clients refuse to talk to a server with a mismatched protocol string.
const Protocol = "graymatter-rpc/1"
