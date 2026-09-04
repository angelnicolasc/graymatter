package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
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
	// Compatibility view for older assertions; persisted exec-form tests read
	// command and args separately and never execute this reconstruction.
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
		if args, ok := hookEntryArgs(hook); ok {
			c += " " + strings.Join(args, " ")
		}
		out = append(out, c)
	}
	return out
}

func TestHooksUninstallAll_RemovesBothScopes(t *testing.T) {
	withHooksEnv(t)
	project, home := t.TempDir(), t.TempDir()
	t.Chdir(project)
	testHomeOverride = home
	t.Cleanup(func() { testHomeOverride = "" })

	exe := filepath.Join(t.TempDir(), "graymatter")
	paths := []string{
		filepath.Join(project, ".claude", "settings.json"),
		filepath.Join(home, ".claude", "settings.json"),
	}
	for i, path := range paths {
		scope := []hookScope{scopeProject, scopeGlobal}[i]
		if _, err := upsertHookSettings(path, exe, true, scope); err != nil {
			t.Fatal(err)
		}
	}

	cmd := hooksUninstallCmd()
	cmd.SetArgs([]string{"--all"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("hooks uninstall --all: %v", err)
	}
	for _, path := range paths {
		if hooks, ok := readSettings(t, path)["hooks"]; ok {
			t.Errorf("%s still has hooks after --all: %#v", path, hooks)
		}
	}
}

func TestHooksDoctorAll_ReportsBothScopes(t *testing.T) {
	withHooksEnv(t)
	project, home := t.TempDir(), t.TempDir()
	t.Chdir(project)
	testHomeOverride = home
	t.Cleanup(func() { testHomeOverride = "" })
	exe, err := resolveOwnBinary()
	if err != nil {
		t.Fatal(err)
	}
	for scope, path := range map[hookScope]string{
		scopeProject: filepath.Join(project, ".claude", "settings.json"),
		scopeGlobal:  filepath.Join(home, ".claude", "settings.json"),
	} {
		if _, err := upsertHookSettings(path, exe, true, scope); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	cmd := hooksDoctorCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--all"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("hooks doctor --all: %v", err)
	}
	got := out.String()
	for _, path := range []string{filepath.Join(".claude", "settings.json"), filepath.Join(home, ".claude", "settings.json")} {
		if !strings.Contains(got, path) {
			t.Errorf("doctor --all omitted %s:\n%s", path, got)
		}
	}
}

func TestHooksInstallAll_RemainsRejected(t *testing.T) {
	cmd := hooksInstallCmd()
	cmd.SetArgs([]string{"--all"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("hooks install --all error = %v, want explicit rejection", err)
	}
}

func TestHooksDoctor_MissingStoreDoesNotCreateState(t *testing.T) {
	bin := buildE2EBinary(t)
	for _, tc := range []struct {
		name     string
		wantCode int
	}{
		{name: "valid settings", wantCode: 0},
		{name: "missing settings", wantCode: 1},
		{name: "corrupt settings", wantCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			settings := filepath.Join(work, ".claude", "settings.json")
			switch tc.name {
			case "valid settings":
				if out, code := runE2E(t, bin, work, "", "hooks", "install"); code != 0 {
					t.Fatalf("hooks install: exit=%d out=%s", code, out)
				}
			case "corrupt settings":
				if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(settings, []byte("{broken"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			before, err := os.ReadDir(work)
			if err != nil {
				t.Fatal(err)
			}
			out, code := runE2E(t, bin, work, "", "hooks", "doctor")
			after, err := os.ReadDir(work)
			if err != nil {
				t.Fatal(err)
			}
			if code != tc.wantCode || !strings.Contains(out, "store not initialised") {
				t.Fatalf("hooks doctor: exit=%d out=%q, want exit %d and uninitialised report", code, out, tc.wantCode)
			}
			if len(after) != len(before) {
				t.Fatalf("doctor created state: before=%v after=%v", before, after)
			}
		})
	}
}

// TestHooksInstall_WritesCanonicalContract pins the emitted settings shape:
// four events, SessionStart split across a startup|resume|fork group and a
// compact group, the user-prompt hook carrying a 10s timeout, and every
// command an exact executable path and args carrying the invocation tokens.
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
			for _, raw := range g["hooks"].([]any) {
				hook := raw.(map[string]any)
				if command, _ := hook["command"].(string); command != exe {
					t.Errorf("%s command = %q, want exact path %q", event, command, exe)
				}
				args, ok := hookEntryArgs(hook)
				want := []string{"hooks", "run", hookRunArg(event), hooksCommandMarker}
				if !ok || !stringSlicesEqual(args, want) {
					t.Errorf("%s args = %q, want %q", event, args, want)
				}
			}
		}
	}
	global := hookGroupsFor(exe, hooksEventPreCompact, scopeGlobal)[0].(map[string]any)
	hook := global["hooks"].([]any)[0].(map[string]any)
	args, ok := hookEntryArgs(hook)
	want := []string{"hooks", "run", "pre-compact", hooksCommandMarker, hooksNoCreateArg}
	if hook["command"] != exe || !ok || !stringSlicesEqual(args, want) {
		t.Errorf("global exec form = command %v args %q, want %q + %q", hook["command"], args, exe, want)
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

// TestHookCommand_LegacyParsing keeps the two shell-form readers available for
// reinstall and uninstall of settings written by previous releases.
func TestHookCommand_LegacyParsing(t *testing.T) {
	withSpaces := `/c/Program Files/hook's run/graymatter`
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

func TestHooksInstall_ExecFormRunsLiteralBinaryPath(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "VOYAGE_API_KEY"} {
		t.Setenv(name, "")
	}
	t.Setenv("GRAYMATTER_OLLAMA_URL", "disabled://hook-command-test")

	root := t.TempDir()
	binDir := filepath.Join(root, "hook path $ (literal)")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "graymatter.exe")
	build := exec.Command("go", "build", "-o", bin, "github.com/angelnicolasc/graymatter/cmd/graymatter")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hook binary: %v\n%s", err, out)
	}

	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(project, ".graymatter")
	t.Cleanup(func() {
		stop := exec.Command(bin, "--dir", storeDir, "daemon", "stop")
		stop.Dir = project
		_ = stop.Run()
		time.Sleep(300 * time.Millisecond)
	})

	settings := filepath.Join(project, ".claude", "settings.json")
	install := exec.Command(bin, "hooks", "install")
	install.Dir = project
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install hooks from literal path: %v\n%s", err, out)
	}
	groups := hookGroups(t, readSettings(t, settings), hooksEventUserPrompt)
	hook := groups[0]["hooks"].([]any)[0].(map[string]any)
	command, _ := hook["command"].(string)
	args, ok := hookEntryArgs(hook)
	if command != bin || !ok {
		t.Fatalf("persisted exec form = command %q args %q", command, args)
	}
	payload, _ := json.Marshal(map[string]string{
		"cwd": project, "prompt": "remember: exec form preserved the literal path",
	})
	run := exec.Command(command, args...)
	run.Dir, run.Stdin = project, strings.NewReader(string(payload))
	out, err := run.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Saved to memory") {
		t.Fatalf("execute persisted command+args: %v\n%s", err, out)
	}
}

