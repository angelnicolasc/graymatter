package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/server"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/spf13/cobra"
)

// This fixture answers only the two summariser routes. CLI embedding auto
// detection receives 404 from /api/tags and falls back to the keyword backend.
func c0ProviderFixture(t *testing.T, falseSummary string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages":
			requests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "msg_fixture", "type": "message", "role": "assistant", "model": "fixture",
				"content":     []map[string]string{{"type": "text", "text": falseSummary}},
				"stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1},
			})
		case "/api/generate":
			requests.Add(1)
			proposal, _ := json.Marshal(map[string]any{"summary": falseSummary, "consumes": []string{"invented-id"}})
			_ = json.NewEncoder(w).Encode(map[string]string{"response": string(proposal)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.Close)
	t.Setenv("ANTHROPIC_API_KEY", "fixture-key")
	t.Setenv("ANTHROPIC_BASE_URL", fixture.URL)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("GRAYMATTER_OLLAMA_URL", fixture.URL)
	t.Setenv("GRAYMATTER_KG", "0")
	return fixture, &requests
}

func runC0Command(t *testing.T, cmd *cobra.Command, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%s %v: %v\n%s", cmd.Name(), args, err, out.String())
	}
	return out.String()
}

func TestC0CLIAndContextPreserveAdversarialSources(t *testing.T) {
	t.Setenv("GRAYMATTER_NO_DAEMON", "1")
	dir := t.TempDir()
	oldDir, oldQuiet := dataDir, quiet
	dataDir, quiet = filepath.Join(dir, "store"), false
	t.Cleanup(func() { dataDir, quiet = oldDir, oldQuiet })

	help := runC0Command(t, consolidateCmd(), "--help")
	if !strings.Contains(help, "Generative compaction is disabled") || !strings.Contains(help, "pruning") {
		t.Errorf("maintenance help is incomplete:\n%s", help)
	}

	// These are the ten adversarial source/summary pairs that passed the old
	// coverage rule. Each projection contains its exact source before and
	// after invoking the real CLI command with Anthropic configured.
	for _, pair := range []struct {
		name, source, summary string
	}{
		{"negation", "Backups are encrypted", "Backups are not encrypted"},
		{"swapped_values", "Production timeout is 30 seconds and staging timeout is 90 seconds", "Production timeout is 90 seconds and staging timeout is 30 seconds"},
		{"wrong_unit", "Backup retention is 45 days", "Backup retention is 45 hours"},
		{"numeric_substring", "Backup retention is 45 days", "Backup retention is 145 days"},
		{"version_substring", "Use service version 1.2", "Use service version 1.20"},
		{"path_substring", "Backups directory is /data/prod", "Backups directory is /data/production"},
		{"and_to_or", "Admin access requires MFA and VPN", "Admin access requires MFA or VPN"},
		{"missing_never", "Never deploy untested builds", "Deploy untested builds"},
		{"swapped_names", "Alice approves Bob deployments", "Bob approves Alice deployments"},
		{"cjk_with_digits", "备份保留45天", "45"},
	} {
		t.Run(pair.name, func(t *testing.T) {
			_, requests := c0ProviderFixture(t, pair.summary)
			seedC0CLIProjection(t, pair.name, pair.source)
			contextArgs := []string{"--dry-run", "--agent", pair.name, "--budget", "1024", "--file", filepath.Join(dir, "AGENTS.md")}
			assertProjection := func(block string) {
				t.Helper()
				lines := strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n")
				if !slices.Contains(lines, "- "+pair.source) || slices.Contains(lines, "- "+pair.summary) {
					t.Errorf("context does not preserve the exact source and exclude the false summary:\n%s", block)
				}
			}
			assertProjection(runC0Command(t, contextSyncCmd(), contextArgs...))
			out := runC0Command(t, consolidateCmd(), pair.name)
			if !strings.Contains(out, "Maintenance complete") || !strings.Contains(out, "Generative compaction is disabled") {
				t.Errorf("consolidate output = %q", out)
			}
			assertProjection(runC0Command(t, contextSyncCmd(), contextArgs...))
			if got := requests.Load(); got != 0 {
				t.Errorf("automatic summariser requests = %d, want zero", got)
			}
		})
	}
}

