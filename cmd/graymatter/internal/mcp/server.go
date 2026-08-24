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
//	graymatter mcp serve                            # stdio (default, used by Claude Code)
//	graymatter mcp serve --http 127.0.0.1:8080      # StreamableHTTP, token required
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/httpauth"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const (
	serverName = "graymatter"

	httpReadHeaderTimeout = 15 * time.Second
	httpIdleTimeout       = 120 * time.Second
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
	version string
	mcpSrv  *server.MCPServer
}

// New creates a configured MCP server on top of backend, announcing version
// in the initialize handshake.
//
// The version is a parameter rather than a constant here on purpose. It used
// to be `const serverVersion`, bumped by hand at release time, and it was
// right in 2 of the first 17 releases: every client that connected to a
// v0.11.x or v0.12.x binary was told it was talking to 0.10.0. Taking it from
// the caller means there is only one version string in the binary, so there
// is nothing left to keep in sync.
func New(backend Backend, version string) *Server {
	s := &Server{backend: backend, version: version}
	s.mcpSrv = server.NewMCPServer(serverName, version,
		server.WithToolCapabilities(true),
	)
	s.registerTools()
	return s
}

// Version reports what this server announces to clients.
func (s *Server) Version() string { return s.version }

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
	return audit.Write(store.DB(), e)
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

// HTTPOption customises the HTTP transport. Without WithHTTPAuthToken or
// WithHTTPAnonymousAccess the server has no credential to match and rejects
// every request, so a wiring mistake fails closed rather than republishing the
// store to the network.
type HTTPOption func(*httpOptions)

type httpOptions struct {
	token     string
	anonymous bool
}

// WithHTTPAuthToken requires callers to present token as an HTTP bearer
// credential, compared in constant time.
func WithHTTPAuthToken(token string) HTTPOption {
	return func(o *httpOptions) { o.token = token }
}

// WithHTTPAnonymousAccess serves the MCP transport with no credential check.
// The caller is responsible for keeping the listener loopback-only.
func WithHTTPAnonymousAccess() HTTPOption {
	return func(o *httpOptions) { o.anonymous = true }
}

// ServeHTTP starts the MCP server over StreamableHTTP on addr
// (e.g. "127.0.0.1:8080").
//
// The transport used to mount the mcp-go handler straight onto
// http.ListenAndServe, so the whole tool surface — memory_add, memory_search,
// memory_reflect, both checkpoint tools — answered to anyone who could reach
// the port. The Mcp-Session-Id looked like a barrier, but the server hands one
// to every caller during initialize. The bearer gate runs first now, so a
// caller without the token never reaches the handshake.
//
// The timeouts are what any exposed listener needs: a slow or idle client
// should not hold a connection open indefinitely. There is no write timeout
// because MCP streams responses.
func (s *Server) ServeHTTP(addr string, opts ...HTTPOption) error {
	var cfg httpOptions
	for _, opt := range opts {
		opt(&cfg)
	}

	var h http.Handler = server.NewStreamableHTTPServer(s.mcpSrv)
	if !cfg.anonymous {
		h = httpauth.Middleware(cfg.token, h)
	}

	fmt.Printf("graymatter MCP server listening on %s\n", addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
	return srv.ListenAndServe()
}

// Tool annotations. Clients read these hints to decide whether a call needs
// user approval, so the defaults matter: mcp-go's NewTool marks every tool
// destructive, non-idempotent and open-world, which makes hosts gate even a
// plain lookup behind a confirmation prompt — and an agent running unattended
// then never calls it. Every GrayMatter tool works against the local store, so
// openWorldHint is false throughout.

// readOnlyTool annotates a tool that only reads. memory_search does bump access
// counters for recency scoring, but that is internal bookkeeping the caller
// cannot observe, so it stays read-only from the client's point of view.
func readOnlyTool() mcp.ToolOption {
	return mcp.WithToolAnnotation(mcp.ToolAnnotation{
		ReadOnlyHint:    mcp.ToBoolPtr(true),
		DestructiveHint: mcp.ToBoolPtr(false),
		IdempotentHint:  mcp.ToBoolPtr(true),
		OpenWorldHint:   mcp.ToBoolPtr(false),
	})
}

// writeTool annotates a tool that writes. destructive is true only where a call
// can remove or supersede an existing fact; appending a fact or a checkpoint is
// additive. Nothing here is idempotent: every call stores a new record.
func writeTool(destructive bool) mcp.ToolOption {
	return mcp.WithToolAnnotation(mcp.ToolAnnotation{
		ReadOnlyHint:    mcp.ToBoolPtr(false),
		DestructiveHint: mcp.ToBoolPtr(destructive),
		IdempotentHint:  mcp.ToBoolPtr(false),
		OpenWorldHint:   mcp.ToBoolPtr(false),
	})
}

func (s *Server) registerTools() {
	// memory_search
	s.mcpSrv.AddTool(
		mcp.NewTool("memory_search",
			mcp.WithDescription("Search GrayMatter memory for relevant facts."),
			readOnlyTool(),
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
			writeTool(false),
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
			writeTool(false),
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
			readOnlyTool(),
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
			writeTool(true),
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
