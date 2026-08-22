package server

import (
	"expvar"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// freshMetrics returns a serverMetrics on maps nothing else in the process
// shares. expvar maps are global singletons keyed by name, so tests that count
// keys need their own namespace.
func freshMetrics(t *testing.T) *serverMetrics {
	t.Helper()
	return newServerMetrics(fmt.Sprintf("test_%s_%d", t.Name(), time.Now().UnixNano()))
}

// TestMetrics_BoundedRouteKeys is the H-08 regression test: the request path
// and method were the metric key, and expvar entries are permanent, so a
// stream of unique paths grew the map until the process ran out of memory.
func TestMetrics_BoundedRouteKeys(t *testing.T) {
	m := freshMetrics(t)

	for i := 0; i < 20000; i++ {
		m.recordRequest(http.MethodGet, fmt.Sprintf("/x%d", i), 404, time.Millisecond)
	}
	// A method is a client-supplied token too, so it needs the same treatment:
	// net/http accepts any RFC 7230 token, not just the nine standard verbs.
	for i := 0; i < 20000; i++ {
		m.recordRequest(fmt.Sprintf("VERB%d", i), "/facts", 405, time.Millisecond)
	}
	// Real traffic still gets real keys.
	m.recordRequest(http.MethodPost, "/remember", 200, time.Millisecond)
	m.recordRequest(http.MethodGet, "/recall", 200, time.Millisecond)

	keys := map[string]bool{}
	m.requestsTotal.Do(func(kv expvar.KeyValue) { keys[kv.Key] = true })

	// GET other, other /facts, POST /remember, GET /recall — four, not 40 002.
	if len(keys) > 8 {
		t.Errorf("requests_total holds %d keys after 40k unique requests: %v", len(keys), keys)
	}
	for _, want := range []string{"GET other", "other /facts", "POST /remember", "GET /recall"} {
		if !keys[want] {
			t.Errorf("expected key %q; got %v", want, keys)
		}
	}

	// The latency map is keyed the same way and must be bounded the same way.
	latencyKeys := 0
	m.requestLatency.Do(func(expvar.KeyValue) { latencyKeys++ })
	if latencyKeys > 8 {
		t.Errorf("request_latency_us holds %d keys, want the same bounded set", latencyKeys)
	}
}

// TestMetrics_AgentKeysAreCapped — agent IDs come from the request body, so
// they are attacker-chosen too, and /metrics used to publish the full list.
func TestMetrics_AgentKeysAreCapped(t *testing.T) {
	m := freshMetrics(t)

	const invented = maxAgentKeys * 3
	for i := 0; i < invented; i++ {
		m.recordFact(fmt.Sprintf("agent-%d", i))
	}

	if got := m.factsTotal.len(); got > maxAgentKeys+1 {
		t.Errorf("facts_total holds %d keys after %d invented agents, cap is %d",
			got, invented, maxAgentKeys)
	}
	if m.factsTotal.Get(otherBucket) == nil {
		t.Error("overflow was dropped instead of folded into the other bucket")
	}

	// An agent that already has a bucket keeps counting past the cap: the cap
	// limits how many keys exist, not how long one lives.
	before := m.factsTotal.Get("agent-0").String()
	m.recordFact("agent-0")
	if after := m.factsTotal.Get("agent-0").String(); after == before {
		t.Errorf("an established agent stopped counting: %s then %s", before, after)
	}
}

func TestMetrics_RecallKeysAreCapped(t *testing.T) {
	m := freshMetrics(t)

	for i := 0; i < maxAgentKeys*2; i++ {
		m.recordRecall(fmt.Sprintf("agent-%d", i))
	}
	if got := m.recallTotal.len(); got > maxAgentKeys+1 {
		t.Errorf("recall_total holds %d keys, cap is %d", got, maxAgentKeys)
	}
}

func TestRouteKey(t *testing.T) {
	tests := []struct {
		method, path, want string
	}{
		{"GET", "/facts", "GET /facts"},
		{"POST", "/remember", "POST /remember"},
		{"GET", "/nope", "GET other"},
		{"GET", "/facts/../../etc", "GET other"},
		{"BREW", "/facts", "other /facts"},
		{"BREW", "/nope", "other other"},
		{"", "", "other other"},
	}
	for _, tc := range tests {
		if got := routeKey(tc.method, tc.path); got != tc.want {
			t.Errorf("routeKey(%q, %q) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

// TestBoundedMap_CountsPreexistingKeys — expvar maps are process-global, so a
// second Server on the same name inherits whatever the first one wrote. The
// cap has to account for that rather than starting from zero.
func TestBoundedMap_CountsPreexistingKeys(t *testing.T) {
	name := fmt.Sprintf("test_%s_%d", t.Name(), time.Now().UnixNano())

	first := newBoundedMap(name, 4)
	for i := 0; i < 4; i++ {
		first.Add(fmt.Sprintf("k%d", i), 1)
	}

	second := newBoundedMap(name, 4)
	second.Add("fresh", 1)

	if second.Get("fresh") != nil {
		t.Error("a second instance minted a key past the cap the first one filled")
	}
	if second.Get(otherBucket) == nil {
		t.Error("the overflow did not land in the other bucket")
	}
}
