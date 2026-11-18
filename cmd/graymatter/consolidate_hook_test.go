package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
)

func TestConsolidationFailureDoesNotBlockSessionEndHook(t *testing.T) {
	binary := buildE2EBinary(t)
	root := t.TempDir()
	const agent = "reporting-hook"
	project := filepath.Join(root, agent)
	store := filepath.Join(project, ".graymatter")
	const pruneMarker = "Synthetic fact to prune after provider rejection"
	seedConsolidationHookStore(t, store, agent, pruneMarker)

	started, release := make(chan struct{}), make(chan struct{})
	var hitCount atomic.Int32
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/hook/v1/messages" || r.Header.Get("x-api-key") != "fake-hook-key" {
			t.Errorf("unexpected hook summary request: %s %s", r.Method, r.URL.Path)
		}
		if hitCount.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"synthetic rejection"}}`)
	}))
	t.Cleanup(fixture.Close)
	env := append(providerURLCLIEnv(root), "ANTHROPIC_API_KEY=fake-hook-key", "ANTHROPIC_BASE_URL="+fixture.URL+"/hook")
	proc := exec.Command(binary, "--dir", store, "daemon", "run", "--idle-exit=0")
	proc.Dir, proc.Env = project, env
	log, err := os.Create(filepath.Join(root, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	proc.Stdout, proc.Stderr = log, log
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	t.Cleanup(func() {
		unblock() // fixture must release before daemon waits for active work
		if daemon.ReadPIDFile(store) == proc.Process.Pid {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stop := exec.CommandContext(ctx, binary, "--dir", store, "daemon", "stop")
			stop.Dir, stop.Env = project, env
			if out, err := stop.CombinedOutput(); err != nil {
				t.Errorf("stop owned daemon: %v: %s", err, out)
			}
		} else {
			_ = proc.Process.Kill()
		}
		select {
		case err := <-done:
			if err != nil {
				data, _ := os.ReadFile(log.Name())
				t.Errorf("owned daemon exit: %v: %s", err, data)
			}
		case <-time.After(5 * time.Second):
			_ = proc.Process.Kill()
			<-done
			t.Error("owned daemon failed to terminate normally")
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for daemon.ReadPIDFile(store) != proc.Process.Pid && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if daemon.ReadPIDFile(store) != proc.Process.Pid {
		t.Fatal("owned daemon did not become ready")
	}
	client, err := daemon.ConnectNoSpawn(store)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The HTTP response is held until after the parent has actually exited.
	// A synchronous implementation cannot pass, even on a fast machine.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hook := exec.CommandContext(ctx, binary, "hooks", "run", "session-end")
	hook.Dir, hook.Env, hook.Stdin = project, env, strings.NewReader(hookStdin(project, ""))
	if out, err := hook.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("hook failed or waited for child HTTP: %v: %q", err, out)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("detached child did not reach summary provider")
	}
	checkpoint, err := client.CheckpointResume(agent)
	if err != nil || checkpoint == nil || checkpoint.State["event"] != "session-end" {
		t.Fatalf("session-end checkpoint = %+v, %v", checkpoint, err)
	}
	findMarker := func() (bool, int) {
		t.Helper()
		facts, err := client.List(agent)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range facts {
			if f.Text == pruneMarker {
				return true, len(facts)
			}
		}
		return false, len(facts)
	}
	if found, count := findMarker(); !found || count != 22 {
		t.Fatalf("unexpected facts before provider response: marker=%v count=%d", found, count)
	}
	unblock()
	deadline = time.Now().Add(10 * time.Second)
	for {
		found, count := findMarker()
		if !found {
			if count != 21 || hitCount.Load() != 1 {
				t.Fatalf("provider failure changed source facts or replayed the summary: count=%d requests=%d", count, hitCount.Load())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached child did not finish the prune phase after provider rejection")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func seedConsolidationHookStore(t *testing.T, dir, agent, marker string) {
	t.Helper()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir, cfg.EmbeddingMode, cfg.AsyncConsolidate = dir, graymatter.EmbeddingKeyword, false
	cfg.AnthropicAPIKey, cfg.ConsolidateLLM = "", ""
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	adv := mem.Advanced()
	for i := range 21 {
		if err := mem.Remember(context.Background(), agent, fmt.Sprintf("Synthetic live hook fact %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mem.Remember(context.Background(), agent, marker); err != nil {
		t.Fatal(err)
	}
	facts, err := adv.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f.Text == marker {
			f.Weight = 0.005
			if err := adv.UpdateFact(agent, f); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("prune marker was not seeded")
}
