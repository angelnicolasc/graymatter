package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── adversarial stdin ───────────────────────────────────────────────────────
//
// Claude Code owns the payload; the runner must survive every shape it can
// send — wrong types, empty objects, truncation, garbage — without crashing
// and without storing junk. readHookPayload never errors by contract; these
// tests pin what it produces for hostile input.

func TestHookPayload_AdversarialStdin(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantPrompt string // "" = prompt must come out empty
	}{
		{"empty object", `{}`, ""},
		{"null", `null`, ""},
		{"wrong type for prompt", `{"prompt": 42}`, ""},
		{"prompt as object", `{"prompt": {"a": 1}}`, ""},
		{"prompt as array", `{"prompt": []}`, ""},
		{"null prompt", `{"prompt": null}`, ""},
		{"truncated json", `{"session_id": "abc", "prom`, ""},
		{"binary garbage", "\x00\x01\x02\xff\xfe", ""},
		{"json with bom", "\xef\xbb\xbf" + `{"prompt":"hello"}`, "hello"},
		{"valid but huge nesting", `{"prompt":"ok","x":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}`, "ok"},
		{"unicode prompt", `{"prompt":"María 🚀 deployment"}`, "María 🚀 deployment"},
		{"crlf inside prompt", "{\"prompt\":\"line one\\r\\nline two\"}", "line one\r\nline two"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := readHookPayload(strings.NewReader(c.input))
			if p.Prompt != c.wantPrompt {
				t.Errorf("prompt = %q, want %q", p.Prompt, c.wantPrompt)
			}
		})
	}
}

// TestHookPayload_OversizedInputIsTruncated: the 1 MiB cap must hold — a
// bloated stdin is truncated, the truncated tail fails to parse as JSON, and
// the payload degrades to empty instead of consuming memory without bound.
func TestHookPayload_OversizedInputIsTruncated(t *testing.T) {
	big := `{"prompt":"` + strings.Repeat("a", 3<<20) + `"}`
	p := readHookPayload(strings.NewReader(big))
	if p.Prompt != "" {
		t.Errorf("oversized payload produced prompt %q (len %d), want empty", p.Prompt, len(p.Prompt))
	}
	if p.SessionID != "" {
		t.Errorf("oversized payload leaked session_id %q", p.SessionID)
	}
}

// ── remember: edge cases ────────────────────────────────────────────────────

func TestHookRemember_EdgeCases(t *testing.T) {
	withHooksEnv(t)
	cwd := mustWorkdir()

	ok := func(prompt, wantStored string, name string) {
		t.Helper()
		out, err := dispatchHook("user-prompt", hookEventPayload{CWD: cwd, Prompt: prompt})
		if err != nil {
			t.Fatalf("%s: dispatch: %v", name, err)
		}
		if !strings.Contains(out, "Saved to memory") {
			t.Errorf("%s: confirmation missing in %q", name, out)
		}
		store, err := openStore()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		facts, _ := store.List(deriveAgentID(cwd))
		found := false
		for _, f := range facts {
			if f.Text == wantStored {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: stored facts do not contain %q (have %d)", name, wantStored, len(facts))
		}
	}

	ok("Remember: capital R works", "capital R works", "capital R prefix")
	ok("REMEMBER: shouty works", "shouty works", "shouty prefix")
	ok("remember:no space still saves", "no space still saves", "no space")
	ok("remember:   padded   ", "padded", "whitespace trim")

	// Non-remember prompts must not store anything new.
	before := countFacts(t, cwd)
	if _, err := dispatchHook("user-prompt", hookEventPayload{CWD: cwd, Prompt: "remembering the old days"}); err != nil {
		t.Fatal(err)
	}
	if after := countFacts(t, cwd); after != before {
		t.Errorf("a prompt merely STARTING with the word remembering stored a fact (%d → %d)", before, after)
	}
}

func countFacts(t *testing.T, cwd string) int {
	t.Helper()
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	facts, _ := store.List(deriveAgentID(cwd))
	return len(facts)
}

// ── throttle state ──────────────────────────────────────────────────────────

// TestHookState_CorruptedStateFileIsUnseen: a corrupt state.json must behave
// as "not seen" (inject again), never crash the hook.
func TestHookState_CorruptedStateFileIsUnseen(t *testing.T) {
	withHooksEnv(t)
	seedHookStoreFacts(t, "the rate limit is 60 requests per minute")

	// Corrupt the state file after a first (injecting) call.
	if _, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "rate limit"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookStatePath(dataDir), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "rate limit again"})
	if err != nil {
		t.Fatalf("corrupt state must not error the hook: %v", err)
	}
	if out == "" {
		t.Error("corrupt state must be treated as unseen and inject again")
	}
}

