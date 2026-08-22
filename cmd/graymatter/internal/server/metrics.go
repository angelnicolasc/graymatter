package server

import (
	"expvar"
	"net/http"
	"sync/atomic"
	"time"
)

// Metric keys used to be built straight from client input — the request method,
// the request path, the agent ID — and expvar.Map entries are permanent. A few
// million requests to /x1, /x2, /x3… grew the map until the process died, and
// the agent-ID maps published a complete list of every agent the server had
// seen to anyone who could read /metrics.
//
// Two bounds fix that. Route and method keys come from fixed sets. Agent IDs
// get their own bucket until there are maxAgentKeys of them, after which the
// rest fold into otherBucket: the totals stay honest even when the breakdown
// stops being complete.

// knownRoutes is every path the server actually serves.
var knownRoutes = map[string]bool{
	"/healthz":     true,
	"/remember":    true,
	"/recall":      true,
	"/consolidate": true,
	"/facts":       true,
	"/forget":      true,
	"/metrics":     true,
}

// knownMethods is the HTTP method set. net/http accepts any RFC 7230 token as
// a method, so this is client input too.
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodConnect: true, http.MethodOptions: true, http.MethodTrace: true,
}

// otherBucket collects everything that does not get a key of its own.
const otherBucket = "other"

// maxAgentKeys bounds how many distinct agent IDs get their own counter. A
// store with more agents than this is unusual; an attacker inventing IDs is
// not.
const maxAgentKeys = 1000

// serverMetrics holds all exported metrics for the REST server.
// Values are published via expvar and exposed at GET /metrics.
type serverMetrics struct {
	requestsTotal  *expvar.Map // key: "METHOD /path"
	requestLatency *expvar.Map // key: "METHOD /path" → cumulative µs (atomic int)
	factsTotal     *boundedMap // key: agentID → count
	recallTotal    *boundedMap // key: agentID → count
}

// getOrNewMap returns the existing expvar.Map for name, or creates a new one.
// expvar.NewMap panics on duplicate registration (global singleton), so we must
// guard against multiple servers sharing the same process (e.g. in tests).
func getOrNewMap(name string) *expvar.Map {
	if v := expvar.Get(name); v != nil {
		if m, ok := v.(*expvar.Map); ok {
			return m
		}
	}
	return expvar.NewMap(name)
}

// boundedMap is an expvar.Map that stops minting new keys past a cap and folds
// the overflow into otherBucket.
type boundedMap struct {
	m    *expvar.Map
	max  int64
	keys atomic.Int64
}

func newBoundedMap(name string, max int) *boundedMap {
	m := getOrNewMap(name)
	b := &boundedMap{m: m, max: int64(max)}
	// The map is a process-global that a previous Server may already have
	// populated, so start the count from what is there.
	var n int64
	m.Do(func(expvar.KeyValue) { n++ })
	b.keys.Store(n)
	return b
}

// Add increments key by delta, or otherBucket if key is new and the map is
// full. Keys already present keep counting: the cap limits how many distinct
// keys exist, not how long an existing one lives.
//
// Two goroutines can both admit the same new key and double-count the slot.
// That is a cap off by a handful, which is not worth a lock on this path.
func (b *boundedMap) Add(key string, delta int64) {
	if b.m.Get(key) == nil {
		if b.keys.Load() >= b.max {
			key = otherBucket
		} else {
			b.keys.Add(1)
		}
	}
	b.m.Add(key, delta)
}

// Get exposes the underlying value for a key. Tests only.
func (b *boundedMap) Get(key string) expvar.Var { return b.m.Get(key) }

// len reports how many keys the map holds right now. Tests only.
func (b *boundedMap) len() int {
	n := 0
	b.m.Do(func(expvar.KeyValue) { n++ })
	return n
}

func newServerMetrics(name string) *serverMetrics {
	return &serverMetrics{
		requestsTotal:  getOrNewMap(name + ".requests_total"),
		requestLatency: getOrNewMap(name + ".request_latency_us"),
		factsTotal:     newBoundedMap(name+".facts_total", maxAgentKeys),
		recallTotal:    newBoundedMap(name+".recall_total", maxAgentKeys),
	}
}

// routeKey maps a request onto one of a fixed set of metric keys.
func routeKey(method, path string) string {
	if !knownMethods[method] {
		method = otherBucket
	}
	if !knownRoutes[path] {
		path = otherBucket
	}
	return method + " " + path
}

func (m *serverMetrics) recordRequest(method, path string, status int, d time.Duration) {
	key := routeKey(method, path)
	m.requestsTotal.Add(key, 1)
	// Accumulate latency in microseconds using an atomic int stored in the map.
	m.requestLatency.Add(key, d.Microseconds())
}

func (m *serverMetrics) recordFact(agentID string) {
	m.factsTotal.Add(agentID, 1)
}

func (m *serverMetrics) recordRecall(agentID string) {
	m.recallTotal.Add(agentID, 1)
}

// metricsHandler wraps expvar.Handler so it can be registered on our mux.
func metricsHandler() http.Handler {
	return expvar.Handler()
}

// instrumentedResponseWriter captures the status code and measures duration.
type instrumentedResponseWriter struct {
	http.ResponseWriter
	status    int
	startedAt time.Time
	written   atomic.Bool
}

func newInstrumentedRW(w http.ResponseWriter) *instrumentedResponseWriter {
	return &instrumentedResponseWriter{
		ResponseWriter: w,
		status:         http.StatusOK,
		startedAt:      time.Now(),
	}
}

func (rw *instrumentedResponseWriter) WriteHeader(code int) {
	if rw.written.CompareAndSwap(false, true) {
		rw.status = code
		rw.ResponseWriter.WriteHeader(code)
	}
}

func (rw *instrumentedResponseWriter) elapsed() time.Duration {
	return time.Since(rw.startedAt)
}
