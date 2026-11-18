package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// These tests use the released CLI shape: executable, on-disk client
// configuration, stdio MCP, and the hook command/argv Claude would execute.
// Every process is confined to the fixture's home and store directories.
type issue81Fixture struct {
	t      *testing.T
	bin    string
	root   string
	home   string
	env    []string
	stores []string
}

type issue81Output struct {
	stdout string
	stderr string
	code   int
	err    error
}

func newIssue81Fixture(t *testing.T) *issue81Fixture {
	t.Helper()
	if testing.Short() {
		t.Skip("requires a built CLI and subprocesses")
	}
	// macOS may hand out /var/... while a child reports the same cwd as
	// /private/var/... . Keep all fixture paths on the child's canonical side.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve fixture root: %v", err)
	}
	runtimeDir := filepath.Join(root, "runtime")
	if runtime.GOOS != "windows" {
		// The daemon's fallback socket includes XDG_RUNTIME_DIR. Test temp
		// paths can exceed macOS's 104-byte Unix socket path limit.
		runtimeDir, err = os.MkdirTemp("/tmp", "gm81-")
		if err != nil {
			t.Fatalf("create short runtime dir: %v", err)
		}
		t.Cleanup(func() {
			if filepath.Dir(runtimeDir) != "/tmp" {
				t.Errorf("refusing cleanup outside /tmp: %s", runtimeDir)
				return
			}
			if err := os.RemoveAll(runtimeDir); err != nil {
				t.Errorf("remove private runtime dir: %v", err)
			}
		})
	}
	bin := filepath.Join(root, "bin", "graymatter.exe")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	copyCurrentE2EBinary(t, bin)
	home := filepath.Join(root, "home")
	for _, path := range []string{home, filepath.Join(root, "tmp"), runtimeDir, filepath.Join(home, "appdata"), filepath.Join(home, "localappdata"), filepath.Join(home, "xdg"), filepath.Join(home, "codex"), filepath.Join(home, "claude")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &issue81Fixture{t: t, bin: bin, root: root, home: home}
	isolated := map[string]string{
		"HOME": home, "USERPROFILE": home,
		"APPDATA": filepath.Join(home, "appdata"), "LOCALAPPDATA": filepath.Join(home, "localappdata"),
		"XDG_CONFIG_HOME": filepath.Join(home, "xdg"), "XDG_RUNTIME_DIR": runtimeDir,
		"CODEX_HOME": filepath.Join(home, "codex"), "CLAUDE_CONFIG_DIR": filepath.Join(home, "claude"),
		"TMP": filepath.Join(root, "tmp"), "TEMP": filepath.Join(root, "tmp"),
		"OPENAI_API_KEY": "", "VOYAGE_API_KEY": "", "ANTHROPIC_API_KEY": "",
		"GRAYMATTER_OLLAMA_URL": "disabled://issue81-tests", "GRAYMATTER_KG": "0",
		"GRAYMATTER_NO_DAEMON": "", "GRAYMATTER_STEM_KEYWORDS": "1",
		"GRAYMATTER_CANDIDATE_RETRIEVAL": "1", "GRAYMATTER_USAGE_ALIAS": "",
		"GRAYMATTER_USAGE_ALIAS_AFFINITY": "",
	}
	for key, value := range isolated {
		t.Setenv(key, value)
	}
	f.env = issue81Env(os.Environ(), isolated)
	t.Cleanup(f.stopDaemons)
	return f
}

