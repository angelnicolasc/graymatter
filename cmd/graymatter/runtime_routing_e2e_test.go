package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
)

func TestRuntimeRoutingHookAndMCPUseOneProjectStore(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "logical-project")
	subdir := filepath.Join(project, "src")
	process := filepath.Join(f.root, "different-process-cwd")
	for _, dir := range []string{subdir, process} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	projectStore := f.registerStore(filepath.Join(project, ".graymatter"))
	processStore := f.registerStore(filepath.Join(process, ".graymatter"))
	f.mustRun(project, "", "init", "--store-only")
	packet, err := json.Marshal(map[string]string{
		"cwd": subdir, "session_id": "routing-e2e", "prompt": "remember: routing stays in project store",
	})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CLAUDE_PROJECT_DIR": project}
	out := f.runCommand(f.bin, process, string(packet), env, "--no-daemon", "hooks", "run", "user-prompt")
	if out.code != 0 || !strings.Contains(out.stdout, "Saved to memory (logical-project)") {
		t.Fatalf("hook routed incorrectly: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(projectStore, "gray.db")); err != nil {
		t.Fatalf("hook did not use project store: %v", err)
	}
	if _, err := os.Lstat(processStore); !os.IsNotExist(err) {
		t.Fatalf("hook wrote process-cwd store: %v", err)
	}
	m := f.startMCP(process, f.bin, []string{"--no-daemon", "mcp", "serve"}, env)
	m.search(3, "logical-project", "routing stays", "routing stays in project store")
	m.close()

	logPath := filepath.Join(projectStore, "hooks.log")
	before := issue81SnapshotFile(t, logPath)
	packet, err = json.Marshal(map[string]string{"cwd": process, "prompt": "remember: should be rejected"})
	if err != nil {
		t.Fatal(err)
	}
	out = f.runCommand(f.bin, process, string(packet), env, "--no-daemon", "hooks", "run", "user-prompt")
	if out.code != 0 || out.stdout != "" || !strings.Contains(out.stderr, "invalid or conflicting project route") {
		t.Fatalf("cross-root hook was not rejected fail-soft: %+v", out)
	}
	issue81AssertFileUnchanged(t, logPath, before)
	if _, err := os.Lstat(processStore); !os.IsNotExist(err) {
		t.Fatalf("rejected hook wrote process-cwd store: %v", err)
	}
	for _, invalidPayload := range []string{
		`{"cwd":42,"prompt":"remember: invalid cwd type must not write"}`,
		`{"cwd":null,"prompt":"remember: null cwd must not write"}`,
		`{"CWD":null,"prompt":"remember: case-variant null cwd must not write"}`,
		`{"cwd":"","cwd":"","prompt":"remember: duplicate cwd must not write"}`,
		`{"cwd":"","CWD":"","prompt":"remember: ambiguous cwd casing must not write"}`,
		`{"cwd":"` + process + `","prompt":"remember: malformed JSON"`,
	} {
		out = f.runCommand(f.bin, process, invalidPayload, env,
			"--no-daemon", "hooks", "run", "user-prompt")
		if out.code != 0 || out.stdout != "" || !strings.Contains(out.stderr, "invalid event payload") {
			t.Fatalf("invalid hook payload was not rejected: %+v", out)
		}
		issue81AssertFileUnchanged(t, logPath, before)
		if _, err := os.Lstat(processStore); !os.IsNotExist(err) {
			t.Fatalf("invalid hook payload wrote process-cwd store: %v", err)
		}
	}

	if err := os.Mkdir(processStore, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(processStore, "MEMORY.md"), []byte("legacy store"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = f.runCommand(f.bin, process, "", env, "mcp", "serve")
	if out.code != 1 || out.stdout != "" || !strings.Contains(out.stderr, "pass --dir explicitly") {
		t.Fatalf("legacy ambiguous MCP route: %+v", out)
	}
	if _, err := os.Lstat(filepath.Join(processStore, "gray.db")); !os.IsNotExist(err) {
		t.Fatalf("legacy conflict opened old DB: %v", err)
	}

	packet, err = json.Marshal(map[string]string{
		"cwd": subdir, "prompt": "remember: explicit store remains explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	out = f.runCommand(f.bin, process, string(packet), env,
		"--no-daemon", "--dir", processStore, "hooks", "run", "user-prompt")
	if out.code != 0 || !strings.Contains(out.stdout, "Saved to memory (logical-project)") {
		t.Fatalf("explicit hook route: %+v", out)
	}
	explicitMCP := f.startMCP(process, f.bin,
		[]string{"--no-daemon", "--dir", processStore, "mcp", "serve"}, env)
	explicitMCP.search(3, "logical-project", "explicit store", "explicit store remains explicit")
	explicitMCP.close()
	checkpointPayload, err := json.Marshal(map[string]string{
		"cwd": subdir, "session_id": "explicit-store-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	out = f.runCommand(f.bin, process, string(checkpointPayload), env,
		"--no-daemon", "--dir", processStore, "hooks", "run", "pre-compact")
	if out.code != 0 || out.stdout != "" {
		t.Fatalf("explicit checkpoint hook: %+v", out)
	}
	logBytes, err := os.ReadFile(filepath.Join(processStore, "hooks.log"))
	if err != nil || !strings.Contains(string(logBytes), `"event":"pre-compact"`) {
		t.Fatalf("explicit store did not receive hook receipt: %v %q", err, logBytes)
	}
	listed := f.mustRun(process, "", "--no-daemon", "--dir", processStore,
		"--json", "checkpoint", "list", "logical-project")
	var checkpoints []session.Checkpoint
	if err := json.Unmarshal([]byte(listed.stdout), &checkpoints); err != nil {
		t.Fatalf("decode explicit-store checkpoints: %v; output=%q", err, listed.stdout)
	}
	if len(checkpoints) != 1 || checkpoints[0].ID == "" || checkpoints[0].AgentID != "logical-project" {
		t.Fatalf("expected one checkpoint in the explicit store: %q", listed.stdout)
	}
	loaded := f.mustRun(process, "", "--no-daemon", "--dir", processStore,
		"checkpoint", "resume", "logical-project", checkpoints[0].ID)
	var marker session.Checkpoint
	if err := json.Unmarshal([]byte(loaded.stdout), &marker); err != nil {
		t.Fatalf("decode explicit-store hook marker: %v; output=%q", err, loaded.stdout)
	}
	if marker.ID != checkpoints[0].ID || marker.State["session_id"] != "explicit-store-session" ||
		marker.State["event"] != "pre-compact" || marker.State["source"] != "hooks" {
		t.Fatalf("explicit store did not receive checkpoint: %q", loaded.stdout)
	}
	latest := f.run(process, "", "--no-daemon", "--dir", processStore,
		"checkpoint", "resume", "logical-project")
	if latest.code != 1 || latest.stdout != "" || !strings.Contains(latest.stderr, `no checkpoints for agent "logical-project"`) {
		t.Fatalf("resume latest did not skip the hook marker: %+v", latest)
	}

	// A literal relative --dir is explicit and remains relative to the
	// process cwd, even when Claude's project root is elsewhere.
	projectDBBefore := issue81SnapshotFile(t, filepath.Join(projectStore, "gray.db"))
	projectLogBefore := issue81SnapshotFile(t, filepath.Join(projectStore, "hooks.log"))
	packet, err = json.Marshal(map[string]string{
		"cwd": subdir, "prompt": "remember: literal relative store stays selected",
	})
	if err != nil {
		t.Fatal(err)
	}
	out = f.runCommand(f.bin, process, string(packet), env,
		"--no-daemon", "--dir", ".graymatter", "hooks", "run", "user-prompt")
	if out.code != 0 || !strings.Contains(out.stdout, "Saved to memory (logical-project)") {
		t.Fatalf("literal relative hook route: %+v", out)
	}
	relativeMCP := f.startMCP(process, f.bin,
		[]string{"--no-daemon", "--dir", ".graymatter", "mcp", "serve"}, env)
	relativeMCP.search(3, "logical-project", "literal relative store", "literal relative store stays selected")
	relativeMCP.close()
	issue81AssertFileUnchanged(t, filepath.Join(projectStore, "gray.db"), projectDBBefore)
	issue81AssertFileUnchanged(t, filepath.Join(projectStore, "hooks.log"), projectLogBefore)
}
