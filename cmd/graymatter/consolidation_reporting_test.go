package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func TestConsolidationReportJSONDirectAndDaemon(t *testing.T) {
	oldDir, oldQuiet, oldJSON, oldNoDaemon := dataDir, quiet, jsonOut, noDaemon
	t.Cleanup(func() { dataDir, quiet, jsonOut, noDaemon = oldDir, oldQuiet, oldJSON, oldNoDaemon })
	quiet, jsonOut = true, true
	t.Setenv("GRAYMATTER_NO_DAEMON", "")

	for _, direct := range []bool{true, false} {
		name := "daemon"
		if direct {
			name = "direct"
		}
		t.Run(name, func(t *testing.T) {
			_, requests := c0ProviderFixture(t, "Unapproved automatic summary")
			dataDir, noDaemon = t.TempDir(), direct
			const agent = "audit"
			store, err := memory.Open(memory.StoreConfig{DataDir: dataDir})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			put := func(text string, age time.Duration) memory.Fact {
				t.Helper()
				fact, err := store.PutReturningFact(t.Context(), agent, text)
				if err != nil {
					t.Fatal(err)
				}
				fact.CreatedAt = time.Now().UTC().Add(-age)
				fact.AccessedAt = fact.CreatedAt
				fact.Weight = 1
				if err := store.UpdateFact(agent, fact); err != nil {
					t.Fatal(err)
				}
				return fact
			}
			put("Stale source that should be pruned", 201*24*time.Hour)
			survivor := put("Source that survives one half-life", 30*24*time.Hour)

			var stop func()
			if direct {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				const token = "fixture-consolidation-report"
				listener, cleanup, err := rpc.Listen(dataDir, token)
				if err != nil {
					t.Fatal(err)
				}
				cfg := graymatter.DefaultConfig()
				cfg.ConsolidateLLM = "anthropic"
				server := rpc.NewServer(store, cfg)
				server.SetAuthToken(token)
				go func() { _ = server.Serve(listener) }()
				stop = sync.OnceFunc(func() {
					server.Stop()
					cleanup()
				})
				t.Cleanup(stop)
			}

			var stdout, stderr bytes.Buffer
			cmd := consolidateCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{agent})
			if err := cmd.Execute(); err != nil || stderr.Len() != 0 {
				t.Fatalf("consolidate: %s / %s, %v", stdout.String(), stderr.String(), err)
			}
			var result consolidationCLIResult
			decoder := json.NewDecoder(&stdout)
			if err := decoder.Decode(&result); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
				t.Fatalf("consolidate must emit one JSON result: %+v, %v", result, err)
			}
			expected := memory.ConsolidationMaintenanceReport{Decay: memory.MaintenanceComplete, Prune: memory.MaintenanceComplete,
				Graph: memory.MaintenanceSkipped, FactsDecayed: 2, FactsPruned: 1}
			if result.SchemaVersion != 1 || result.AgentID != agent || result.Error != nil || result.Summary == nil ||
				result.Summary.Status != memory.ConsolidationSkipped || result.Summary.Reason != "disabled" ||
				result.Summary.Maintenance != nil || result.Summary.SourcesRetired != 0 ||
				result.Maintenance == nil || *result.Maintenance != expected {
				t.Fatalf("unexpected report: %+v", result)
			}
			if !direct {
				stop()
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := memory.Open(memory.StoreConfig{DataDir: dataDir, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			facts, err := reopened.List(agent)
			if err != nil || len(facts) != 1 || facts[0].ID != survivor.ID || facts[0].IsSuperseded() || math.Abs(facts[0].Weight-0.5) > 1e-4 {
				t.Fatalf("reopened state differs from confirmed report: %+v, %v", facts, err)
			}
			if requests.Load() != 0 {
				t.Fatalf("configured provider received %d summary requests", requests.Load())
			}
		})
	}
}

func TestConsolidationUnknownJSONDoesNotInventMaintenance(t *testing.T) {
	result := commandConsolidationResult("audit", memory.ConsolidationReport{},
		memory.NewConsolidationError("outcome_unknown", errors.New("private cause")))
	if result.Summary != nil || result.Maintenance != nil || result.Error == nil || result.Error.Code != "outcome_unknown" {
		t.Fatalf("unexpected unknown result: %+v", result)
	}
}

func TestConsolidationReportCancellationBeforeOpen(t *testing.T) {
	oldJSON := jsonOut
	jsonOut = true
	t.Cleanup(func() { jsonOut = oldJSON })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	cmd := consolidateCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"audit"})
	if err := cmd.ExecuteContext(ctx); !errors.Is(err, errConsolidationReported) {
		t.Fatalf("canceled report: %v", err)
	}
	var result consolidationCLIResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Error == nil || result.Error.Code != "canceled" ||
		result.Maintenance != nil || result.Summary != nil {
		t.Fatalf("canceled report: %+v, %v", result, err)
	}
}