func issue81Env(base []string, overrides map[string]string) []string {
	normalize := func(key string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(key)
		}
		return key
	}
	values := make(map[string]string, len(base)+len(overrides))
	for _, item := range base {
		key, value, ok := strings.Cut(item, "=")
		if ok && key != "" {
			// A present-but-empty Claude root is invalid for runtime routing.
			// Fixtures inherit no host project anchor unless a case explicitly
			// supplies one through overrides.
			if strings.EqualFold(key, "CLAUDE_PROJECT_DIR") {
				found := false
				for override := range overrides {
					if strings.EqualFold(override, key) {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			values[normalize(key)] = value
		}
	}
	for key, value := range overrides {
		values[normalize(key)] = value
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

func (f *issue81Fixture) registerStore(dir string) string {
	f.t.Helper()
	abs, err := filepath.Abs(dir)
	if err != nil {
		f.t.Fatal(err)
	}
	f.stores = append(f.stores, abs)
	return abs
}

func (f *issue81Fixture) run(dir, stdin string, args ...string) issue81Output {
	return f.runCommand(f.bin, dir, stdin, nil, args...)
}

func (f *issue81Fixture) runCommand(bin, dir, stdin string, extra map[string]string, args ...string) issue81Output {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = issue81Env(f.env, extra)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := issue81Output{stdout: stdout.String(), stderr: stderr.String(), err: err}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.code = exit.ExitCode()
		} else {
			result.code = -1
		}
	}
	if ctx.Err() != nil {
		result.err = fmt.Errorf("%v: %w", err, ctx.Err())
		result.code = -1
	}
	return result
}

func (f *issue81Fixture) mustRun(dir, stdin string, args ...string) issue81Output {
	f.t.Helper()
	out := f.run(dir, stdin, args...)
	if out.code != 0 {
		f.t.Fatalf("%q failed: exit=%d err=%v stdout=%q stderr=%q", args, out.code, out.err, out.stdout, out.stderr)
	}
	return out
}

func (f *issue81Fixture) stopDaemons() {
	// The registry is populated before any process can start. Stop only the
	// fixture's known absolute paths, then observe the bbolt lock release.
	seen := map[string]bool{}
	for _, store := range f.stores {
		if seen[store] {
			continue
		}
		seen[store] = true
		_ = f.run(f.root, "", "--dir", store, "daemon", "stop")
		db := filepath.Join(store, "gray.db")
		if _, err := os.Stat(db); err != nil {
			continue // never open a missing bbolt path: Open could create it
		}
		deadline := time.Now().Add(8 * time.Second)
		for {
			b, err := bolt.Open(db, 0o600, &bolt.Options{ReadOnly: true, Timeout: 100 * time.Millisecond})
			if err == nil {
				_ = b.Close()
				break
			}
			if time.Now().After(deadline) {
				f.t.Errorf("daemon did not release %s: %v", db, err)
				break
			}
		}
	}
}

func issue81WriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

type issue81Snapshot struct {
	hash  [32]byte
	mtime time.Time
}

func issue81SnapshotFile(t *testing.T, path string) issue81Snapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return issue81Snapshot{sha256.Sum256(data), info.ModTime()}
}

func issue81AssertFileUnchanged(t *testing.T, path string, want issue81Snapshot) {
	t.Helper()
	got := issue81SnapshotFile(t, path)
	if got != want {
		t.Errorf("file changed: %s", path)
	}
}

func issue81AssertOnlyMarker(t *testing.T, store string) {
	t.Helper()
	entries, err := os.ReadDir(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "MEMORY.md" || !entries[0].Type().IsRegular() {
		t.Fatalf("store-only made unexpected entries in %s: %v", store, entries)
	}
}

func issue81Result(t *testing.T, out issue81Output, status, marker, dir string) {
	t.Helper()
	if out.code != 0 || out.stderr != "" {
		t.Fatalf("init --store-only: exit=%d err=%v stdout=%q stderr=%q", out.code, out.err, out.stdout, out.stderr)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(out.stdout), &value); err != nil {
		t.Fatalf("invalid JSON %q: %v", out.stdout, err)
	}
	want := map[string]any{"schema_version": float64(1), "mode": "store-only", "data_dir": dir,
		"status": status, "marker": marker, "runtime_verified": false}
	if len(value) != len(want) {
		t.Fatalf("JSON keys = %v, want %v", value, want)
	}
	for key, expected := range want {
		if value[key] != expected {
			t.Errorf("JSON %s = %#v, want %#v", key, value[key], expected)
		}
	}
}

type issue81MCP struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
	readErr chan error
	stderr  issue81LockedBuffer
	cancel  context.CancelFunc
	closed  sync.Once
	trace   func(int, string, any, []byte)
}

type issue81LockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *issue81LockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *issue81LockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (f *issue81Fixture) startMCP(dir, command string, args []string, extra map[string]string) *issue81MCP {
	return f.startMCPWithTrace(dir, command, args, extra, nil)
}

func (f *issue81Fixture) startMCPWithTrace(dir, command string, args []string, extra map[string]string, trace func(int, string, any, []byte)) *issue81MCP {
	f.t.Helper()
	// Preserve this fixture's complete legacy catalog contract.
	args = append(append([]string(nil), args...), "--tools", "full")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir, cmd.Env = dir, issue81Env(f.env, extra)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		f.t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		f.t.Fatal(err)
	}
	m := &issue81MCP{t: f.t, cmd: cmd, stdin: stdin, lines: make(chan []byte, 16), readErr: make(chan error, 1), cancel: cancel, trace: trace}
	cmd.Stderr = &m.stderr
	if err := cmd.Start(); err != nil {
		cancel()
		f.t.Fatalf("start MCP: %v", err)
	}
	go func() {
		r := bufio.NewReader(stdout)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				m.lines <- line
			}
			if err != nil {
				m.readErr <- err
				return
			}
		}
	}()
	f.t.Cleanup(m.close)
	m.request(1, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "issue81-e2e", "version": "1"}})
	m.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	listed := m.request(2, "tools/list", map[string]any{})
	var catalog struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listed, &catalog); err != nil {
		f.t.Fatalf("decode MCP tools/list: %v", err)
	}
	want := map[string]bool{
		"memory_search": true, "memory_search_batch": true,
		"memory_add": true, "memory_alias": true, "memory_reflect": true,
		"checkpoint_save": true, "checkpoint_resume": true,
	}
	for _, tool := range catalog.Tools {
		delete(want, tool.Name)
	}
	if len(want) != 0 {
		f.t.Fatalf("MCP tools/list is incomplete: got=%d missing=%v", len(catalog.Tools), want)
	}
	return m
}

