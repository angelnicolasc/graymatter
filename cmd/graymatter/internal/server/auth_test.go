package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
)

// TestAuth_RejectsUnauthenticatedRequests is the regression test for H-01: the
// REST API used to serve read, write and delete on every agent's memory to
// anyone who could reach the port.
//
// /healthz is deliberately excluded — an orchestrator has to be able to probe
// liveness — and it is covered separately below.
func TestAuth_RejectsUnauthenticatedRequests(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	// Seed a fact so a successful read would be visibly successful.
	if status, body := doJSON(t, http.MethodPost, base+"/remember", map[string]string{
		"agent": "victim",
		"text":  "internal credential: hunter2",
	}); status != http.StatusOK {
		t.Fatalf("seed remember: %d %s", status, body)
	}

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"remember", http.MethodPost, "/remember", map[string]string{"agent": "victim", "text": "planted"}},
		{"recall", http.MethodGet, "/recall?agent=victim&q=credential", nil},
		{"facts", http.MethodGet, "/facts?agent=victim", nil},
		{"consolidate", http.MethodPost, "/consolidate", map[string]string{"agent": "victim"}},
		{"forget", http.MethodDelete, "/forget", map[string]string{"agent": "victim", "query": "credential"}},
		// /metrics lists every agent ID the server has seen: a target list.
		{"metrics", http.MethodGet, "/metrics", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, cred := range []struct{ label, token string }{
				{"no header", ""},
				{"wrong token", "not-the-token"},
				{"prefix of the real token", testToken[:len(testToken)-1]},
			} {
				status, body := doJSONAuth(t, tc.method, base+tc.path, tc.body, cred.token)
				if status != http.StatusUnauthorized {
					t.Errorf("%s with %s: status = %d, want 401; body: %s",
						tc.name, cred.label, status, body)
				}
				if strings.Contains(string(body), "hunter2") {
					t.Errorf("%s with %s: leaked stored memory: %s", tc.name, cred.label, body)
				}
			}
		})
	}

	// The fact survived every unauthenticated delete attempt.
	status, body := doJSON(t, http.MethodGet, base+"/facts?agent=victim", nil)
	if status != http.StatusOK {
		t.Fatalf("facts: %d %s", status, body)
	}
	if !strings.Contains(string(body), "hunter2") {
		t.Errorf("unauthenticated caller managed to delete the fact: %s", body)
	}
}

// TestAuth_HealthzStaysOpen pins the one documented exception, so nobody has
// to guess whether an open /healthz is a leak or a decision.
func TestAuth_HealthzStaysOpen(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	status, body := doJSONAuth(t, http.MethodGet, base+"/healthz", nil, "")
	if status != http.StatusOK {
		t.Fatalf("healthz without credential: %d %s", status, body)
	}
}

// TestAuth_AcceptsBearerCaseInsensitively — RFC 7235 makes the scheme token
// case-insensitive, and clients differ.
func TestAuth_AcceptsBearerCaseInsensitively(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		req, err := http.NewRequestWithContext(context.Background(),
			http.MethodGet, base+"/facts?agent=nobody", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", scheme+" "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("scheme %q: status = %d, want 200", scheme, resp.StatusCode)
		}
	}
}

// TestAuth_NoTokenConfiguredFailsClosed covers the wiring mistake: a Server
// built without WithAuthToken or WithAnonymousAccess must reject everything
// rather than wave everyone through.
func TestAuth_NoTokenConfiguredFailsClosed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := New(ln.Addr().String(), unreadyStore{err: errStoreGone}, nil)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	base := "http://" + ln.Addr().String()
	for _, token := range []string{"", "anything"} {
		status, body := doJSONAuth(t, http.MethodGet, base+"/facts?agent=x", nil, token)
		if status != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401; body: %s", token, status, body)
		}
	}
}

// TestAuth_AnonymousAccessIsOptIn — the escape hatch has to actually work, or
// users on a loopback-only setup have no migration path.
func TestAuth_AnonymousAccessIsOptIn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := New(ln.Addr().String(), unreadyStore{err: errStoreGone}, nil, WithAnonymousAccess())
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	status, body := doJSONAuth(t, http.MethodGet,
		"http://"+ln.Addr().String()+"/metrics", nil, "")
	if status != http.StatusOK {
		t.Errorf("anonymous access: status = %d, want 200; body: %s", status, body)
	}
}
