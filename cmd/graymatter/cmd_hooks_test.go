package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The settings.json this package writes is a contract with Claude Code's hook
// system and with every future re-install: the tests below pin the emitted
// shape, the merge semantics (foreign hooks survive), the drift detection,
// and the runners' behaviour on the happy and every failure path.

// withHooksEnv points dataDir at a temp dir and forces the direct store path
// so runner tests exercise the full openStore contract without a daemon.
func withHooksEnv(t *testing.T) string {
	t.Helper()
	oldData, oldNoDaemon := dataDir, noDaemon
	dataDir = t.TempDir()
	noDaemon = true
	t.Cleanup(func() { dataDir, noDaemon = oldData, oldNoDaemon })
	return dataDir
}

// readSettings parses the settings file at path into a generic map.
func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return root
}

func hookGroups(t *testing.T, root map[string]any, event string) []map[string]any {
	t.Helper()
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}
	arr, _ := hooks[event].([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, g := range arr {
		m, ok := g.(map[string]any)
		if !ok {
			t.Fatalf("%s: matcher group is %T, want object", event, g)
		}
		out = append(out, m)
	}
	return out
}

func groupCommands(t *testing.T, group map[string]any) []string {
	t.Helper()
	list, _ := group["hooks"].([]any)
	out := make([]string, 0, len(list))
	for _, h := range list {
		hook, ok := h.(map[string]any)
		if !ok {
			t.Fatalf("hook entry is %T, want object", h)
		}
		if typ, _ := hook["type"].(string); typ != "command" {
			t.Errorf("hook type = %v, want command", hook["type"])
		}
		c, _ := hook["command"].(string)
		out = append(out, c)
	}
	return out
}

// TestHooksInstall_WritesCanonicalContract pins the emitted settings shape:
// four events, SessionStart split across a startup|resume|fork group and a
// compact group, the user-prompt hook carrying a 10s timeout, and every
// command an absolute path ending in the documented form.
func TestHooksInstall_WritesCanonicalContract(t *testing.T) {
	withHooksEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")

	exe := filepath.Join(dir, "graymatter.exe")
	res, err := upsertHookSettings(path, exe, true)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.Changed {
		t.Error("first install on a missing file must report changed")
	}

	root := readSettings(t, path)

	ss := hookGroups(t, root, "SessionStart")
	if len(ss) != 2 {
		t.Fatalf("SessionStart groups = %d, want 2 (startup|resume|fork + compact)", len(ss))
	}
	if m, _ := ss[0]["matcher"].(string); m != "startup|resume|fork" {
		t.Errorf("SessionStart group 0 matcher = %v, want startup|resume|fork", ss[0]["matcher"])
	}
	if m, _ := ss[1]["matcher"].(string); m != "compact" {
		t.Errorf("SessionStart group 1 matcher = %v, want compact", ss[1]["matcher"])
	}

	up := hookGroups(t, root, "UserPromptSubmit")
	if len(up) != 1 {
		t.Fatalf("UserPromptSubmit groups = %d, want 1", len(up))
	}
	hooks := up[0]["hooks"].([]any)
	hook0 := hooks[0].(map[string]any)
	if to, _ := hook0["timeout"].(float64); to != 10 {
		t.Errorf("user-prompt timeout = %v, want 10", hook0["timeout"])
	}

	for _, event := range hookEventNames() {
		for _, g := range hookGroups(t, root, event) {
			for _, c := range groupCommands(t, g) {
				if !strings.Contains(c, hooksCommandMarker) {
					t.Errorf("%s command %q lacks the marker", event, c)
				}
				if !strings.HasSuffix(c, hookRunArg(event)) {
					t.Errorf("command %q must end with the run event name %q", c, hookRunArg(event))
				}
				if !filepath.IsAbs(hookBinaryPath(c)) {
					t.Errorf("command %q must record an absolute binary path", c)
				}
			}
		}
	}
}

// TestHooksInstall_PreservesForeignHooks is the merge-never-overwrite gate:
// the user's own hooks — a formatter on PreToolUse and an extra command on
// UserPromptSubmit — must survive byte-for-byte in meaning.
func TestHooksInstall_PreservesForeignHooks(t *testing.T) {
	withHooksEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")

	existing := map[string]any{
		"permissions": map[string]any{"allow": []any{"Bash(ls*)"}},
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "Edit|Write",
				"hooks":   []any{map[string]any{"type": "command", "command": "prettier --write $FILE"}},
			}},
			"UserPromptSubmit": []any{map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": "my-logger --turn"}},
			}},
		},
	}
	blob, _ := json.MarshalIndent(existing, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := upsertHookSettings(path, "/opt/gm/graymatter", true); err != nil {
		t.Fatalf("install: %v", err)
	}

	root := readSettings(t, path)
	if _, ok := root["permissions"]; !ok {
		t.Error("foreign top-level keys must survive")
	}
	pre := hookGroups(t, root, "PreToolUse")
	if len(pre) != 1 || strings.Join(groupCommands(t, pre[0]), "") != "prettier --write $FILE" {
		t.Errorf("foreign PreToolUse hooks damaged: %+v", pre)
	}
	up := hookGroups(t, root, "UserPromptSubmit")
	if len(up) != 2 {
		t.Fatalf("UserPromptSubmit groups = %d, want 2 (foreign + ours)", len(up))
	}
	if cmds := groupCommands(t, up[0]); len(cmds) != 1 || cmds[0] != "my-logger --turn" {
		t.Errorf("foreign UserPromptSubmit entry must come first untouched, got %+v", up[0])
	}
}