func (m *issue81MCP) close() {
	m.closed.Do(func() {
		cmd, stdin := m.cmd, m.stdin
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				m.t.Errorf("MCP exit: %v stderr=%q", err, m.stderr.String())
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			m.t.Error("MCP did not exit after stdin closed")
		}
		m.cancel()
	})
}

func (m *issue81MCP) send(v any) {
	m.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		m.t.Fatal(err)
	}
	if _, err := m.stdin.Write(append(data, '\n')); err != nil {
		m.t.Fatalf("MCP send: %v stderr=%q", err, m.stderr.String())
	}
}

func (m *issue81MCP) request(id int, method string, params any) []byte {
	m.t.Helper()
	result := m.requestAllowToolError(id, method, params)
	if bytes.Contains(result, []byte(`"isError":true`)) {
		m.t.Fatalf("MCP tool failed for %s: %s", method, result)
	}
	return result
}

func (m *issue81MCP) requestAllowToolError(id int, method string, params any) []byte {
	m.t.Helper()
	m.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case line := <-m.lines:
			var envelope struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(line, &envelope); err != nil {
				m.t.Fatalf("MCP invalid JSON %q: %v", line, err)
			}
			if envelope.ID != id {
				m.t.Fatalf("MCP response id=%d, want %d: %s", envelope.ID, id, line)
			}
			if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
				m.t.Fatalf("MCP error for %s: %s", method, line)
			}
			if len(envelope.Result) == 0 {
				m.t.Fatalf("MCP result absent for %s: %s", method, line)
			}
			if m.trace != nil {
				m.trace(id, method, params, line)
			}
			return envelope.Result
		case err := <-m.readErr:
			m.t.Fatalf("MCP pipe closed before %s response: %v stderr=%q", method, err, m.stderr.String())
		case <-deadline.C:
			m.t.Fatalf("MCP %s timed out; stderr=%q", method, m.stderr.String())
		}
	}
}

func (m *issue81MCP) add(id int, agent, fact string) {
	m.t.Helper()
	m.request(id, "tools/call", map[string]any{"name": "memory_add", "arguments": map[string]any{"agent_id": agent, "text": fact}})
}

func (m *issue81MCP) search(id int, agent, query, fact string) {
	m.t.Helper()
	result := m.request(id, "tools/call", map[string]any{"name": "memory_search", "arguments": map[string]any{"agent_id": agent, "query": query, "top_k": 8}})
	if !bytes.Contains(result, []byte(fact)) {
		m.t.Fatalf("MCP search %q did not return fact %q: %s", query, fact, result)
	}
}

func issue81HookSpec(t *testing.T, settingsPath, event string) (string, []string) {
	t.Helper()
	groups := hookGroups(t, readSettings(t, settingsPath), event)
	for _, group := range groups {
		entries, _ := group["hooks"].([]any)
		for _, raw := range entries {
			hook, _ := raw.(map[string]any)
			if hook == nil || !hookEntryHasArg(hook, "--no-create") {
				continue
			}
			command, _ := hook["command"].(string)
			args, ok := hookEntryArgs(hook)
			if command == "" || !ok {
				t.Fatalf("managed hook is not exec form: %v", hook)
			}
			return command, args
		}
	}
	t.Fatalf("no guarded managed %s hook in %s", event, settingsPath)
	return "", nil
}

