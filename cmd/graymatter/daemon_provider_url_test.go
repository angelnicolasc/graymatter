package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestProviderURLDaemonLifetime(t *testing.T) {
	bin := buildE2EBinary(t)
	root := t.TempDir()
	store := filepath.Join(root, "store")
	path := filepath.Join(root, "agent.md")
	if err := os.WriteFile(path, []byte(backgroundTestAgent), 0o600); err != nil {
		t.Fatal(err)
	}
	const fakeKey = "synthetic-daemon-key"
	var hitsA, hitsB atomic.Int32
	gateway := func(prefix string, hits *atomic.Int32) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Model string
				Input []string
			}
			err := json.NewDecoder(r.Body).Decode(&request)
			if err != nil || r.Method != http.MethodPost || r.URL.Path != prefix+"/v1/embeddings" ||
				r.Header.Get("Authorization") != "Bearer "+fakeKey || request.Model != "text-embedding-3-small" ||
				len(request.Input) != 1 {
				t.Errorf("unexpected embeddings request: %s %s %+v, %v", r.Method, r.URL.Path, request, err)
			}
			hits.Add(1)
			vector := make([]float32, 1536)
			vector[0] = 1
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": vector}}})
		}))
		t.Cleanup(server.Close)
		return server
	}
	a := gateway("/first", &hitsA)
	b := gateway("/second", &hitsB)
	env := func(base string) []string {
		return append(providerURLCLIEnv(root), "OPENAI_API_KEY="+fakeKey, "OPENAI_BASE_URL="+base)
	}

	cli := func(clientEnv []string, args ...string) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append([]string{"--dir", store}, args...)...)
		cmd.Env, cmd.Dir = clientEnv, root
		return cmd.CombinedOutput()
	}

	start := func(base string) (int, func()) {
		t.Helper()
		log, err := os.CreateTemp(root, "foreground-daemon-*.log")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, "--dir", store, "daemon", "run", "--idle-exit=0")
		cmd.Env, cmd.Dir = env(base), root
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			_ = log.Close()
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		stopped := false
		stop := func() {
			t.Helper()
			if stopped {
				return
			}
			stopped = true
			defer log.Close()
			if daemon.ReadPIDFile(store) == pid {
				if out, err := cli(env(base), "daemon", "stop"); err != nil {
					t.Errorf("daemon stop: %v: %s", err, out)
				}
			} else {
				_ = cmd.Process.Kill()
			}
			select {
			case err := <-done:
				if err != nil {
					details, _ := os.ReadFile(log.Name())
					t.Errorf("foreground daemon exited: %v: %s", err, details)
				}
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Error("foreground daemon did not terminate normally")
			}
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(5 * time.Second)
		for daemon.ReadPIDFile(store) != pid && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if daemon.ReadPIDFile(store) != pid {
			t.Fatalf("daemon startup did not publish owned PID %d", pid)
		}
		return pid, stop
	}

	baseA, baseB := a.URL+"/first/v1", b.URL+"/second/v1"
	pidA, stopA := start(baseA)
	remember := func(clientEnv []string, pid int, fact string, wantA, wantB int32) {
		t.Helper()
		if out, err := cli(clientEnv, "remember", "synthetic-agent", fact); err != nil {
			t.Fatalf("remember through daemon: %v: %s", err, out)
		}
		if owner := daemon.ReadPIDFile(store); owner != pid || hitsA.Load() != wantA || hitsB.Load() != wantB {
			t.Fatalf("owner A/B calls = %d/%d/%d; want %d/%d/%d", owner, hitsA.Load(), hitsB.Load(), pid, wantA, wantB)
		}
	}
	remember(env(baseA), pidA, "Synthetic daemon A fact", 1, 0)
	remember(env(baseB), pidA, "Synthetic warm daemon changed client fact", 2, 0)
	invalid := append(env("http://synthetic:EMBED-CANARY@127.0.0.1:0"),
		"ANTHROPIC_BASE_URL=http://synthetic:RUN-CANARY@127.0.0.1:0")
	remember(invalid, pidA, "Synthetic warm daemon invalid client fact", 3, 0)
	if out, err := cli(invalid, "run", path); err == nil || !strings.Contains(string(out), "provider=anthropic") ||
		strings.Contains(string(out), "RUN-CANARY") || hitsA.Load() != 3 || hitsB.Load() != 0 {
		t.Fatalf("run must reject its own invalid provider before persistence: %v: %s", err, out)
	}
	stopA()
	pidB, stopB := start(baseB)
	remember(env(baseA), pidB, "Synthetic restarted daemon B fact", 3, 1)
	stopB()
}

