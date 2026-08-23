package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/server"
)

// TestRESTServer_ThroughDaemon is the issue #19 acceptance test.
//
// The REST server used to open bbolt itself. Because the daemon owns the write
// lock in normal operation, the server lost that race, came up with a nil store
// and answered every data route with 503 while /healthz still reported ok. It
// now takes a Store, and *Client satisfies that interface directly, so this
// exercises the exact configuration the bug lived in: a daemon owning the store
// while the API serves traffic against it.
//
// It lives here rather than in internal/server because spawning a daemon needs
// this package's build-and-point-at-it helper, and it belongs beside the other
// "several owners of one store" tests.
func TestRESTServer_ThroughDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	withBuiltDaemon(t)
	dir := t.TempDir()

	// The daemon takes the write lock. Under the old code this is precisely
	// what made the server unusable.
	c, err := Connect(dir)
	if err != nil {
		t.Fatalf("Connect (should auto-spawn): %v", err)
	}
	defer func() { _ = c.Close() }()

	if pid := ReadPIDFile(dir); pid == 0 {
		t.Fatal("expected a daemon to own the store before starting the server")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// The daemon client backs every route directly; no second bbolt handle is
	// opened anywhere. Production wraps this in package main's reconnecting
	// store, which is what supplies Ready there.
	// This test is about the store plumbing, not the bearer gate, so it opts
	// out of authentication the way a loopback-only setup does.
	srv := server.New(ln.Addr().String(), restStore{c}, nil, server.WithAnonymousAccess())
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	base := "http://" + ln.Addr().String()

	// Write through the API.
	status, body := doRequest(t, http.MethodPost, base+"/remember", map[string]string{
		"agent": "rest-agent",
		"text":  "the REST API reaches the store through the daemon",
	})
	if status != http.StatusOK {
		t.Fatalf("POST /remember: got %d, want 200; body: %s", status, body)
	}

	// Read it back through the API.
	status, body = doRequest(t, http.MethodGet, base+"/recall?agent=rest-agent&q=daemon", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /recall: got %d, want 200; body: %s", status, body)
	}
	var recalled struct {
		Results []string `json:"results"`
	}
	if err := json.Unmarshal(body, &recalled); err != nil {
		t.Fatalf("decode recall response: %v\n%s", err, body)
	}
	if len(recalled.Results) != 1 || recalled.Results[0] != "the REST API reaches the store through the daemon" {
		t.Fatalf("recall returned %v", recalled.Results)
	}

	// And the same fact is visible to a plain daemon client, confirming both
	// surfaces are talking to one store rather than two.
	facts, err := c.List("rest-agent")
	if err != nil {
		t.Fatalf("List through the daemon client: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("daemon client sees %d facts, want 1", len(facts))
	}

	// The daemon is still the owner; the server never took the lock from it.
	if pid := ReadPIDFile(dir); pid == 0 {
		t.Error("daemon stopped owning the store while the server was running")
	}

	if err := c.Shutdown(); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// restStore adds Ready to *Client so it satisfies server.Store, reusing the
// protocol ping the RPC client already exposes.
type restStore struct{ *Client }

func (r restStore) Ready() error { return r.Ping() }

func doRequest(t *testing.T, method, url string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}
