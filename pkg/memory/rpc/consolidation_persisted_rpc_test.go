package rpc

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
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
func (c persistedReportConfig) GetDecayHalfLife() time.Duration {
	return 30 * 24 * time.Hour
}

type persistedReportServer struct {
	*Server
	calls       atomic.Int32
	legacy      atomic.Int32
	committed   chan ConsolidateWithReportResponse
	drop        net.Conn
	waitForExit <-chan struct{}
}

func (s *persistedReportServer) Consolidate(_ *ConsolidateRequest, _ *ConsolidateResponse) error {
	s.legacy.Add(1)
	return errors.New("unexpected legacy consolidation")
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

func TestReportResponseLostAfterPersistedMaintenance(t *testing.T) {
	const agent = "audit"
	for _, name := range []string{"disconnect", "cancel"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := memory.Open(memory.StoreConfig{DataDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			fixture := func(text string, age time.Duration) memory.Fact {
				t.Helper()
				fact, err := store.PutReturningFact(context.Background(), agent, text)
				if err != nil {
					t.Fatal(err)
				}
				fact.Weight = 1
				fact.CreatedAt = time.Now().UTC().Add(-age)
				fact.AccessedAt = fact.CreatedAt
				if err := store.UpdateFact(agent, fact); err != nil {
					t.Fatal(err)
				}
				return fact
			}
			fixture("Old source that should be pruned", 201*24*time.Hour)
			survivor := fixture("Old source that should survive with a decayed weight", 30*24*time.Hour)

			var providerRequests atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerRequests.Add(1)
				http.NotFound(w, r)
			}))
			t.Cleanup(provider.Close)
			handler := &persistedReportServer{
				Server:    NewServer(store, persistedReportConfig{url: provider.URL}),
				committed: make(chan ConsolidateWithReportResponse, 2),
			}
			client, transport := reportPipeConnection(t, handler)
			handler.drop = transport
			if name == "cancel" {
				release := make(chan struct{})
				handler.waitForExit = release
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
			select {
			case outcome := <-handler.committed:
				want := memory.ConsolidationReport{Status: memory.ConsolidationSkipped, Reason: "disabled"}
				expectedEffects := memory.ConsolidationMaintenanceReport{Decay: memory.MaintenanceComplete, Prune: memory.MaintenanceComplete, Graph: memory.MaintenanceSkipped, FactsDecayed: 2, FactsPruned: 1}
				if outcome.Fatal != nil || outcome.Report.Status != want.Status || outcome.Report.Reason != want.Reason || outcome.Report.Maintenance == nil || *outcome.Report.Maintenance != expectedEffects {
					t.Fatalf("server did not confirm maintenance completion: %+v", outcome)
				}
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
			if handler.calls.Load() != 1 || handler.legacy.Load() != 0 || providerRequests.Load() != 0 {
				t.Fatalf("unexpected report=%d legacy=%d provider=%d", handler.calls.Load(), handler.legacy.Load(), providerRequests.Load())
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
			if err != nil || len(facts) != 1 || facts[0].ID != survivor.ID || facts[0].IsSuperseded() || math.Abs(facts[0].Weight-0.5) > 1e-4 {
				t.Fatalf("reopened maintenance differs: %+v, %v", facts, err)
			}
		})
	}
}
