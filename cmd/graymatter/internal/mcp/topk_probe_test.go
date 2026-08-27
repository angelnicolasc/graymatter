package mcp

import (
	"context"
	"testing"
)

// TestTopKNegativeThroughHandler is the empirical probe for TD-003: a
// malicious or buggy client sending top_k=-5 must not panic the server, and
// its observable behaviour must be one of the documented ones (default or
// error), never a slice-bounds crash.
func TestTopKNegativeThroughHandler(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		if err := mem.Remember(ctx, "tk", "fact number for probe"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	cases := []struct {
		name string
		topK float64
	}{
		{"negative", -5},
		{"very negative", -1 << 40},
		{"zero", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PANIC with top_k=%v: %v", tc.topK, r)
				}
			}()
			res, err := s.handleMemorySearch(ctx, reflectReq(map[string]any{
				"agent_id": "tk", "query": "fact number", "top_k": tc.topK,
			}))
			if err != nil {
				t.Fatalf("handler returned Go error: %v", err)
			}
			if res.IsError {
				t.Logf("top_k=%v -> tool error (acceptable documented path)", tc.topK)
				return
			}
			t.Logf("top_k=%v -> success: %.120s", tc.topK, resultText(t, res))
		})
	}
}
