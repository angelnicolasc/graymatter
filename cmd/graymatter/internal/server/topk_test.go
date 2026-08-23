package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
)

// The REST server answered recall with 5 facts while the library, the CLI, the
// MCP tools and the TUI all answered with 8. Same store, same query, two
// different amounts of context depending on which door you came through — and
// nothing in the code said the divergence was intended.
//
// This test reads the library default rather than repeating a number, so the
// two cannot drift apart again silently: change TopK in DefaultConfig and this
// fails until the REST default follows.
func TestDefaultTopK_MatchesLibraryDefault(t *testing.T) {
	want := graymatter.DefaultConfig().TopK
	if defaultTopK != want {
		t.Errorf("REST recall defaults to %d facts, the library to %d. "+
			"One store should not answer the same question two ways.", defaultTopK, want)
	}
}

// TestRecall_DefaultReturnsLibraryTopK checks it end-to-end over HTTP, so the
// constant being right is not the only thing keeping it true.
func TestRecall_DefaultReturnsLibraryTopK(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	want := graymatter.DefaultConfig().TopK

	// Store more facts than any plausible default so the response size is
	// governed by the default rather than by what is available.
	for i := 0; i < want+6; i++ {
		body := map[string]any{
			"agent": "topk-agent",
			"text":  fmt.Sprintf("deployment note number %d about the release pipeline", i),
		}
		if status, _ := doJSON(t, http.MethodPost, base+"/remember", body); status != http.StatusOK &&
			status != http.StatusCreated {
			t.Fatalf("remember %d: status %d", i, status)
		}
	}

	status, resp := doJSON(t, http.MethodGet, base+"/recall?agent=topk-agent&q=release+pipeline", nil)
	if status != http.StatusOK {
		t.Fatalf("recall: status %d", status)
	}
	results := decodeResults(t, resp)
	if len(results) != want {
		t.Errorf("GET /recall with no k returned %d facts, want the library default of %d",
			len(results), want)
	}
}

// TestRecall_ExplicitKStillOverrides guards the fix from overcorrecting: the
// default moved, the k parameter did not.
func TestRecall_ExplicitKStillOverrides(t *testing.T) {
	base, stop := startTestServer(t)
	defer stop()

	for i := 0; i < 12; i++ {
		body := map[string]any{
			"agent": "override-agent",
			"text":  fmt.Sprintf("deployment note number %d about the release pipeline", i),
		}
		if status, _ := doJSON(t, http.MethodPost, base+"/remember", body); status != http.StatusOK &&
			status != http.StatusCreated {
			t.Fatalf("remember %d: status %d", i, status)
		}
	}

	status, resp := doJSON(t, http.MethodGet, base+"/recall?agent=override-agent&q=release+pipeline&k=3", nil)
	if status != http.StatusOK {
		t.Fatalf("recall: status %d", status)
	}
	results := decodeResults(t, resp)
	if len(results) != 3 {
		t.Errorf("GET /recall?k=3 returned %d facts, want 3", len(results))
	}
}

// decodeResults pulls the results array out of a /recall response body.
func decodeResults(t *testing.T, body []byte) []string {
	t.Helper()
	var parsed struct {
		Results []string `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode recall response %s: %v", body, err)
	}
	return parsed.Results
}