func seedC0CLIProjection(t *testing.T, agent, text string) {
	t.Helper()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dataDir
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	store := mem.Advanced()
	for i := 0; i < cfg.ConsolidateThreshold; i++ {
		f := putC0FixtureFact(t, store, agent, fmt.Sprintf("Pinned control %02d", i))
		f.Pinned, f.PinnedAt = true, time.Now().UTC()
		if err := store.UpdateFact(agent, f); err != nil {
			t.Fatal(err)
		}
	}
	putC0FixtureFact(t, store, agent, "Unpinned control")
	f := putC0FixtureFact(t, store, agent, text)
	f.Weight = 0.7
	if err := store.UpdateFact(agent, f); err != nil {
		t.Fatal(err)
	}
}

func putC0FixtureFact(t *testing.T, store graymatter.AdvancedStore, agent, text string) memory.Fact {
	t.Helper()
	putter := store.(interface {
		PutReturningFact(context.Context, string, string) (memory.Fact, error)
	})
	fact, err := putter.PutReturningFact(t.Context(), agent, text)
	if err != nil {
		t.Fatal(err)
	}
	return fact
}

func openC0ProductStore(t *testing.T, provider string) (*directStore, *atomic.Int64) {
	t.Helper()
	fixture, requests := c0ProviderFixture(t, "Backups are not encrypted")
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.ConsolidateLLM = provider
	cfg.ConsolidateThreshold = 2
	cfg.OllamaURL = fixture.URL
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	return &directStore{mem: mem, store: mem.Advanced()}, requests
}