func TestHooksInstall_MigratesPreviousCommandFormsWithoutDrift(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "graymatter.exe")
	cases := []struct {
		name    string
		scope   hookScope
		command string
	}{
		{"string project", scopeProject, hookCommand(exe, "user-prompt")},
		{"string global", scopeGlobal, hookCommand(exe, "user-prompt", scopeGlobal)},
		{"pre-marker project", scopeProject, filepath.ToSlash(exe) + " graymatter hooks run user-prompt"},
		{"pre-marker global", scopeGlobal, filepath.ToSlash(exe) + " graymatter hooks run user-prompt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			legacy := map[string]any{"hooks": map[string]any{
				hooksEventUserPrompt: []any{map[string]any{"hooks": []any{
					map[string]any{"type": "command", "command": tc.command, "timeout": userPromptHookTimeout},
				}}},
			}}
			write := func(value map[string]any) {
				t.Helper()
				data, _ := json.MarshalIndent(value, "", "  ")
				if err := os.WriteFile(path, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write(legacy)
			if drift, err := hookSettingsDrift(path, exe, tc.scope); err != nil || drift {
				t.Fatalf("previous generation reported drift=%v err=%v", drift, err)
			}
			res, err := upsertHookSettings(path, exe, true, tc.scope)
			if err != nil || !res.Changed || res.Drifted {
				t.Fatalf("migration result=%+v err=%v", res, err)
			}
			group := hookGroups(t, readSettings(t, path), hooksEventUserPrompt)[0]
			hook := group["hooks"].([]any)[0].(map[string]any)
			if hook["command"] != exe {
				t.Fatalf("migrated command = %v, want %q", hook["command"], exe)
			}
			wantArgs := []string{"hooks", "run", "user-prompt", hooksCommandMarker}
			if tc.scope == scopeGlobal {
				wantArgs = append(wantArgs, hooksNoCreateArg)
			}
			if args, ok := hookEntryArgs(hook); !ok || !stringSlicesEqual(args, wantArgs) {
				t.Fatalf("migrated args = %q", args)
			}

			write(legacy)
			res, err = upsertHookSettings(path, exe, false, tc.scope)
			if err != nil || !res.Changed {
				t.Fatalf("uninstall previous generation result=%+v err=%v", res, err)
			}
			if _, exists := readSettings(t, path)["hooks"]; exists {
				t.Fatal("uninstall left the previous-generation hook installed")
			}

			edited := legacy["hooks"].(map[string]any)[hooksEventUserPrompt].([]any)[0].(map[string]any)
			edited["hooks"].([]any)[0].(map[string]any)["timeout"] = 60
			write(legacy)
			if drift, err := hookSettingsDrift(path, exe, tc.scope); err != nil || !drift {
				t.Fatalf("real edit reported drift=%v err=%v", drift, err)
			}
		})
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
	if !strings.HasPrefix(out, hookRecallMarker(agent)+"\n## Memory\n") || !strings.Contains(out, "release checklist") {
		t.Errorf("injection = %q, want the hook marker and a Memory block with the fact", out)
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

// --- __shared__ injection (HS-28AGO) --------------------------------------------
//
// AGENTS.md directs project-wide conventions to the reserved __shared__
// namespace; the hooks are the automatic injection path, so both reading
// events must merge it in, and the remember: path must be able to write it.

// seedSharedFacts plants facts in the __shared__ namespace of the test store.
func seedSharedFacts(t *testing.T, facts ...string) {
	t.Helper()
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, f := range facts {
		if err := store.PutShared(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.Close()
}

// TestHookRun_SessionStart_SharedNamespace: session-start must inject the
// shared namespace alongside the agent's own — shared-only, agent-only,
// and both together.
func TestHookRun_SessionStart_SharedNamespace(t *testing.T) {
	t.Run("shared only", func(t *testing.T) {
		withHooksEnv(t)
		seedSharedFacts(t, "Deploys freeze on Fridays: do not deploy to production on Fridays")

		out, err := dispatchHook("session-start", hookEventPayload{CWD: mustWorkdir()})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if !strings.Contains(out, "## Shared memory (project-wide)") || !strings.Contains(out, "Deploys freeze on Fridays") {
			t.Errorf("shared-only injection = %q, want the shared section carrying the convention", out)
		}
		if strings.Contains(out, "## Memory\n") {
			t.Errorf("shared-only injection must not render an empty agent section: %q", out)
		}
	})

	t.Run("agent only", func(t *testing.T) {
		withHooksEnv(t)
		agent := deriveAgentID(mustWorkdir())
		store, err := openStore()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Remember(context.Background(), agent, "The catalog service talks to Postgres on db-01"); err != nil {
			t.Fatal(err)
		}
		_ = store.Close()

		out, err := dispatchHook("session-start", hookEventPayload{CWD: mustWorkdir()})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if !strings.HasPrefix(out, hookRecallMarker(agent)+"\n## Memory\n") || !strings.Contains(out, "Postgres on db-01") {
			t.Errorf("agent-only injection = %q, want the hook marker and Memory block", out)
		}
		if strings.Contains(out, "Shared memory") {
			t.Errorf("agent-only injection must not carry a shared section: %q", out)
		}
	})

	t.Run("both namespaces", func(t *testing.T) {
		withHooksEnv(t)
		agent := deriveAgentID(mustWorkdir())
		seedSharedFacts(t, "Deploys freeze on Fridays: do not deploy to production on Fridays")
		store, err := openStore()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Remember(context.Background(), agent, "The catalog service talks to Postgres on db-01"); err != nil {
			t.Fatal(err)
		}
		_ = store.Close()

		out, err := dispatchHook("session-start", hookEventPayload{CWD: mustWorkdir()})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		memIdx := strings.Index(out, "## Memory\n")
		shrIdx := strings.Index(out, "## Shared memory (project-wide)")
		if memIdx < 0 || shrIdx < 0 || memIdx > shrIdx {
			t.Errorf("both-namespaces injection = %q, want Memory section before Shared memory", out)
		}
		if !strings.Contains(out, "Postgres on db-01") || !strings.Contains(out, "Deploys freeze on Fridays") {
			t.Errorf("both-namespaces injection lost a fact: %q", out)
		}
	})
}

// TestHookRun_UserPrompt_SharedNamespace: the per-turn recall must match
// against the shared namespace too — a prompt about a project convention
// brings the convention back.
func TestHookRun_UserPrompt_SharedNamespace(t *testing.T) {
	withHooksEnv(t)
	agent := deriveAgentID(mustWorkdir())

	// Shared-only.
	seedSharedFacts(t, "The API rate limit for shared tenants is 60 requests per minute")
	out, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "what is the rate limit"})
	if err != nil {
		t.Fatalf("shared-only store: %v", err)
	}
	if !strings.Contains(out, "rate limit") || !strings.Contains(out, "## Shared memory (project-wide)") {
		t.Errorf("shared-only per-turn injection = %q, want the shared convention", out)
	}

	// Both namespaces against one query.
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Remember(context.Background(), agent, "The agent-local retry budget is three attempts"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	out, err = dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "retry budget and rate limit"})
	if err != nil {
		t.Fatalf("both namespaces: %v", err)
	}
	if !strings.Contains(out, "retry budget") || !strings.Contains(out, "rate limit") {
		t.Errorf("per-turn injection lost a namespace: %q", out)
	}
}

