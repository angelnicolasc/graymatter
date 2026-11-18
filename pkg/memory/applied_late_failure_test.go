package memory

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestConsolidationReportKeepsAppliedResultAfterMaintenanceFailure(t *testing.T) {
	const agent = "audit"
	const summary = "Synthetic summary applied before a maintenance failure"
	anthropicServer(t, http.StatusOK, anthropicTextMessage(summary))
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	dir := t.TempDir()
	var callbacks []error
	store, err := Open(StoreConfig{DataDir: dir, OnConsolidateError: func(_ string, err error) {
		callbacks = append(callbacks, err)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedBatch(t, store, agent)

	retired := 0
	writes := consolidationWrites{retirement: func(update func(*bolt.Tx) error) error {
		if err := store.db.Update(update); err != nil {
			return err
		}
		retired++
		if retired == 2 {
			if err := store.db.Close(); err != nil {
				t.Errorf("close test DB after confirmed retirements: %v", err)
			}
		}
		return nil
	}}
	report, err := store.consolidateWithReport(context.Background(), agent, anthropicCfg(), writes)
	var typed *ConsolidationError
	if report.Status != ConsolidationApplied || report.Reason != "summary_applied" || report.SourcesRetired != 2 ||
		report.Diagnostic != nil || !errors.As(err, &typed) || typed.Diagnostic().Code != "store_error" || !errors.Is(err, bolt.ErrDatabaseNotOpen) {
		t.Fatalf("unexpected report/error after closed DB: %+v %v", report, err)
	}
	if len(callbacks) != 1 || !errors.Is(callbacks[0], bolt.ErrDatabaseNotOpen) ||
		!strings.Contains(callbacks[0].Error(), "record consolidation counters") {
		t.Fatalf("best-effort counter failure did not reach the legacy callback: %v", callbacks)
	}

	reopened, err := Open(StoreConfig{DataDir: dir, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	facts, err := reopened.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	summaryMatches, retiredMatches := 0, 0
	for _, fact := range facts {
		if fact.ID == report.SummaryFactID && fact.Text == summary && !fact.IsSuperseded() {
			summaryMatches++
		}
		if fact.SupersededBy == report.SummaryFactID {
			retiredMatches++
		}
	}
	if summaryMatches != 1 || retiredMatches != report.SourcesRetired {
		t.Fatalf("reopened store disagrees with the applied report %+v: %+v", report, facts)
	}
	if cycles, sources := reopened.ConsolidationCounters(); cycles != 0 || sources != 0 {
		t.Fatalf("counter write unexpectedly committed: %d/%d", cycles, sources)
	}
}
