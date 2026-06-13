// Package mcp exposes GrayMatter memory as a Model Context Protocol server.
// Claude Code, Cursor, and any MCP-compatible client can use the four tools:
//
//   - memory_search  — recall facts for a query
//   - memory_add     — store a new fact
//   - checkpoint_save   — snapshot agent state
//   - checkpoint_resume — restore last checkpoint
//
// Usage:
//
//	graymatter mcp serve            # stdio (default, used by Claude Code)
//	graymatter mcp serve --http :8080  # StreamableHTTP
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const (
	serverName    = "graymatter"
	serverVersion = "0.1.0"
)

// Backend is the persistence surface the MCP handlers need. Two
// implementations exist: the daemon client (default — lets several MCP
// hosts and the TUI share one store, issue #8) and DirectBackend
// (in-process, for --no-daemon and tests).
type Backend interface {
	Remember(ctx context.Context, agentID, text string) error
	// Recall with topK<=0 uses the store's configured default.
	Recall(ctx context.Context, agentID, query string, topK int) ([]string, error)
	List(agentID string) ([]memory.Fact, error)
	UpdateFact(agentID string, f memory.Fact) error
	CheckpointSave(cp session.Checkpoint) (session.Checkpoint, error)
	CheckpointResume(agentID string) (*session.Checkpoint, error)
	AuditWrite(e audit.Entry) error
	KGLink(from, to, relation string) error
}

// KGLinker is a narrow interface for creating knowledge-graph edges.
// Implemented by *kg.GraphAdapter in production.
type KGLinker interface {
	LinkNodes(from, to, relation string) error
	UpsertNode(id, label, entityType string) error
}

// Server wraps mcp-go with GrayMatter memory handlers.
type Server struct {
	backend Backend
	mcpSrv  *server.MCPServer
}

// New creates a configured MCP server on top of backend.
func New(backend Backend) *Server {
	s := &Server{backend: backend}
	s.mcpSrv = server.NewMCPServer(serverName, serverVersion,
		server.WithToolCapabilities(true),
	)
	s.registerTools()
	return s
}

// DirectBackend implements Backend against an in-process Memory. The KG
// linker is optional; without it the link action reports unavailability.
type DirectBackend struct {
	mem      *graymatter.Memory
	kgLinker KGLinker
}

// NewDirectBackend wraps mem (and an optional kg linker) as a Backend.
func NewDirectBackend(mem *graymatter.Memory, kgLinker KGLinker) *DirectBackend {
	return &DirectBackend{mem: mem, kgLinker: kgLinker}
}

func (b *DirectBackend) Remember(ctx context.Context, agentID, text string) error {
	return b.mem.Remember(ctx, agentID, text)
}

func (b *DirectBackend) Recall(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	if topK <= 0 {
		return b.mem.Recall(ctx, agentID, query)
	}
	store := b.mem.Advanced()
	if store == nil {
		return nil, errors.New("memory store not initialised")
	}
	return store.Recall(ctx, agentID, query, topK)
}

func (b *DirectBackend) List(agentID string) ([]memory.Fact, error) {
	store := b.mem.Advanced()
	if store == nil {
		return nil, errors.New("memory store not initialised")
	}
	return store.List(agentID)
}

func (b *DirectBackend) UpdateFact(agentID string, f memory.Fact) error {
	store := b.mem.Advanced()
	if store == nil {
		return errors.New("memory store not initialised")
	}
	return store.UpdateFact(agentID, f)
}

func (b *DirectBackend) CheckpointSave(cp session.Checkpoint) (session.Checkpoint, error) {
	store := b.mem.Advanced()
	if store == nil {
		return session.Checkpoint{}, errors.New("memory store not initialised")
	}
	return session.Save(store.DB(), cp)
}

func (b *DirectBackend) CheckpointResume(agentID string) (*session.Checkpoint, error) {
	store := b.mem.Advanced()
	if store == nil {
		return nil, errors.New("memory store not initialised")
	}
	return session.Resume(store.DB(), agentID)
}

