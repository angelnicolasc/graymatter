package memory

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

type receiptFault uint8

const (
	receiptNormal receiptFault = iota
	receiptBeforeUpdate
	receiptLostAcknowledgement
	receiptUnconfirmedWithoutVisibleWrite
)

func runReceiptUpdate(db *bolt.DB, update func(*bolt.Tx) error, fault receiptFault, injected error) error {
	if fault == receiptBeforeUpdate {
		return injected
	}
	if fault == receiptUnconfirmedWithoutVisibleWrite {
		// Simulate an updater that returns a late error after preparation,
		// with the write absent. The inner callback and return value offer
		// the same evidence to the caller as a lost acknowledgement.
		return db.Update(func(tx *bolt.Tx) error {
			if err := update(tx); err != nil {
				return err
			}
			return injected
		})
	}
	if err := db.Update(update); err != nil {
		return err
	}
	if fault == receiptLostAcknowledgement {
		return injected
	}
	return nil
}

func TestConsolidationReceiptMatchesReopenedStore(t *testing.T) {
	const agentID = "audit"
	const summary = "Synthetic summary for the persistence test."
	const canary = "synthetic-database-error"
	injected := errors.New(canary)

	for _, tc := range []struct {
		name                         string
		summaryFault                 receiptFault
		retireFaults                 []receiptFault
		status                       ConsolidationSummaryStatus
		reason                       string
		wantSummary, wantConfirmedID bool
		confirmed, persisted         int
		retirementErr, unknownErr    bool
		lateMaintenanceError         bool
	}{
		{
			name: "summary rejected before update", summaryFault: receiptBeforeUpdate,
			status: ConsolidationFailed, reason: "summary_write_failed",
		},
		{
			name: "summary acknowledgement missing", summaryFault: receiptLostAcknowledgement,
			status: ConsolidationUncertain, reason: "summary_commit_uncertain",
			wantSummary: true, unknownErr: true,
		},
		{
			name: "summary acknowledgement missing with no reopened write", summaryFault: receiptUnconfirmedWithoutVisibleWrite,
			status: ConsolidationUncertain, reason: "summary_commit_uncertain", unknownErr: true,
		},
		{
			name: "summary acknowledgement missing before later maintenance error", summaryFault: receiptLostAcknowledgement,
			status: ConsolidationUncertain, reason: "summary_commit_uncertain",
			wantSummary: true, unknownErr: true, lateMaintenanceError: true,
		},
		{
			name: "no source retirement commits", retireFaults: []receiptFault{receiptBeforeUpdate, receiptBeforeUpdate},
			status: ConsolidationPartial, reason: "source_retirement_failed", wantSummary: true,
			wantConfirmedID: true, retirementErr: true,
		},
		{
			name: "some source retirement commits", retireFaults: []receiptFault{receiptBeforeUpdate, receiptNormal},
			status: ConsolidationPartial, reason: "source_retirement_failed", wantSummary: true,
			wantConfirmedID: true, confirmed: 1, persisted: 1, retirementErr: true,
		},
		{
			name:         "retirement acknowledgement missing and later source commits",
			retireFaults: []receiptFault{receiptLostAcknowledgement, receiptNormal},
			status:       ConsolidationUncertain, reason: "source_retirement_uncertain", wantSummary: true,
			wantConfirmedID: true, confirmed: 1, persisted: 2, retirementErr: true, unknownErr: true,
		},
		{
			name:         "retirement acknowledgement missing with zero confirmed and a rejected source",
			retireFaults: []receiptFault{receiptLostAcknowledgement, receiptBeforeUpdate},
			status:       ConsolidationUncertain, reason: "source_retirement_uncertain", wantSummary: true,
			wantConfirmedID: true, confirmed: 0, persisted: 1, retirementErr: true, unknownErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			anthropicServer(t, http.StatusOK, anthropicTextMessage(summary))
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
			var callbacks []error
			dir := t.TempDir()
			store, err := Open(StoreConfig{
				DataDir: dir,
				OnConsolidateError: func(_ string, err error) {
					callbacks = append(callbacks, err)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if store != nil {
					_ = store.Close()
				}
			})
			seedBatch(t, store, agentID)
			initial, err := store.List(agentID)
			if err != nil {
				t.Fatal(err)
			}
			original := make(map[string]string, len(initial))
			for _, fact := range initial {
				original[fact.ID] = fact.Text
			}

			var summaryCalls, retireCalls int
			writes := consolidationWrites{
				summary: func(fn func(*bolt.Tx) error) error {
					summaryCalls++
					err := runReceiptUpdate(store.db, fn, tc.summaryFault, injected)
					if tc.lateMaintenanceError {
						// The next maintenance List must distinguish its fatal
						// cause from the earlier unconfirmed summary commit.
						if closeErr := store.db.Close(); closeErr != nil {
							t.Errorf("close test DB: %v", closeErr)
						}
					}
					return err
				},
				retirement: func(fn func(*bolt.Tx) error) error {
					retireCalls++
					if retireCalls > len(tc.retireFaults) {
						t.Errorf("unexpected retirement attempt %d", retireCalls)
						return injected
					}
					return runReceiptUpdate(store.db, fn, tc.retireFaults[retireCalls-1], injected)
				},
			}
			report, err := store.consolidateWithReport(context.Background(), agentID, anthropicCfg(), writes)
			if report.Status != tc.status || report.Reason != tc.reason || report.Provider != "anthropic" ||
				report.SourcesRetired != tc.confirmed || (report.SummaryFactID != "") != tc.wantConfirmedID {
				t.Fatalf("unexpected report: %+v; error=%v", report, err)
			}
			d := report.Diagnostic
			if d == nil || d.Code != tc.reason || d.Provider != "" || d.HTTPStatus != 0 || strings.Contains(d.Message, canary) {
				t.Fatalf("unexpected public diagnostic: %+v", d)
			}
			if summaryCalls != 1 || retireCalls != len(tc.retireFaults) {
				t.Fatalf("write attempts summary=%d retirement=%d; want 1/%d", summaryCalls, retireCalls, len(tc.retireFaults))
			}
			if len(callbacks) != 1 || !errors.Is(callbacks[0], injected) || errors.Is(callbacks[0], ErrMutationOutcomeUnknown) {
				t.Fatalf("legacy callback cause changed: %v", callbacks)
			}
			if errors.Is(callbacks[0], ErrConsolidationRetirement) != tc.retirementErr {
				t.Fatalf("legacy retirement sentinel mismatch: %v", callbacks[0])
			}

			var fatal *ConsolidationDiagnostic
			if tc.retirementErr || tc.unknownErr {
				var reported *ConsolidationError
				if !errors.As(err, &reported) || !errors.Is(err, injected) ||
					errors.Is(err, ErrConsolidationRetirement) != tc.retirementErr ||
					errors.Is(err, ErrMutationOutcomeUnknown) != tc.unknownErr ||
					strings.Contains(err.Error(), canary) {
					t.Fatalf("unexpected safe error or wrapped causes: %v", err)
				}
				if tc.lateMaintenanceError && !errors.Is(err, bolt.ErrDatabaseNotOpen) {
					t.Fatal("the safe uncertainty error lost the later maintenance failure")
				}
				diagnostic := reported.Diagnostic()
				fatal = &diagnostic
				if fatal.Code != tc.reason {
					t.Fatalf("fatal code %q, want %q", fatal.Code, tc.reason)
				}
			} else if err != nil {
				t.Fatalf("confirmed summary failure changed the outer error policy: %v", err)
			}
			if _, _, err := ValidateConsolidationResult(report, fatal); err != nil {
				t.Fatalf("core produced an invalid report/error tuple: %v", err)
			}

			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = nil
			reopened, err := Open(StoreConfig{DataDir: dir, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			persisted, err := reopened.List(agentID)
			if err != nil {
				t.Fatal(err)
			}
			var storedSummaryID string
			for _, fact := range persisted {
				if fact.Text == summary {
					if storedSummaryID != "" || fact.IsSuperseded() {
						t.Fatalf("expected at most one live summary, found %+v", fact)
					}
					storedSummaryID = fact.ID
				}
			}
			if (storedSummaryID != "") != tc.wantSummary || tc.wantConfirmedID && storedSummaryID != report.SummaryFactID {
				t.Fatalf("reopened summary ID = %q, report = %+v", storedSummaryID, report)
			}
			retired, sourceCount := 0, 0
			for _, fact := range persisted {
				if storedSummaryID != "" && fact.ID == storedSummaryID {
					continue
				}
				sourceCount++
				if text, ok := original[fact.ID]; !ok || text != fact.Text {
					t.Errorf("unexpected source after reopen: %+v", fact)
				}
				if fact.IsSuperseded() {
					if storedSummaryID == "" || fact.SupersededBy != storedSummaryID {
						t.Errorf("invalid retirement receipt for %q toward %q", fact.ID, fact.SupersededBy)
					}
					retired++
				}
			}
			if sourceCount != len(initial) || retired != tc.persisted {
				t.Fatalf("reopened store has %d source facts and %d retirements, want %d and %d", sourceCount, retired, len(initial), tc.persisted)
			}
			wantCycles := 0
			if tc.confirmed > 0 {
				wantCycles = 1
			}
			if cycles, sources := reopened.ConsolidationCounters(); cycles != wantCycles || sources != tc.confirmed {
				t.Fatalf("reopened counters = %d/%d, want %d/%d", cycles, sources, wantCycles, tc.confirmed)
			}
		})
	}
}
