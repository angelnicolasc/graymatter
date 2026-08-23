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
//	DELETE /forget             body: {"agent":"<id>","id":"<fact-id>"}
//	                           or:   {"agent":"<id>","query":"<q>","confirm":true}
//	DELETE /forget/{id}?agent=<id>
//	GET    /healthz
//
// Every route except /healthz requires a bearer token (see Option and package
// httpauth). /healthz stays open so orchestrators can probe liveness without a
// credential; it answers "ok" or "store unavailable" and nothing else.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/httpauth"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const (
	// defaultTopK must stay equal to graymatter.DefaultConfig().TopK. The REST
	// surface used to answer with 5 facts while every other entry point
	// answered with 8, so the same query returned different amounts of context
	// depending on which door the caller came through. Pinned by
	// TestDefaultTopK_MatchesLibraryDefault; the ?k= parameter still overrides.
	defaultTopK   = 8
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

// options holds what Option values accumulate into.
type options struct {
	token     string
	anonymous bool
}

// Option customises a Server at construction. Existing three-argument calls to
// New keep compiling; without WithAuthToken or WithAnonymousAccess the server
// is built with no credential to match and therefore rejects every request but
// /healthz. Failing closed is deliberate: this listener used to serve the whole
// memory store to anyone who could reach the port.
type Option func(*options)

// WithAuthToken requires callers to present token as an HTTP bearer
// credential. The token is compared in constant time.
func WithAuthToken(token string) Option {
	return func(o *options) { o.token = token }
}

// WithAnonymousAccess serves every route without any credential check.
//
// This exists so a local single-user setup can keep scripting against the API
// the way it did before authentication landed. The caller is responsible for
// making sure the listener is loopback-only — `graymatter server` refuses to
// combine --no-auth with an address other people can reach.
func WithAnonymousAccess() Option {
	return func(o *options) { o.anonymous = true }
}

// New creates a Server bound to store that will listen on addr
// (e.g. "127.0.0.1:8080").
func New(addr string, store Store, logger *slog.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	var cfg options
	for _, opt := range opts {
		opt(&cfg)
	}

	m := newServerMetrics("graymatter_server")
	s := &Server{
		store:   store,
		metrics: m,
		addr:    addr,
		logger:  logger,
	}

	// Everything that touches memory goes behind the bearer gate. /healthz is
	// registered on the outer mux, so it — and only it — answers without one.
	protected := http.NewServeMux()
	protected.HandleFunc("POST /remember", s.handleRemember)
	protected.HandleFunc("GET /recall", s.handleRecall)
	protected.HandleFunc("POST /consolidate", s.handleConsolidate)
	protected.HandleFunc("GET /facts", s.handleFacts)
	protected.HandleFunc("DELETE /forget", s.handleForget)
	protected.HandleFunc("DELETE /forget/{id}", s.handleForgetByID)
	// /metrics lists every agent ID the server has seen, which is a free
	// target list for anyone enumerating. It belongs behind the gate too.
	protected.Handle("GET /metrics", metricsHandler())

	var gated http.Handler = protected
	if !cfg.anonymous {
		gated = httpauth.Middleware(cfg.token, protected)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("/", gated)

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
		s.writeInternalError(w, "remember", err)
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
		s.writeInternalError(w, "recall", err)
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
		s.writeInternalError(w, "consolidate", err)
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
		s.writeInternalError(w, "list facts", err)
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

	// ID deletes exactly that fact and nothing else. Preferred over Query.
	ID string `json:"id"`

	// Confirm has to be true for a query-based delete to actually delete.
	// Without it the request is a dry run that names the candidate.
	Confirm bool `json:"confirm"`
}

// handleForgetByID deletes one fact by its exact ID. DELETE /forget/{id}.
//
// The similarity-based delete below has no undo and picks its victim through
// an embedder, so there needs to be a way to say exactly which fact to remove.
func (s *Server) handleForgetByID(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	id := r.PathValue("id")
	if agent == "" || id == "" {
		writeError(w, http.StatusBadRequest, "agent query param and a fact id are required")
		return
	}

	// Confirm the fact exists before reporting a deletion, so a caller can
	// tell "removed it" from "there was nothing there".
	facts, err := s.store.List(agent)
	if err != nil {
		s.writeInternalError(w, "list facts", err)
		return
	}
	for _, f := range facts {
		if f.ID == id {
			if err := s.store.Delete(agent, id); err != nil {
				s.writeInternalError(w, "delete fact", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "deleted_id": id})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"status": "not_found"})
}

// handleForget deletes the single most-similar fact to the query.
//
// "Most similar" is whatever the configured embedder thinks, so an ambiguous
// query used to silently delete the wrong fact with no way back. The delete
// now needs "confirm": true; without it the response names the candidate and
// its ID, which the caller can then pass to DELETE /forget/{id}.
func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	var req forgetRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return
	}

	// An exact ID in the body does the same thing as the path form.
	if req.ID != "" {
		r.SetPathValue("id", req.ID)
		q := r.URL.Query()
		q.Set("agent", req.Agent)
		r.URL.RawQuery = q.Encode()
		s.handleForgetByID(w, r)
		return
	}

	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "id or query is required")
		return
	}

	// Recall 1 result to find the best match, then delete its fact ID.
	results, err := s.store.Recall(r.Context(), req.Agent, req.Query, 1)
	if err != nil {
		s.writeInternalError(w, "recall", err)
		return
	}
	if len(results) == 0 {
		writeJSON(w, http.StatusOK, map[string]string{"status": "not_found"})
		return
	}

	// Find the fact ID by matching the recalled text.
	facts, err := s.store.List(req.Agent)
	if err != nil {
		s.writeInternalError(w, "list facts", err)
		return
	}
	for _, f := range facts {
		if f.Text == results[0] {
			if !req.Confirm {
				writeJSON(w, http.StatusOK, map[string]any{
					"status":    "confirm_required",
					"candidate": map[string]string{"id": f.ID, "text": f.Text},
					"hint":      `re-send with "confirm": true, or DELETE /forget/` + f.ID + "?agent=" + req.Agent,
				})
				return
			}
			if err := s.store.Delete(req.Agent, f.ID); err != nil {
				s.writeInternalError(w, "delete fact", err)
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
	h := w.Header()
	h.Set("Content-Type", "application/json")
	// Responses carry stored memory, which is the sensitive part of this
	// service. Nothing between here and the caller should keep a copy, and
	// nothing should be free to guess at the content type.
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // headers already sent; encoding errors are unrecoverable
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeInternalError logs the real failure and tells the client nothing.
//
// Handlers used to return err.Error() verbatim, and store errors carry
// absolute filesystem paths, daemon state and bbolt internals — reconnaissance
// handed to whoever can reach the port. Validation errors are different: those
// describe the caller's own input, so they stay detailed.
func (s *Server) writeInternalError(w http.ResponseWriter, op string, err error) {
	s.logger.Error("request failed", "op", op, "error", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// maxBodyBytes caps a request body. The largest legitimate body here is one
// fact, and a megabyte of prose is already an implausible fact.
const maxBodyBytes = 1 << 20

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes))
			return false
		}
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
