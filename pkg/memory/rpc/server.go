package rpc

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"sync"
	"sync/atomic"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Backend is the in-process surface the RPC server wraps.
//
// It is deliberately a strict subset of graymatter.AdvancedStore: DB()
// is excluded because raw bbolt handles cannot cross a process
// boundary, and Consolidate keeps the in-process signature so
// *memory.Store satisfies us directly.
//
// The concrete *memory.Store satisfies this interface (verified by the
// compile-time assertion below). Tests may supply their own minimal
// implementation.
type Backend interface {
	Put(ctx context.Context, agentID, text string) error
	PutShared(ctx context.Context, text string) error
	Recall(ctx context.Context, agentID, query string, topK int) ([]string, error)
	RecallShared(ctx context.Context, query string, topK int) ([]string, error)
	RecallAll(ctx context.Context, agentID, query string, topK int) ([]string, error)
	List(agentID string) ([]memory.Fact, error)
	ListAgents() ([]string, error)
	Stats(agentID string) (memory.MemoryStats, error)
	Delete(agentID, factID string) error
	UpdateFact(agentID string, f memory.Fact) error
	Consolidate(ctx context.Context, agentID string, cfg memory.ConsolidateConfig) error
	PendingVectorCount() int
}

// compile-time guarantee that *memory.Store satisfies Backend.
var _ Backend = (*memory.Store)(nil)

// Server exposes a Backend over net/rpc + JSON.
//
// One Server instance handles many concurrent client connections; net/rpc
// dispatches each call on its own goroutine. The underlying Backend is
// expected to be safe for concurrent use (memory.Store is).
//
// Server tracks a "last activity" timestamp updated on every successful
// dispatch. Daemon mode uses this to drive idle-exit; standalone callers
// can ignore it.
type Server struct {
	backend          Backend
	consolidateCfg   memory.ConsolidateConfig
	lastActivityUnix int64 // atomic; UnixNano
	activeConns      int64 // atomic
	token            string
	extra            map[string]any // additional net/rpc services, by name

	stop      chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
	listener  net.Listener
	listenErr atomic.Value // error

	connsMu sync.Mutex
	conns   map[net.Conn]struct{}
}

// NewServer constructs a Server wrapping backend. The consolidateCfg is
// used when handling RPC ConsolidateRequest calls; pass the same Config
// the daemon uses elsewhere so consolidation policy is consistent.
func NewServer(backend Backend, consolidateCfg memory.ConsolidateConfig) *Server {
	s := &Server{
		backend:        backend,
		consolidateCfg: consolidateCfg,
		stop:           make(chan struct{}),
		conns:          make(map[net.Conn]struct{}),
	}
	s.touch()
	return s
}

// trackConn registers an authenticated connection for shutdown teardown.
func (s *Server) trackConn(c net.Conn) {
	s.connsMu.Lock()
	s.conns[c] = struct{}{}
	s.connsMu.Unlock()
}

// untrackConn removes a connection after its codec loop exits.
func (s *Server) untrackConn(c net.Conn) {
	s.connsMu.Lock()
	delete(s.conns, c)
	s.connsMu.Unlock()
}

// touch updates the last-activity timestamp to now.
func (s *Server) touch() {
	atomic.StoreInt64(&s.lastActivityUnix, time.Now().UnixNano())
}

// LastActivity returns the wall-clock time of the most recent successful RPC
// dispatch or connection close. Useful for idle-exit timers.
func (s *Server) LastActivity() time.Time {
	return time.Unix(0, atomic.LoadInt64(&s.lastActivityUnix))
}

// ActiveConns returns the number of currently connected clients. Idle-exit
// logic must check this is zero in addition to LastActivity — a TUI sitting
// open without making calls still holds a connection and must keep the
// daemon alive.
func (s *Server) ActiveConns() int {
	return int(atomic.LoadInt64(&s.activeConns))
}

// SetAuthToken installs the shared-secret token clients must present as a
// connection preamble. An empty token disables authentication (tests only —
// the daemon always sets one). Must be called before Serve.
func (s *Server) SetAuthToken(token string) { s.token = token }

