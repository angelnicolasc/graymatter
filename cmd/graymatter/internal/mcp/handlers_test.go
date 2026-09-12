package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	bolt "go.etcd.io/bbolt"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// newTestServer returns an MCP Server backed by a real Memory in a temp dir,
// through the DirectBackend (the same code path --no-daemon uses).
func newTestServer(t *testing.T) (*Server, *graymatter.Memory) {
	t.Helper()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	return New(NewDirectBackend(mem, nil), "test"), mem
}

func reflectReq(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: args}}
}

func TestMemoryAdd_AndSearch(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	// add requires agent_id and text
	if res, _ := s.handleMemoryAdd(ctx, reflectReq(map[string]any{"text": "x"})); !res.IsError {
		t.Error("memory_add without agent_id should error")
	}
	if res, _ := s.handleMemoryAdd(ctx, reflectReq(map[string]any{"agent_id": "a1"})); !res.IsError {
		t.Error("memory_add without text should error")
	}

	res, err := s.handleMemoryAdd(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "text": "the sky is blue",
	}))
	if err != nil || res.IsError {
		t.Fatalf("memory_add failed: %v / %s", err, resultText(t, res))
	}

	// search finds it
	res, err = s.handleMemorySearch(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "query": "sky", "top_k": float64(5),
	}))
	if err != nil || res.IsError {
		t.Fatalf("memory_search failed: %v / %s", err, resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "sky is blue") {
		t.Errorf("search result missing the fact: %s", resultText(t, res))
	}

	// search validation
	if res, _ := s.handleMemorySearch(ctx, reflectReq(map[string]any{"query": "x"})); !res.IsError {
		t.Error("memory_search without agent_id should error")
	}
	if res, _ := s.handleMemorySearch(ctx, reflectReq(map[string]any{"agent_id": "a1"})); !res.IsError {
		t.Error("memory_search without query should error")
	}

	// empty result is a clean message, not an error
	res, _ = s.handleMemorySearch(ctx, reflectReq(map[string]any{
		"agent_id": "nobody", "query": "nothing here",
	}))
	if res.IsError || !strings.Contains(resultText(t, res), "No memories found") {
		t.Errorf("expected clean empty-state, got: %s", resultText(t, res))
	}
}

func TestCheckpoint_SaveResume(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	if res, _ := s.handleCheckpointSave(ctx, reflectReq(map[string]any{})); !res.IsError {
		t.Error("checkpoint_save without agent_id should error")
	}
	// bad JSON state is rejected
	if res, _ := s.handleCheckpointSave(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "state": "{not json",
	})); !res.IsError {
		t.Error("checkpoint_save with invalid JSON state should error")
	}

	res, err := s.handleCheckpointSave(ctx, reflectReq(map[string]any{
		"agent_id": "a1", "state": `{"step":3}`,
	}))
	if err != nil || res.IsError {
		t.Fatalf("checkpoint_save failed: %v / %s", err, resultText(t, res))
	}

	res, err = s.handleCheckpointResume(ctx, reflectReq(map[string]any{"agent_id": "a1"}))
	if err != nil || res.IsError {
		t.Fatalf("checkpoint_resume failed: %v / %s", err, resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), "restored") {
		t.Errorf("resume result unexpected: %s", resultText(t, res))
	}

	// resume for an unknown agent errors
	if res, _ := s.handleCheckpointResume(ctx, reflectReq(map[string]any{"agent_id": "ghost"})); !res.IsError {
		t.Error("checkpoint_resume for unknown agent should error")
	}
}