// TestHooksInstall_Idempotent: a second install must not rewrite the file.
func TestHooksInstall_Idempotent(t *testing.T) {
	withHooksEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")

	if _, err := upsertHookSettings(path, "/opt/gm/graymatter", true); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	res, err := upsertHookSettings(path, "/opt/gm/graymatter", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.BackedUp || res.Drifted {
		t.Errorf("second install rewrote: changed=%v backup=%v drifted=%v", res.Changed, res.BackedUp, res.Drifted)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("second install must leave the file byte-identical")
	}
}

// TestHooksUninstall_RemovesOursKeepsForeign: uninstall removes only
// GrayMatter entries, drops emptied event keys, and keeps the .bak.
func TestHooksUninstall_RemovesOursKeepsForeign(t *testing.T) {
	withHooksEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if _, err := upsertHookSettings(path, "/opt/gm/graymatter", true); err != nil {
		t.Fatal(err)
	}
	// Add a foreign hook alongside ours after install.
	root := readSettings(t, path)
	hooks := root["hooks"].(map[string]any)
	hooks["PreToolUse"] = []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "lint-staged"}},
	}}
	blob, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := upsertHookSettings(path, "/opt/gm/graymatter", false)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !res.Changed {
		t.Error("uninstall with our entries present must change the file")
	}

	after := readSettings(t, path)
	hooksAfter, ok := after["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("foreign PreToolUse lost: %+v", after)
	}
	if _, has := hooksAfter["SessionStart"]; has {
		t.Error("SessionStart must be gone after uninstall")
	}
	if _, has := hooksAfter["UserPromptSubmit"]; has {
		t.Error("UserPromptSubmit must be gone after uninstall")
	}
	pre, _ := hooksAfter["PreToolUse"].([]any)
	if len(pre) != 1 {
		t.Fatalf("foreign PreToolUse entries = %d, want 1", len(pre))
	}
	if _, has := after["hooks"]; has == false {
		// hooks key may remain holding PreToolUse — that is correct; the
		// key must only disappear when nothing foreign remains.
		t.Error("hooks key should survive while foreign entries remain")
	}
	if _, err := os.Stat(path + hooksBackupSuffix); err != nil {
		t.Error("uninstall must keep a .bak of the previous file")
	}
}