// RegisterExtra registers an additional net/rpc service on this server under
// name. The daemon uses this to expose host-level services (checkpoints,
// sessions, knowledge graph) next to the core store service without the
// library package needing to know about them. Must be called before Serve.
func (s *Server) RegisterExtra(name string, rcvr any) {
	if s.extra == nil {
		s.extra = map[string]any{}
	}
	s.extra[name] = rcvr
}

// maxTokenLine bounds the auth preamble read: 64 hex chars + newline slack.
const maxTokenLine = 256

// authenticate reads the token preamble (a single line) from conn and
// compares it in constant time. The read is deadline-bounded so a client
// that connects and sends nothing cannot pin a goroutine forever.
func (s *Server) authenticate(conn net.Conn) bool {
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return false
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	line := make([]byte, 0, len(s.token)+1)
	buf := [1]byte{}
	for len(line) < maxTokenLine {
		n, err := conn.Read(buf[:])
		if err != nil {
			return false
		}
		if n == 0 {
			continue
		}
		if buf[0] == '\n' {
			return subtle.ConstantTimeCompare(line, []byte(s.token)) == 1
		}
		line = append(line, buf[0])
	}
	return false
}

// Serve registers the service on a fresh net/rpc.Server, accepts connections
// from listener until Stop is called or listener errors out, and returns
// the first non-nil error encountered.
//
// Serve blocks; run it in its own goroutine.
func (s *Server) Serve(listener net.Listener) error {
	// Publish the listener under connsMu so a concurrent Stop (which closes
	// it to unblock Accept) cannot race the assignment.
	s.connsMu.Lock()
	s.listener = listener
	s.connsMu.Unlock()

	rpcSrv := rpc.NewServer()
	if err := rpcSrv.RegisterName(ServiceName, s); err != nil {
		return fmt.Errorf("rpc.RegisterName: %w", err)
	}
	for name, rcvr := range s.extra {
		if err := rpcSrv.RegisterName(name, rcvr); err != nil {
			return fmt.Errorf("rpc.RegisterName %s: %w", name, err)
		}
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.stop:
				// Graceful shutdown — listener was closed by Stop.
				s.wg.Wait()
				return nil
			default:
			}
			// Surfaceable error.
			s.listenErr.Store(err)
			s.wg.Wait()
			return err
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() { _ = c.Close() }()

			if s.token != "" && !s.authenticate(c) {
				return // wrong or missing token: drop before any dispatch
			}

			s.trackConn(c)
			atomic.AddInt64(&s.activeConns, 1)
			defer func() {
				atomic.AddInt64(&s.activeConns, -1)
				s.untrackConn(c)
				// Idle countdown starts when the last client leaves, not at
				// its last RPC — otherwise a long-lived quiet client could
				// outlive the timeout and get its daemon killed under it.
				s.touch()
			}()

			rpcSrv.ServeCodec(jsonrpc.NewServerCodec(c))
		}(conn)
	}
}

// Stop closes the listener, tears down active client connections, and waits
// for their codec loops to drain. In-flight calls already dispatched to the
// backend complete; idle connections are cut. Safe to call multiple times.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.connsMu.Lock()
		if s.listener != nil {
			_ = s.listener.Close()
		}
		for c := range s.conns {
			_ = c.Close()
		}
		s.connsMu.Unlock()
	})
	s.wg.Wait()
}

// --- RPC method handlers ---
//
// Each handler matches net/rpc's required signature:
//   func (s *Server) Method(req *Req, resp *Resp) error
//
// On error, we return the error to net/rpc which wraps and ships its
// .Error() string to the client. There are no sentinel errors in
// pkg/memory at this point, so plain string round-trip is sufficient.
// When PR #7 lands ErrStoreReadOnly, this is the place to add a kind
// prefix (e.g. "[read_only] ...") and a client-side mapper.

// Put handles a remote AdvancedStore.Put.
func (s *Server) Put(req *PutRequest, resp *PutResponse) error {
	defer s.touch()
	return s.backend.Put(context.Background(), req.AgentID, req.Text)
}