type maintenanceListener struct {
	net.Listener
	first chan net.Conn
	once  sync.Once
}

func (l *maintenanceListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.once.Do(func() { l.first <- conn })
	}
	return conn, err
}

type interruptedMaintenanceBackend struct {
	*memory.Store
	first <-chan net.Conn
	calls atomic.Int32
}

func (*interruptedMaintenanceBackend) MaintenanceOnly() bool             { return true }
func (*interruptedMaintenanceBackend) SupportsConsolidationReport() bool { return false }

func (b *interruptedMaintenanceBackend) Consolidate(ctx context.Context, agent string, cfg memory.ConsolidateConfig) error {
	first := b.calls.Add(1) == 1
	if err := b.Store.Consolidate(ctx, agent, cfg); err != nil {
		return err
	}
	if first {
		// Drop only this reply. The daemon can accept a reconnect and a replay.
		return (<-b.first).Close()
	}
	return nil
}

func TestTextConsolidationLostReplyDoesNotReplay(t *testing.T) {
	oldDir, oldQuiet, oldJSON, oldNoDaemon := dataDir, quiet, jsonOut, noDaemon
	t.Cleanup(func() { dataDir, quiet, jsonOut, noDaemon = oldDir, oldQuiet, oldJSON, oldNoDaemon })
	dataDir, quiet, jsonOut, noDaemon = t.TempDir(), false, false, false
	t.Setenv("GRAYMATTER_NO_DAEMON", "")

	store, err := memory.Open(memory.StoreConfig{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	closeStore := sync.OnceValue(store.Close)
	t.Cleanup(func() { _ = closeStore() })
	const agent = "maintenance"
	fact, err := store.PutReturningFact(t.Context(), agent, "Retain this fact across maintenance.")
	if err != nil {
		t.Fatal(err)
	}
	cfg := graymatter.DefaultConfig()
	fact.Weight = 1
	fact.CreatedAt = time.Now().UTC().Add(-cfg.DecayHalfLife)
	fact.AccessedAt = fact.CreatedAt
	if err := store.UpdateFact(agent, fact); err != nil {
		t.Fatal(err)
	}

	const token = "fixture-text-maintenance"
	listener, cleanup, err := rpc.Listen(dataDir, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	first := make(chan net.Conn, 1)
	backend := &interruptedMaintenanceBackend{Store: store, first: first}
	server := rpc.NewServer(backend, cfg)
	server.SetAuthToken(token)
	go func() { _ = server.Serve(&maintenanceListener{Listener: listener, first: first}) }()
	t.Cleanup(server.Stop)

	var stdout bytes.Buffer
	cmd := consolidateCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{agent})
	if err := cmd.Execute(); !errors.Is(err, memory.ErrMutationOutcomeUnknown) {
		t.Fatalf("lost maintenance reply: %v", err)
	}
	if backend.calls.Load() != 1 || strings.Contains(stdout.String(), "Maintenance complete") {
		t.Fatalf("maintenance dispatches=%d stdout=%q", backend.calls.Load(), stdout.String())
	}
	server.Stop()
	if err := closeStore(); err != nil {
		t.Fatal(err)
	}
	reopened, err := memory.Open(memory.StoreConfig{DataDir: dataDir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	facts, err := reopened.List(agent)
	if err != nil || len(facts) != 1 || facts[0].ID != fact.ID || math.Abs(facts[0].Weight-0.5) > 1e-4 {
		t.Fatalf("reopened maintenance: %+v, %v", facts, err)
	}
}
