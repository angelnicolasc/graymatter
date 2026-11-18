package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type persistedReportConfig struct {
	rpcTestConfig
	url string
}

func (c persistedReportConfig) GetConsolidateLLM() string    { return "ollama" }
func (c persistedReportConfig) GetConsolidateThreshold() int { return 1 }
func (c persistedReportConfig) GetOllamaURL() string         { return c.url }

type persistedReportServer struct {
	*Server
	calls       atomic.Int32
	committed   chan ConsolidateWithReportResponse
	drop        net.Conn
	waitForExit <-chan struct{}
}

func (s *persistedReportServer) ConsolidateWithReport(req *ConsolidateWithReportRequest, resp *ConsolidateWithReportResponse) error {
	s.calls.Add(1)
	if err := s.Server.ConsolidateWithReport(req, resp); err != nil {
		return err
	}
	s.committed <- *resp
	if s.waitForExit != nil {
		<-s.waitForExit
	}
	return s.drop.Close()
}

func TestReportResponseLostAfterPersistedCommit(t *testing.T) {
	const agent = "audit"
	const summary = "Synthetic summary from the real store behind RPC"
	for _, name := range []string{"disconnect", "cancel"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := memory.Open(memory.StoreConfig{DataDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			first, err := store.PutReturningFact(context.Background(), agent, "Synthetic first input")
			if err != nil {
				t.Fatal(err)
			}
			second, err := store.PutReturningFact(context.Background(), agent, "Synthetic second input")
			if err != nil {
				t.Fatal(err)
			}
			var providerRequests atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerRequests.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || r.URL.Path != "/api/generate" {
					t.Errorf("unexpected provider request: %s %s (%v)", r.Method, r.URL.Path, err)
				}
				source := first.ID
				if !strings.Contains(string(body), source) {
					source = second.ID
				}
				if !strings.Contains(string(body), source) {
					t.Error("provider request did not contain a seeded source")
				}
				proposal, _ := json.Marshal(map[string]any{"summary": summary, "consumes": []string{source}})
				_ = json.NewEncoder(w).Encode(map[string]string{"response": string(proposal)})
			}))
			t.Cleanup(provider.Close)
			server := &persistedReportServer{
				Server:    NewServer(store, persistedReportConfig{url: provider.URL}),
				committed: make(chan ConsolidateWithReportResponse, 2),
			}
			client, transport := reportPipeConnection(t, server)
			server.drop = transport
			if name == "cancel" {
				release := make(chan struct{})
				server.waitForExit = release
				t.Cleanup(func() { close(release) })
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out := make(chan error, 1)
			go func() {
				result, err := client.ConsolidateWithReport(ctx, agent)
				if result != (memory.ConsolidationReport{}) {
					t.Errorf("client returned a report it did not receive: %+v", result)
				}
				out <- err
			}()
			var committed memory.ConsolidationReport
			select {
			case outcome := <-server.committed:
				if outcome.Fatal != nil || outcome.Report.Status != memory.ConsolidationApplied || outcome.Report.SourcesRetired != 1 {
					t.Fatalf("server did not confirm the expected mutation: %+v", outcome)
				}
				committed = outcome.Report
			case <-ctx.Done():
				t.Fatal("server did not reach the response boundary")
			}
			if name == "cancel" {
				cancel()
			}
			select {
			case err := <-out:
				var typed *memory.ConsolidationError
				if !errors.Is(err, memory.ErrMutationOutcomeUnknown) || !errors.As(err, &typed) || typed.Diagnostic().Code != "outcome_unknown" {
					t.Fatalf("lost response execution returned %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("report call did not return")
			}
			if server.calls.Load() != 1 || providerRequests.Load() != 1 {
				t.Fatalf("unexpected mutation calls=%d provider requests=%d", server.calls.Load(), providerRequests.Load())
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := memory.Open(memory.StoreConfig{DataDir: dir, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			facts, err := reopened.List(agent)
			if err != nil {
				t.Fatal(err)
			}
			summaries, retired := 0, 0
			for _, f := range facts {
				if f.ID == committed.SummaryFactID && f.Text == summary && !f.IsSuperseded() {
					summaries++
				}
				if (f.ID == first.ID || f.ID == second.ID) && f.SupersededBy == committed.SummaryFactID {
					retired++
				}
			}
			if len(facts) != 3 || summaries != 1 || retired != 1 {
				t.Fatalf("reopened store differs from server result %+v: %+v", committed, facts)
			}
		})
	}
}