// PutShared handles a remote AdvancedStore.PutShared.
func (s *Server) PutShared(req *PutSharedRequest, resp *PutSharedResponse) error {
	defer s.touch()
	return s.backend.PutShared(context.Background(), req.Text)
}

// Recall handles a remote AdvancedStore.Recall.
func (s *Server) Recall(req *RecallRequest, resp *RecallResponse) error {
	defer s.touch()
	facts, err := s.backend.Recall(context.Background(), req.AgentID, req.Query, req.TopK)
	if err != nil {
		return err
	}
	resp.Facts = facts
	return nil
}

// RecallShared handles a remote AdvancedStore.RecallShared.
func (s *Server) RecallShared(req *RecallSharedRequest, resp *RecallSharedResponse) error {
	defer s.touch()
	facts, err := s.backend.RecallShared(context.Background(), req.Query, req.TopK)
	if err != nil {
		return err
	}
	resp.Facts = facts
	return nil
}

// RecallAll handles a remote Store.RecallAll (agent + shared merged).
func (s *Server) RecallAll(req *RecallAllRequest, resp *RecallAllResponse) error {
	defer s.touch()
	facts, err := s.backend.RecallAll(context.Background(), req.AgentID, req.Query, req.TopK)
	if err != nil {
		return err
	}
	resp.Facts = facts
	return nil
}

// List handles a remote AdvancedStore.List.
func (s *Server) List(req *ListRequest, resp *ListResponse) error {
	defer s.touch()
	facts, err := s.backend.List(req.AgentID)
	if err != nil {
		return err
	}
	resp.Facts = facts
	return nil
}

// ListAgents handles a remote AdvancedStore.ListAgents.
func (s *Server) ListAgents(req *ListAgentsRequest, resp *ListAgentsResponse) error {
	defer s.touch()
	ids, err := s.backend.ListAgents()
	if err != nil {
		return err
	}
	resp.AgentIDs = ids
	return nil
}

// Stats handles a remote AdvancedStore.Stats.
func (s *Server) Stats(req *StatsRequest, resp *StatsResponse) error {
	defer s.touch()
	stats, err := s.backend.Stats(req.AgentID)
	if err != nil {
		return err
	}
	resp.Stats = stats
	return nil
}

// Delete handles a remote AdvancedStore.Delete.
func (s *Server) Delete(req *DeleteRequest, resp *DeleteResponse) error {
	defer s.touch()
	return s.backend.Delete(req.AgentID, req.FactID)
}

// UpdateFact handles a remote AdvancedStore.UpdateFact.
func (s *Server) UpdateFact(req *UpdateFactRequest, resp *UpdateFactResponse) error {
	defer s.touch()
	return s.backend.UpdateFact(req.AgentID, req.Fact)
}

// Consolidate handles a remote consolidation request, applying the
// daemon-side consolidation policy.
func (s *Server) Consolidate(req *ConsolidateRequest, resp *ConsolidateResponse) error {
	defer s.touch()
	if s.consolidateCfg == nil {
		return errors.New("consolidation not configured on server")
	}
	return s.backend.Consolidate(context.Background(), req.AgentID, s.consolidateCfg)
}

// PendingVectorCount handles a remote AdvancedStore.PendingVectorCount.
func (s *Server) PendingVectorCount(req *PendingVectorCountRequest, resp *PendingVectorCountResponse) error {
	defer s.touch()
	resp.Count = s.backend.PendingVectorCount()
	return nil
}

// Ping handles a liveness probe. Touches activity so a heartbeat keeps
// the daemon alive — useful for long-idle TUIs that just want the
// dashboard refreshed without making writes.
func (s *Server) Ping(req *PingRequest, resp *PingResponse) error {
	defer s.touch()
	resp.Protocol = Protocol
	return nil
}

// errIsClosedConn returns true if err means "the other side closed cleanly".
// Useful when distinguishing legitimate disconnects from real errors.
func errIsClosedConn(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	return false
}