func issue81HookPayload(t *testing.T, cwd, prompt string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{"cwd": cwd, "session_id": "issue81-e2e", "hook_event_name": "UserPromptSubmit", "prompt": prompt})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type issue81MCPDefinition struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

func issue81MCPFromConfig(t *testing.T, path string) issue81MCPDefinition {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		MCPServers map[string]issue81MCPDefinition `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	def := config.MCPServers["graymatter"]
	if def.Command == "" || len(def.Args) == 0 {
		t.Fatalf("MCP fixture has no executable definition: %s", path)
	}
	return def
}

func TestStoreOnlyE01_GlobalMCPAndHooks(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "project-é")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	configPath := filepath.Join(f.home, ".claude.json")
	server := map[string]any{"command": f.bin, "args": []string{"mcp", "serve"}, "env": map[string]string{"GRAYMATTER_OLLAMA_URL": "disabled://issue81-tests"}}
	issue81WriteJSON(t, configPath, map[string]any{"mcpServers": map[string]any{"graymatter": server}, "other": "preserve"})
	if err := os.WriteFile(filepath.Join(f.home, "CLAUDE.md"), []byte("global instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectConfig := filepath.Join(project, ".mcp.json")
	issue81WriteJSON(t, projectConfig, map[string]any{"mcpServers": map[string]any{"other": map[string]string{"command": "foreign"}}})
	f.mustRun(project, "", "hooks", "install", "--scope", "global")
	settings := filepath.Join(f.home, ".claude", "settings.json")
	hookCommand, hookArgs := issue81HookSpec(t, settings, hooksEventUserPrompt)
	watches := map[string]issue81Snapshot{}
	for _, path := range []string{configPath, settings, filepath.Join(f.home, "CLAUDE.md"), projectConfig} {
		watches[path] = issue81SnapshotFile(t, path)
	}
	first := f.run(project, "", "init", "--store-only", "--json")
	issue81Result(t, first, "created", "MEMORY.md", store)
	issue81AssertOnlyMarker(t, store)
	markerSnapshot := issue81SnapshotFile(t, filepath.Join(store, "MEMORY.md"))
	if quiet := f.mustRun(project, "", "init", "--store-only", "--quiet"); quiet.stdout != "" || quiet.stderr != "" {
		t.Fatalf("quiet output: stdout=%q stderr=%q", quiet.stdout, quiet.stderr)
	}
	second := f.run(project, "", "init", "--store-only", "--json")
	issue81Result(t, second, "already_prepared", "MEMORY.md", store)
	issue81AssertOnlyMarker(t, store)
	issue81AssertFileUnchanged(t, filepath.Join(store, "MEMORY.md"), markerSnapshot)
	for path, want := range watches {
		issue81AssertFileUnchanged(t, path, want)
	}
	mcpDef := issue81MCPFromConfig(t, configPath)
	m := f.startMCP(project, mcpDef.Command, mcpDef.Args, mcpDef.Env)
	projectFact := "issue81 global fixture contract is preserved"
	sharedFact := "issue81 shared fixture convention is retained"
	m.add(3, "project", projectFact) // namespace is explicit, independent of cwd
	m.search(4, "project", "global fixture contract", projectFact)
	m.add(5, "__shared__", sharedFact)
	m.search(6, "__shared__", "shared fixture convention", sharedFact)
	if _, err := os.Stat(filepath.Join(store, "gray.db")); err != nil {
		t.Fatalf("MCP did not create store: %v", err)
	}
	status := f.mustRun(project, "", "daemon", "status")
	if !strings.Contains(status.stdout, "daemon: running") || !strings.Contains(status.stdout, "addr:") || !strings.Contains(status.stdout, "pid:") {
		t.Fatalf("auto-started daemon not discoverable: %+v", status)
	}
	if db, err := bolt.Open(filepath.Join(store, "gray.db"), 0o600, &bolt.Options{ReadOnly: true, Timeout: 100 * time.Millisecond}); err == nil {
		_ = db.Close()
		t.Fatal("daemon was reported running but did not hold the database lock")
	} else if !errors.Is(err, bolt.ErrTimeout) {
		t.Fatalf("database lock probe failed unexpectedly: %v", err)
	}
	hook := f.runCommand(hookCommand, project, issue81HookPayload(t, project, "global fixture contract"), nil, hookArgs...)
	if hook.code != 0 || !strings.Contains(hook.stdout, projectFact) || !strings.Contains(hook.stdout, sharedFact) {
		t.Fatalf("persisted global hook: exit=%d stdout=%q stderr=%q", hook.code, hook.stdout, hook.stderr)
	}
	whileActive := f.run(project, "", "init", "--store-only", "--json")
	issue81Result(t, whileActive, "already_prepared", "gray.db", store)
	for path, want := range watches {
		issue81AssertFileUnchanged(t, path, want)
	}
	// A second MCP process uses the persisted client definition and the same store.
	m.close()
	m2 := f.startMCP(project, mcpDef.Command, mcpDef.Args, mcpDef.Env)
	m2.search(3, "project", "global fixture contract", projectFact)
	m2.search(4, "__shared__", "shared fixture convention", sharedFact)
}

func TestStoreOnlyE02_GlobalGuardAndDBOnly(t *testing.T) {
	f := newIssue81Fixture(t)
	installDir := filepath.Join(f.root, "install-cwd")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.mustRun(installDir, "", "hooks", "install", "--scope", "global")
	hookCommand, hookArgs := issue81HookSpec(t, filepath.Join(f.home, ".claude", "settings.json"), hooksEventUserPrompt)
	project := filepath.Join(f.root, "guarded-project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	pre := f.runCommand(hookCommand, project, issue81HookPayload(t, project, "remember: unprepared fixture fact"), nil, hookArgs...)
	if pre.code != 0 || pre.stdout != "" || pre.stderr != "" {
		t.Fatalf("unprepared guarded hook: %+v", pre)
	}
	if entries, err := os.ReadDir(project); err != nil || len(entries) != 0 {
		t.Fatalf("guarded hook created files: %v err=%v", entries, err)
	}
	issue81Result(t, f.run(project, "", "init", "--store-only", "--json"), "created", "MEMORY.md", store)
	issue81AssertOnlyMarker(t, store)
	fact := "guarded marker admits this unique remembered fact"
	post := f.runCommand(hookCommand, project, issue81HookPayload(t, project, "remember: "+fact), nil, hookArgs...)
	if post.code != 0 || !strings.Contains(post.stdout, "Saved to memory") {
		t.Fatalf("prepared guarded hook: %+v", post)
	}
	if _, err := os.Stat(filepath.Join(store, "gray.db")); err != nil {
		t.Fatalf("hook did not create DB after marker: %v", err)
	}
	m := f.startMCP(project, f.bin, []string{"mcp", "serve"}, nil)
	m.search(3, "guarded-project", "guarded marker", fact)
	dbOnly := filepath.Join(f.root, "db-only-project")
	if err := os.MkdirAll(dbOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	dbOnlyStore := f.registerStore(filepath.Join(dbOnly, ".graymatter"))
	f.mustRun(dbOnly, "", "remember", "db-only-project", "DB-only fixture exists without MEMORY")
	if _, err := os.Stat(filepath.Join(dbOnlyStore, "MEMORY.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("seed unexpectedly created MEMORY: %v", err)
	}
	issue81Result(t, f.run(dbOnly, "", "init", "--store-only", "--json"), "already_prepared", "gray.db", dbOnlyStore)
	dbHook := f.runCommand(hookCommand, dbOnly, issue81HookPayload(t, dbOnly, "DB-only fixture"), nil, hookArgs...)
	if dbHook.code != 0 || !strings.Contains(dbHook.stdout, "DB-only fixture exists") {
		t.Fatalf("DB-only guarded hook: %+v", dbHook)
	}
	dbMCP := f.startMCP(dbOnly, f.bin, []string{"mcp", "serve"}, nil)
	dbMCP.search(3, "db-only-project", "DB-only fixture", "DB-only fixture exists without MEMORY")
	if _, err := os.Stat(filepath.Join(dbOnlyStore, "MEMORY.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("DB-only flow added MEMORY: %v", err)
	}
}

func TestStoreOnlyE03_MCPWithoutInit(t *testing.T) {
	f := newIssue81Fixture(t)
	installDir := filepath.Join(f.root, "install-cwd")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.mustRun(installDir, "", "hooks", "install", "--scope", "global")
	hookCommand, hookArgs := issue81HookSpec(t, filepath.Join(f.home, ".claude", "settings.json"), hooksEventUserPrompt)

	clean := filepath.Join(f.root, "mcp-without-init")
	if err := os.MkdirAll(clean, 0o755); err != nil {
		t.Fatal(err)
	}
	cleanStore := f.registerStore(filepath.Join(clean, ".graymatter"))
	m := f.startMCP(clean, f.bin, []string{"mcp", "serve"}, nil)
	m.add(3, "mcp-without-init", "MCP can initialize a clean directory")
	if _, err := os.Stat(filepath.Join(cleanStore, "gray.db")); err != nil {
		t.Fatalf("MCP did not initialize its store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cleanStore, "MEMORY.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("MCP created marker unexpectedly: %v", err)
	}
	hook := f.runCommand(hookCommand, clean, issue81HookPayload(t, clean, "MCP can initialize"), nil, hookArgs...)
	if hook.code != 0 || !strings.Contains(hook.stdout, "MCP can initialize") {
		t.Fatalf("DB-only guarded hook: %+v", hook)
	}
}

func TestStoreOnlyE04_ExplicitDirRouting(t *testing.T) {
	f := newIssue81Fixture(t)
	cwd := filepath.Join(f.root, "other-cwd")
	storeA := f.registerStore(filepath.Join(f.root, "store-A"))
	storeB := f.registerStore(filepath.Join(f.root, "store-B"))
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	f.mustRun(cwd, "", "hooks", "install", "--scope", "global")
	command, args := issue81HookSpec(t, filepath.Join(f.home, ".claude", "settings.json"), hooksEventUserPrompt)
	writeRoute := func(path, store string) {
		t.Helper()
		hookEntry := map[string]any{"type": "command", "command": command,
			"args": append([]string{"--dir", store}, args...)}
		hookGroup := map[string]any{"hooks": []any{hookEntry}}
		issue81WriteJSON(t, path, map[string]any{
			"mcpServers": map[string]any{"graymatter": issue81MCPDefinition{
				Command: f.bin, Args: []string{"--dir", store, "mcp", "serve"},
				Env: map[string]string{"GRAYMATTER_OLLAMA_URL": "disabled://issue81-tests"},
			}},
			"hooks": map[string]any{hooksEventUserPrompt: []any{hookGroup}},
		})
	}
	configA := filepath.Join(f.root, "route-A.json")
	configB := filepath.Join(f.root, "route-B.json")
	writeRoute(configA, storeA)
	writeRoute(configB, storeB)
	commandA, argsA := issue81HookSpec(t, configA, hooksEventUserPrompt)
	mcpA := issue81MCPFromConfig(t, configA)
	issue81Result(t, f.run(cwd, "", "--dir", storeA, "init", "--store-only", "--json"), "created", "MEMORY.md", storeA)
	if _, err := os.Stat(storeB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparing A also prepared B: %v", err)
	}
	fact := "explicit route A receives this fact"
	hook := f.runCommand(commandA, cwd, issue81HookPayload(t, cwd, "remember: "+fact), nil, argsA...)
	if hook.code != 0 || !strings.Contains(hook.stdout, "Saved to memory") {
		t.Fatalf("explicit hook route A: %+v", hook)
	}
	m := f.startMCP(cwd, mcpA.Command, mcpA.Args, mcpA.Env)
	m.search(3, filepath.Base(cwd), "explicit route A", fact)
	if _, err := os.Stat(filepath.Join(cwd, ".graymatter")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("custom route created default cwd store: %v", err)
	}
	commandB, argsB := issue81HookSpec(t, configB, hooksEventUserPrompt)
	wrong := f.runCommand(commandB, cwd, issue81HookPayload(t, cwd, "remember: should stay absent"), nil, argsB...)
	if wrong.code != 0 || wrong.stdout != "" {
		t.Fatalf("unprepared B hook was active: %+v", wrong)
	}
	if _, err := os.Stat(storeB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched hook created B: %v", err)
	}
}

func TestStoreOnlyT20_ThirtyTwoProcesses(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "concurrent")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	results := make([]issue81Output, 32)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = f.run(project, "", "init", "--store-only", "--json")
		}(i)
	}
	wg.Wait()
	created := 0
	for i, result := range results {
		if result.code != 0 {
			t.Errorf("process %d: exit=%d err=%v stdout=%q stderr=%q", i, result.code, result.err, result.stdout, result.stderr)
			continue
		}
		var value struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(result.stdout), &value); err != nil {
			t.Errorf("process %d invalid JSON: %v %q", i, err, result.stdout)
		}
		if value.Status == "created" {
			created++
		} else if value.Status != "already_prepared" {
			t.Errorf("process %d status %q", i, value.Status)
		}
	}
	if created != 1 {
		t.Errorf("created count=%d, want 1", created)
	}
	issue81AssertOnlyMarker(t, store)
	if b, err := os.ReadFile(filepath.Join(store, "MEMORY.md")); err != nil || len(b) == 0 {
		t.Fatalf("concurrent marker incomplete: size=%d err=%v", len(b), err)
	}
}

func TestStoreOnlyT12T14_AliasedAndRelativeDataDirs(t *testing.T) {
	f := newIssue81Fixture(t)
	realDir := f.registerStore(filepath.Join(f.root, "real-é space"))
	alias := filepath.Join(f.root, "alias-é space")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatalf("directory symlink unavailable; T12 requires a runner with link support: %v", err)
	}
	issue81Result(t, f.run(f.root, "", "--dir", alias, "init", "--store-only", "--json"), "created", "MEMORY.md", alias)
	issue81AssertOnlyMarker(t, realDir)
	if entries, err := os.ReadDir(alias); err != nil || len(entries) != 1 {
		t.Fatalf("alias did not resolve to selected directory: %v err=%v", entries, err)
	}
	realParent := filepath.Join(f.root, "real-parent")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	parentAlias := filepath.Join(f.root, "parent-alias")
	if err := os.Symlink(realParent, parentAlias); err != nil {
		t.Fatalf("ancestor symlink unavailable; T12 requires a runner with link support: %v", err)
	}
	aliasChild := f.registerStore(filepath.Join(parentAlias, "child-é", "store"))
	issue81Result(t, f.run(f.root, "", "--dir", aliasChild, "init", "--store-only", "--json"), "created", "MEMORY.md", aliasChild)
	issue81AssertOnlyMarker(t, filepath.Join(realParent, "child-é", "store"))
	bad := filepath.Join(f.root, "dangling-alias")
	if err := os.Symlink(filepath.Join(f.root, "absent-destination"), bad); err != nil {
		t.Fatal(err)
	}
	if got := f.run(f.root, "", "--dir", bad, "init", "--store-only", "--json"); got.code == 0 || got.stdout != "" {
		t.Fatalf("dangling data dir accepted: %+v", got)
	}
	if _, err := os.Lstat(bad); err != nil {
		t.Fatalf("dangling alias replaced: %v", err)
	}
	cycle := filepath.Join(f.root, "cyclic-alias")
	if err := os.Symlink(cycle, cycle); err != nil {
		t.Fatalf("cycle symlink unavailable; T13 requires a runner with link support: %v", err)
	}
	if got := f.run(f.root, "", "--dir", cycle, "init", "--store-only", "--json"); got.code == 0 || got.stdout != "" {
		t.Fatalf("cyclic data dir accepted: %+v", got)
	}
	if _, err := os.Lstat(cycle); err != nil {
		t.Fatalf("cyclic alias replaced: %v", err)
	}
	file := filepath.Join(f.root, "not-a-directory")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := f.run(f.root, "", "--dir", file, "init", "--store-only", "--json"); got.code == 0 || got.stdout != "" {
		t.Fatalf("file data dir accepted: %+v", got)
	}
	relative := filepath.Join(f.root, "relative é space")
	if err := os.MkdirAll(relative, 0o755); err != nil {
		t.Fatal(err)
	}
	f.registerStore(relative)
	issue81Result(t, f.run(relative, "", "--dir", ".", "init", "--store-only", "--json"), "created", "MEMORY.md", relative)
	issue81AssertOnlyMarker(t, relative)
	if _, err := os.Stat(filepath.Join(f.root, ".graymatter")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relative --dir created store in unrelated cwd: %v", err)
	}
}

func TestStoreOnlyT15T17_CLIFlagAndOutputContract(t *testing.T) {
	f := newIssue81Fixture(t)
	rejected := []struct {
		name string
		args []string
	}{
		{"positional", []string{"anything"}},
		{"positional-after-double-dash", []string{"--", "anything"}},
		{"global", []string{"--global"}},
		{"hooks", []string{"--hooks"}},
		{"interactive", []string{"--interactive"}},
		{"interactive-short", []string{"-i"}},
		{"antigravity", []string{"--with-antigravity"}},
		{"kg", []string{"--kg"}},
		{"only-empty", []string{"--only", ""}},
		{"only-commas", []string{"--only", ",,,"}},
		{"unknown-flag", []string{"--unrecognized"}},
		{"invalid-bool", []string{"--global=perhaps"}},
	}
	for _, tc := range rejected {
		t.Run("reject-"+tc.name, func(t *testing.T) {
			cwd := filepath.Join(f.root, "reject-"+tc.name)
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			store := f.registerStore(filepath.Join(cwd, ".graymatter"))
			args := append([]string{"--dir", store, "init", "--store-only"}, tc.args...)
			out := f.run(cwd, "", args...)
			if out.code != 1 || out.stderr == "" || strings.Contains(out.stdout, `"schema_version"`) {
				t.Fatalf("invalid invocation %q: exit=%d stdout=%q stderr=%q", args, out.code, out.stdout, out.stderr)
			}
			if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid invocation created store: %v", err)
			}
		})
	}
	for _, badDir := range []string{"", "  \t "} {
		t.Run(fmt.Sprintf("reject-dir-%q", badDir), func(t *testing.T) {
			cwd := filepath.Join(f.root, "blank-dir")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			out := f.run(cwd, "", "--dir", badDir, "init", "--store-only", "--json")
			if out.code != 1 || out.stdout != "" || out.stderr == "" {
				t.Fatalf("blank --dir: %+v", out)
			}
			if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
				t.Fatalf("blank --dir mutated cwd: %v err=%v", entries, err)
			}
		})
	}
	accepted := []struct {
		name string
		args []string
	}{
		{"incompatible-explicit-false", []string{"--global=false", "--hooks=false", "--interactive=false", "--with-antigravity=false", "--kg=false"}},
		{"redundant-explicit-true", []string{"--skip-claudecode=true", "--skip-cursor=true", "--skip-codex=true", "--skip-opencode=true", "--skip-instructions=true", "--no-path=true", "--no-daemon=true"}},
		{"redundant-explicit-false", []string{"--skip-claudecode=false", "--skip-cursor=false", "--skip-codex=false", "--skip-opencode=false", "--skip-instructions=false", "--no-path=false", "--no-daemon=false"}},
	}
	for _, tc := range accepted {
		t.Run("accept-"+tc.name, func(t *testing.T) {
			cwd := filepath.Join(f.root, "accept-"+tc.name)
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			store := f.registerStore(filepath.Join(cwd, ".graymatter"))
			args := append([]string{"--dir", store, "init", "--store-only", "--json"}, tc.args...)
			issue81Result(t, f.run(cwd, "", args...), "created", "MEMORY.md", store)
			issue81AssertOnlyMarker(t, store)
		})
	}
	t.Run("json-over-quiet", func(t *testing.T) {
		cwd := filepath.Join(f.root, "json-over-quiet")
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		store := f.registerStore(filepath.Join(cwd, ".graymatter"))
		out := f.run(cwd, "", "--dir", store, "--quiet", "--json", "init", "--store-only")
		issue81Result(t, out, "created", "MEMORY.md", store)
		if !strings.HasSuffix(out.stdout, "\n") || strings.Count(out.stdout, "\n") != 1 {
			t.Errorf("JSON output should be one newline-terminated object: %q", out.stdout)
		}
	})
	t.Run("human-and-quiet", func(t *testing.T) {
		cwd := filepath.Join(f.root, "human-and-quiet")
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		store := f.registerStore(filepath.Join(cwd, ".graymatter"))
		created := f.mustRun(cwd, "", "--dir", store, "init", "--store-only")
		if created.stdout != "GrayMatter store prepared at "+store+" (created MEMORY.md).\n" || created.stderr != "" {
			t.Errorf("created human output: %+v", created)
		}
		already := f.mustRun(cwd, "", "--dir", store, "init", "--store-only")
		if already.stdout != "GrayMatter store already prepared at "+store+" (MEMORY.md).\n" || already.stderr != "" {
			t.Errorf("already-prepared human output: %+v", already)
		}
		quiet := f.mustRun(cwd, "", "--dir", store, "init", "--store-only", "--quiet")
		if quiet.stdout != "" || quiet.stderr != "" {
			t.Errorf("quiet output: %+v", quiet)
		}
	})
	t.Run("help-no-write", func(t *testing.T) {
		cwd := filepath.Join(f.root, "help-no-write")
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		store := f.registerStore(filepath.Join(cwd, ".graymatter"))
		out := f.run(cwd, "", "--dir", store, "init", "--store-only", "--help")
		if out.code != 0 || !strings.Contains(out.stdout, "--store-only") || out.stderr != "" {
			t.Fatalf("help: %+v", out)
		}
		if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("help created store: %v", err)
		}
	})
}