func (b *DirectBackend) AuditWrite(e audit.Entry) error {
	store := b.mem.Advanced()
	if store == nil {
		return nil
	}
	audit.Write(store.DB(), e)
	return nil
}

func (b *DirectBackend) KGLink(from, to, relation string) error {
	if b.kgLinker == nil {
		return errors.New("knowledge graph not available in this server instance")
	}
	return b.kgLinker.LinkNodes(from, to, relation)
}

// ServeStdio starts the MCP server over stdin/stdout (used by Claude Code).
// Blocks until the client disconnects.
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.mcpSrv)
}

// ServeHTTP starts the MCP server over StreamableHTTP on addr (e.g. ":8080").
func (s *Server) ServeHTTP(addr string) error {
	h := server.NewStreamableHTTPServer(s.mcpSrv)
	fmt.Printf("graymatter MCP server listening on %s\n", addr)
	return http.ListenAndServe(addr, h)
}

func (s *Server) registerTools() {
	// memory_search
	s.mcpSrv.AddTool(
		mcp.NewTool("memory_search",
			mcp.WithDescription("Search GrayMatter memory for relevant facts."),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The agent whose memory to search."),
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Natural-language query to match against stored facts."),
			),
			mcp.WithNumber("top_k",
				mcp.Description("Maximum number of facts to return (default: 8)."),
			),
		),
		s.handleMemorySearch,
	)

	// memory_add
	s.mcpSrv.AddTool(
		mcp.NewTool("memory_add",
			mcp.WithDescription("Store a new fact in GrayMatter memory."),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The agent to associate this memory with."),
			),
			mcp.WithString("text",
				mcp.Required(),
				mcp.Description("The observation or fact to remember."),
			),
		),
		s.handleMemoryAdd,
	)

	// checkpoint_save
	s.mcpSrv.AddTool(
		mcp.NewTool("checkpoint_save",
			mcp.WithDescription("Save a checkpoint of current agent state."),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The agent to checkpoint."),
			),
			mcp.WithString("state",
				mcp.Description("Optional JSON object with arbitrary state to persist."),
			),
		),
		s.handleCheckpointSave,
	)

	// checkpoint_resume
	s.mcpSrv.AddTool(
		mcp.NewTool("checkpoint_resume",
			mcp.WithDescription("Restore the latest checkpoint for an agent."),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The agent whose checkpoint to restore."),
			),
		),
		s.handleCheckpointResume,
	)

	// memory_reflect
	s.mcpSrv.AddTool(
		mcp.NewTool("memory_reflect",
			mcp.WithDescription("Update your own knowledge graph mid-session. Use when you discover a contradiction, complete a task, or learn a user preference that should persist."),
			mcp.WithString("action",
				mcp.Required(),
				mcp.Description("One of: add, update, forget, link."),
				mcp.Enum("add", "update", "forget", "link"),
			),
			mcp.WithString("agent",
				mcp.Required(),
				mcp.Description("The agent whose memory to modify."),
			),
			mcp.WithString("text",
				mcp.Description("The fact text for add/update, the fact to forget (alternative to target), or the source node ID for link."),
			),
			mcp.WithString("target",
				mcp.Description("For update: the fact text to supersede. For forget: the fact to remove (or pass it via text). For link: the target node ID."),
			),
		),
		s.handleMemoryReflect,
	)
}

// toolError wraps an error as an MCP tool result with isError=true.
func toolError(msg string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(msg), nil
}

// toolText wraps a string as a successful MCP tool result.
func toolText(text string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText(text), nil
}

// getString extracts a required string argument from MCP tool call arguments.
func getString(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// getInt extracts an optional integer argument, returning def if absent.
func getInt(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return def
}

// Ensure context is used.
var _ context.Context = context.Background()
