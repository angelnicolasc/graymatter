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
	"encoding/json"
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
	// RecallExplain is Recall's ranking with per-fact receipts (v0.17.0).
	// topK<=0 uses the store's configured default, like Recall.
	RecallExplain(ctx context.Context, agentID, query string, topK int) ([]memory.RecallReceipt, error)
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
// and the session instructions (see instructions.go) in the initialize
// handshake.
//
// The version is a parameter rather than a constant here on purpose. It used
// to be `const serverVersion`, bumped by hand at release time, and it was
// right in 2 of the first 17 releases: every client that connected to a
// v0.11.x or v0.12.x binary was told it was talking to 0.10.0. Taking it from
// the caller means there is only one version string in the binary, so there
// is nothing left to keep in sync.
func New(backend Backend, version string, opts ...Option) *Server {
	cfg := defaultServerOptions()
	for _, opt := range opts {
		opt(&cfg)
	}
	s := &Server{backend: backend, version: version}
	s.mcpSrv = server.NewMCPServer(serverName, version,
		server.WithToolCapabilities(true),
		server.WithInstructions(cfg.instructions),
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

// RecallExplain reaches the concrete store: AdvancedStore deliberately does
// not grow a method for every retrieval variant, and the concrete store
// implements this one (same pattern DirectBackend's peers use for RecallAll).
func (b *DirectBackend) RecallExplain(ctx context.Context, agentID, query string, topK int) ([]memory.RecallReceipt, error) {
	if topK <= 0 {
		topK = b.mem.Config().TopK
	}
	store := b.mem.Advanced()
	if store == nil {
		return nil, errors.New("memory store not initialised")
	}
	if re, ok := store.(interface {
		RecallExplain(ctx context.Context, agentID, query string, topK int) ([]memory.RecallReceipt, error)
	}); ok {
		return re.RecallExplain(ctx, agentID, query, topK)
	}
	return nil, errors.New("memory store does not expose RecallExplain")
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

// writeTool annotates a tool that writes. Nothing here is idempotent: every
// call stores a new record.
//
// memory_reflect passes destructive=false even though its forget/update
// actions can retire a fact. The annotation is per-tool and three of its four
// actions are purely additive, so advertising destructive would push hosts
// into gating every self-edit behind an approval prompt — including plain
// adds — which is how unattended agents quietly stop calling the tool. The
// guardrail lives in the handler instead, where it is testable: forget
// requires an exact-text match and both retiring actions leave a tombstone
// pointing at the replacement (never a hard delete), with every action
// recorded in the audit trail.
func writeTool() mcp.ToolOption {
	return mcp.WithToolAnnotation(mcp.ToolAnnotation{
		ReadOnlyHint:    mcp.ToBoolPtr(false),
		DestructiveHint: mcp.ToBoolPtr(false),
		IdempotentHint:  mcp.ToBoolPtr(false),
		OpenWorldHint:   mcp.ToBoolPtr(false),
	})
}

func (s *Server) registerTools() {
	// memory_search
	s.mcpSrv.AddTool(
		mcp.NewTool("memory_search",
			mcp.WithToolTitle("Search agent memory"),
			mcp.WithDescription("Search one agent's stored facts and return the most relevant ones ranked by hybrid recall (semantic + keyword + recency). Call once with your agent_id, then again with agent_id \"__shared__\" for project-wide conventions. Returns a numbered list with a count header, or a \"No memories found\" notice when nothing matches; superseded and decayed facts never appear. To store new facts use memory_add; to correct, remove, or pin existing ones use memory_reflect."),
			readOnlyTool(),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("Stable agent identifier, e.g. \"graymatter-backend\". Pass \"__shared__\" for project-wide memory."),
			),
			mcp.WithString("query",
				mcp.Required(),
				mcp.Description("Natural-language query; matching is hybrid (semantic + keyword + recency)."),
			),
			mcp.WithNumber("top_k",
				mcp.Description("Optional cap on returned facts. Omitted or non-positive uses the store default."),
				mcp.DefaultNumber(8),
			),
			mcp.WithBoolean("explain",
				mcp.Description("Set true to receive per-fact receipts instead of bare text: each fact under `explained` carries the per-signal ranks (vector/keyword/recency, 0 = signal absent) that produced its fused RRF score, the stored weight, its age in days, and provenance (fact_id, written_at, tombstone state). The ranking is identical either way — explain only reads it out. Use it when the caller wants to know WHY a fact was returned."),
			),
			outputSchemaOf[searchResult](),
		),
		s.handleMemorySearch,
	)

	// memory_add
	s.mcpSrv.AddTool(
		mcp.NewTool("memory_add",
			mcp.WithToolTitle("Store a durable fact"),
			mcp.WithDescription("Store one durable fact for an agent so future sessions can recall it. Use for conclusions worth keeping — decisions, preferences, gotchas — one atomic sentence per call, not transcripts or conversation logs. To correct or retire an existing fact use memory_reflect instead. Nothing is hard-deleted: untouched facts decay on a 30-day half-life unless pinned. Returns a confirmation naming the agent."),
			writeTool(),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("Stable agent identifier to associate this fact with."),
			),
			mcp.WithString("text",
				mcp.Required(),
				mcp.Description("The fact to remember: one atomic, self-contained sentence."),
			),
			outputSchemaOf[addResult](),
		),
		s.handleMemoryAdd,
	)

	// checkpoint_save
	s.mcpSrv.AddTool(
		mcp.NewTool("checkpoint_save",
			mcp.WithToolTitle("Save a session checkpoint"),
			mcp.WithDescription("Persist a snapshot of an agent's working state so a later session can pick the task back up with checkpoint_resume. Use before major refactors or when ending a session mid-task. Each save keeps its own ID and nothing is overwritten; checkpoint_resume returns the latest one. state must be a valid JSON object and is rejected otherwise. Returns a confirmation containing the new checkpoint ID."),
			writeTool(),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The agent to checkpoint."),
			),
			mcp.WithString("state",
				mcp.Description("Optional JSON object persisted with the checkpoint, e.g. {\"branch\": \"main\"}. Values that do not parse as an object are rejected."),
			),
			outputSchemaOf[checkpointSaveResult](),
		),
		s.handleCheckpointSave,
	)

	// checkpoint_resume
	s.mcpSrv.AddTool(
		mcp.NewTool("checkpoint_resume",
			mcp.WithToolTitle("Load the latest checkpoint"),
			mcp.WithDescription("Read an agent's most recent checkpoint without modifying anything: returns its ID, creation time (RFC3339), and the saved state as indented JSON, plus a message-turn count when messages were captured. Use at session start to detect and resume interrupted work; checkpoints are created with checkpoint_save. Errors with \"no checkpoint found\" when the agent has none."),
			readOnlyTool(),
			mcp.WithString("agent_id",
				mcp.Required(),
				mcp.Description("The agent whose latest checkpoint to load."),
			),
			outputSchemaOf[checkpointResumeResult](),
		),
		s.handleCheckpointResume,
	)

	// memory_reflect
	//
	// The input schema is hand-authored raw JSON because the contract cannot
	// be expressed with typed helpers: exactly one of agent_id (canonical) or
	// agent (deprecated alias) is required, which is an anyOf over two
	// required-lists — mcp-go's typed builders only produce flat required
	// lists, and requiring `agent` here would re-break the caller class the
	// alias exists for (issue #77, step 3 of the canonical flip).
	//
	// NewTool always seeds InputSchema.Type, and MarshalJSON rejects a tool
	// carrying both schema forms, so the structured schema is dropped and the
	// raw one takes its place before registration.
	reflectTool := mcp.NewTool("memory_reflect",
		mcp.WithToolTitle("Curate your own memory"),
		mcp.WithDescription("Curate an agent's memory: add a fact, update (supersede) an existing one, forget, pin or unpin against decay, or link two knowledge-graph nodes. Use it mid-task when you notice a contradiction, finish a task, or learn a durable preference; for a brand-new fact memory_add is simpler. update requires the exact old fact text in target; forget, pin and unpin accept the fact via target or text, with target winning when both are set. Retired facts keep a tombstone receipt in the audit log — nothing is ever hard-deleted. Returns a per-action confirmation."),
		writeTool(),
		outputSchemaOf[reflectResult](),
	)
	reflectTool.InputSchema = mcp.ToolInputSchema{}
	reflectTool.RawInputSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["add", "update", "forget", "link", "pin", "unpin"],
      "description": "The self-curation action: add a new fact, update (supersede) an existing one, forget (retire with a tombstone), pin or unpin against decay, or link two knowledge-graph nodes."
    },
    "agent_id": {
      "type": "string",
      "description": "The agent whose memory to modify."
    },
    "agent": {
      "type": "string",
      "description": "Deprecated alias of agent_id, accepted for compatibility with callers that predate the canonical flip. agent_id wins when both are set."
    },
    "text": {
      "type": "string",
      "description": "The fact text for add/update, the fact to forget or pin (alternative to target), or the source node ID for link."
    },
    "target": {
      "type": "string",
      "description": "For update: the fact text to supersede. For forget/pin/unpin: the fact (or pass it via text). For link: the target node ID."
    }
  },
  "required": ["action"],
  "anyOf": [
    {"required": ["agent_id"]},
    {"required": ["agent"]}
  ],
  "additionalProperties": false
}`)
	s.mcpSrv.AddTool(reflectTool, s.handleMemoryReflect)
}

// toolError wraps an error as an MCP tool result with isError=true.
func toolError(msg string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(msg), nil
}

// outputSchemaOf declares a tool's output schema from a Go type, failing fast
// when generation is impossible. mcp-go's WithOutputSchema swallows generation
// errors to the server's stderr, which would publish the tool without its
// declared contract and without any client-visible signal (TD-002); these
// types are compile-time constants, so a failure is a programming error and
// panicking at registration is the honest behaviour.
func outputSchemaOf[T any]() mcp.ToolOption {
	raw, err := mcp.SchemaForRaw[T]()
	if err != nil {
		var zero T
		panic(fmt.Sprintf("graymatter/mcp: cannot generate output schema for %T: %v", zero, err))
	}
	return mcp.WithRawOutputSchema(raw)
}

// toolStructured returns a result whose structuredContent is payload and whose
// text content is the unchanged human-readable prose. Every success path in
// this package goes through it, so the text contract survives independently of
// the structured one.
func toolStructured(payload any, text string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultStructured(payload, text), nil
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

// getBool extracts an optional boolean argument, returning false if absent.
// Clients that send the JSON literals true/false arrive here as bool.
func getBool(args map[string]any, key string) bool {
	v, ok := args[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return b && ok
}

// Ensure context is used.
var _ context.Context = context.Background()
