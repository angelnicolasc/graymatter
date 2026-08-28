package main

import (
	"bufio"
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

	graymatter "github.com/angelnicolasc/graymatter"
)

// The daemon-mode end-to-end: every other hooks test runs against the direct
// store. Production is the daemon — clients spawn it, share it, and survive
// its lifecycle — so this suite exercises the real binary, a real daemon, and
// real subprocess hooks exactly the way Claude Code fires them:
//
//	session-start   → injects the freshest facts
//	user-prompt     → injects a recall; remember: saves deterministically
//	pre-compact     → checkpoints silently
//	session-end     → checkpoints AND spawns a detached consolidate whose
//	                  effect is observable: a 0.005-weight fact is pruned
//
// plus `hooks doctor` green, and memory_search(explain) over real MCP stdio.

func buildE2EBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("spawns the real binary and daemon; skipped in -short")
	}
	bin := filepath.Join(t.TempDir(), "graymatter-e2e.exe")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/angelnicolasc/graymatter/cmd/graymatter").CombinedOutput()
	if err != nil {
		t.Fatalf("build e2e binary: %v: %s", err, out)
	}
	return bin
}

func runE2E(t *testing.T, bin, dir string, stdinJSON string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdinJSON)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v: %s", args, err, out.String())
	}
	return out.String(), code
}

func hookStdin(dir, prompt string) string {
	if prompt != "" {
		return fmt.Sprintf(`{"session_id":"e2e","cwd":%q,"hook_event_name":"UserPromptSubmit","prompt":%q}`, dir, prompt)
	}
	return fmt.Sprintf(`{"session_id":"e2e","cwd":%q,"hook_event_name":"SessionEnd"}`, dir)
}

