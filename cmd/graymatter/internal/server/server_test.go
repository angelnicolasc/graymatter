package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// startTestServer starts the REST server on a random free port and returns
// the base URL + a cleanup function that shuts it down.
// The server is shut down via t.Cleanup so the store is closed before
// TempDir removal (important on Windows where bbolt holds a file lock).
func startTestServer(t *testing.T) (baseURL string, cleanup func()) {
	t.Helper()

	// Create data dir manually so we control cleanup ordering:
	// shutdown must happen before the directory is removed.
	dataDir := t.TempDir()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := New(ln.Addr().String(), dataDir, nil)
	go func() { _ = srv.Serve(ln) }()

	stop := func() { _ = srv.Shutdown(context.Background()) }
	t.Cleanup(stop)

	return "http://" + ln.Addr().String(), stop
}

func doJSON(t *testing.T, method, url string, body any) (statusCode int, respBody []byte) {
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
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestHealthz(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	status, body := doJSON(t, http.MethodGet, base+"/healthz", nil)
	if status != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200; body: %s", status, body)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("status = %q, want %q", got["status"], "ok")
	}
}

func TestRememberAndRecall(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	// Remember two facts for agent "alice".
	for _, text := range []string{
		"The capital of France is Paris.",
		"The Eiffel Tower is in Paris.",
	} {
		status, body := doJSON(t, http.MethodPost, base+"/remember", map[string]string{
			"agent": "alice",
			"text":  text,
		})
		if status != http.StatusOK {
			t.Fatalf("remember status = %d; body: %s", status, body)
		}
	}

	// Recall.
	status, body := doJSON(t, http.MethodGet,
		fmt.Sprintf("%s/recall?agent=alice&q=Paris&k=5", base), nil)
	if status != http.StatusOK {
		t.Fatalf("recall status = %d; body: %s", status, body)
	}
	var got map[string][]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal recall: %v", err)
	}
	if len(got["results"]) == 0 {
		t.Error("expected recall results, got none")
	}
	// At least one result should mention Paris.
	found := false
	for _, r := range got["results"] {
		if strings.Contains(r, "Paris") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no result mentions Paris; results = %v", got["results"])
	}
}

func TestRemember_MissingFields(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	cases := []map[string]string{
		{"agent": "", "text": "hello"},
		{"agent": "bob", "text": ""},
		{},
	}
	for _, c := range cases {
		status, _ := doJSON(t, http.MethodPost, base+"/remember", c)
		if status != http.StatusBadRequest {
			t.Errorf("expected 400 for %v, got %d", c, status)
		}
	}
}

func TestRecall_MissingParams(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	cases := []string{
		base + "/recall",
		base + "/recall?agent=x",
		base + "/recall?q=hello",
	}
	for _, url := range cases {
		status, _ := doJSON(t, http.MethodGet, url, nil)
		if status != http.StatusBadRequest {
			t.Errorf("expected 400 for %s, got %d", url, status)
		}
	}
}

func TestFacts(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	// Remember 3 facts.
	for i := 0; i < 3; i++ {
		doJSON(t, http.MethodPost, base+"/remember", map[string]string{
			"agent": "charlie",
			"text":  fmt.Sprintf("fact number %d", i+1),
		})
	}

	status, body := doJSON(t, http.MethodGet, base+"/facts?agent=charlie", nil)
	if status != http.StatusOK {
		t.Fatalf("facts status = %d; body: %s", status, body)
	}
	var got map[string][]map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal facts: %v", err)
	}
	if len(got["facts"]) != 3 {
		t.Errorf("expected 3 facts, got %d", len(got["facts"]))
	}
}

func TestFacts_LimitParam(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	for i := 0; i < 5; i++ {
		doJSON(t, http.MethodPost, base+"/remember", map[string]string{
			"agent": "limited",
			"text":  fmt.Sprintf("fact %d", i+1),
		})
	}

	status, body := doJSON(t, http.MethodGet, base+"/facts?agent=limited&limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("facts status = %d", status)
	}
	var got map[string][]map[string]any
	_ = json.Unmarshal(body, &got)
	if len(got["facts"]) != 2 {
		t.Errorf("expected 2 facts with limit=2, got %d", len(got["facts"]))
	}
}

