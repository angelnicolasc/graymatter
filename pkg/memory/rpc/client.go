package rpc

import (
	"context"
	"fmt"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"sync"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Client is a thin wrapper around a net/rpc client that speaks the
// GrayMatter RPC service.
//
// Methods mirror Backend (which itself mirrors the in-process
// AdvancedStore minus DB() and the ConsolidateConfig argument). All
// methods are safe for concurrent use — net/rpc serialises requests on
// a single connection, so callers do not need to add their own locking.
//
// Construct with Dial; release with Close.
type Client struct {
	rpc         *rpc.Client
	conn        net.Conn
	addr        string
	callTimeout time.Duration
	closeMu     sync.Mutex
	closed      bool
}

// DialOptions tunes how Dial finds and connects to a daemon.
type DialOptions struct {
	// DataDir locates the daemon's discovery file (graymatter.addr).
	// The default is ".graymatter".
	DataDir string

	// DialTimeout caps how long a single dial attempt may block.
	// Defaults to 2 seconds.
	DialTimeout time.Duration

	// CallTimeout bounds each RPC round-trip. A hung daemon otherwise
	// blocks net/rpc forever and with it every one-shot CLI command.
	// Defaults to 30 seconds. Consolidate uses an independent, longer
	// bound (it proxies an LLM call).
	CallTimeout time.Duration

	// PingOnDial, if true, sends a Ping RPC after connecting and verifies
	// the server protocol matches Protocol. Defaults to true; turn off
	// only for tests against a server you trust unconditionally.
	PingOnDial bool
}

// consolidateTimeout bounds Consolidate calls; the daemon-side work may
// include an LLM round-trip, so the regular CallTimeout is too tight.
const consolidateTimeout = 5 * time.Minute

// Dial opens a connection to a running daemon at dataDir, presenting the
// auth token recorded in the discovery file.
//
// Dial does NOT spawn the daemon if none is running — that is the job of
// higher-level callers. Dial returns net.ErrClosed-shaped errors if the
// discovery file is missing or the listener refuses connections, so
// spawn-on-connect logic can errors.Is for that case.
func Dial(opts DialOptions) (*Client, error) {
	if opts.DataDir == "" {
		opts.DataDir = ".graymatter"
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 2 * time.Second
	}
	if opts.CallTimeout == 0 {
		opts.CallTimeout = 30 * time.Second
	}

	addr, token, err := readDiscovery(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("rpc: read discovery: %w", err)
	}

	conn, err := dialAddr(addr, opts.DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("rpc: dial %s: %w", addr, err)
	}

	// Token preamble: one line before the JSON-RPC stream starts. The
	// server drops the connection silently on mismatch — the subsequent
	// Ping surfaces that as a useful error.
	if token != "" {
		if err := conn.SetWriteDeadline(time.Now().Add(opts.DialTimeout)); err == nil {
			_, _ = conn.Write([]byte(token + "\n"))
			_ = conn.SetWriteDeadline(time.Time{})
		}
	}

	c := &Client{
		rpc:         rpc.NewClientWithCodec(jsonrpc.NewClientCodec(conn)),
		conn:        conn,
		addr:        addr,
		callTimeout: opts.CallTimeout,
	}

	if opts.PingOnDial {
		if err := c.Ping(); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("rpc: ping after dial: %w", err)
		}
	}
	return c, nil
}

// Close releases the underlying connection. Safe to call multiple times.
func (c *Client) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.rpc.Close()
}

// Addr returns the discovery address this client connected to. Useful
// for logs and tests.
func (c *Client) Addr() string { return c.addr }

// call invokes a core-service method with the default timeout.
func (c *Client) call(method string, req, resp any) error {
	return c.CallService(ServiceName, method, req, resp, c.callTimeout)
}

// CallService invokes service.method with an explicit timeout. It exists so
// the daemon's host-level services (registered via Server.RegisterExtra) can
// reuse this client and its connection rather than maintaining a second one.
//
// net/rpc cannot cancel an in-flight call, so on timeout the connection is
// closed — which fails all pending calls — and the client must be re-dialed.
// A poisoned connection is strictly worse than a dropped one.
func (c *Client) CallService(service, method string, req, resp any, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = c.callTimeout
	}
	done := c.rpc.Go(service+"."+method, req, resp, make(chan *rpc.Call, 1)).Done

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case call := <-done:
		return call.Error
	case <-t.C:
		_ = c.Close()
		return fmt.Errorf("rpc: %s.%s timed out after %s (connection closed)", service, method, timeout)
	}
}

