package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// seedFact stores one fact and returns its ID.
func seedFact(t *testing.T, base, agent, text string) string {
	t.Helper()
	if status, body := doJSON(t, http.MethodPost, base+"/remember", map[string]string{
		"agent": agent, "text": text,
	}); status != http.StatusOK {
		t.Fatalf("remember: %d %s", status, body)
	}

	status, body := doJSON(t, http.MethodGet, base+"/facts?agent="+agent, nil)
	if status != http.StatusOK {
		t.Fatalf("facts: %d %s", status, body)
	}
	var got struct {
		Facts []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"facts"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal facts: %v", err)
	}
	for _, f := range got.Facts {
		if f.Text == text {
			return f.ID
		}
	}
	t.Fatalf("seeded fact %q not found in %s", text, body)
	return ""
}

func factTexts(t *testing.T, base, agent string) []string {
	t.Helper()
	status, body := doJSON(t, http.MethodGet, base+"/facts?agent="+agent, nil)
	if status != http.StatusOK {
		t.Fatalf("facts: %d %s", status, body)
	}
	var got struct {
		Facts []struct {
			Text string `json:"text"`
		} `json:"facts"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal facts: %v", err)
	}
	texts := make([]string, 0, len(got.Facts))
	for _, f := range got.Facts {
		texts = append(texts, f.Text)
	}
	return texts
}

// TestForgetByID_DeletesExactlyThatFact is the H-11 fix: there was no way to
// delete a specific fact, only "whatever the embedder thinks is closest".
func TestForgetByID_DeletesExactlyThatFact(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	keep := "The Eiffel Tower is in Paris."
	drop := "The capital of France is Paris."
	seedFact(t, base, "alice", keep)
	dropID := seedFact(t, base, "alice", drop)

	status, body := doJSON(t, http.MethodDelete,
		fmt.Sprintf("%s/forget/%s?agent=alice", base, dropID), nil)
	if status != http.StatusOK {
		t.Fatalf("delete by id: %d %s", status, body)
	}
	if !strings.Contains(string(body), dropID) {
		t.Errorf("response does not name the deleted id: %s", body)
	}

	remaining := factTexts(t, base, "alice")
	if len(remaining) != 1 || remaining[0] != keep {
		t.Errorf("wrong facts survived: %v", remaining)
	}
}

func TestForgetByID_UnknownIDIs404(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()
	seedFact(t, base, "alice", "something")

	status, _ := doJSON(t, http.MethodDelete, base+"/forget/01NOSUCHFACTID?agent=alice", nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestForgetByID_RequiresAgent(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	status, _ := doJSON(t, http.MethodDelete, base+"/forget/01ABC", nil)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

// TestForgetByQuery_NeedsConfirmation — similarity delete has no undo and
// picks its victim through an embedder, so an ambiguous query used to remove
// the wrong fact silently.
func TestForgetByQuery_NeedsConfirmation(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	text := "The capital of France is Paris."
	seedFact(t, base, "alice", text)

	// Unconfirmed: a dry run that names the candidate.
	status, body := doJSON(t, http.MethodDelete, base+"/forget", map[string]any{
		"agent": "alice", "query": "France",
	})
	if status != http.StatusOK {
		t.Fatalf("dry run: %d %s", status, body)
	}
	var dry struct {
		Status    string            `json:"status"`
		Candidate map[string]string `json:"candidate"`
	}
	if err := json.Unmarshal(body, &dry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if dry.Status != "confirm_required" {
		t.Errorf("status = %q, want confirm_required; body: %s", dry.Status, body)
	}
	if dry.Candidate["text"] != text || dry.Candidate["id"] == "" {
		t.Errorf("dry run did not name the candidate: %s", body)
	}
	if got := factTexts(t, base, "alice"); len(got) != 1 {
		t.Fatalf("the dry run deleted something: %v", got)
	}

	// Confirmed: it goes.
	status, body = doJSON(t, http.MethodDelete, base+"/forget", map[string]any{
		"agent": "alice", "query": "France", "confirm": true,
	})
	if status != http.StatusOK || !strings.Contains(string(body), "deleted_id") {
		t.Fatalf("confirmed delete: %d %s", status, body)
	}
	if got := factTexts(t, base, "alice"); len(got) != 0 {
		t.Errorf("fact survived a confirmed delete: %v", got)
	}
}

// TestForget_AcceptsExactIDInBody keeps the body form and the path form
// equivalent, so callers do not have to build URLs.
func TestForget_AcceptsExactIDInBody(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	keep := "keep this one"
	seedFact(t, base, "alice", keep)
	id := seedFact(t, base, "alice", "drop this one")

	status, body := doJSON(t, http.MethodDelete, base+"/forget", map[string]any{
		"agent": "alice", "id": id,
	})
	if status != http.StatusOK {
		t.Fatalf("delete by id in body: %d %s", status, body)
	}
	if got := factTexts(t, base, "alice"); len(got) != 1 || got[0] != keep {
		t.Errorf("wrong facts survived: %v", got)
	}
}

func TestForget_RequiresIDOrQuery(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	status, _ := doJSON(t, http.MethodDelete, base+"/forget", map[string]any{"agent": "alice"})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

// TestDecodeBody_RejectsOversizedBodies — decodeBody had no limit, so a client
// could stream an arbitrarily large body into memory.
func TestDecodeBody_RejectsOversizedBodies(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	huge := strings.Repeat("a", maxBodyBytes+4096)
	payload := []byte(`{"agent":"alice","text":"` + huge + `"}`)

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, base+"/remember", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413; body: %s", resp.StatusCode, body)
	}
	// And nothing was stored.
	if got := factTexts(t, base, "alice"); len(got) != 0 {
		t.Errorf("an oversized body still stored something: %d facts", len(got))
	}
}

// TestHardeningHeaders — responses carry stored memory, so nothing in between
// should cache them and nothing should sniff the content type.
func TestHardeningHeaders(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()
	seedFact(t, base, "alice", "sensitive detail")

	for _, tc := range []struct {
		name, url, token string
	}{
		{"data route", base + "/facts?agent=alice", testToken},
		{"healthz", base + "/healthz", ""},
		{"401", base + "/facts?agent=alice", "wrong"},
	} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, tc.url, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		resp.Body.Close()

		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", tc.name, got)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", tc.name, got)
		}
	}
}

// errLeaky carries the kind of detail store errors really carry.
var errLeaky = errors.New(`open store: C:\Users\someone\project\.graymatter\gray.db is locked by another process (pid 4711)`)

type leakyStore struct{ Store }

func (leakyStore) Ready() error { return nil }
func (leakyStore) Remember(context.Context, string, string) error {
	return errLeaky
}
func (leakyStore) Recall(context.Context, string, string, int) ([]string, error) {
	return nil, errLeaky
}
func (leakyStore) Consolidate(context.Context, string) error { return errLeaky }

// TestInternalErrorsAreNotLeaked is H-12: handlers returned err.Error()
// verbatim, and store errors carry absolute paths, PIDs and daemon state.
func TestInternalErrorsAreNotLeaked(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := New(ln.Addr().String(), leakyStore{}, nil, WithAuthToken(testToken))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	base := "http://" + ln.Addr().String()

	cases := []struct {
		name, method, path string
		body               any
	}{
		{"remember", http.MethodPost, "/remember", map[string]string{"agent": "a", "text": "t"}},
		{"recall", http.MethodGet, "/recall?agent=a&q=x", nil},
		{"consolidate", http.MethodPost, "/consolidate", map[string]string{"agent": "a"}},
	}
	for _, tc := range cases {
		status, body := doJSON(t, tc.method, base+tc.path, tc.body)
		if status != http.StatusInternalServerError {
			t.Errorf("%s: status = %d, want 500", tc.name, status)
		}
		for _, leak := range []string{"gray.db", "Users", "pid 4711", "locked by"} {
			if strings.Contains(string(body), leak) {
				t.Errorf("%s: response leaked %q: %s", tc.name, leak, body)
			}
		}
		if !strings.Contains(string(body), "internal error") {
			t.Errorf("%s: response is not the generic message: %s", tc.name, body)
		}
	}
}

// TestValidationErrorsStayDetailed — the caller's own input is theirs to know
// about, so 4xx messages must not be flattened along with the 500s.
func TestValidationErrorsStayDetailed(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	status, body := doJSON(t, http.MethodPost, base+"/remember", map[string]string{"agent": ""})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if !strings.Contains(string(body), "agent and text are required") {
		t.Errorf("validation error lost its detail: %s", body)
	}
}