// TestHookState_PerAgentKeys: two agents in one project must not suppress
// each other's injections.
func TestHookState_PerAgentKeys(t *testing.T) {
	withHooksEnv(t)
	seedHookStoreFacts(t, "the rate limit is 60 requests per minute", "postgres runs on db-01")

	agent := deriveAgentID(mustWorkdir())
	if _, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "rate limit"}); err != nil {
		t.Fatal(err)
	}

	// Simulate a second agent: same state file, different key.
	path := hookStatePath(dataDir)
	data, _ := os.ReadFile(path)
	var st map[string]string
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("state file unreadable: %v", err)
	}
	if _, ok := st["last_block_sha256:"+agent]; !ok {
		t.Fatalf("state file does not key the block per agent: %v", st)
	}
}

// seedHookStoreFacts plants facts under the cwd-derived agent so the recall
// paths in these tests have something to find.
func seedHookStoreFacts(t *testing.T, facts ...string) {
	t.Helper()
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	agent := deriveAgentID(mustWorkdir())
	for _, f := range facts {
		if err := store.Remember(context.Background(), agent, f); err != nil {
			t.Fatal(err)
		}
	}
}

// ── hooks.log concurrent append ─────────────────────────────────────────────
//
// Claude Code can fire hooks concurrently (SessionStart while a prompt is in
// flight). Every appended line must survive intact: O_APPEND writes are
// atomic per write call on every platform Go supports, and hookLog writes one
// line per call.

func TestHookLog_ConcurrentAppendsStayIntact(t *testing.T) {
	withHooksEnv(t)
	const writers = 8
	const lines = 25

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < lines; i++ {
				hookLog(hookEventPayload{SessionID: "conc"}, "user-prompt",
					time.Duration(w*lines+i)*time.Millisecond, "ok", "injected")
			}
		}(w)
	}
	wg.Wait()

	data, err := os.ReadFile(filepath.Join(dataDir, "hooks.log"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(got) != writers*lines {
		t.Fatalf("hooks.log has %d lines, want %d — concurrent writes clobbered each other", len(got), writers*lines)
	}
	for i, line := range got {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line %d is not valid JSON (concurrent append interleaving): %v\n%s", i, err, line)
		}
	}
}

// ── fuzz targets ────────────────────────────────────────────────────────────
//
// The two inputs the hooks own that come from outside the process: the
// event payload Claude Code pipes in, and the settings.json we merge with.
// Neither may panic, hang, or corrupt state for any byte sequence.

func FuzzReadHookPayload(f *testing.F) {
	f.Add([]byte(`{"prompt":"x","cwd":"C:/p","session_id":"s"}`))
	f.Add([]byte(`{"prompt": "remember: save this"}`))
	f.Add([]byte(``))
	f.Add([]byte("null"))
	f.Add([]byte("\xef\xbb\xbf{}"))
	f.Add([]byte("\x00\xff\xfe garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p := readHookPayload(bytes.NewReader(data))
		// The only invariant: it returns. A panic or hang is the failure.
		_ = p.Prompt
		_ = p.CWD
	})
}