func TestHooksDaemonE2E_FullLifecycle(t *testing.T) {
	bin := buildE2EBinary(t)

	root := t.TempDir()
	proj := filepath.Join(root, "proj-e2e") // basename → agent id "proj-e2e"
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(proj, ".graymatter")

	// Seed through the library, then close: the daemon must own the store
	// from here on. The prune marker (weight 0.005 < the 0.01 floor) gives
	// the detached consolidate child an observable effect.
	func() {
		cfg := graymatter.DefaultConfig()
		cfg.DataDir = storeDir
		cfg.VectorReconcileInterval = 0
		cfg.AsyncConsolidate = false
		mem, err := graymatter.NewWithConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mem.Close() }()
		ctx := context.Background()
		agent := "proj-e2e"
		for _, f := range []string{
			"the release checklist lives in docs/release.md",
			"staging redeploys every night at 02:00 utc",
		} {
			if err := mem.Remember(ctx, agent, f); err != nil {
				t.Fatal(err)
			}
		}
		// The shared namespace must ride the same injection path through the
		// daemon: one project-wide convention seeded before any hook fires.
		if err := mem.RememberShared(ctx, "production deploys freeze on fridays; wait until monday"); err != nil {
			t.Fatal(err)
		}
		if err := mem.Remember(ctx, agent, "ancient fact waiting to be pruned"); err != nil {
			t.Fatal(err)
		}
		adv := mem.Advanced()
		if adv == nil {
			t.Fatal("store not initialised")
		}
		facts, err := adv.List(agent)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range facts {
			if f.Text == "ancient fact waiting to be pruned" {
				f.Weight = 0.005
				if err := adv.UpdateFact(agent, f); err != nil {
					t.Fatal(err)
				}
			}
		}
	}()

	defer func() {
		// The daemon spawned from the E2E binary locks it on Windows; stop it
		// so the temp root is releasable.
		stop := exec.Command(bin, "--dir", storeDir, "daemon", "stop")
		stop.Stdout = os.Stderr
		_ = stop.Run()
		time.Sleep(500 * time.Millisecond)
	}()

	// 1. install
	if out, code := runE2E(t, bin, proj, "", "hooks", "install"); code != 0 || !strings.Contains(out, "hooks installed") {
		t.Fatalf("hooks install: exit=%d out=%s", code, out)
	}

	// 2. session-start injects the agent's facts AND the shared convention
	out, code := runE2E(t, bin, proj, hookStdin(proj, ""), "hooks", "run", "session-start")
	if code != 0 || !strings.Contains(out, "## Memory") || !strings.Contains(out, "release checklist") {
		t.Fatalf("session-start: exit=%d out=%q", code, out)
	}
	if !strings.Contains(out, "## Shared memory (project-wide)") || !strings.Contains(out, "freeze on fridays") {
		t.Errorf("session-start lost the shared namespace: %q", out)
	}

	// 3. user-prompt recall injects
	out, code = runE2E(t, bin, proj, hookStdin(proj, "release checklist please"), "hooks", "run", "user-prompt")
	if code != 0 || !strings.Contains(out, "release checklist") {
		t.Fatalf("user-prompt recall: exit=%d out=%q", code, out)
	}

	// 4. user-prompt remember: saves
	out, code = runE2E(t, bin, proj, hookStdin(proj, "remember: the offcall rota lives in ops/rota.md"), "hooks", "run", "user-prompt")
	if code != 0 || !strings.Contains(out, "Saved to memory") {
		t.Fatalf("user-prompt remember: exit=%d out=%q", code, out)
	}

	// 4b. user-prompt remember shared: saves into the shared namespace, and
	// the fact is readable back over the daemon wire.
	out, code = runE2E(t, bin, proj, hookStdin(proj, "remember shared: the weekly demo is thursdays at 15:00 utc"), "hooks", "run", "user-prompt")
	if code != 0 || !strings.Contains(out, "Saved to shared memory") {
		t.Fatalf("user-prompt remember shared: exit=%d out=%q", code, out)
	}
	out, code = runE2E(t, bin, proj, "", "--dir", storeDir, "recall", "proj-e2e", "weekly demo", "--shared")
	if code != 0 || !strings.Contains(out, "weekly demo is thursdays") {
		t.Errorf("remember shared: fact not readable via recall --shared (exit=%d): %q", code, out)
	}

	// 5. pre-compact checkpoints silently
	out, code = runE2E(t, bin, proj, hookStdin(proj, ""), "hooks", "run", "pre-compact")
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("pre-compact: exit=%d out=%q (must be silent)", code, out)
	}

	// 6. session-end checkpoints and spawns consolidation detached
	start := time.Now()
	out, code = runE2E(t, bin, proj, hookStdin(proj, ""), "hooks", "run", "session-end")
	elapsed := time.Since(start)
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("session-end: exit=%d out=%q (must be silent)", code, out)
	}
	if elapsed > 5*time.Second {
		t.Errorf("session-end took %v; the detached design exists so it returns fast", elapsed)
	}

	// The consolidate child runs through the daemon and prunes the marker
	// fact — the empirical proof the detached spawn landed. Observe through
	// the daemon (a CLI recall), not by opening bbolt directly: on Windows
	// the daemon's write lock excludes readers, so a direct open would time
	// out and misreport a successful prune as a missing one. With
	// MinRelevance=0 a recall always returns top-k by recency — even for a
	// query matching nothing — so the check is the ABSENCE of the fact's
	// text, never the "No memories found" notice.
	prunedWithin := func() bool {
		out, code := runE2E(t, bin, proj, "", "--dir", storeDir, "recall", "proj-e2e", "ancient fact waiting to be pruned", "--top-k", "8", "--quiet")
		return code == 0 && !strings.Contains(out, "ancient fact waiting to be pruned")
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if prunedWithin() {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !prunedWithin() {
		// The hook degrades silently by contract; the failure receipt lives
		// in hooks.log. Dump it and the daemon log so the failure is
		// diagnosable from the test output alone.
		if logData, err := os.ReadFile(filepath.Join(storeDir, "hooks.log")); err == nil {
			t.Logf("hooks.log:\n%s", logData)
		} else {
			t.Logf("hooks.log unreadable: %v", err)
		}
		if daemonLog, err := os.ReadFile(filepath.Join(storeDir, "daemon.log")); err == nil {
			lines := strings.Split(strings.TrimRight(string(daemonLog), "\n"), "\n")
			if len(lines) > 15 {
				lines = lines[len(lines)-15:]
			}
			t.Logf("daemon.log (tail):\n%s", strings.Join(lines, "\n"))
		}
		out, _ := runE2E(t, bin, proj, "", "--dir", storeDir, "recall", "proj-e2e", "ancient fact", "--top-k", "5")
		t.Logf("recall still returns: %s", out)
		t.Error("the detached consolidate child never pruned the 0.005-weight fact — the spawn did not land")
	}

	// 7. both hook checkpoints exist
	list, _ := runE2E(t, bin, proj, "", "checkpoint", "list", "proj-e2e")
	if strings.Contains(list, "No checkpoints") {
		t.Errorf("no checkpoints after pre-compact + session-end:\n%s", list)
	}

	// 8. doctor green
	_, code = runE2E(t, bin, proj, "", "hooks", "doctor")
	if code != 0 {
		t.Errorf("hooks doctor must be green after the lifecycle (exit=%d)", code)
	}

	// 9. MCP explain over real stdio, daemon-backed
	out, code = runE2E(t, bin, proj,
		strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_search","arguments":{"agent_id":"proj-e2e","query":"release checklist","explain":true}}}`,
		}, "\n")+"\n",
		"mcp", "serve")
	if code != 0 {
		t.Fatalf("mcp serve: exit=%d out=%s", code, out)
	}
	var explainResult struct {
		Result struct {
			StructuredContent struct {
				Count     int `json:"count"`
				Explained []struct {
					Text  string `json:"text"`
					Ranks struct {
						RecencyRank int     `json:"recency_rank"`
						FusedScore  float64 `json:"fused_score"`
						K           float64 `json:"k"`
					} `json:"ranks"`
					Provenance struct {
						FactID string `json:"fact_id"`
					} `json:"provenance"`
				} `json:"explained"`
			} `json:"structuredContent"`
		} `json:"result"`
		ID int `json:"id"`
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	found := false
	for sc.Scan() {
		var line map[string]any
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if id, ok := line["id"].(float64); ok && int(id) == 2 {
			if err := json.Unmarshal(sc.Bytes(), &explainResult); err != nil {
				t.Fatalf("decode explain response: %v\n%s", err, sc.Text())
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no response for tools/call id=2:\n%s", out)
	}
	if explainResult.Result.StructuredContent.Count == 0 || len(explainResult.Result.StructuredContent.Explained) == 0 {
		t.Fatalf("daemon-backed explain returned no receipts:\n%s", out)
	}
	r0 := explainResult.Result.StructuredContent.Explained[0]
	if r0.Provenance.FactID == "" || r0.Ranks.K != 60 || r0.Ranks.RecencyRank <= 0 {
		t.Errorf("receipt over the daemon wire is incomplete: %+v", r0)
	}
}