// TestHookRun_UserPrompt_SharedThrottle: the identical-block suppression must
// hold on merged blocks — an unchanged turn injects nothing, a changed shared
// namespace re-injects.
func TestHookRun_UserPrompt_SharedThrottle(t *testing.T) {
	withHooksEnv(t)
	seedSharedFacts(t, "Deploys freeze on Fridays: do not deploy to production on Fridays")

	first, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "can I deploy on a Friday"})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if !strings.Contains(first, "Deploys freeze on Fridays") {
		t.Fatalf("first injection = %q, want the shared convention", first)
	}

	second, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "and what about Saturday"})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if second != "" {
		t.Errorf("second injection = %q, want suppressed (identical merged block)", second)
	}

	// A changed shared fact changes the block: it must inject again.
	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutShared(context.Background(), "Ship window reopens Monday 09:00 UTC"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	third, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "when does the ship window reopen"})
	if err != nil {
		t.Fatalf("third turn: %v", err)
	}
	if third == "" || !strings.Contains(third, "Ship window reopens") {
		t.Errorf("changed shared namespace must re-inject, got %q", third)
	}
}

// TestHookRun_UserPromptRememberShared: "remember shared: <text>" is the
// deterministic instant-save into the namespace every agent reads.
func TestHookRun_UserPromptRememberShared(t *testing.T) {
	withHooksEnv(t)
	agent := deriveAgentID(mustWorkdir())

	out, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "remember shared: All PRs need one approval from a CODEOWNER"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(out, "Saved to shared memory") || !strings.Contains(out, "All PRs need one approval") {
		t.Errorf("confirmation = %q, want it to name the shared save", out)
	}

	// Case-insensitive prefix, same destination.
	if _, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "REMEMBER SHARED: Standups are async in writing"}); err != nil {
		t.Fatalf("shouty prefix: %v", err)
	}

	store, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	shared, err := store.List(memory.SharedAgentID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range shared {
		got[f.Text] = true
	}
	for _, want := range []string{
		"All PRs need one approval from a CODEOWNER",
		"Standups are async in writing",
	} {
		if !got[want] {
			t.Errorf("shared namespace missing %q (has %v)", want, got)
		}
	}
	agentFacts, err := store.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(agentFacts) != 0 {
		t.Errorf("remember shared: leaked %d facts into the agent namespace", len(agentFacts))
	}

	// "remember shared:" with nothing after it is a user error the runner
	// must survive (error → silent path), not store junk.
	if _, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "remember shared:   "}); err == nil {
		t.Error("empty shared remember must be an error")
	}
}

