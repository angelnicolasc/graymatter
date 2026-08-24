// Package daemon implements GrayMatter daemon mode (issue #8): one process
// owns the bbolt store and serves it over the local RPC endpoint from
// pkg/memory/rpc; every other process — TUI, MCP server, one-shot CLI
// commands, the run harness — connects as a client. This removes the
// single-writer lock fights between concurrent graymatter processes.
//
// The package has three parts:
//
//   - Host: the daemon-side RPC service exposing binary-level subsystems
//     (checkpoints, harness sessions, knowledge graph, audit, token ledger)
//     that the core store service in pkg/memory/rpc deliberately knows
//     nothing about.
//   - Run: the daemon lifecycle — strict-write store open, listener +
//     discovery file, pidfile, idle-exit, signal handling.
//   - Connect: the client side — dial, spawn-on-absent with backoff, and
//     typed wrappers for every host method.
package daemon

import (
	"context"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

// HostServiceName is the net/rpc service name for the host-level service,
// registered next to the core store service on the same listener.
const HostServiceName = "GrayMatterHost"

// Host is the daemon-side receiver for host-level RPCs.
type Host struct {
	mem     *graymatter.Memory
	db      *bolt.DB
	graph   *kg.Graph
	adapter *kg.GraphAdapter
	kgAuto  bool // whether consolidation feeds the graph (opts.KG / env / sentinel)
	stop    func() // initiates graceful daemon shutdown
}

// --- wire types -------------------------------------------------------------

type CheckpointSaveRequest struct{ CP session.Checkpoint }
type CheckpointSaveResponse struct{ CP session.Checkpoint }

type CheckpointLoadRequest struct{ AgentID, CheckpointID string }
type CheckpointLoadResponse struct{ CP session.Checkpoint }

type CheckpointResumeRequest struct{ AgentID string }
type CheckpointResumeResponse struct{ CP session.Checkpoint }

type CheckpointListRequest struct{ AgentID string }
type CheckpointListResponse struct{ CPs []session.Checkpoint }

type SessionListRequest struct{}
type SessionListResponse struct{ Sessions []harness.HarnessSession }

type SessionSaveRequest struct{ S harness.HarnessSession }
type SessionSaveResponse struct{}

type SessionKillRequest struct{ ID string }
type SessionKillResponse struct{}

type SessionResolveRequest struct{ AgentID, SessionID string }
type SessionResolveResponse struct{ ID string }

type KGNodesRequest struct{}
type KGNodesResponse struct{ Nodes []kg.Node }

type KGEdgesRequest struct{}
type KGEdgesResponse struct{ Edges []kg.Edge }

type KGLinkRequest struct{ From, To, Relation string }
type KGLinkResponse struct{}

type KGUpsertRequest struct{ ID, Label, EntityType string }
type KGUpsertResponse struct{}

type KGExportObsidianRequest struct{ OutDir string }
type KGExportObsidianResponse struct{}

type AuditWriteRequest struct{ E audit.Entry }
type AuditWriteResponse struct{}

type TokenRecordRequest struct {
	Agent, Model                         string
	Input, Output, CacheRead, CacheWrite uint64
}
type TokenRecordResponse struct{}

type TokenSummaryRequest struct{ Days int }
type TokenSummaryResponse struct{ S harness.TokenUsageSummary }

type ShutdownRequest struct{}
type ShutdownResponse struct{}

// AgentSummary is one agent's row in a StoreOverview.
type AgentSummary struct {
	Agent     string    `json:"agent"`
	LiveFacts int       `json:"live_facts"`
	AvgWeight float64   `json:"avg_weight"`
	OldestAt  time.Time `json:"oldest_at,omitempty"`
	NewestAt  time.Time `json:"newest_at,omitempty"`
}

type StoreOverviewRequest struct{}
type StoreOverviewResponse struct {
	TotalAgents      int            `json:"total_agents"`
	TotalLiveFacts   int            `json:"total_live_facts"`
	TotalTombstones  int            `json:"total_tombstones"`
	PendingVectorOps int            `json:"pending_vector_ops"`
	Agents           []AgentSummary `json:"agents"`
}

type KGStateRequest struct{}
type KGStateResponse struct {
	AutoPopulate bool `json:"auto_populate"`
	Nodes        int  `json:"nodes"`
	Edges        int  `json:"edges"`
}

// --- handlers ---------------------------------------------------------------

var errNoKG = errors.New("knowledge graph not available on the daemon")

// CheckpointSave persists a checkpoint and returns it with ID/timestamps set.
func (h *Host) CheckpointSave(req *CheckpointSaveRequest, resp *CheckpointSaveResponse) error {
	saved, err := session.Save(h.db, req.CP)
	if err != nil {
		return err
	}
	resp.CP = saved
	return nil
}

// CheckpointLoad retrieves one checkpoint by ID.
func (h *Host) CheckpointLoad(req *CheckpointLoadRequest, resp *CheckpointLoadResponse) error {
	cp, err := session.Load(h.db, req.AgentID, req.CheckpointID)
	if err != nil {
		return err
	}
	resp.CP = *cp
	return nil
}

// CheckpointResume retrieves the most recent checkpoint for an agent.
func (h *Host) CheckpointResume(req *CheckpointResumeRequest, resp *CheckpointResumeResponse) error {
	cp, err := session.Resume(h.db, req.AgentID)
	if err != nil {
		return err
	}
	resp.CP = *cp
	return nil
}

// CheckpointList returns all checkpoints for an agent, newest first.
func (h *Host) CheckpointList(req *CheckpointListRequest, resp *CheckpointListResponse) error {
	cps, err := session.List(h.db, req.AgentID)
	if err != nil {
		return err
	}
	resp.CPs = cps
	return nil
}

// SessionList returns all harness session records, newest first.
func (h *Host) SessionList(req *SessionListRequest, resp *SessionListResponse) error {
	sessions, err := harness.ListSessionsDB(h.db)
	if err != nil {
		return err
	}
	resp.Sessions = sessions
	return nil
}

// SessionSave persists a harness session record.
func (h *Host) SessionSave(req *SessionSaveRequest, resp *SessionSaveResponse) error {
	return harness.SaveSessionDB(h.db, req.S)
}

// SessionKill signals the recorded PID for a running session and marks it killed.
func (h *Host) SessionKill(req *SessionKillRequest, resp *SessionKillResponse) error {
	return harness.KillSessionDB(h.db, req.ID)
}

// SessionResolve resolves "latest" (optionally per-agent) to a concrete session ID.
func (h *Host) SessionResolve(req *SessionResolveRequest, resp *SessionResolveResponse) error {
	id, err := harness.ResolveSessionIDDB(h.db, req.AgentID, req.SessionID)
	if err != nil {
		return err
	}
	resp.ID = id
	return nil
}

// KGNodes returns every knowledge-graph node.
func (h *Host) KGNodes(req *KGNodesRequest, resp *KGNodesResponse) error {
	if h.graph == nil {
		resp.Nodes = nil
		return nil
	}
	nodes, err := h.graph.AllNodes()
	if err != nil {
		return err
	}
	resp.Nodes = nodes
	return nil
}

// KGEdges returns every knowledge-graph edge (empty when no graph exists).
func (h *Host) KGEdges(req *KGEdgesRequest, resp *KGEdgesResponse) error {
	if h.graph == nil {
		resp.Edges = nil
		return nil
	}
	edges, err := h.graph.AllEdges()
	if err != nil {
		return err
	}
	resp.Edges = edges
	return nil
}

// KGLink creates an edge between two nodes.
func (h *Host) KGLink(req *KGLinkRequest, resp *KGLinkResponse) error {
	if h.adapter == nil {
		return errNoKG
	}
	return h.adapter.LinkNodes(req.From, req.To, req.Relation)
}

// KGUpsert inserts or updates a node.
func (h *Host) KGUpsert(req *KGUpsertRequest, resp *KGUpsertResponse) error {
	if h.adapter == nil {
		return errNoKG
	}
	return h.adapter.UpsertNode(req.ID, req.Label, req.EntityType)
}

// KGExportObsidian writes the graph's entity notes and canvas into outDir,
// next to whatever the facts export produced. Runs host-side because the
// graph lives on the daemon's bbolt handle; only the destination path
// crosses the wire.
func (h *Host) KGExportObsidian(req *KGExportObsidianRequest, resp *KGExportObsidianResponse) error {
	if h.graph == nil {
		return errNoKG
	}
	return h.graph.ExportObsidian(req.OutDir)
}

// AuditWrite records an agent self-edit event.
//
// The error reaches the caller now instead of being swallowed here. Callers
// still treat it as best-effort — the operation being audited has already
// happened — but a trail going incomplete is no longer invisible.
func (h *Host) AuditWrite(req *AuditWriteRequest, resp *AuditWriteResponse) error {
	if err := audit.Write(h.db, req.E); err != nil {
		return err
	}
	return nil
}

// TokenRecord adds token usage to the pre-aggregated ledger.
func (h *Host) TokenRecord(req *TokenRecordRequest, resp *TokenRecordResponse) error {
	return harness.RecordTokenUsage(h.db, req.Agent, req.Model, req.Input, req.Output, req.CacheRead, req.CacheWrite)
}

// TokenSummary aggregates the token ledger over the trailing N days.
func (h *Host) TokenSummary(req *TokenSummaryRequest, resp *TokenSummaryResponse) error {
	s, err := harness.LoadTokenUsageSummary(h.db, req.Days)
	if err != nil {
		return err
	}
	resp.S = s
	return nil
}

// StoreOverview aggregates per-agent fact statistics for `graymatter status`
// in one call, so a status screen costs one round-trip instead of N.
// Tombstones are excluded from live counts and weights: they never reach a
// prompt and reporting them as memory would overstate the store.
func (h *Host) StoreOverview(req *StoreOverviewRequest, resp *StoreOverviewResponse) error {
	adv := h.mem.Advanced()
	if adv == nil {
		return errors.New("store not initialised")
	}
	agents, err := adv.ListAgents()
	if err != nil {
		return err
	}
	resp.Agents = make([]AgentSummary, 0, len(agents))
	for _, a := range agents {
		facts, err := adv.List(a)
		if err != nil {
			return err
		}
		sum := AgentSummary{Agent: a}
		var weightSum float64
		for _, f := range facts {
			if f.SupersededBy != "" {
				resp.TotalTombstones++
				continue
			}
			sum.LiveFacts++
			weightSum += f.Weight
			if sum.OldestAt.IsZero() || f.CreatedAt.Before(sum.OldestAt) {
				sum.OldestAt = f.CreatedAt
			}
			if f.CreatedAt.After(sum.NewestAt) {
				sum.NewestAt = f.CreatedAt
			}
		}
		if sum.LiveFacts > 0 {
			sum.AvgWeight = weightSum / float64(sum.LiveFacts)
		}
		resp.TotalAgents++
		resp.TotalLiveFacts += sum.LiveFacts
		resp.Agents = append(resp.Agents, sum)
	}
	resp.PendingVectorOps = adv.PendingVectorCount()
	return nil
}

// KGState reports whether auto-population is on for this daemon and how much
// graph exists right now. The graph itself is opened unconditionally by the
// daemon; only extraction is gated, which is why Nodes/Edges can be non-zero
// while AutoPopulate is false (explicit agent links populate it too).
func (h *Host) KGState(req *KGStateRequest, resp *KGStateResponse) error {
	resp.AutoPopulate = h.kgAuto
	if h.graph == nil {
		return nil
	}
	nodes, err := h.graph.AllNodes()
	if err != nil {
		return err
	}
	resp.Nodes = len(nodes)
	edges, err := h.graph.AllEdges()
	if err != nil {
		return err
	}
	resp.Edges = len(edges)
	return nil
}

// Shutdown asks the daemon to stop gracefully. The stop is deferred a beat so
// the RPC reply reaches the client before connections are torn down.
func (h *Host) Shutdown(req *ShutdownRequest, resp *ShutdownResponse) error {
	if h.stop != nil {
		go func() {
			time.Sleep(150 * time.Millisecond)
			h.stop()
		}()
	}
	return nil
}

// --- daemon-side core backend ----------------------------------------------

// rememberBackend adapts *graymatter.Memory to the core rpc.Backend so that
// remote Puts get full Remember semantics (async consolidation included) and
// remote Recalls with TopK<=0 use the daemon's configured TopK.
type rememberBackend struct {
	graymatter.AdvancedStore
	mem *graymatter.Memory
}

func (b rememberBackend) Put(ctx context.Context, agentID, text string) error {
	return b.mem.Remember(ctx, agentID, text)
}

func (b rememberBackend) PutShared(ctx context.Context, text string) error {
	return b.mem.RememberShared(ctx, text)
}

func (b rememberBackend) Recall(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	if topK <= 0 {
		return b.mem.Recall(ctx, agentID, query)
	}
	return b.AdvancedStore.Recall(ctx, agentID, query, topK)
}

func (b rememberBackend) RecallShared(ctx context.Context, query string, topK int) ([]string, error) {
	if topK <= 0 {
		return b.mem.RecallShared(ctx, query)
	}
	return b.AdvancedStore.RecallShared(ctx, query, topK)
}

func (b rememberBackend) RecallAll(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	if topK <= 0 {
		return b.mem.RecallAll(ctx, agentID, query)
	}
	// AdvancedStore does not expose RecallAll; the concrete store does.
	if ra, ok := b.AdvancedStore.(interface {
		RecallAll(ctx context.Context, agentID, query string, topK int) ([]string, error)
	}); ok {
		return ra.RecallAll(ctx, agentID, query, topK)
	}
	return b.mem.RecallAll(ctx, agentID, query)
}
