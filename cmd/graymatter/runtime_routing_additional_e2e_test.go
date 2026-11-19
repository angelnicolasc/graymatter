package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
)

func TestRuntimeHTTPNoAuthLoopbackUsesSelectedStore(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "http-no-auth-project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	f.mustRun(project, "", "init", "--store-only")
	addr := runtimeFreeTCPAddress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	cmd := exec.CommandContext(ctx, f.bin, "--no-daemon", "mcp", "serve", "--no-create", "--http", addr, "--no-auth")
	cmd.Dir, cmd.Env = project, issue81Env(f.env, nil)
	var stdout, stderr issue81LockedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
			"clientInfo": map[string]string{"name": "routing-no-auth-test", "version": "1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil || resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("serverInfo")) {
				t.Fatalf("loopback --no-auth initialize: status=%d body=%q err=%v", resp.StatusCode, body, readErr)
			}
			if _, err := os.Stat(filepath.Join(store, "gray.db")); err != nil {
				t.Fatalf("accepted HTTP MCP did not open selected store: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(store, "graymatter.http-token")); !os.IsNotExist(err) {
				t.Fatalf("--no-auth unexpectedly created token: %v", err)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("loopback --no-auth MCP did not listen: stdout=%q stderr=%q", stdout.String(), stderr.String())
}

func runtimeFreeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestRuntimeHTTPColdPrepareReconnectAndExplicitAgentTools(t *testing.T) {
	f := newIssue81Fixture(t)
	service := filepath.Join(f.root, "http-service")
	otherProject := filepath.Join(f.root, "inherited-claude-project")
	for _, dir := range []string{service, otherProject} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := f.registerStore(filepath.Join(service, ".graymatter"))
	otherStore := filepath.Join(otherProject, ".graymatter")
	addr := runtimeFreeTCPAddress(t)
	env := map[string]string{"CLAUDE_PROJECT_DIR": otherProject, "GRAYMATTER_HTTP_TOKEN": ""}
	cold := f.runCommand(f.bin, service, "", env,
		"--no-daemon", "mcp", "serve", "--no-create", "--http", addr)
	if cold.code != 1 || cold.stdout != "" || !strings.Contains(cold.stderr, "store is not prepared") {
		t.Fatalf("cold HTTP MCP did not reject before listener/token: %+v", cold)
	}
	if _, err := os.Lstat(store); !os.IsNotExist(err) {
		t.Fatalf("cold HTTP MCP created service store: %v", err)
	}
	if _, err := os.Lstat(otherStore); !os.IsNotExist(err) {
		t.Fatalf("cold HTTP MCP used inherited Claude store: %v", err)
	}
	if conn, err := net.DialTimeout("tcp", addr, 150*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("cold rejected HTTP MCP left a listener behind")
	}
	f.mustRun(service, "", "init", "--store-only")
	issue81AssertOnlyMarker(t, store)

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	cmd := exec.CommandContext(ctx, f.bin, "--no-daemon", "mcp", "serve", "--tools", "full", "--no-create", "--http", addr)
	cmd.Dir, cmd.Env = service, issue81Env(f.env, env)
	var stdout, stderr issue81LockedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	client := &http.Client{Timeout: 2 * time.Second}
	post := func(payload any, token, session string) (int, []byte, string, error) {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, "", err
		}
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", bytes.NewReader(data))
		if err != nil {
			return 0, nil, "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
			req.Header.Set("Mcp-Protocol-Version", "2025-03-26")
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, "", err
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, body, resp.Header.Get("Mcp-Session-Id"), readErr
	}
	initialize := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
			"clientInfo": map[string]string{"name": "routing-http-test", "version": "1"},
		},
	}
	deadline := time.Now().Add(8 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		status, _, _, err := post(initialize, "", "")
		if err == nil {
			if status != http.StatusUnauthorized {
				t.Fatalf("HTTP auth default changed after preparation: status=%d", status)
			}
			ready = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("prepared HTTP MCP did not listen: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	tokenBytes, err := os.ReadFile(filepath.Join(store, "graymatter.http-token"))
	if err != nil {
		t.Fatalf("prepared service store token missing: %v", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	status, body, session, err := post(initialize, token, "")
	if err != nil || status != http.StatusOK || !bytes.Contains(body, []byte("serverInfo")) {
		t.Fatalf("authenticated initialize: status=%d body=%q err=%v", status, body, err)
	}
	status, body, _, err = post(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, token, session)
	if err != nil || (status != http.StatusAccepted && status != http.StatusNoContent && status != http.StatusOK) {
		t.Fatalf("initialized notification: status=%d body=%q err=%v", status, body, err)
	}
	status, body, _, err = post(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, token, session)
	if err != nil || status != http.StatusOK || !bytes.Contains(body, []byte("memory_add")) || !bytes.Contains(body, []byte("memory_search")) {
		t.Fatalf("authenticated tools/list: status=%d body=%q err=%v", status, body, err)
	}
	for _, name := range []string{"memory_search", "memory_search_batch", "memory_add", "memory_alias", "memory_reflect", "checkpoint_save", "checkpoint_resume"} {
		if !bytes.Contains(body, []byte(`"name":"`+name+`"`)) {
			t.Fatalf("authenticated tools/list omitted %s: %s", name, body)
		}
	}
	status, body, _, err = post(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{
		"name": "memory_add", "arguments": map[string]any{"text": "implicit identity must fail"},
	}}, token, session)
	if err != nil || (status != http.StatusOK && status != http.StatusBadRequest) ||
		!bytes.Contains(body, []byte("agent_id")) ||
		(!bytes.Contains(body, []byte(`"isError":true`)) && !bytes.Contains(body, []byte(`"error"`))) {
		t.Fatalf("HTTP memory_add accepted missing agent_id: status=%d body=%q err=%v", status, body, err)
	}
	const fact = "remote explicit identity reaches service store"
	status, body, _, err = post(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{
		"name": "memory_add", "arguments": map[string]any{"agent_id": "remote-client", "text": fact},
	}}, token, session)
	if err != nil || status != http.StatusOK || bytes.Contains(body, []byte(`"isError":true`)) {
		t.Fatalf("authenticated memory_add: status=%d body=%q err=%v", status, body, err)
	}
	status, body, _, err = post(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": map[string]any{
		"name": "memory_search", "arguments": map[string]any{"agent_id": "remote-client", "query": "explicit identity", "top_k": 8},
	}}, token, session)
	if err != nil || status != http.StatusOK || !bytes.Contains(body, []byte(fact)) || bytes.Contains(body, []byte(`"isError":true`)) {
		t.Fatalf("authenticated memory_search: status=%d body=%q err=%v", status, body, err)
	}
	if _, err := os.Lstat(otherStore); !os.IsNotExist(err) {
		t.Fatalf("HTTP tool inferred inherited Claude project store: %v", err)
	}
}

func TestRuntimeAliasedDatabaseRejectsEveryMCPAndHookMode(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "aliased-db-project")
	store := filepath.Join(project, ".graymatter")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.root, "external-db-target")
	if err := os.WriteFile(target, []byte("external database fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(store, "gray.db")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("file symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "MEMORY.md"), []byte("prepared"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := issue81SnapshotFile(t, target)
	for _, args := range [][]string{
		{"--no-daemon", "mcp", "serve"},
		{"--no-daemon", "mcp", "serve", "--no-create"},
		{"mcp", "serve"},
		{"mcp", "serve", "--no-create"},
		{"--no-daemon", "mcp", "serve", "--http", "127.0.0.1:0", "--no-auth"},
		{"--no-daemon", "mcp", "serve", "--http", "127.0.0.1:0", "--no-create"},
	} {
		out := f.run(project, "", args...)
		if out.code != 1 || out.stdout != "" || !strings.Contains(out.stderr, "gray.db is not a regular file") {
			t.Fatalf("MCP accepted DB alias with %q: %+v", args, out)
		}
		issue81AssertFileUnchanged(t, target, want)
	}
	packet := issue81HookPayload(t, project, "remember: DB alias must not write")
	for _, args := range [][]string{
		{"--no-daemon", "hooks", "run", "user-prompt"},
		{"--no-daemon", "hooks", "run", "user-prompt", "--no-create"},
		{"hooks", "run", "user-prompt"},
		{"hooks", "run", "user-prompt", "--no-create"},
	} {
		out := f.run(project, packet, args...)
		if out.code != 0 || out.stdout != "" || !strings.Contains(out.stderr, "invalid store entry") {
			t.Fatalf("hook accepted DB alias with %q: %+v", args, out)
		}
		issue81AssertFileUnchanged(t, target, want)
	}
	for _, leaf := range []string{"hooks.log", "daemon.log", "graymatter.http-token"} {
		if _, err := os.Lstat(filepath.Join(store, leaf)); !os.IsNotExist(err) {
			t.Fatalf("rejected runtime created %s: %v", leaf, err)
		}
	}
}

func TestRuntimeUnsafeDatabaseWithoutMarkerAcrossTransports(t *testing.T) {
	f := newIssue81Fixture(t)
	for _, leafKind := range []string{"symlink", "directory"} {
		t.Run(leafKind, func(t *testing.T) {
			project := filepath.Join(f.root, "no-marker-"+leafKind)
			store := filepath.Join(project, ".graymatter")
			if err := os.MkdirAll(store, 0o755); err != nil {
				t.Fatal(err)
			}
			var target string
			if leafKind == "symlink" {
				target = filepath.Join(f.root, "no-marker-external-db")
				if err := os.WriteFile(target, []byte("external database fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(store, "gray.db")); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("file symlink unavailable: %v", err)
					}
					t.Fatal(err)
				}
			} else if err := os.Mkdir(filepath.Join(store, "gray.db"), 0o755); err != nil {
				t.Fatal(err)
			}
			var canary issue81Snapshot
			if target != "" {
				canary = issue81SnapshotFile(t, target)
			}
			for _, args := range [][]string{
				{"--no-daemon", "mcp", "serve"},
				{"mcp", "serve"},
				{"--no-daemon", "mcp", "serve", "--no-create"},
				{"mcp", "serve", "--no-create"},
				{"--no-daemon", "mcp", "serve", "--http", "127.0.0.1:0", "--no-auth"},
				{"mcp", "serve", "--http", "127.0.0.1:0", "--no-create"},
			} {
				out := f.run(project, "", args...)
				if out.code != 1 || out.stdout != "" || !strings.Contains(out.stderr, "gray.db is not a regular file") {
					t.Fatalf("%s DB accepted by MCP %q: %+v", leafKind, args, out)
				}
			}
			packet := issue81HookPayload(t, project, "remember: unsafe DB must not write")
			for _, args := range [][]string{
				{"--no-daemon", "hooks", "run", "user-prompt"},
				{"hooks", "run", "user-prompt"},
				{"--no-daemon", "hooks", "run", "user-prompt", "--no-create"},
				{"hooks", "run", "user-prompt", "--no-create"},
			} {
				out := f.run(project, packet, args...)
				if out.code != 0 || out.stdout != "" || !strings.Contains(out.stderr, "invalid store entry") {
					t.Fatalf("%s DB accepted by hook %q: %+v", leafKind, args, out)
				}
			}
			if target != "" {
				issue81AssertFileUnchanged(t, target, canary)
			}
			for _, leaf := range []string{"MEMORY.md", "hooks.log", "daemon.log", "graymatter.http-token"} {
				if _, err := os.Lstat(filepath.Join(store, leaf)); !os.IsNotExist(err) {
					t.Fatalf("rejected %s DB created %s: %v", leafKind, leaf, err)
				}
			}
		})
	}
}

// E10: every installed hook event must reject an unsafe database leaf before
// opening a store, with or without a marker, in eager and guarded modes and
// with direct or daemon-backed execution selected. All paths stay in this
// fixture; a real symlink target is a canary for external writes.
func TestRuntimeUnsafeDatabaseHooksAllEventsAndModes(t *testing.T) {
	f := newIssue81Fixture(t)
	events := []struct {
		cli  string
		wire string
	}{
		{"session-start", "SessionStart"},
		{"user-prompt", "UserPromptSubmit"},
		{"pre-compact", "PreCompact"},
		{"session-end", "SessionEnd"},
	}
	for _, leafKind := range []string{"symlink", "directory"} {
		for _, hasMarker := range []bool{false, true} {
			markerName := "without-marker"
			if hasMarker {
				markerName = "with-marker"
			}
			t.Run(leafKind+"/"+markerName, func(t *testing.T) {
				project := filepath.Join(f.root, "hook-matrix-"+leafKind+"-"+markerName)
				store := filepath.Join(project, ".graymatter")
				t.Cleanup(func() { _ = f.run(project, "", "--dir", store, "daemon", "stop") })
				if err := os.MkdirAll(store, 0o755); err != nil {
					t.Fatal(err)
				}
				db := filepath.Join(store, "gray.db")
				var target string
				var canary issue81Snapshot
				if leafKind == "symlink" {
					target = filepath.Join(f.root, "hook-matrix-external-"+markerName)
					if err := os.WriteFile(target, []byte("external database must remain intact"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, db); err != nil {
						if runtime.GOOS == "windows" {
							t.Skipf("file symlink unavailable; real leaf integration coverage omitted: %v", err)
						}
						t.Fatal(err)
					}
					canary = issue81SnapshotFile(t, target)
				} else if err := os.Mkdir(db, 0o755); err != nil {
					t.Fatal(err)
				}
				var markerSnapshot issue81Snapshot
				marker := filepath.Join(store, "MEMORY.md")
				if hasMarker {
					if err := os.WriteFile(marker, []byte("prepared marker must remain intact"), 0o600); err != nil {
						t.Fatal(err)
					}
					markerSnapshot = issue81SnapshotFile(t, marker)
				}
				for _, event := range events {
					payload := map[string]string{
						"cwd": project, "session_id": "unsafe-db-hook-matrix", "hook_event_name": event.wire,
					}
					switch event.cli {
					case "session-start":
						payload["source"] = "startup"
					case "user-prompt":
						payload["prompt"] = "remember: this must never reach an external database"
					}
					packet, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					for _, execution := range []struct {
						name     string
						noDaemon bool
					}{{"direct", true}, {"daemon", false}} {
						for _, guard := range []struct {
							name     string
							noCreate bool
						}{{"eager", false}, {"no-create", true}} {
							t.Run(event.cli+"/"+execution.name+"/"+guard.name, func(t *testing.T) {
								args := []string{"hooks", "run", event.cli}
								if execution.noDaemon {
									args = append([]string{"--no-daemon"}, args...)
								}
								if guard.noCreate {
									args = append(args, "--no-create")
								}
								out := f.run(project, string(packet), args...)
								if out.code != 0 || out.stdout != "" || !strings.Contains(out.stderr, "invalid store entry") {
									t.Fatalf("unsafe %s DB accepted by hook %q: %+v", leafKind, args, out)
								}
								if target != "" {
									issue81AssertFileUnchanged(t, target, canary)
								}
								if hasMarker {
									issue81AssertFileUnchanged(t, marker, markerSnapshot)
								} else if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
									t.Fatalf("rejected hook created marker: %v", err)
								}
								for _, leaf := range []string{"hooks.log", "daemon.log", "graymatter.http-token", "graymatter.pid", "graymatter.addr"} {
									if _, err := os.Lstat(filepath.Join(store, leaf)); !errors.Is(err, os.ErrNotExist) {
										t.Fatalf("rejected hook created %s: %v", leaf, err)
									}
								}
							})
						}
					}
				}
				if leafKind == "symlink" {
					got, err := os.Readlink(db)
					if err != nil || got != target {
						t.Fatalf("rejected hook replaced DB alias: target=%q err=%v", got, err)
					}
				} else if entries, err := os.ReadDir(db); err != nil || len(entries) != 0 {
					t.Fatalf("rejected hook modified DB directory: entries=%v err=%v", entries, err)
				}
				status := f.run(project, "", "daemon", "status")
				if status.code != 0 || !strings.Contains(status.stdout, "daemon: not running") {
					t.Fatalf("rejected hook left a daemon running: %+v", status)
				}
			})
		}
	}
}

func TestRuntimeNoCreateAdmitsRegularDatabaseBeforeHealthCheck(t *testing.T) {
	f := newIssue81Fixture(t)
	for _, tc := range []struct {
		name         string
		contents     []byte
		healthyAfter bool
	}{
		{name: "empty", healthyAfter: true},
		{name: "corrupt", contents: []byte("not a bbolt database")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := filepath.Join(f.root, tc.name)
			store := filepath.Join(project, ".graymatter")
			if err := os.MkdirAll(store, 0o755); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(store, "gray.db")
			if err := os.WriteFile(db, tc.contents, 0o600); err != nil {
				t.Fatal(err)
			}
			inspection, err := inspectRuntimeStore(store, true)
			if err != nil || inspection.preparedBy != "gray.db" {
				t.Fatalf("regular DB failed no-create admission: %+v, %v", inspection, err)
			}
			out := f.run(project, "", "--no-daemon", "mcp", "serve", "--no-create")
			if out.stdout != "" || strings.Contains(out.stderr, "store is not prepared") ||
				strings.Contains(out.stderr, "not a regular file") {
				t.Fatalf("DB health was confused with no-create eligibility: %+v", out)
			}
			if tc.healthyAfter {
				if out.code != 0 {
					t.Fatalf("empty regular DB should be initialized after admission: %+v", out)
				}
				info, err := os.Stat(db)
				if err != nil || info.Size() == 0 {
					t.Fatalf("runtime did not initialize empty DB: %v, %+v", err, info)
				}
			} else if out.code != 1 || !strings.Contains(out.stderr, "open memory") {
				t.Fatalf("corrupt regular DB was not rejected by runtime health: %+v", out)
			}
		})
	}
	t.Run("valid-readonly", func(t *testing.T) {
		project := filepath.Join(f.root, "valid-readonly")
		store := filepath.Join(project, ".graymatter")
		cfg := graymatter.DefaultConfig()
		cfg.DataDir = store
		cfg.EmbeddingMode = graymatter.EmbeddingKeyword
		cfg.VectorReconcileInterval = 0
		cfg.AsyncConsolidate = false
		mem, err := graymatter.NewWithConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Remember(context.Background(), "valid-readonly", "readonly admission fixture fact"); err != nil {
			_ = mem.Close()
			t.Fatal(err)
		}
		if err := mem.Close(); err != nil {
			t.Fatal(err)
		}
		db := filepath.Join(store, "gray.db")
		if err := os.Chmod(db, 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(db, 0o600) })
		info, err := os.Stat(db)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o200 != 0 {
			t.Fatalf("fixture database remained writable after chmod: %s", info.Mode())
		}
		inspection, err := inspectRuntimeStore(store, true)
		if err != nil || inspection.preparedBy != "gray.db" {
			t.Fatalf("read-only regular DB failed no-create admission: %+v, %v", inspection, err)
		}
		out := f.run(project, "", "--no-daemon", "mcp", "serve", "--no-create")
		if out.stdout != "" || strings.Contains(out.stderr, "store is not prepared") ||
			strings.Contains(out.stderr, "not a regular file") ||
			(out.code != 0 && (out.code != 1 || !strings.Contains(out.stderr, "open memory"))) {
			t.Fatalf("read-only DB admission was confused with runtime health: %+v", out)
		}
	})
}

