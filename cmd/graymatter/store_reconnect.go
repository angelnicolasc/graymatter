package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	netrpc "net/rpc"
	"sync"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/server"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The wrapper stands in for a store handle everywhere one is used, so both
// contracts are asserted here: a change to either fails to compile rather than
// at runtime.
var (
	_ server.Store = (*reconnectingStore)(nil)
	_ cliStore     = (*reconnectingStore)(nil)
)

// reconnectingStore keeps a long-lived process usable across daemon restarts.
//
// net/rpc offers no reconnection: once a connection closes, every later call
// fails with ErrShutdown, and pkg/memory/rpc states plainly that re-dialling is
// the caller's job. CLI commands never notice because their handle lives for
// milliseconds. The REST server holds one for as long as it runs, so a
// `graymatter daemon stop`, a daemon crash, or an upgrade would otherwise leave
// it answering 500 forever (issue #19).
//
// Recovery is cheap here because openStore spawns a daemon when none is
// running, so the usual outcome of a redial is a fresh daemon and a served
// request rather than an error.
type reconnectingStore struct {
	mu  sync.Mutex
	cur cliStore
}

func newReconnectingStore(initial cliStore) *reconnectingStore {
	return &reconnectingStore{cur: initial}
}

// reopenStore returns a fresh, unwrapped daemon handle.
//
// It must not go through openStore: that wraps, so a redial would nest another
// reconnectingStore on every reconnection. Swapped in tests to exercise the
// redial path without standing up a daemon, following the same hook pattern as
// resolveExecutable and testHomeOverride.
var reopenStore = func() (cliStore, error) {
	c, err := daemon.Connect(dataDir)
	if err != nil {
		return nil, err
	}
	return daemonStore{Client: c}, nil
}

func (r *reconnectingStore) snapshot() cliStore {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

// redial replaces failed with a fresh handle. If another goroutine already
// reconnected, that handle is returned instead of opening a second one.
func (r *reconnectingStore) redial(failed cliStore) (cliStore, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != failed {
		return r.cur, nil
	}
	next, err := reopenStore()
	if err != nil {
		return nil, err
	}
	_ = failed.Close()
	r.cur = next
	return next, nil
}

// do runs fn, and on a dead connection reconnects and runs it exactly once
// more. Only connection death is retried: a store error means the call reached
// the daemon and genuinely failed, and repeating a write that already landed
// would be worse than reporting it.
func (r *reconnectingStore) do(fn func(cliStore) error) error {
	s := r.snapshot()
	err := fn(s)
	if err == nil || !connDead(err) {
		return err
	}
	next, rerr := r.redial(s)
	if rerr != nil {
		return fmt.Errorf("%w (reconnect failed: %v)", err, rerr)
	}
	return fn(next)
}

// connDead reports whether err means the connection is gone rather than the
// operation having failed on its merits.
//
// A call that times out is not listed: pkg/memory/rpc closes the connection on
// timeout, so the next call surfaces ErrShutdown and recovers there. Retrying
// the timed-out call itself could duplicate a write that is still in flight.
func connDead(err error) bool {
	return errors.Is(err, netrpc.ErrShutdown) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed)
}

// --- cliStore, every call routed through do ---
//
// This is deliberately mechanical. Embedding the wrapped store instead would be
// shorter and wrong: after a redial the embedded value still points at the dead
// handle, so any method that was not overridden would keep talking to it.

func (r *reconnectingStore) Remember(ctx context.Context, agentID, text string) error {
	return r.do(func(s cliStore) error { return s.Remember(ctx, agentID, text) })
}

func (r *reconnectingStore) PutReturningFact(ctx context.Context, agentID, text string) (memory.Fact, error) {
	var fact memory.Fact
	err := r.do(func(s cliStore) error {
		writer, ok := s.(returningFactStore)
		if !ok {
			return errors.New("store does not expose PutReturningFact")
		}
		var err error
		fact, err = writer.PutReturningFact(ctx, agentID, text)
		return err
	})
	return fact, err
}

func (r *reconnectingStore) PutShared(ctx context.Context, text string) error {
	return r.do(func(s cliStore) error { return s.PutShared(ctx, text) })
}

func (r *reconnectingStore) RecallDefault(ctx context.Context, agentID, query string) ([]string, error) {
	var out []string
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.RecallDefault(ctx, agentID, query)
		return e
	})
	return out, err
}

func (r *reconnectingStore) RecallShared(ctx context.Context, query string, topK int) ([]string, error) {
	var out []string
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.RecallShared(ctx, query, topK)
		return e
	})
	return out, err
}

func (r *reconnectingStore) RecallAll(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	var out []string
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.RecallAll(ctx, agentID, query, topK)
		return e
	})
	return out, err
}

func (r *reconnectingStore) RecallExplain(ctx context.Context, agentID, query string, topK int) ([]memory.RecallReceipt, error) {
	var out []memory.RecallReceipt
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.RecallExplain(ctx, agentID, query, topK)
		return e
	})
	return out, err
}

func (r *reconnectingStore) RecallDetailed(ctx context.Context, agentID, query string, topK int) ([]string, string, error) {
	var texts []string
	var feedback string
	err := r.do(func(s cliStore) error {
		var e error
		texts, feedback, e = s.RecallDetailed(ctx, agentID, query, topK)
		return e
	})
	return texts, feedback, err
}

