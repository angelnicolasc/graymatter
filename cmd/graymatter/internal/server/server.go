// Package server exposes GrayMatter memory operations over a minimal HTTP/JSON
// REST API. It is intentionally small — its only purpose is to let non-Go
// processes (Python scripts, shell agents, etc.) interact with the same bbolt
// store that the CLI uses.
//
// The store arrives as a constructor argument (see Store). This package never
// opens bbolt, so it reaches the same store every other command does, through
// the daemon when one owns it.
//
// Routes:
//
//	POST   /remember           body: {"agent":"<id>","text":"<text>"}
//	GET    /recall?agent=<id>&q=<query>[&k=<int>]
//	POST   /consolidate        body: {"agent":"<id>"}
//	GET    /facts?agent=<id>[&limit=<int>]
//	DELETE /forget             body: {"agent":"<id>","query":"<query>"}
//	GET    /healthz
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const (
	defaultTopK   = 5
	defaultLimit  = 50
	readTimeout   = 15 * time.Second
	writeTimeout  = 30 * time.Second
	idleTimeout   = 60 * time.Second
	shutdownGrace = 5 * time.Second
)

// Store is the persistence surface the handlers need.
//
// The server takes this rather than opening bbolt itself. bbolt is single
// writer and the daemon owns the lock in normal operation (issue #8), so a
// second opener simply fails: the server used to come up anyway with a nil
// store and 503 every data route while /healthz still reported ok. The caller
// passes in whatever `openStore` returns, daemon client or direct store, and
// the server stays out of the lock business entirely (issue #19).
//
// The caller owns the store's lifecycle. Shutdown does not close it.
type Store interface {
	Remember(ctx context.Context, agentID, text string) error
	Recall(ctx context.Context, agentID, query string, topK int) ([]string, error)
	List(agentID string) ([]memory.Fact, error)
	Delete(agentID, factID string) error
	Consolidate(ctx context.Context, agentID string) error

	// Ready reports whether the store answers right now. It backs /healthz,
	// which otherwise has no way to tell "serving" apart from "serving
	// nothing but errors".
	Ready() error
}

// Server wraps an HTTP server backed by a GrayMatter memory store.
type Server struct {
	httpSrv *http.Server
	store   Store
	metrics *serverMetrics
	addr    string
	logger  *slog.Logger
}

