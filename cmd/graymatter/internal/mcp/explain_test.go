package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// memory_search explain=true is the receipts branch: same ranking as the
// plain path, one receipt per fact, structured payload under the optional
// `explained` key. These tests pin the branch end-to-end through the handler,
// against the real store.

func TestMemorySearchExplain_ReturnsReceipts(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	if res, _ := s.handleMemoryAdd(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "text": "the sky is blue",
	})); res.IsError {
		t.Fatalf("memory_add failed: %s", resultText(t, res))
	}
	if res, _ := s.handleMemoryAdd(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "text": "grass is green in spring",
	})); res.IsError {
		t.Fatalf("memory_add failed: %s", resultText(t, res))
	}

	res, err := s.handleMemorySearch(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "query": "sky", "top_k": float64(5), "explain": true,
	}))
	if err != nil || res.IsError {
		t.Fatalf("explain search failed: %v / %s", err, resultText(t, res))
	}

	// Prose fallback must still carry the fact text and the receipt shape.
	text := resultText(t, res)
	if !strings.Contains(text, "sky is blue") {
		t.Errorf("explain text missing the fact: %s", text)
	}
	if !strings.Contains(text, "fact_id") || !strings.Contains(text, "score") {
		t.Errorf("explain text missing receipt fields: %s", text)
	}

	// Structured payload: the explained array carries full receipts.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	var got struct {
		Count   int `json:"count"`
		Facts   []any `json:"facts"`
		Explained []struct {
			Text    string  `json:"text"`
			Weight  float64 `json:"weight"`
			AgeDays float64 `json:"age_days"`
			Ranks   struct {
				VectorRank  int     `json:"vector_rank"`
				KeywordRank int     `json:"keyword_rank"`
				RecencyRank int     `json:"recency_rank"`
				FusedScore  float64 `json:"fused_score"`
				K           float64 `json:"k"`
			} `json:"ranks"`
			Provenance struct {
				FactID    string `json:"fact_id"`
				WrittenAt string `json:"written_at"`
			} `json:"provenance"`
		} `json:"explained"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode structuredContent: %v\n%s", err, raw)
	}
	if got.Count != 2 || len(got.Explained) != 2 {
		t.Fatalf("count=%d explained=%d, want 2/2\n%s", got.Count, len(got.Explained), raw)
	}
	if len(got.Facts) != 0 {
		t.Errorf("explain mode must not populate the bare facts array (got %d)", len(got.Facts))
	}
	for i, r := range got.Explained {
		if r.Provenance.FactID == "" || r.Provenance.WrittenAt == "" {
			t.Errorf("receipt %d: missing provenance (%+v)", i, r.Provenance)
		}
		if r.Ranks.K != 60 {
			t.Errorf("receipt %d: k = %v, want 60", i, r.Ranks.K)
		}
		if r.Ranks.RecencyRank <= 0 {
			t.Errorf("receipt %d: recency rank %d, want >= 1", i, r.Ranks.RecencyRank)
		}
	}
}

func TestMemorySearchExplain_EmptyIsCleanNotice(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	res, _ := s.handleMemorySearch(ctx, reflectReq(map[string]any{
		"agent_id": "nobody", "query": "nothing", "explain": true,
	}))
	if res.IsError {
		t.Fatalf("empty explain search must not be an error: %s", resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "No memories found") {
		t.Errorf("expected clean empty-state, got: %s", resultText(t, res))
	}
}

func TestMemorySearchExplain_SameFactsAsPlain(t *testing.T) {
	// The whole point of explain: nothing about the ranking changes. Seed one
	// store, query both ways, the fact texts must agree in order.
	s, mem := newTestServer(t)
	ctx := context.Background()

	facts := []string{
		"deployments are signed with the team gpg key before publishing",
		"the staging cluster restarts every night",
		"release notes are drafted from merged pull requests",
	}
	for _, f := range facts {
		if res, _ := s.handleMemoryAdd(ctx, reflectReq(map[string]any{
			"agent_id": "a1", "text": f,
		})); res.IsError {
			t.Fatalf("memory_add failed: %s", resultText(t, res))
		}
	}

	plain, err := s.backend.Recall(ctx, "a1", "gpg signing deployments", 5)
	if err != nil {
		t.Fatalf("plain recall: %v", err)
	}
	explained, err := s.backend.RecallExplain(ctx, "a1", "gpg signing deployments", 5)
	if err != nil {
		t.Fatalf("explain recall: %v", err)
	}
	_ = mem

	if len(plain) != len(explained) {
		t.Fatalf("plain returned %d facts, explain returned %d", len(plain), len(explained))
	}
	for i := range plain {
		if plain[i] != explained[i].Text {
			t.Errorf("position %d: plain %q != explain %q", i, plain[i], explained[i].Text)
		}
	}
}