func (r *reconnectingStore) PutAlias(ctx context.Context, agentID, term string, equivalents []string) error {
	return r.do(func(s cliStore) error {
		return s.PutAlias(ctx, agentID, term, equivalents)
	})
}

func (r *reconnectingStore) ListAgents() ([]string, error) {
	var out []string
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.ListAgents()
		return e
	})
	return out, err
}

func (r *reconnectingStore) Stats(agentID string) (memory.MemoryStats, error) {
	var out memory.MemoryStats
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.Stats(agentID)
		return e
	})
	return out, err
}

func (r *reconnectingStore) UpdateFact(agentID string, f memory.Fact) error {
	return r.do(func(s cliStore) error { return s.UpdateFact(agentID, f) })
}

func (r *reconnectingStore) CheckpointSave(cp session.Checkpoint) (session.Checkpoint, error) {
	var out session.Checkpoint
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.CheckpointSave(cp)
		return e
	})
	return out, err
}

func (r *reconnectingStore) CheckpointLoad(agentID, checkpointID string) (*session.Checkpoint, error) {
	var out *session.Checkpoint
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.CheckpointLoad(agentID, checkpointID)
		return e
	})
	return out, err
}

func (r *reconnectingStore) CheckpointResume(agentID string) (*session.Checkpoint, error) {
	var out *session.Checkpoint
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.CheckpointResume(agentID)
		return e
	})
	return out, err
}

func (r *reconnectingStore) CheckpointList(agentID string) ([]session.Checkpoint, error) {
	var out []session.Checkpoint
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.CheckpointList(agentID)
		return e
	})
	return out, err
}

func (r *reconnectingStore) SessionsList() ([]harness.HarnessSession, error) {
	var out []harness.HarnessSession
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.SessionsList()
		return e
	})
	return out, err
}

func (r *reconnectingStore) SessionKill(id string) error {
	return r.do(func(s cliStore) error { return s.SessionKill(id) })
}

func (r *reconnectingStore) SessionResolve(agentID, sessionID string) (string, error) {
	var out string
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.SessionResolve(agentID, sessionID)
		return e
	})
	return out, err
}

func (r *reconnectingStore) SessionSave(hs harness.HarnessSession) error {
	return r.do(func(s cliStore) error { return s.SessionSave(hs) })
}

func (r *reconnectingStore) KGNodes() ([]kg.Node, error) {
	var out []kg.Node
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.KGNodes()
		return e
	})
	return out, err
}

func (r *reconnectingStore) KGLink(from, to, relation string) error {
	return r.do(func(s cliStore) error { return s.KGLink(from, to, relation) })
}

func (r *reconnectingStore) KGEdges() ([]kg.Edge, error) {
	var out []kg.Edge
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.KGEdges()
		return e
	})
	return out, err
}

func (r *reconnectingStore) ExportGraphObsidian(outDir string) error {
	return r.do(func(s cliStore) error { return s.ExportGraphObsidian(outDir) })
}

func (r *reconnectingStore) AuditWrite(e audit.Entry) error {
	return r.do(func(s cliStore) error { return s.AuditWrite(e) })
}

func (r *reconnectingStore) TokenSummary(days int) (harness.TokenUsageSummary, error) {
	var out harness.TokenUsageSummary
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.TokenSummary(days)
		return e
	})
	return out, err
}

func (r *reconnectingStore) TokenRecord(agent, model string, input, output, cacheRead, cacheWrite uint64) error {
	return r.do(func(s cliStore) error {
		return s.TokenRecord(agent, model, input, output, cacheRead, cacheWrite)
	})
}

func (r *reconnectingStore) StoreOverview() (*daemon.StoreOverviewResponse, error) {
	var out *daemon.StoreOverviewResponse
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.StoreOverview()
		return e
	})
	return out, err
}

func (r *reconnectingStore) KGState() (*daemon.KGStateResponse, error) {
	var out *daemon.KGStateResponse
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.KGState()
		return e
	})
	return out, err
}

// IsReadOnly is always false through the daemon: every client is a full peer.
// The wrapper only ever holds daemon handles, so this cannot be anything else.
func (r *reconnectingStore) IsReadOnly() bool { return r.snapshot().IsReadOnly() }

func (r *reconnectingStore) Recall(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	var out []string
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.Recall(ctx, agentID, query, topK)
		return e
	})
	return out, err
}

func (r *reconnectingStore) List(agentID string) ([]memory.Fact, error) {
	var out []memory.Fact
	err := r.do(func(s cliStore) error {
		var e error
		out, e = s.List(agentID)
		return e
	})
	return out, err
}

func (r *reconnectingStore) Delete(agentID, factID string) error {
	return r.do(func(s cliStore) error { return s.Delete(agentID, factID) })
}

func (r *reconnectingStore) Consolidate(ctx context.Context, agentID string) error {
	return r.do(func(s cliStore) error { return s.Consolidate(ctx, agentID) })
}

// Ready reports whether the store answers right now, reconnecting first if the
// connection died. It defers to the wrapped store's own probe rather than
// picking one here: through the daemon that is the protocol ping, which is both
// cheaper than listing agents and the only check that catches a version
// mismatch after a reconnect landed on an upgraded daemon.
func (r *reconnectingStore) Ready() error {
	return r.do(func(s cliStore) error { return s.Ready() })
}

func (r *reconnectingStore) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur.Close()
}