// hookRecallStub is a cliStore whose two recall calls answer from canned
// values, so the degradation contract is testable without a store that fails
// selectively. Only Recall and RecallShared are ever called on it.
type hookRecallStub struct {
	cliStore
	agentFacts  []string
	agentErr    error
	sharedFacts []string
	sharedErr   error
}

func (s *hookRecallStub) Recall(context.Context, string, string, int) ([]string, error) {
	return s.agentFacts, s.agentErr
}

func (s *hookRecallStub) RecallShared(context.Context, string, int) ([]string, error) {
	return s.sharedFacts, s.sharedErr
}

// TestHookRecallBlock_Degradation: one namespace failing never costs the
// other's facts; only a total failure aborts the injection.
func TestHookRecallBlock_Degradation(t *testing.T) {
	okAgent := &hookRecallStub{agentFacts: []string{"agent fact"}, sharedErr: fmt.Errorf("shared boom")}
	block, degrade := hookRecallBlock(okAgent, "a", "q", 3, 3)
	if block == "" || !strings.Contains(block, "agent fact") {
		t.Errorf("shared failure must still inject agent facts, got %q", block)
	}
	if degrade == nil || !strings.Contains(degrade.Error(), "shared recall failed") {
		t.Errorf("shared failure must surface a degrade error, got %v", degrade)
	}

	okShared := &hookRecallStub{agentErr: fmt.Errorf("agent boom"), sharedFacts: []string{"shared fact"}}
	block, degrade = hookRecallBlock(okShared, "a", "q", 3, 3)
	if block == "" || !strings.Contains(block, "shared fact") {
		t.Errorf("agent failure must still inject shared facts, got %q", block)
	}
	if degrade == nil || !strings.Contains(degrade.Error(), "agent recall failed") {
		t.Errorf("agent failure must surface a degrade error, got %v", degrade)
	}

	bothDown := &hookRecallStub{agentErr: fmt.Errorf("agent boom"), sharedErr: fmt.Errorf("shared boom")}
	if block, degrade := hookRecallBlock(bothDown, "a", "q", 3, 3); block != "" || degrade == nil {
		t.Errorf("both namespaces failing must be a hard error, got block=%q degrade=%v", block, degrade)
	}

	// The only populated namespace failing is also a hard error, even though
	// the other call succeeded with an empty result.
	emptyAgent := &hookRecallStub{agentFacts: nil, sharedErr: fmt.Errorf("shared boom")}
	if block, degrade := hookRecallBlock(emptyAgent, "a", "q", 3, 3); block != "" || degrade == nil {
		t.Errorf("shared failure with no agent facts must be a hard error, got block=%q degrade=%v", block, degrade)
	}
}

