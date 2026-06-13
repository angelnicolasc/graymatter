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

type KGLinkRequest struct{ From, To, Relation string }
type KGLinkResponse struct{}

type KGUpsertRequest struct{ ID, Label, EntityType string }
type KGUpsertResponse struct{}

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

// AuditWrite records an agent self-edit event. Best-effort by design.
func (h *Host) AuditWrite(req *AuditWriteRequest, resp *AuditWriteResponse) error {
	audit.Write(h.db, req.E)
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