func FuzzRewriteHookEvent(f *testing.F) {
	f.Add([]byte(`[{"matcher":"startup","hooks":[{"type":"command","command":"/x graymatter hooks run session-start"}]}]`))
	f.Add([]byte(`[null, {"hooks": null}]`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`"not an array"`))
	f.Add([]byte(`[{"hooks":[{"command":"echo graymatter hooks run user-prompt"}]}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var existing any
		if err := json.Unmarshal(data, &existing); err != nil {
			return // settings that don't parse are rejected upstream, unchanged
		}
		exe := "/opt/gm/graymatter"
		want := hookGroupsFor(exe, hooksEventUserPrompt)
		next, _, drift, managed := rewriteHookEvent(existing, want, true)
		if !managed {
			return // unmanageable values pass through untouched
		}
		// Managed arrays must stay JSON-serialisable and must contain our
		// canonical groups after an install.
		blob, err := json.Marshal(next)
		if err != nil {
			t.Fatalf("merged event array is not serialisable: %v", err)
		}
		if !strings.Contains(string(blob), `"args":["hooks","run","user-prompt","--graymatter-managed-hook"]`) {
			t.Fatalf("install lost our group: %s", blob)
		}
		_ = drift
	})
}

func TestHooksOwnership_RejectsMalformedStructuredArgs(t *testing.T) {
	for _, tc := range []struct{ command string; args any }{{"/opt/gm/graymatter", nil}, {"/opt/gm/graymatter", "hooks run user-prompt"}, {"/opt/gm/graymatter", []any{"hooks", 7, "user-prompt"}}, {"echo", []any{hooksCommandMarker}}} {
		group := map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": tc.command, "args": tc.args,
		}}}
		if hookGroupIsOurs(group, "/opt/gm/graymatter") {
			t.Errorf("malformed entry %+v was claimed as a managed hook", tc)
		}
	}
}

// A foreign command that only mentions the legacy marker is never ours.
func TestHooksMerge_ForeignCommandContainingMarker(t *testing.T) {
	withHooksEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	foreignCommand := filepath.ToSlash(filepath.Join(dir, "echo")) + " graymatter hooks run user-prompt"

	existing := map[string]any{
		"hooks": map[string]any{
			// Unmanaged event: never touched by install or uninstall.
			"PreToolUse": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command",
					"command": "echo 'reminder: graymatter hooks run session-start is how memory works'"}},
			}},
			// Managed event: a foreign entry that happens to carry the marker.
			"UserPromptSubmit": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command",
					"command": foreignCommand}},
			}},
		},
	}
	blob, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	// Install: our groups are added; the unmanaged event is untouched.
	if _, err := upsertHookSettings(path, "/opt/gm/graymatter", true); err != nil {
		t.Fatal(err)
	}
	root := readSettings(t, path)
	pre := hookGroups(t, root, "PreToolUse")
	if len(pre) != 1 {
		t.Fatalf("unmanaged event must keep exactly its foreign group, got %d", len(pre))
	}

	if _, err := upsertHookSettings(path, "/opt/gm/graymatter", false); err != nil {
		t.Fatal(err)
	}
	after := readSettings(t, path)
	hooksAfter, _ := after["hooks"].(map[string]any)
	if hooksAfter == nil {
		t.Fatal("the unmanaged event must survive uninstall")
	}
	if _, still := hooksAfter["PreToolUse"]; !still {
		t.Error("unmanaged events must survive uninstall untouched")
	}
	up := hookGroups(t, after, "UserPromptSubmit")
	if len(up) != 1 || groupCommands(t, up[0])[0] != foreignCommand {
		t.Error("foreign command mentioning the legacy marker did not survive")
	}
}

// TestHooksMerge_DriftOnExtraField: a hand-added timeout on OUR SessionStart
// group is drift; the rewrite reports and restores it.
func TestHooksMerge_DriftOnExtraField(t *testing.T) {
	withHooksEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if _, err := upsertHookSettings(path, "/opt/gm/graymatter", true); err != nil {
		t.Fatal(err)
	}
	root := readSettings(t, path)
	ss := hookGroups(t, root, "SessionStart")
	ss[0]["hooks"].([]any)[0].(map[string]any)["timeout"] = float64(99)
	blob, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := upsertHookSettings(path, "/opt/gm/graymatter", true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Drifted || !res.Changed {
		t.Errorf("extra-field drift must be detected and rewritten (drifted=%v changed=%v)", res.Drifted, res.Changed)
	}
}

// TestHooksMerge_NonArrayEventLeftUntouched: a "hooks": {"PreCompact": "oops"}
// value must warn and survive, not be flattened.
func TestHooksMerge_NonArrayEventLeftUntouched(t *testing.T) {
	withHooksEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	blob := `{"hooks": {"PreCompact": "oops"}}`
	if err := os.WriteFile(path, []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := upsertHookSettings(path, "/opt/gm/graymatter", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Warn == "" {
		t.Error("non-array event value must warn")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"oops"`) {
		t.Errorf("the unmanageable value was destroyed:\n%s", data)
	}
	// Our other events must still have been installed around it.
	root := readSettings(t, path)
	if len(hookGroups(t, root, "SessionStart")) != 2 {
		t.Error("managed events must install even when one event value is unmanageable")
	}
}

// TestHooksMerge_NullGroupEntriesSurvive: null entries inside a hook array
// (hand-edit artefacts) are treated as foreign, preserved verbatim, and do
// not crash the install — the merged array keeps them plus our two groups.
func TestHooksMerge_NullGroupEntriesSurvive(t *testing.T) {
	withHooksEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	blob := `{"hooks": {"SessionStart": [null, {"matcher": "startup|resume|fork", "hooks": [null]}]}}`
	if err := os.WriteFile(path, []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := upsertHookSettings(path, "/opt/gm/graymatter", true); err != nil {
		t.Fatalf("null entries must not error the install: %v", err)
	}

	root := readSettings(t, path)
	arr, _ := root["hooks"].(map[string]any)["SessionStart"].([]any)
	// [null, foreign-with-null-hook, ours(startup), ours(compact)]
	if len(arr) != 4 {
		t.Fatalf("SessionStart entries = %d, want 4 (null + foreign + 2 ours)", len(arr))
	}
	if arr[0] != nil {
		t.Errorf("foreign null entry must be preserved verbatim, got %T", arr[0])
	}
	if res, err := upsertHookSettings(path, "/opt/gm/graymatter", false); err != nil {
		t.Fatalf("uninstall with null entries: %v", err)
	} else if !res.Changed {
		t.Error("uninstall must remove our groups")
	}
	after := readSettings(t, path)
	arrAfter, _ := after["hooks"].(map[string]any)["SessionStart"].([]any)
	if len(arrAfter) != 2 {
		t.Fatalf("after uninstall SessionStart entries = %d, want 2 (null + foreign)", len(arrAfter))
	}
}