// TestHookRun_DegradeReceiptInLog: a degraded injection still goes out, and
// the failure leaves its error receipt in hooks.log — the session never sees
// the half-broken store, the log does.
func TestHookRun_DegradeReceiptInLog(t *testing.T) {
	dir := withHooksEnv(t)
	seedHookStoreFacts(t, "The rate limit is 60 requests per minute")

	oldRecall := hookRecallBlock
	hookRecallBlock = func(store cliStore, agent, query string, agentTopK, sharedTopK int) (string, error) {
		facts, err := store.Recall(context.Background(), agent, query, agentTopK)
		if err != nil {
			return "", err
		}
		return renderMemoryBlock(agent, facts, nil), fmt.Errorf("shared recall failed, injecting agent facts only: broken on purpose")
	}
	t.Cleanup(func() { hookRecallBlock = oldRecall })

	out, err := dispatchHook("user-prompt", hookEventPayload{CWD: mustWorkdir(), Prompt: "rate limit check"})
	if err != nil {
		t.Fatalf("degraded injection must not error the dispatch: %v", err)
	}
	if !strings.Contains(out, "rate limit") {
		t.Errorf("degraded injection lost the agent facts: %q", out)
	}

	logData, err := os.ReadFile(filepath.Join(dir, "hooks.log"))
	if err != nil {
		t.Fatalf("hooks.log: %v", err)
	}
	if !strings.Contains(string(logData), `"outcome":"error"`) ||
		!strings.Contains(string(logData), "injecting agent facts only") {
		t.Errorf("hooks.log missing the degradation receipt: %s", logData)
	}
}