// New creates a Server bound to store that will listen on addr (e.g. ":8080").
func New(addr string, store Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	m := newServerMetrics("graymatter_server")
	s := &Server{
		store:   store,
		metrics: m,
		addr:    addr,
		logger:  logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /remember", s.handleRemember)
	mux.HandleFunc("GET /recall", s.handleRecall)
	mux.HandleFunc("POST /consolidate", s.handleConsolidate)
	mux.HandleFunc("GET /facts", s.handleFacts)
	mux.HandleFunc("DELETE /forget", s.handleForget)
	mux.Handle("GET /metrics", metricsHandler())

	s.httpSrv = &http.Server{
		Addr:         addr,
		Handler:      s.loggingMiddleware(mux),
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	}
	return s
}

// Addr returns the address the server is listening on.
func (s *Server) Addr() string { return s.addr }

// ListenAndServe starts the HTTP server. Blocks until shutdown.
func (s *Server) ListenAndServe() error {
	s.logger.Info("graymatter REST API listening", "addr", s.addr)
	return s.httpSrv.ListenAndServe()
}

// Serve accepts connections on l. Used in tests to bind to a free port.
func (s *Server) Serve(l net.Listener) error {
	s.addr = l.Addr().String()
	s.logger.Info("graymatter REST API listening", "addr", s.addr)
	return s.httpSrv.Serve(l)
}

// Shutdown gracefully stops the HTTP server. The store belongs to the caller
// that constructed it, so closing it is the caller's job.
func (s *Server) Shutdown(ctx context.Context) error {
	shutCtx, cancel := context.WithTimeout(ctx, shutdownGrace)
	defer cancel()
	return s.httpSrv.Shutdown(shutCtx)
}

// --- handlers ---

// handleHealthz reports readiness, not just liveness.
//
// This endpoint used to answer ok unconditionally, which is how a server that
// 503'd every data route still looked healthy to anything watching it (issue
// #19). A GrayMatter server that cannot reach its store has nothing to offer,
// so the store is the one dependency worth probing here.
//
// The reason for a failure goes to the log, not the response: a probe is often
// reachable from further away than the service itself.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ready(); err != nil {
		s.logger.Error("healthz: store not ready", "error", err)
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type rememberRequest struct {
	Agent string `json:"agent"`
	Text  string `json:"text"`
}

func (s *Server) handleRemember(w http.ResponseWriter, r *http.Request) {
	var req rememberRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Agent == "" || req.Text == "" {
		writeError(w, http.StatusBadRequest, "agent and text are required")
		return
	}
	if err := s.store.Remember(r.Context(), req.Agent, req.Text); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.metrics.recordFact(req.Agent)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	query := r.URL.Query().Get("q")
	if agent == "" || query == "" {
		writeError(w, http.StatusBadRequest, "agent and q query params are required")
		return
	}
	topK := defaultTopK
	if ks := r.URL.Query().Get("k"); ks != "" {
		if v, err := strconv.Atoi(ks); err == nil && v > 0 {
			topK = v
		}
	}
	results, err := s.store.Recall(r.Context(), agent, query, topK)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.metrics.recordRecall(agent)
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

type consolidateRequest struct {
	Agent string `json:"agent"`
}

func (s *Server) handleConsolidate(w http.ResponseWriter, r *http.Request) {
	var req consolidateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return
	}
	// No API-key gate here. Consolidation is mostly decay and pruning, which
	// need no LLM; summarisation is one conditional step that runs when the
	// store owner has a provider configured and the agent is over threshold.
	// Gating on this process's environment was wrong in both directions once
	// the work moved behind the daemon: it rejected requests the daemon could
	// have served, and admitted ones where the daemon had no key anyway.
	// Whoever owns the store owns the policy.
	if err := s.store.Consolidate(r.Context(), req.Agent); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleFacts(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeError(w, http.StatusBadRequest, "agent query param is required")
		return
	}
	limit := defaultLimit
	if ls := r.URL.Query().Get("limit"); ls != "" {
		if v, err := strconv.Atoi(ls); err == nil && v > 0 {
			limit = v
		}
	}
	facts, err := s.store.List(agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(facts) > limit {
		facts = facts[:limit]
	}

	// Return only the fields needed by external callers.
	type factView struct {
		ID        string    `json:"id"`
		Text      string    `json:"text"`
		Weight    float64   `json:"weight"`
		CreatedAt time.Time `json:"created_at"`
	}
	views := make([]factView, len(facts))
	for i, f := range facts {
		views[i] = factView{ID: f.ID, Text: f.Text, Weight: f.Weight, CreatedAt: f.CreatedAt}
	}
	writeJSON(w, http.StatusOK, map[string]any{"facts": views})
}

type forgetRequest struct {
	Agent string `json:"agent"`
	Query string `json:"query"`
}

// handleForget deletes the single most-similar fact to the query.
func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	var req forgetRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Agent == "" || req.Query == "" {
		writeError(w, http.StatusBadRequest, "agent and query are required")
		return
	}
	// Recall 1 result to find the best match, then delete its fact ID.
	results, err := s.store.Recall(r.Context(), req.Agent, req.Query, 1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(results) == 0 {
		writeJSON(w, http.StatusOK, map[string]string{"status": "not_found"})
		return
	}

	// Find the fact ID by matching the recalled text.
	facts, err := s.store.List(req.Agent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, f := range facts {
		if f.Text == results[0] {
			if err := s.store.Delete(req.Agent, f.ID); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "deleted_id": f.ID})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "not_found"})
}

// --- HTTP utilities ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // headers already sent; encoding errors are unrecoverable
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := newInstrumentedRW(w)
		next.ServeHTTP(rw, r)
		elapsed := rw.elapsed()
		s.logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", elapsed.String(),
		)
		s.metrics.recordRequest(r.Method, r.URL.Path, rw.status, elapsed)
	})
}
