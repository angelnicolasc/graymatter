package memory

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestConsolidationReportMatchesReopenedMaintenance(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			dir := t.TempDir()
			s, closeStore := openConsolidationSafetyStore(t, StoreConfig{DataDir: dir})
			putConsolidationSafetyFact(t, s, "Expired source below the pruning floor", 1, 201*24*time.Hour, false)
			survivor := putConsolidationSafetyFact(t, s, "Source that survives one half-life of decay", 1, 30*24*time.Hour, false)
			response := "Unapproved automatic summary"
			if provider == "ollama" {
				response = consolidationSafetyProposal(t, response, survivor.ID)
			}
			cfg, requests := consolidationSafetyProvider(t, provider, response)
			graph, extractor := &decayRecordingKG{}, &recordingExtractor{}
			s.SetKG(graph, extractor)

			report, err := s.ConsolidateWithReport(context.Background(), consolidationSafetyAgent, cfg)
			want := ConsolidationReport{Status: ConsolidationSkipped, Reason: "disabled"}
			wantEffects := ConsolidationMaintenanceReport{Decay: MaintenanceComplete, Prune: MaintenanceComplete, Graph: MaintenanceComplete, FactsDecayed: 2, FactsPruned: 1}
			if err != nil || report.Status != want.Status || report.Reason != want.Reason || report.SourcesRetired != 0 || report.Maintenance == nil || *report.Maintenance != wantEffects {
				t.Fatalf("ConsolidateWithReport = %+v, %v, want %+v", report, err, want)
			}
			if extractor.calls != 1 || len(graph.upserts) != 2 || len(graph.decayCalls) != 1 || graph.decayCalls[0] != cfg.halfLife {
				t.Fatalf("one maintenance pass: extracts=%d nodes=%v graph decay=%v", extractor.calls, graph.upserts, graph.decayCalls)
			}

			closeStore()
			reopened, _ := openConsolidationSafetyStore(t, StoreConfig{DataDir: dir})
			requireNoConsolidationGeneration(t, reopened, requests)
			row, ok := reopened.loadFact(consolidationSafetyAgent, survivor.ID)
			if !ok || math.Abs(row.Weight-0.5) > 1e-12 {
				t.Fatalf("decayed fact after reopen: %+v, present=%t", row, ok)
			}
			survivor.Weight = row.Weight
			requireConsolidationSafetyFacts(t, reopened, survivor)
			if sig, ok := reopened.extractedSignature(consolidationSafetyAgent, survivor.ID); !ok || sig != textSignature(survivor.Text) {
				t.Fatal("extraction watermark did not persist")
			}
		})
	}
}

type reportExtractor func(string) ([]string, error)

func (e reportExtractor) ExtractIDs(text string) ([]string, error) { return e(text) }

func TestConsolidationReportKeepsConfirmedCountsWhenGraphFailsOrStops(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		name := "failed"
		if interrupted {
			name = "incomplete"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, closeStore := openConsolidationSafetyStore(t, StoreConfig{DataDir: dir})
			putConsolidationSafetyFact(t, s, "Expired graph fixture", 1, 201*24*time.Hour, false)
			first := putConsolidationSafetyFact(t, s, "First surviving graph fixture", 1, 30*24*time.Hour, false)
			second := putConsolidationSafetyFact(t, s, "Second surviving graph fixture", 1, 30*24*time.Hour, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			graph := &decayRecordingKG{}
			s.SetKG(graph, reportExtractor(func(string) ([]string, error) {
				if interrupted {
					cancel()
					return []string{"node"}, nil
				}
				return nil, errors.New("configured extractor failed")
			}))

			report, err := s.ConsolidateWithReport(ctx, consolidationSafetyAgent, defaultTestCfg())
			wantStatus := MaintenanceFailed
			if interrupted {
				wantStatus = MaintenanceIncomplete
			}
			wantEffects := ConsolidationMaintenanceReport{Decay: MaintenanceComplete, Prune: MaintenanceComplete, Graph: wantStatus, FactsDecayed: 3, FactsPruned: 1}
			if interrupted && !errors.Is(err, context.Canceled) || !interrupted && err != nil || report.Maintenance == nil || *report.Maintenance != wantEffects {
				t.Fatalf("report=%+v maintenance=%+v error=%v", report, report.Maintenance, err)
			}
			closeStore()
			reopened, _ := openConsolidationSafetyStore(t, StoreConfig{DataDir: dir})
			first.Weight, second.Weight = 0.5, 0.5
			requireConsolidationSafetyFacts(t, reopened, first, second)
		})
	}
}