// TestRenderMemoryBlock_Sections pins the block shape: every non-empty block
// carries the hook's exact namespace, agent facts come before the shared
// section, exact duplicates render once, multi-line facts are folded, and an
// empty pair of lists renders nothing (including no marker).
func TestRenderMemoryBlock_Sections(t *testing.T) {
	const agent = "my-project"
	const wantMarker = `[GrayMatter hook recall ran for agent_id="my-project".]`
	if got := hookRecallMarker(agent); got != wantMarker {
		t.Fatalf("hook recall marker = %q, want stable contract %q", got, wantMarker)
	}

	cases := []struct {
		name        string
		agent, shrd []string
		want        string
	}{
		{"agent only", []string{"a fact"}, nil, wantMarker + "\n## Memory\n- a fact"},
		{"shared only", nil, []string{"a convention"}, wantMarker + "\n## Shared memory (project-wide)\n- a convention"},
		{
			"both sections, agent first",
			[]string{"own history"},
			[]string{"project convention"},
			wantMarker + "\n## Memory\n- own history\n\n## Shared memory (project-wide)\n- project convention",
		},
		{
			"exact duplicate renders once, under Memory",
			[]string{"same text"},
			[]string{"same text"},
			wantMarker + "\n## Memory\n- same text",
		},
		{"multi-line folded", []string{"line one\nline two"}, nil, wantMarker + "\n## Memory\n- line one line two"},
		{"both empty is nothing", nil, nil, ""},
	}
	for _, c := range cases {
		if got := renderMemoryBlock(agent, c.agent, c.shrd); got != c.want {
			t.Errorf("%s: renderMemoryBlock = %q, want %q", c.name, got, c.want)
		}
	}
}