func TestForget(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	doJSON(t, http.MethodPost, base+"/remember", map[string]string{
		"agent": "dave",
		"text":  "The sky is blue.",
	})
	doJSON(t, http.MethodPost, base+"/remember", map[string]string{
		"agent": "dave",
		"text":  "Grass is green.",
	})

	status, body := doJSON(t, http.MethodDelete, base+"/forget", map[string]string{
		"agent": "dave",
		"query": "sky blue",
	})
	if status != http.StatusOK {
		t.Fatalf("forget status = %d; body: %s", status, body)
	}
	var got map[string]string
	_ = json.Unmarshal(body, &got)
	if got["status"] != "ok" && got["status"] != "not_found" {
		t.Errorf("unexpected status: %q", got["status"])
	}
}

func TestConsolidate_NoAPIKey(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	// Without ANTHROPIC_API_KEY set the server should return 503.
	t.Setenv("ANTHROPIC_API_KEY", "")

	status, body := doJSON(t, http.MethodPost, base+"/consolidate", map[string]string{
		"agent": "eve",
	})
	if status != http.StatusServiceUnavailable {
		t.Errorf("expected 503 without API key, got %d; body: %s", status, body)
	}
}

func TestUnknownRoute(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	resp, err := http.Get(base + "/nosuchroute") //nolint:noctx
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown route, got %d", resp.StatusCode)
	}
}

// TestConcurrentRememberAndRecall validates that the single-store architecture
// (Fase-0A fix) handles concurrent reads and writes without races or errors.
// Before the fix, bbolt's file lock caused every concurrent request to fail.
func TestConcurrentRememberAndRecall(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	const writers = 10
	const readers = 10
	const factsPerWriter = 5

	errs := make(chan string, (writers+readers)*factsPerWriter)

	// Pre-populate one fact so recall has something to return.
	status, body := doJSON(t, http.MethodPost, base+"/remember", map[string]string{
		"agent": "concurrent-agent",
		"text":  "seed fact for concurrent recall test",
	})
	if status != http.StatusOK {
		t.Fatalf("seed remember failed: %d %s", status, body)
	}

	done := make(chan struct{})
	// Writers.
	for w := 0; w < writers; w++ {
		go func(id int) {
			for i := 0; i < factsPerWriter; i++ {
				st, b := doJSON(t, http.MethodPost, base+"/remember", map[string]string{
					"agent": "concurrent-agent",
					"text":  fmt.Sprintf("writer %d fact %d", id, i),
				})
				if st != http.StatusOK {
					errs <- fmt.Sprintf("writer %d/%d: status %d body %s", id, i, st, b)
				}
			}
			done <- struct{}{}
		}(w)
	}
	// Readers.
	for r := 0; r < readers; r++ {
		go func(id int) {
			for i := 0; i < factsPerWriter; i++ {
				st, b := doJSON(t, http.MethodGet,
					fmt.Sprintf("%s/recall?agent=concurrent-agent&q=fact&k=5", base), nil)
				if st != http.StatusOK {
					errs <- fmt.Sprintf("reader %d/%d: status %d body %s", id, i, st, b)
				}
			}
			done <- struct{}{}
		}(r)
	}

	for i := 0; i < writers+readers; i++ {
		<-done
	}
	close(errs)

	for e := range errs {
		t.Error(e)
	}

	// Verify all written facts are persisted.
	st, body := doJSON(t, http.MethodGet,
		fmt.Sprintf("%s/facts?agent=concurrent-agent&limit=1000", base), nil)
	if st != http.StatusOK {
		t.Fatalf("facts status = %d", st)
	}
	var got map[string][]map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := 1 + writers*factsPerWriter // seed + all writers
	if len(got["facts"]) != want {
		t.Errorf("expected %d facts, got %d", want, len(got["facts"]))
	}
}

// TestRequestContext_Cancellation verifies that using a cancelled context on the
// client side is handled gracefully — the server must not deadlock or panic.
func TestRequestContext_Cancellation(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	// Seed a fact so there's data to recall.
	doJSON(t, http.MethodPost, base+"/remember", map[string]string{
		"agent": "ctx-agent",
		"text":  "context cancellation test fact",
	})

	// Fire several requests with an already-cancelled context.
	// The client will fail fast; the server must not be left in a bad state.
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			base+"/recall?agent=ctx-agent&q=test&k=5", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		// We accept either an error (context cancelled) or a 200 (server was
		// fast enough). What we must not see is a hang or a 5xx.
		if err == nil && resp.StatusCode >= 500 {
			t.Errorf("unexpected 5xx after cancelled context: %d", resp.StatusCode)
		}
	}

	// Server must still be healthy after all the cancelled requests.
	st, _ := doJSON(t, http.MethodGet, base+"/healthz", nil)
	if st != http.StatusOK {
		t.Errorf("server unhealthy after cancelled requests: status %d", st)
	}
}