// TestCheckpointResumeOnMissingEmpty covers the opt-in absence result
// (ADR-015): default and explicit "error" keep the historical text-only tool
// error; "empty" is a successful result carrying the typed absence marker.
func TestCheckpointResumeOnMissingEmpty(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	historical := `no checkpoint found for agent "ghost": no checkpoints for agent "ghost"`
	for _, args := range []map[string]any{
		{"agent_id": "ghost"},
		{"agent_id": "ghost", "on_missing": "error"},
	} {
		res, err := s.handleCheckpointResume(ctx, reflectReq(args))
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if !res.IsError || res.StructuredContent != nil {
			t.Fatalf("args %v: result = %+v, want text-only isError", args, res)
		}
		if got := resultText(t, res); got != historical {
			t.Errorf("args %v: text = %q, want %q", args, got, historical)
		}
	}

	res, err := s.handleCheckpointResume(ctx, reflectReq(map[string]any{
		"agent_id":   "ghost",
		"on_missing": "empty",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("on_missing=empty must be a success result: %s", resultText(t, res))
	}
	payload, ok := res.StructuredContent.(checkpointResumeEmpty)
	if !ok {
		t.Fatalf("structuredContent = %T, want checkpointResumeEmpty", res.StructuredContent)
	}
	if payload.Found || payload.AgentID != "ghost" {
		t.Fatalf("absence payload = %+v, want found=false agent_id=ghost", payload)
	}
	if got, want := resultText(t, res), `No checkpoint saved for agent "ghost" yet.`; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	validateStructuredAgainstToolSchema(t, "checkpoint_resume", res.StructuredContent)
}

// TestCheckpointResumeEmptyResultWireShape pins the false marker on the wire.
// found carries no omitempty precisely so it survives encoding; a regression
// there serialises the payload as {} and re-creates the #117 failure.
func TestCheckpointResumeEmptyResultWireShape(t *testing.T) {
	s, _ := newTestServer(t)
	res, err := s.handleCheckpointResume(context.Background(), reflectReq(map[string]any{
		"agent_id":   "wire-ghost",
		"on_missing": "empty",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var wire struct {
		StructuredContent map[string]json.RawMessage `json:"structuredContent"`
		IsError           bool                       `json:"isError"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if wire.IsError {
		t.Fatalf("isError present on the empty path: %s", raw)
	}
	found, ok := wire.StructuredContent["found"]
	if !ok {
		t.Fatalf("structuredContent dropped \"found\" (omitempty regression): %s", raw)
	}
	var value bool
	if err := json.Unmarshal(found, &value); err != nil || value {
		t.Fatalf("found = %s (%v), want false", found, err)
	}
	if got := string(wire.StructuredContent["agent_id"]); got != `"wire-ghost"` {
		t.Fatalf("agent_id = %s, want \"wire-ghost\"", got)
	}
}

// TestCheckpointResumeOnMissingValidation covers the values a caller can send
// outside the declared enum. mcp-go does not enforce input enums, so the
// handler must, and it must not fall back to the default for a malformed value.
func TestCheckpointResumeOnMissingValidation(t *testing.T) {
	s, _ := newTestServer(t)
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"unknown string", "sometimes"},
		{"empty string", ""},
		{"number", float64(1)},
		{"bool", true},
		{"null", nil},
		{"object", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.handleCheckpointResume(context.Background(), reflectReq(map[string]any{
				"agent_id":   "ghost",
				"on_missing": tc.value,
			}))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if !res.IsError || res.StructuredContent != nil {
				t.Fatalf("on_missing=%v result = %+v, want text-only tool error", tc.value, res)
			}
			if got, want := resultText(t, res), `on_missing must be "error" or "empty"`; got != want {
				t.Errorf("text = %q, want %q", got, want)
			}
		})
	}
}

// TestCheckpointResumeOperationalFailuresStayProseOnly proves the empty mode
// never swallows an operational failure as absence: only the ErrNoCheckpoint
// sentinel selects the found:false result.
func TestCheckpointResumeOperationalFailuresStayProseOnly(t *testing.T) {
	causes := []struct {
		name string
		err  error
	}{
		{"unwrapped absence text", errors.New("no checkpoints")},
		{"daemon failure", errors.New("daemon connection lost")},
		{"storage failure", errors.New("bbolt: database is unavailable")},
	}
	for _, onMissing := range []string{"error", "empty"} {
		for _, cause := range causes {
			t.Run("on_missing="+onMissing+"/"+cause.name, func(t *testing.T) {
				s, _ := newTestServer(t)
				s.backend = resumeErrorBackend{Backend: s.backend, err: cause.err}
				res, err := s.handleCheckpointResume(context.Background(), reflectReq(map[string]any{
					"agent_id":   "sc-a",
					"on_missing": onMissing,
				}))
				if err != nil || !res.IsError || res.StructuredContent != nil {
					t.Fatalf("result = %+v, error = %v; want text-only tool error", res, err)
				}
				if got, want := resultText(t, res), "checkpoint resume error: "+cause.err.Error(); got != want {
					t.Errorf("text = %q, want %q", got, want)
				}
			})
		}
	}
}

// TestCheckpointResumeUninitializedStoreStaysProseOnly keeps the empty mode
// from reporting an unusable store as a successful absence.
func TestCheckpointResumeUninitializedStoreStaysProseOnly(t *testing.T) {
	var empty graymatter.Memory
	for _, onMissing := range []string{"error", "empty"} {
		t.Run("on_missing="+onMissing, func(t *testing.T) {
			s := New(NewDirectBackend(&empty, nil), "test")
			res, err := s.handleCheckpointResume(context.Background(), reflectReq(map[string]any{
				"agent_id":   "sc-a",
				"on_missing": onMissing,
			}))
			if err != nil || !res.IsError || res.StructuredContent != nil {
				t.Fatalf("result = %+v, error = %v; want text-only tool error", res, err)
			}
			if got := resultText(t, res); !strings.Contains(got, "not initialised") {
				t.Errorf("error text = %q, want uninitialized-store diagnostic", got)
			}
		})
	}
}

// TestCheckpointResumeCorruptRecordStaysProseOnly is the decoding-failure side
// of the same rule: a damaged record is not an absent checkpoint, in either
// mode (the #118 classification was pinned for the default mode; the empty
// mode must not weaken it).
func TestCheckpointResumeCorruptRecordStaysProseOnly(t *testing.T) {
	s, mem := newTestServer(t)
	if err := mem.Advanced().DB().Update(func(tx *bolt.Tx) error {
		b, err := tx.Bucket([]byte("sessions")).CreateBucketIfNotExists([]byte("corrupt-agent"))
		if err != nil {
			return err
		}
		return b.Put([]byte("corrupt-id"), []byte("{not valid json"))
	}); err != nil {
		t.Fatalf("write corrupt checkpoint: %v", err)
	}

	for _, onMissing := range []string{"error", "empty"} {
		t.Run("on_missing="+onMissing, func(t *testing.T) {
			res, err := s.handleCheckpointResume(context.Background(), reflectReq(map[string]any{
				"agent_id":   "corrupt-agent",
				"on_missing": onMissing,
			}))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if !res.IsError || res.StructuredContent != nil {
				t.Fatalf("result = %+v, want text-only tool error", res)
			}
			if got := resultText(t, res); !strings.Contains(got, "decode checkpoint") {
				t.Errorf("error text = %q, want decode diagnostic", got)
			}
			if got := resultText(t, res); strings.Contains(got, "not_found") {
				t.Errorf("corrupt checkpoint was misclassified as not_found: %q", got)
			}
		})
	}
}

func TestMemoryReflect_LinkRequiresKG(t *testing.T) {
	// DirectBackend with no KG linker: link must report unavailability.
	s, _ := newTestServer(t)
	res, _ := s.handleMemoryReflect(context.Background(), reflectReq(map[string]any{
		"action": "link", "agent": "a1", "text": "node-a", "target": "node-b",
	}))
	if !res.IsError || !strings.Contains(resultText(t, res), "knowledge graph") {
		t.Errorf("expected KG-unavailable error, got: %s", resultText(t, res))
	}
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content type %T", res.Content[0])
	}
	return tc.Text
}

// factWeight returns the weight of the fact with the given text, or -1 if absent.
func factWeight(t *testing.T, mem *graymatter.Memory, agentID, text string) float64 {
	t.Helper()
	facts, err := mem.Advanced().List(agentID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, f := range facts {
		if f.Text == text {
			return f.Weight
		}
	}
	return -1
}

// factSuperseded reports whether the fact with the given text is retired,
// or false if absent.
func factSuperseded(t *testing.T, mem *graymatter.Memory, agentID, text string) bool {
	t.Helper()
	facts, err := mem.Advanced().List(agentID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, f := range facts {
		if f.Text == text {
			return f.IsSuperseded()
		}
	}
	return false
}

func TestMemoryReflect_ForgetViaText(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()
	const fact = "Workaround for Node 14 bug (project now on Node 18)"

	if err := mem.Remember(ctx, "a1", fact); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	// The exact call shape from the docs that PR #10 reported as broken:
	// forget with the fact in text, no target.
	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "forget",
		"agent":  "a1",
		"text":   fact,
	}))
	if err != nil {
		t.Fatalf("handleMemoryReflect: %v", err)
	}
	if res.IsError {
		t.Fatalf("forget via text should succeed, got error: %s", resultText(t, res))
	}
	// The tombstone keeps the weight decay gave it: zeroing it here would let
	// the next consolidation prune collect the receipt at once (ADR-007).
	if w := factWeight(t, mem, "a1", fact); w <= 0 {
		t.Errorf("fact weight after forget = %v, want it preserved so the receipt survives pruning", w)
	}
}

func TestMemoryReflect_ForgetViaTarget(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()
	const fact = "stale fact"

	if err := mem.Remember(ctx, "a1", fact); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "forget",
		"agent":  "a1",
		"target": fact,
	}))
	if err != nil {
		t.Fatalf("handleMemoryReflect: %v", err)
	}
	if res.IsError {
		t.Fatalf("forget via target should succeed, got error: %s", resultText(t, res))
	}
	if w := factWeight(t, mem, "a1", fact); w <= 0 {
		t.Errorf("fact weight after forget = %v, want it preserved so the receipt survives pruning", w)
	}
}

func TestMemoryReflect_ForgetTargetWinsOverText(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()

	if err := mem.Remember(ctx, "a1", "fact A"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := mem.Remember(ctx, "a1", "fact B"); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "forget",
		"agent":  "a1",
		"text":   "fact A",
		"target": "fact B",
	}))
	if err != nil {
		t.Fatalf("handleMemoryReflect: %v", err)
	}
	if res.IsError {
		t.Fatalf("forget should succeed, got error: %s", resultText(t, res))
	}
	if !factSuperseded(t, mem, "a1", "fact B") {
		t.Errorf("target fact B not retired (target must win)")
	}
	if factSuperseded(t, mem, "a1", "fact A") {
		t.Errorf("text fact A was retired, but target should have won")
	}
}

func TestMemoryReflect_Validation(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{
			name:    "forget without text or target",
			args:    map[string]any{"action": "forget", "agent": "a1"},
			wantErr: "fact to forget",
		},
		{
			name:    "add without text",
			args:    map[string]any{"action": "add", "agent": "a1"},
			wantErr: "text",
		},
		{
			name:    "update without target",
			args:    map[string]any{"action": "update", "agent": "a1", "text": "new"},
			wantErr: "target",
		},
		{
			name:    "update without text",
			args:    map[string]any{"action": "update", "agent": "a1", "target": "old"},
			wantErr: "text",
		},
		{
			name:    "link without text",
			args:    map[string]any{"action": "link", "agent": "a1", "target": "node-b"},
			wantErr: "text",
		},
		{
			name:    "missing agent",
			args:    map[string]any{"action": "add", "text": "x"},
			wantErr: "agent",
		},
		{
			name:    "unknown action",
			args:    map[string]any{"action": "destroy", "agent": "a1", "text": "x"},
			wantErr: "unknown action",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.handleMemoryReflect(ctx, reflectReq(tc.args))
			if err != nil {
				t.Fatalf("handleMemoryReflect: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected tool error, got success: %s", resultText(t, res))
			}
			if got := resultText(t, res); !strings.Contains(got, tc.wantErr) {
				t.Errorf("error %q does not mention %q", got, tc.wantErr)
			}
		})
	}
}

func TestMemoryReflect_UpdateSupersedes(t *testing.T) {
	s, mem := newTestServer(t)
	ctx := context.Background()
	const oldFact = "API base URL is https://api.v1.example.com"
	const newFact = "API base URL is https://api.v2.example.com"

	if err := mem.Remember(ctx, "a1", oldFact); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
		"action": "update",
		"agent":  "a1",
		"text":   newFact,
		"target": oldFact,
	}))
	if err != nil {
		t.Fatalf("handleMemoryReflect: %v", err)
	}
	if res.IsError {
		t.Fatalf("update should succeed, got error: %s", resultText(t, res))
	}
	// Retirement is signalled by the tombstone; the weight is deliberately
	// preserved so the receipt survives pruning (ADR-007).
	if !factSuperseded(t, mem, "a1", oldFact) {
		t.Errorf("old fact not superseded after update")
	}
	if w := factWeight(t, mem, "a1", oldFact); w <= 0 {
		t.Errorf("old fact weight = %v, want preserved (> 0)", w)
	}
	if w := factWeight(t, mem, "a1", newFact); w <= 0 {
		t.Errorf("new fact weight = %v, want > 0", w)
	}
}

func TestMemoryReflect_AllExactMatches(t *testing.T) {
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	for _, transport := range []string{"direct", "daemon_rpc"} {
		t.Run(transport, func(t *testing.T) {
			for _, action := range []string{"forget", "update", "pin", "unpin", "update_same_text"} {
				t.Run(action, func(t *testing.T) {
					s, _ := newTestServer(t)
					if transport == "daemon_rpc" {
						s = New(newDaemonReflectBackend(t), "test")
					}
					ctx := context.Background()
					const text = "the staging database is phoenix-staging.eu-west-1"
					replacement := "the staging database is phoenix-staging.eu-west-2"
					if action == "update_same_text" {
						action, replacement = "update", text
					}
					for _, value := range []string{text, text, text, strings.ToUpper(text), text + " "} {
						mustAdd(t, s, "duplicates", value)
					}
					mustAdd(t, s, "other", text)
					before, err := s.backend.List("duplicates")
					if err != nil || len(before) != 5 {
						t.Fatalf("seed: %v / %d facts", err, len(before))
					}
					retiredID := ""
					for i := range before {
						f := &before[i]
						if f.Text != text {
							continue
						}
						if retiredID == "" {
							retiredID, f.SupersededBy = f.ID, "historical-replacement"
						}
						f.Pinned = action == "unpin"
						if f.Pinned {
							f.PinnedAt = time.Unix(1, 0).UTC()
						}
						if err := s.backend.UpdateFact("duplicates", *f); err != nil {
							t.Fatal(err)
						}
					}
					res, err := s.handleMemoryReflect(ctx, reflectReq(map[string]any{
						"action": action, "agent_id": "duplicates", "target": text, "text": replacement,
					}))
					if err != nil || res.IsError {
						t.Fatalf("%s: %v / %s", action, err, resultText(t, res))
					}
					after, err := s.backend.List("duplicates")
					if err != nil {
						t.Fatal(err)
					}
					byID := make(map[string]memory.Fact)
					replacementID := ""
					for _, f := range after {
						byID[f.ID] = f
						if f.Text == replacement && !f.IsSuperseded() {
							if replacementID != "" {
								t.Fatal("update wrote more than one replacement")
							}
							replacementID = f.ID
						}
					}
					if action == "update" && replacementID == "" {
						t.Fatal("update did not write a live replacement")
					}
					for _, f := range before {
						want := f
						if f.Text == text && (f.ID != retiredID || action == "unpin") {
							switch action {
							case "forget":
								want.SupersededBy = memory.SupersededByAgent
							case "update":
								want.SupersededBy = replacementID
							case "pin":
								want.Pinned, want.PinnedAt = true, byID[f.ID].PinnedAt
								if want.PinnedAt.IsZero() {
									t.Error("pin omitted its timestamp")
								}
							case "unpin":
								want.Pinned, want.PinnedAt = false, time.Time{}
							}
						}
						if !reflect.DeepEqual(byID[f.ID], want) {
							t.Errorf("fact %s: got %+v, want %+v", f.ID, byID[f.ID], want)
						}
					}
					other, err := s.backend.List("other")
					if err != nil || len(other) != 1 || other[0].IsSuperseded() || other[0].Pinned {
						t.Fatalf("other namespace changed: %v / %+v", err, other)
					}
					got := search(t, s, "duplicates", "staging database phoenix")
					if (action == "forget" || action == "update") && replacement != text && strings.Contains(got, ". "+text+"\n") {
						t.Fatalf("retired text is still recallable: %s", got)
					}
					if action == "update" && !strings.Contains(got, replacement) {
						t.Fatalf("replacement is not recallable: %s", got)
					}
					if action == "forget" || (action == "update" && replacement != text) {
						res, err = s.handleMemoryReflect(ctx, reflectReq(map[string]any{
							"action": action, "agent_id": "duplicates", "target": text, "text": replacement,
						}))
						if err != nil || !res.IsError {
							t.Fatalf("retired-only target must fail: %v / %+v", err, res)
						}
						retried, err := s.backend.List("duplicates")
						if err != nil || len(retried) != len(after) {
							t.Fatalf("retired-only target created a fact: %v / %+v", err, retried)
						}
					}
				})
			}
		})
	}
}

type reflectWriteFailureBackend struct {
	*DirectBackend
	updates         int
	failReplacement bool
}

func (b *reflectWriteFailureBackend) UpdateFact(agentID string, f memory.Fact) error {
	b.updates++
	if b.updates == 2 {
		return errors.New("injected write failure")
	}
	return b.DirectBackend.UpdateFact(agentID, f)
}

func (b *reflectWriteFailureBackend) PutReturningFact(ctx context.Context, agentID, text string) (memory.Fact, error) {
	if b.failReplacement {
		return memory.Fact{}, errors.New("injected replacement failure")
	}
	return b.DirectBackend.PutReturningFact(ctx, agentID, text)
}

func TestMemoryReflect_DuplicateWriteFailure(t *testing.T) {
	for _, action := range []string{"forget", "update", "pin", "unpin", "replacement_failure"} {
		t.Run(action, func(t *testing.T) {
			s, _ := newTestServer(t)
			mustAdd(t, s, "a1", staleFact)
			mustAdd(t, s, "a1", staleFact)
			backend := &reflectWriteFailureBackend{DirectBackend: s.backend.(*DirectBackend), failReplacement: action == "replacement_failure"}
			s.backend = backend
			if backend.failReplacement {
				action = "update"
			}
			res, err := s.handleMemoryReflect(context.Background(), reflectReq(map[string]any{
				"action": action, "agent_id": "a1", "target": staleFact, "text": freshFact,
			}))
			if err != nil || !res.IsError || !strings.Contains(resultText(t, res), "injected") {
				t.Fatalf("write failure reported success: %v / %+v", err, res)
			}
			wantUpdates, wantFacts := 2, 2
			if backend.failReplacement {
				wantUpdates = 0
			} else if action == "update" {
				wantFacts++ // replacement must exist before any victim is retired
			}
			facts, err := backend.List("a1")
			if err != nil || len(facts) != wantFacts || backend.updates != wantUpdates {
				t.Fatalf("failure state: err=%v facts=%d updates=%d; want %d/%d", err, len(facts), backend.updates, wantFacts, wantUpdates)
			}
		})
	}
}
