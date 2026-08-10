package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	netrpc "net/rpc"
	"sync"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/server"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The REST server consumes exactly this. Asserting it here means a change to
// either side fails to compile rather than at runtime.
var _ server.Store = (*reconnectingStore)(nil)

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

// reopenStore is how a reconnect obtains a fresh handle. Swapped in tests to
// exercise the redial path without standing up a daemon, following the same
// hook pattern as resolveExecutable and testHomeOverride.
var reopenStore = openStore

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

// --- the surface the REST server consumes ---

func (r *reconnectingStore) Remember(ctx context.Context, agentID, text string) error {
	return r.do(func(s cliStore) error { return s.Remember(ctx, agentID, text) })
}

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
// connection died. ListAgents is the cheapest call that proves a real
// round-trip to whoever owns the store.
func (r *reconnectingStore) Ready() error {
	return r.do(func(s cliStore) error {
		_, err := s.ListAgents()
		return err
	})
}

func (r *reconnectingStore) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur.Close()
}