func TestC0HTTPConsolidateRunsMaintenanceOnly(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			st, requests := openC0ProductStore(t, provider)
			const agent = "rest-fixture"
			for _, text := range []string{"Backups are encrypted", "Preserved peer", "Another peer"} {
				putC0FixtureFact(t, st.store, agent, text)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			const token = "fixture-token"
			srv := server.New(ln.Addr().String(), st, nil, server.WithAuthToken(token))
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Shutdown(context.Background()); _ = ln.Close() })

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+ln.Addr().String()+"/consolidate", strings.NewReader(`{"agent":"rest-fixture"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			client := http.Client{Timeout: 10 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var result map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || resp.StatusCode != http.StatusOK || result["status"] != "ok" {
				t.Fatalf("POST /consolidate: status=%d response=%v err=%v", resp.StatusCode, result, err)
			}
			assertC0Preserved(t, st, agent, 3, requests)
		})
	}
}

func TestC0DemoDashboardReportsMaintenanceOnly(t *testing.T) {
	store, err := openDemoStore(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := storeMaintenanceMode(store); got != daemon.MaintenanceOnly {
		t.Fatalf("demo maintenance mode = %q, want %q", got, daemon.MaintenanceOnly)
	}
}

func TestC0TUIAddSchedulesMaintenanceOnly(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			st, requests := openC0ProductStore(t, provider)
			const agent = "workbench-fixture"
			putC0FixtureFact(t, st.store, agent, "Backups are encrypted")
			old := putC0FixtureFact(t, st.store, agent, "Stale unpinned control")
			old.AccessedAt = time.Now().UTC().Add(-201 * 24 * time.Hour)
			if err := st.store.UpdateFact(agent, old); err != nil {
				t.Fatal(err)
			}

			m := newTUIModel(st, st.mem.Config().DataDir, false, "dark", false)
			m.namespace = agent
			m.updateMemoryKey(keyMsg('a'))
			if m.modal != "add" {
				t.Fatalf("workbench Add did not open: %q", m.modal)
			}
			m.editor.SetValue("User-authored workbench addition")
			_, save := m.handleModalKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
			if save == nil {
				t.Fatal("workbench Add returned no save command")
			}
			if msg := save().(mutationMsg); msg.err != nil {
				t.Fatalf("workbench Add: %v", msg.err)
			}

			// Pruning the stale row witnesses the async maintenance scheduled
			// by the real workbench write. The historical summariser ran before
			// that prune, so this also gives a deterministic request barrier.
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			for {
				rows, err := st.List(agent)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.ContainsFunc(rows, func(f memory.Fact) bool { return f.ID == old.ID }) {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("workbench Add did not prune the stale row")
				case <-tick.C:
				}
			}
			assertC0Preserved(t, st, agent, 2, requests)
			dashboard := m.loadDashboard()()
			model, _ := m.Update(dashboard)
			if got := model.(tuiModel).statusContent(); !strings.Contains(got, "generative compaction disabled") {
				t.Errorf("workbench status omitted effective maintenance mode:\n%s", got)
			}
		})
	}
}

func assertC0Preserved(t *testing.T, st *directStore, agent string, count int, requests *atomic.Int64) {
	t.Helper()
	if got := requests.Load(); got != 0 {
		t.Errorf("summariser requests = %d, want zero", got)
	}
	rows, err := st.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != count || !slices.ContainsFunc(rows, func(f memory.Fact) bool {
		return f.Text == "Backups are encrypted" && !f.IsSuperseded()
	}) {
		t.Errorf("wanted %d retained rows and a live original, got %+v", count, rows)
	}
	for _, f := range rows {
		if f.IsSuperseded() || f.Text == "Backups are not encrypted" {
			t.Errorf("unexpected row after maintenance: %+v", f)
		}
	}
	cycles, consumed := memory.ReadConsolidationCounters(st.store.DB())
	if cycles != 0 || consumed != 0 {
		t.Errorf("fresh historical counters = (%d, %d), want (0, 0)", cycles, consumed)
	}
}

func TestC0CLIThroughDaemonRunsMaintenanceOnly(t *testing.T) {
	const agent = "daemon-fixture"
	const source = "Backups are encrypted"
	const summary = "Backups are not encrypted"
	_, requests := c0ProviderFixture(t, summary)
	t.Setenv("GRAYMATTER_NO_DAEMON", "0")
	oldDir, oldNoDaemon, oldQuiet := dataDir, noDaemon, quiet
	dataDir, noDaemon, quiet = t.TempDir(), false, false
	t.Cleanup(func() { dataDir, noDaemon, quiet = oldDir, oldNoDaemon, oldQuiet })
	seedC0CLIProjection(t, agent, source)

	ready := make(chan struct{})
	done := make(chan error, 1)
	dir := dataDir
	go func() {
		done <- daemon.Run(daemon.RunOptions{
			DataDir: dir,
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
		t.Fatalf("daemon exited before ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not become ready")
	}

	client, err := daemon.ConnectNoSpawn(dir)
	if err != nil {
		t.Fatalf("connect daemon: %v", err)
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

	out := runC0Command(t, consolidateCmd(), agent)
	if !strings.Contains(out, "Maintenance complete") || !strings.Contains(out, "Generative compaction is disabled") {
		t.Errorf("daemon consolidate output = %q", out)
	}
	facts, err := client.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(facts, func(f memory.Fact) bool { return f.Text == source && !f.IsSuperseded() }) ||
		slices.ContainsFunc(facts, func(f memory.Fact) bool { return f.Text == summary }) {
		t.Error("daemon did not preserve the original and exclude the false summary")
	}
	overview, err := client.StoreOverview()
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 || overview.Consolidations != 0 || overview.FactsConsumed != 0 ||
		overview.MaintenanceMode != daemon.MaintenanceOnly {
		t.Errorf("requests = %d, store mode = %q, historical counters = (%d, %d)",
			requests.Load(), overview.MaintenanceMode, overview.Consolidations, overview.FactsConsumed)
	}
}