func TestDaemonInvalidProviderURLStartupDiagnostic(t *testing.T) {
	bin := buildE2EBinary(t)
	root := t.TempDir()
	store := filepath.Join(root, "store")
	const forbidden = "STARTUP-URL-CANARY"
	var probes atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/tags" {
			t.Errorf("startup attempted an inference request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fixture.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--dir", store, "remember", "synthetic", "synthetic startup fact")
	cmd.Dir = root
	cmd.Env = append(providerURLCLIEnv(root), "OPENAI_API_KEY=synthetic-key",
		"OPENAI_BASE_URL=http://dummy:"+forbidden+"@127.0.0.1:0", "GRAYMATTER_OLLAMA_URL="+fixture.URL)
	out, err := cmd.CombinedOutput()
	log, logErr := os.ReadFile(filepath.Join(store, "daemon.log"))
	wantLog := "code=invalid_provider_base_url provider=openai reason=userinfo"
	// Cobra's existing usage can push the structured log line out of the
	// fixed tail; the returned sanitized error still carries the diagnosis.
	wantTail := "invalid_provider_base_url: provider=openai field=OpenAIBaseURL reason=userinfo"
	if err == nil || ctx.Err() != nil || logErr != nil ||
		!strings.Contains(string(out), wantTail) || !strings.Contains(string(log), wantLog) ||
		strings.Contains(string(out), forbidden) || strings.Contains(string(log), forbidden) || probes.Load() != 1 {
		t.Fatalf("daemon startup = %v: %s; log = %v: %s; Ollama probes = %d", err, out, logErr, log, probes.Load())
	}
	if _, err := os.Stat(filepath.Join(store, "gray.db")); !os.IsNotExist(err) || daemon.ReadPIDFile(store) != 0 {
		t.Fatalf("rejected daemon acquired persistence or published a PID: gray.db stat = %v", err)
	}
}

func TestProviderURLDaemonOwnsExplicitReviewAndGraphExtraction(t *testing.T) {
	const (
		namespace = "synthetic-owner-project"
		key       = "synthetic-owner-key"
		entity    = "Owner fixture entity"
		summary   = "Owner fixture proposal"
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	store, err := memory.Open(memory.StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	fact, err := store.PutReturningFact(ctx, namespace, "The fixture stores a local synthetic fact.")
	if closeErr := store.Close(); err != nil || closeErr != nil {
		t.Fatalf("prepare store: write=%v close=%v", err, closeErr)
	}

	var extractionCalls, reviewCalls, wrongCalls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.EscapedPath() == "/api/tags" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/owner%2F/v1/messages" ||
			r.Header.Get("X-Api-Key") != key || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			wrongCalls.Add(1)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		var request struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode fixture request: %v", err)
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		var reply []byte
		switch request.MaxTokens {
		case 1024:
			extractionCalls.Add(1)
			reply, _ = json.Marshal(map[string]any{"nodes": []map[string]string{{"label": entity, "entity_type": "concept"}}, "edges": []any{}})
		case 2048:
			reviewCalls.Add(1)
			reply, _ = json.Marshal(map[string]any{"summary": summary, "consumes": []string{fact.ID}})
		default:
			wrongCalls.Add(1)
			http.Error(w, "unexpected fixture operation", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_fixture", "type": "message", "role": "assistant", "model": "fixture-model",
			"content":     []map[string]string{{"type": "text", "text": string(reply)}},
			"stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	t.Cleanup(gateway.Close)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", key)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", gateway.URL+"/owner%2F")
	t.Setenv("GRAYMATTER_KG_LLM_EXTRACT", "1")
	t.Setenv("GRAYMATTER_OLLAMA_URL", gateway.URL)

	ready, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- daemon.Run(daemon.RunOptions{
			DataDir: dir,
			KG:      true,
			Logf: func(format string, _ ...any) {
				if strings.HasPrefix(format, "graymatter daemon ready:") {
					close(ready)
				}
			},
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before startup: %v", err)
	case <-ctx.Done():
		t.Fatal("daemon did not become ready")
	}

	client, err := daemon.ConnectNoSpawn(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Shutdown()
		_ = client.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})

	// Change the caller environment only after the owner's config is fixed.
	t.Setenv("ANTHROPIC_BASE_URL", gateway.URL+"/wrong-client")
	t.Setenv("ANTHROPIC_API_KEY", "other-synthetic-key")
	if err := client.Consolidate(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	if extractionCalls.Load() != 1 || reviewCalls.Load() != 0 || wrongCalls.Load() != 0 {
		t.Fatalf("maintenance requests: graph=%d review=%d wrong=%d", extractionCalls.Load(), reviewCalls.Load(), wrongCalls.Load())
	}
	nodes, err := client.KGNodes()
	if err != nil || len(nodes) != 1 || nodes[0].Label != entity {
		t.Fatalf("owner extraction did not reach the graph: %+v, %v", nodes, err)
	}
	rows, err := client.ReviewAnalyze(ctx, namespace, "anthropic", "")
	if err != nil || len(rows) != 1 || rows[0].Kind != memory.ReviewKindSummary || reviewCalls.Load() != 1 || wrongCalls.Load() != 0 {
		t.Fatalf("owner review: results=%+v calls=%d wrong=%d err=%v", rows, reviewCalls.Load(), wrongCalls.Load(), err)
	}
	facts, err := client.List(namespace)
	if err != nil || len(facts) != 1 || facts[0].ID != fact.ID || facts[0].IsSuperseded() {
		t.Fatalf("review changed active facts: %+v, %v", facts, err)
	}
}