// TestHooksInstall_DetectsHandEdits: entries installed by us, then edited by
// hand, must be reported as drifted on the next install and restored — with
// the hand-edited file preserved as .bak.
func TestHooksInstall_DetectsHandEdits(t *testing.T) {
	withHooksEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if _, err := upsertHookSettings(path, "/opt/gm/graymatter", true); err != nil {
		t.Fatal(err)
	}

	// Hand edit: change our user-prompt timeout.
	root := readSettings(t, path)
	up := hookGroups(t, root, "UserPromptSubmit")
	hook0 := up[0]["hooks"].([]any)[0].(map[string]any)
	hook0["timeout"] = float64(60)
	blob, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	handEdited, _ := os.ReadFile(path)

	res, err := upsertHookSettings(path, "/opt/gm/graymatter", true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Drifted {
		t.Error("hand-edited entries must be reported as drifted")
	}
	if !res.Changed || !res.BackedUp {
		t.Errorf("drifted entries must be rewritten with a backup: changed=%v backup=%v", res.Changed, res.BackedUp)
	}
	bak, err := os.ReadFile(path + hooksBackupSuffix)
	if err != nil || string(bak) != string(handEdited) {
		t.Errorf(".bak must hold the hand-edited bytes (err=%v)", err)
	}

	// Restored to canonical: a third install is a no-op again.
	res3, err := upsertHookSettings(path, "/opt/gm/graymatter", true)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Drifted || res3.Changed {
		t.Errorf("post-restore install must be clean: drifted=%v changed=%v", res3.Drifted, res3.Changed)
	}
}

// TestHooksSettings_NeverClobbersUnparseableFile: an invalid settings.json is
// reported and left exactly as it was.
func TestHooksSettings_NeverClobbersUnparseableFile(t *testing.T) {
	withHooksEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	junk := "{ this is not json ]"
	if err := os.WriteFile(path, []byte(junk), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := upsertHookSettings(path, "/opt/gm/graymatter", true)
	if err != nil {
		t.Fatalf("install over invalid JSON must not error: %v", err)
	}
	if res.Warn == "" {
		t.Error("expected a warning for the unparseable file")
	}
	if res.Changed {
		t.Error("must never write over an unparseable file")
	}
	data, _ := os.ReadFile(path)
	if string(data) != junk {
		t.Error("the unparseable file was modified")
	}
}

// TestHooksSettings_NonObjectHooksLeftUntouched.
func TestHooksSettings_NonObjectHooksLeftUntouched(t *testing.T) {
	withHooksEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	blob := `{"hooks": "not an object"}`
	if err := os.WriteFile(path, []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := upsertHookSettings(path, "/opt/gm/graymatter", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Warn == "" || res.Changed {
		t.Errorf("warn=%q changed=%v, want warn + no change", res.Warn, res.Changed)
	}
	data, _ := os.ReadFile(path)
	if string(data) != blob {
		t.Error("a non-object hooks key must be left untouched")
	}
}

// TestHookCommand_Quoting: binaries under paths with spaces must record a
// quoted command, and hookBinaryPath must round-trip it.
func TestHookCommand_Quoting(t *testing.T) {
	withSpaces := `/c/Program Files/graymatter/graymatter`
	cmd := hookCommand(withSpaces, "user-prompt")
	if !strings.HasPrefix(cmd, `"`) {
		t.Errorf("command %q must quote a path containing spaces", cmd)
	}
	got := hookBinaryPath(cmd)
	if got != filepath.FromSlash(withSpaces) {
		t.Errorf("hookBinaryPath round-trip = %q, want %q", got, filepath.FromSlash(withSpaces))
	}

	plain := hookCommand("/usr/local/bin/graymatter", "session-start")
	if strings.HasPrefix(plain, `"`) {
		t.Errorf("command %q need not be quoted", plain)
	}
}

// TestDeriveAgentID pins the folder→agent mapping the hooks and doctor use.
func TestDeriveAgentID(t *testing.T) {
	cases := []struct{ dir, want string }{
		{`C:\code\My Project`, "my-project"},
		{`/home/user/graymatter`, "graymatter"},
		{`/opt/Tools_4.Devs`, "tools-4-devs"},
		{`/tmp/demo-01`, "demo-01"},
		{``, "project"},
	}
	for _, c := range cases {
		if got := deriveAgentID(c.dir); got != c.want {
			t.Errorf("deriveAgentID(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

// --- runner tests ---------------------------------------------------------------

// TestHookRun_UserPromptRemember is the instant-save path: deterministic
// store, confirmation out, no model involved.
func TestHookRun_UserPromptRemember(t *testing.T) {
	withHooksEnv(t)
	payload := hookEventPayload{SessionID: "s1", CWD: mustWorkdir(), Prompt: "remember: deploys freeze on Fridays"}
	out, err := dispatchHook("user-prompt", payload)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(out, "Saved to memory") || !strings.Contains(out, "deploys freeze on Fridays") {
		t.Errorf("confirmation = %q, want it to name the saved fact", out)
	}

	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	facts, err := store.List(deriveAgentID(mustWorkdir()))
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Text != "deploys freeze on Fridays" {
		t.Errorf("stored facts = %+v, want exactly the remembered sentence", facts)
	}
}

// TestHookRun_UserPromptEmptyRemember: "remember:" with nothing after it is a
// user error the runner must survive (error → silent path), not store junk.
func TestHookRun_UserPromptEmptyRemember(t *testing.T) {
	withHooksEnv(t)
	if _, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "remember:   "}); err == nil {
		t.Error("empty remember must be an error (which runHookEvent then degrades to silence)")
	}
}

// TestHookRun_UserPromptRecallAndThrottle: first matching prompt injects, an
// identical consecutive turn injects nothing, a different prompt injects.
func TestHookRun_UserPromptRecallAndThrottle(t *testing.T) {
	withHooksEnv(t)
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	agent := deriveAgentID(mustWorkdir())
	ctx := context.Background()
	for _, f := range []string{"The API rate limit is 60 requests per minute", "Postgres runs on the db-01 box"} {
		if err := store.Remember(ctx, agent, f); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.Close()

	first, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "rate limit"})
	if err != nil {
		t.Fatalf("first recall: %v", err)
	}
	if !strings.Contains(first, "rate limit") {
		t.Errorf("first injection = %q, want the matching fact", first)
	}

	second, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "rate limit again please"})
	if err != nil {
		t.Fatalf("second recall: %v", err)
	}
	if second != "" {
		t.Errorf("second injection = %q, want suppressed (same block hash)", second)
	}
	// The throttle state lives in <dataDir>/hooks/state.json.
	if _, err := os.Stat(hookStatePath(dataDir)); err != nil {
		t.Errorf("state file missing: %v", err)
	}

	third, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "where does postgres run"})
	if err != nil {
		t.Fatalf("third recall: %v", err)
	}
	if third == "" {
		t.Error("a different query must inject again")
	}
}

// TestHookRun_SessionStart injects only when there is something to inject.
func TestHookRun_SessionStart(t *testing.T) {
	withHooksEnv(t)
	agent := deriveAgentID(mustWorkdir())

	empty, err := dispatchHook("session-start", hookEventPayload{CWD: mustWorkdir()})
	if err != nil {
		t.Fatalf("empty store: %v", err)
	}
	if empty != "" {
		t.Errorf("empty store must inject nothing, got %q", empty)
	}

	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Remember(context.Background(), agent, "The release checklist lives in docs/release.md"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	out, err := dispatchHook("session-start", hookEventPayload{CWD: mustWorkdir()})
	if err != nil {
		t.Fatalf("seeded store: %v", err)
	}
	if !strings.HasPrefix(out, "## Memory\n") || !strings.Contains(out, "release checklist") {
		t.Errorf("injection = %q, want a Memory block with the fact", out)
	}
}

// TestHookRun_FailurePathsAreSilent: with a store that cannot open, every
// event errors at the dispatch layer — and runHookEvent turns that into
// exit 0, empty stdout, and a hooks.log receipt.
func TestHookRun_FailurePathsAreSilent(t *testing.T) {
	dir := t.TempDir()
	// A garbage gray.db makes every store open fail, while the directory
	// itself stays writable so hooks.log can land.
	if err := os.WriteFile(filepath.Join(dir, "gray.db"), []byte("this is not a bbolt database"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldData, oldNoDaemon := dataDir, noDaemon
	dataDir = dir
	noDaemon = true
	t.Cleanup(func() { dataDir, noDaemon = oldData, oldNoDaemon })

	for _, event := range []string{"session-start", "user-prompt", "pre-compact", "session-end"} {
		payload := hookEventPayload{CWD: dir, Prompt: "anything"}
		if _, err := dispatchHook(event, payload); err == nil {
			t.Errorf("%s: expected an error from an unusable store", event)
		}
	}

	// The degradation contract: runHookEvent returns nil (exit 0) and logs.
	if err := runHookEvent("session-start"); err != nil {
		t.Errorf("runHookEvent must swallow hook errors (exit 0), got %v", err)
	}
	logData, err := os.ReadFile(filepath.Join(dir, "hooks.log"))
	if err != nil {
		t.Fatalf("hooks.log must be written next to the configured data dir: %v", err)
	}
	if !strings.Contains(string(logData), `"outcome":"error"`) || !strings.Contains(string(logData), `"event":"session-start"`) {
		t.Errorf("hooks.log missing the error receipt: %s", logData)
	}
}

// TestHookRun_UnknownEvent: a typo in settings.json must fail loudly at
// dispatch (the installer never emits it, so this only happens by hand).
func TestHookRun_UnknownEvent(t *testing.T) {
	withHooksEnv(t)
	if _, err := dispatchHook("session-middle", hookEventPayload{}); err == nil {
		t.Error("unknown event must be a dispatch error")
	}
}