func TestRuntimeHTTPBusyPortDoesNotAnnounceListening(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "busy-port-project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	f.mustRun(project, "", "init", "--store-only")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	addr := listener.Addr().String()
	out := f.run(project, "", "--no-daemon", "mcp", "serve", "--no-create", "--http", addr, "--no-auth")
	if out.code != 1 || !strings.Contains(out.stderr, "bind") || strings.Contains(out.stdout, "listening on") {
		t.Fatalf("occupied port was announced as listening: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(store, "gray.db")); err != nil {
		t.Fatalf("accepted store did not reach runtime before bind failure: %v", err)
	}
}

func TestRuntimeDaemonRouteSurvivesRestartAndKeepsSharedNamespace(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "daemon-project")
	subdir := filepath.Join(project, "src")
	process := filepath.Join(f.root, "other-process")
	for _, dir := range []string{subdir, process} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	otherStore := filepath.Join(process, ".graymatter")
	f.mustRun(project, "", "init", "--store-only")
	env := map[string]string{"CLAUDE_PROJECT_DIR": project}
	m := f.startMCP(process, f.bin, []string{"mcp", "serve", "--no-create"}, env)
	m.add(3, "daemon-project", "daemon route survives first connection")
	packet := issue81HookPayload(t, subdir, "remember: hook and MCP use one daemon store")
	out := f.runCommand(f.bin, process, packet, env, "hooks", "run", "user-prompt", "--no-create")
	if out.code != 0 || !strings.Contains(out.stdout, "Saved to memory (daemon-project)") {
		t.Fatalf("daemon-backed hook route failed: %+v", out)
	}
	m.search(4, "daemon-project", "one daemon store", "hook and MCP use one daemon store")
	shared := issue81HookPayload(t, subdir, "remember shared: shared daemon route remains selected")
	out = f.runCommand(f.bin, process, shared, env, "hooks", "run", "user-prompt", "--no-create")
	if out.code != 0 || !strings.Contains(out.stdout, "Saved to shared memory") {
		t.Fatalf("daemon-backed shared hook route failed: %+v", out)
	}
	m.search(5, "__shared__", "shared daemon route", "shared daemon route remains selected")
	if out := f.run(process, "", "--dir", store, "daemon", "stop"); out.code != 0 {
		t.Fatalf("stop fixture daemon: %+v", out)
	}
	deadline := time.Now().Add(8 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		status := f.run(process, "", "--dir", store, "daemon", "status")
		if status.code == 0 && strings.Contains(status.stdout, "not running") {
			stopped = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("fixture daemon did not stop before reconnect check")
	}
	m.add(6, "daemon-project", "reconnected to the original selected store")
	m.search(7, "daemon-project", "reconnected original selected", "reconnected to the original selected store")
	m.close()
	if _, err := os.Lstat(otherStore); !os.IsNotExist(err) {
		t.Fatalf("daemon route created process-cwd store: %v", err)
	}
}

func TestRuntimeSessionEndDetachedChildUsesExplicitStore(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "detached-project")
	subdir := filepath.Join(project, "src")
	process := filepath.Join(f.root, "detached-process")
	for _, dir := range []string{subdir, process} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := f.registerStore(filepath.Join(f.root, "explicit-detached-store"))
	agent := "detached-project"
	const decayedFact = "ancient route-specific fact awaiting detached prune"
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = store
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.VectorReconcileInterval = 0
	cfg.AsyncConsolidate = false
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Remember(context.Background(), agent, decayedFact); err != nil {
		_ = mem.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	markerID := ageDetachedPruneFact(t, mem.Advanced(), agent, decayedFact)
	if err := mem.Close(); err != nil {
		t.Fatal(err)
	}

	// Start the daemon without touching the fact and verify that the
	// selected store contains the marker before running the hook.
	f.mustRun(process, "", "--dir", store, "checkpoint", "list", agent)
	client, err := daemon.ConnectNoSpawn(store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if !detachedPruneFactExists(t, client, agent, markerID) {
		t.Fatal("prune fixture fact missing before SessionEnd")
	}
	payload, err := json.Marshal(map[string]string{"cwd": subdir, "session_id": "detached-routing-session"})
	if err != nil {
		t.Fatal(err)
	}
	out := f.runCommand(f.bin, process, string(payload), map[string]string{"CLAUDE_PROJECT_DIR": project},
		"--dir", store, "hooks", "run", "session-end", "--no-create")
	if out.code != 0 || out.stdout != "" {
		t.Fatalf("explicit SessionEnd hook failed: %+v", out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !detachedPruneFactExists(t, client, agent, markerID) {
			for _, wrong := range []string{filepath.Join(project, ".graymatter"), filepath.Join(process, ".graymatter")} {
				if _, err := os.Lstat(wrong); !os.IsNotExist(err) {
					t.Fatalf("detached route created wrong store %s: %v", wrong, err)
				}
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	logData, _ := os.ReadFile(filepath.Join(store, "hooks.log"))
	t.Fatalf("detached consolidate did not prune fact in selected store; hooks.log=%q", logData)
}