// Ping verifies the server is reachable and protocol-compatible.
// Returns nil on success.
func (c *Client) Ping() error {
	var resp PingResponse
	if err := c.call("Ping", &PingRequest{}, &resp); err != nil {
		return err
	}
	if resp.Protocol != Protocol {
		return fmt.Errorf("rpc: protocol mismatch: server=%q client=%q", resp.Protocol, Protocol)
	}
	return nil
}

// --- typed methods mirroring Backend ---

// Put writes a fact for agentID.
//
// The ctx argument is currently retained for API symmetry; net/rpc does
// not propagate cancellation across the wire, so a cancelled ctx will
// not interrupt an in-flight server-side call. This is acceptable for
// foundation-mode because individual operations complete in <50 ms;
// a future revision may add per-call deadline forwarding.
func (c *Client) Put(ctx context.Context, agentID, text string) error {
	return c.call("Put", &PutRequest{AgentID: agentID, Text: text}, &PutResponse{})
}

// PutShared writes a fact to the __shared__ namespace.
func (c *Client) PutShared(ctx context.Context, text string) error {
	return c.call("PutShared", &PutSharedRequest{Text: text}, &PutSharedResponse{})
}

// Recall returns the top-k most relevant facts for agentID given query.
func (c *Client) Recall(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	var resp RecallResponse
	if err := c.call("Recall", &RecallRequest{AgentID: agentID, Query: query, TopK: topK}, &resp); err != nil {
		return nil, err
	}
	return resp.Facts, nil
}

// RecallShared returns the top-k most relevant shared facts for query.
func (c *Client) RecallShared(ctx context.Context, query string, topK int) ([]string, error) {
	var resp RecallSharedResponse
	if err := c.call("RecallShared", &RecallSharedRequest{Query: query, TopK: topK}, &resp); err != nil {
		return nil, err
	}
	return resp.Facts, nil
}

// RecallAll returns the merged agent + shared facts for query. TopK<=0 uses
// the daemon's configured default.
func (c *Client) RecallAll(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	var resp RecallAllResponse
	if err := c.call("RecallAll", &RecallAllRequest{AgentID: agentID, Query: query, TopK: topK}, &resp); err != nil {
		return nil, err
	}
	return resp.Facts, nil
}

// List returns every fact for agentID, newest first.
func (c *Client) List(agentID string) ([]memory.Fact, error) {
	var resp ListResponse
	if err := c.call("List", &ListRequest{AgentID: agentID}, &resp); err != nil {
		return nil, err
	}
	return resp.Facts, nil
}

// ListAgents returns every known agent ID.
func (c *Client) ListAgents() ([]string, error) {
	var resp ListAgentsResponse
	if err := c.call("ListAgents", &ListAgentsRequest{}, &resp); err != nil {
		return nil, err
	}
	return resp.AgentIDs, nil
}

// Stats returns aggregate fact statistics for agentID.
func (c *Client) Stats(agentID string) (memory.MemoryStats, error) {
	var resp StatsResponse
	if err := c.call("Stats", &StatsRequest{AgentID: agentID}, &resp); err != nil {
		return memory.MemoryStats{}, err
	}
	return resp.Stats, nil
}

// Delete removes a fact by ID.
func (c *Client) Delete(agentID, factID string) error {
	return c.call("Delete", &DeleteRequest{AgentID: agentID, FactID: factID}, &DeleteResponse{})
}

// UpdateFact persists a modified fact.
func (c *Client) UpdateFact(agentID string, f memory.Fact) error {
	return c.call("UpdateFact", &UpdateFactRequest{AgentID: agentID, Fact: f}, &UpdateFactResponse{})
}

// Consolidate triggers server-side consolidation for agentID using the
// daemon's own configuration. Clients cannot override the policy. Uses a
// longer timeout than other calls because consolidation may proxy an LLM
// round-trip.
func (c *Client) Consolidate(ctx context.Context, agentID string) error {
	return c.CallService(ServiceName, "Consolidate", &ConsolidateRequest{AgentID: agentID}, &ConsolidateResponse{}, consolidateTimeout)
}

// PendingVectorCount returns the number of facts waiting to be re-indexed.
func (c *Client) PendingVectorCount() (int, error) {
	var resp PendingVectorCountResponse
	if err := c.call("PendingVectorCount", &PendingVectorCountRequest{}, &resp); err != nil {
		return 0, err
	}
	return resp.Count, nil
}
