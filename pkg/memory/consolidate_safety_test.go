package memory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const consolidationSafetyAgent = "consolidation-safety"

var consolidationSafetyNow = time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)

// These are the ten counterexamples accepted by the audited coverage rule.
var consolidationSafetyPairs = []struct{ name, source, summary string }{
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
}

func TestConsolidationSafetyAdversarialPairs(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			for _, pair := range consolidationSafetyPairs {
				t.Run(pair.name, func(t *testing.T) {
					ctx := context.Background()
					embedder := &consolidationSafetyEmbedder{}
					s, _ := openConsolidationSafetyStore(t, StoreConfig{DataDir: t.TempDir(), Embedder: embedder})
					source := putConsolidationSafetyFact(t, s, pair.source, 0.25, 0, false)
					other := putConsolidationSafetyFact(t, s, "Young out-of-batch memory", 1, 0, false)
					pinned := putConsolidationSafetyFact(t, s, "Pinned standing requirement", 0.005, 201*24*time.Hour, true)
					want := []Fact{source, other, pinned}

					graph := &consolidationSafetyGraph{labels: map[string]string{}, sources: map[string]string{}}
					s.SetKG(graph, graph)
					response := pair.summary
					if provider == "ollama" {
						response = consolidationSafetyProposal(t, pair.summary, source.ID)
					}
					cfg, requests := consolidationSafetyProvider(t, provider, response)
					if err := s.Consolidate(ctx, consolidationSafetyAgent, cfg); err != nil {
						t.Fatalf("Consolidate: %v", err)
					}
					requireNoConsolidationGeneration(t, s, requests)
					requireConsolidationSafetyFacts(t, s, want...)

					wantTexts := []string{source.Text, other.Text, pinned.Text}
					if got := embedder.inputs(); !slices.Equal(got, wantTexts) {
						t.Errorf("embedder received %q, want only stored facts %q", got, wantTexts)
					}
					results, err := s.vectors.Query(ctx, consolidationSafetyAgent, []float32{1, 0}, 10)
					if err != nil {
						t.Fatalf("query stored vectors: %v", err)
					}
					indexed := make(map[string]string)
					for _, result := range results {
						indexed[result.ID] = result.Content
					}
					if len(indexed) != len(want) {
						t.Errorf("vector index entries = %v, want exactly %d originals", indexed, len(want))
					}
					for _, f := range want {
						if indexed[f.ID] != f.Text {
							t.Errorf("vector index for %q = %q, want %q", f.ID, indexed[f.ID], f.Text)
						}
						id := consolidationSafetyEntityID(f.Text)
						if graph.labels[id] != f.Text || graph.sources[id] != f.ID {
							t.Errorf("extraction for %q did not preserve the original text and source ID", f.Text)
						}
					}
					if len(graph.labels) != len(want) || len(graph.sources) != len(want) || graph.decays != 1 {
						t.Errorf("graph got labels %v, sources %v and %d decay passes", graph.labels, graph.sources, graph.decays)
					}
					if _, found := graph.labels[consolidationSafetyEntityID(pair.summary)]; found {
						t.Errorf("generated text reached the knowledge graph: %q", pair.summary)
					}
					requireConsolidationSafetyRecall(t, s, want...)
				})
			}
		})
	}
}

func TestConsolidationSafetyProposalShapes(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			for _, shape := range []string{"malformed", "stale", "fenced", "invented-consumes"} {
				t.Run(shape, func(t *testing.T) {
					s, _ := openConsolidationSafetyStore(t, StoreConfig{DataDir: t.TempDir()})
					source := putConsolidationSafetyFact(t, s, "Current cache expiry is 60 minutes", 0.5, 0, false)
					other := putConsolidationSafetyFact(t, s, "An unrelated current constraint", 1, 0, false)
					const falseSummary = "Current cache expiry is 5 minutes"
					var reply string
					switch shape {
					case "malformed":
						reply = "{\"summary\":\"" + falseSummary
					case "stale":
						deleted := putConsolidationSafetyFact(t, s, "Old cache expiry is 5 minutes", 0.25, 0, false)
						if err := s.Delete(consolidationSafetyAgent, deleted.ID); err != nil {
							t.Fatalf("delete stale fact: %v", err)
						}
						reply = consolidationSafetyProposal(t, falseSummary, deleted.ID)
					case "fenced":
						const fence = "```"
						reply = fence + "json\n" + consolidationSafetyProposal(t, falseSummary, source.ID) + "\n" + fence
					case "invented-consumes":
						reply = consolidationSafetyProposal(t, falseSummary, source.ID, "invented-source-id")
					}
					cfg, requests := consolidationSafetyProvider(t, provider, reply)
					if err := s.MaybeConsolidate(context.Background(), consolidationSafetyAgent, cfg); err != nil {
						t.Fatalf("MaybeConsolidate: %v", err)
					}
					requireNoConsolidationGeneration(t, s, requests)
					requireConsolidationSafetyFacts(t, s, source, other)
					requireConsolidationSafetyRecall(t, s, source, other)
				})
			}
		})
	}
}

func TestConsolidationSafetyAsyncMaintenanceReopen(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			dir := t.TempDir()
			var backgroundErrors atomic.Int64
			s, closeStore := openConsolidationSafetyStore(t, StoreConfig{
				DataDir: dir,
				OnConsolidateError: func(_ string, err error) {
					backgroundErrors.Add(1)
					t.Errorf("background maintenance: %v", err)
				},
			})
			const days201 = 201 * 24 * time.Hour
			const days30 = 30 * 24 * time.Hour
			pruned := putConsolidationSafetyFact(t, s, "An unpinned 201-day-old observation", 1, days201, false)
			decayed := putConsolidationSafetyFact(t, s, "An observation one half-life old", 1, days30, false)
			young := putConsolidationSafetyFact(t, s, "A fresh high-weight fact outside the former batch", 1, 0, false)
			pinned := putConsolidationSafetyFact(t, s, "An old pinned decision below the pruning floor", 0.005, days201, true)
			legacy := putConsolidationSafetyFact(t, s, "A previously stored summary", 1, 0, false)

			response := "An obsolete replacement of the source"
			if provider == "ollama" {
				response = consolidationSafetyProposal(t, response, decayed.ID)
			}
			cfg, requests := consolidationSafetyProvider(t, provider, response)
			cfg.threshold = 6
			s.LaunchAsyncConsolidate(consolidationSafetyAgent, cfg)
			// Vector reconciliation is disabled for this fixture. Wait for the
			// launched goroutine before inspecting results or closing the store.
			s.wg.Wait()
			requireConsolidationSafetyFacts(t, s, pruned, decayed, young, pinned, legacy)

			cfg.threshold = 3
			s.LaunchAsyncConsolidate(consolidationSafetyAgent, cfg)
			s.wg.Wait()
			if got := backgroundErrors.Load(); got != 0 {
				t.Errorf("background errors = %d, want 0", got)
			}
			requireNoConsolidationGeneration(t, s, requests)
			current, ok := s.loadFact(consolidationSafetyAgent, decayed.ID)
			if !ok || math.Abs(current.Weight-0.5) > 1e-12 {
				t.Errorf("one-half-life fact: got weight %g, exists %t, want 0.5", current.Weight, ok)
			}
			decayed.Weight = current.Weight
			want := []Fact{decayed, young, pinned, legacy}
			requireConsolidationSafetyFacts(t, s, want...)

			closeStore()
			reopened, _ := openConsolidationSafetyStore(t, StoreConfig{DataDir: dir})
			requireNoConsolidationGeneration(t, reopened, requests)
			requireConsolidationSafetyFacts(t, reopened, want...)
			requireConsolidationSafetyRecall(t, reopened, want...)
		})
	}
}

func openConsolidationSafetyStore(t *testing.T, cfg StoreConfig) (*Store, func()) {
	t.Helper()
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s.now = func() time.Time { return consolidationSafetyNow }
	closeStore := sync.OnceFunc(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	t.Cleanup(closeStore)
	return s, closeStore
}

func putConsolidationSafetyFact(t *testing.T, s *Store, text string, weight float64, age time.Duration, pinned bool) Fact {
	t.Helper()
	f, err := s.PutWithOptionsReturningFact(context.Background(), consolidationSafetyAgent, text, WriteOptions{})
	if err != nil {
		t.Fatalf("put %q: %v", text, err)
	}
	f.Weight = weight
	f.CreatedAt = consolidationSafetyNow.Add(-age)
	f.AccessedAt = f.CreatedAt
	f.Pinned = pinned
	if pinned {
		f.PinnedAt = f.CreatedAt
	}
	if err := s.UpdateFact(consolidationSafetyAgent, f); err != nil {
		t.Fatalf("set fixture metadata for %q: %v", text, err)
	}
	return f
}

func consolidationSafetyProposal(t *testing.T, summary string, ids ...string) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Summary  string   `json:"summary"`
		Consumes []string `json:"consumes"`
	}{Summary: summary, Consumes: ids})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func consolidationSafetyProvider(t *testing.T, provider, response string) (*testConsolidateCfg, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	path := "/api/generate"
	body := map[string]any{"response": response}
	if provider == "anthropic" {
		path = "/v1/messages"
		body = map[string]any{
			"id": "msg_fixture", "type": "message", "role": "assistant", "model": "claude-test",
			"content":     []map[string]string{{"type": "text", "text": response}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != path {
			t.Errorf("unexpected summariser request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write model fixture: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	return &testConsolidateCfg{
		threshold: 1, halfLife: 30 * 24 * time.Hour, llm: provider,
		apiKey: "fixture-key", model: "claude-test", ollamaURL: server.URL, ollamaMdl: "local-test",
	}, &requests
}

func requireNoConsolidationGeneration(t *testing.T, s *Store, requests *atomic.Int64) {
	t.Helper()
	if got := requests.Load(); got != 0 {
		t.Errorf("automatic summariser requests = %d, want 0", got)
	}
	if cycles, consumed := s.ConsolidationCounters(); cycles != 0 || consumed != 0 {
		t.Errorf("proposal counters = (%d, %d), want (0, 0)", cycles, consumed)
	}
}

func requireConsolidationSafetyFacts(t *testing.T, s *Store, want ...Fact) {
	t.Helper()
	got, err := s.List(consolidationSafetyAgent)
	if err != nil {
		t.Fatalf("list canonical facts: %v", err)
	}
	byID := make(map[string]Fact)
	for _, f := range got {
		byID[f.ID] = f
	}
	if len(got) != len(want) {
		t.Errorf("canonical store has %d rows, want %d: %+v", len(got), len(want), got)
	}
	for _, expected := range want {
		actual := byID[expected.ID]
		if !reflect.DeepEqual(actual, expected) {
			t.Errorf("canonical fact %q changed:\ngot  %+v\nwant %+v", expected.Text, actual, expected)
		}
	}
}

func requireConsolidationSafetyRecall(t *testing.T, s *Store, want ...Fact) {
	t.Helper()
	ctx := context.Background()
	const query = "retrieval verification"
	got, err := s.Recall(ctx, consolidationSafetyAgent, query, 10)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	wantTexts := make([]string, 0, len(want))
	wantByID := make(map[string]string)
	for _, f := range want {
		wantTexts = append(wantTexts, f.Text)
		wantByID[f.ID] = f.Text
	}
	slices.Sort(got)
	slices.Sort(wantTexts)
	if !slices.Equal(got, wantTexts) {
		t.Errorf("Recall returned %q, want %q", got, wantTexts)
	}
	receipts, err := s.RecallExplain(ctx, consolidationSafetyAgent, query, 10)
	if err != nil {
		t.Fatalf("RecallExplain: %v", err)
	}
	if len(receipts) != len(want) {
		t.Errorf("RecallExplain returned %d receipts, want %d", len(receipts), len(want))
	}
	for _, receipt := range receipts {
		id := receipt.Provenance.FactID
		if wantByID[id] != receipt.Text || receipt.Provenance.SupersededBy != "" || len(receipt.Provenance.Supersedes) != 0 {
			t.Errorf("RecallExplain returned unexpected provenance: %+v", receipt)
		}
		delete(wantByID, id)
	}
	if len(wantByID) != 0 {
		t.Errorf("RecallExplain missed original fact IDs: %v", wantByID)
	}
}

type consolidationSafetyEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (e *consolidationSafetyEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.texts = append(e.texts, text)
	return []float32{1, 0}, nil
}

func (e *consolidationSafetyEmbedder) Dimensions() int { return 2 }
func (e *consolidationSafetyEmbedder) Name() string    { return "consolidation-safety-fixture" }

func (e *consolidationSafetyEmbedder) inputs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.texts)
}

type consolidationSafetyGraph struct {
	labels  map[string]string
	sources map[string]string
	decays  int
}

func consolidationSafetyEntityID(text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}

func (g *consolidationSafetyGraph) ExtractIDs(text string) ([]string, error) {
	return []string{consolidationSafetyEntityID(text)}, nil
}

func (g *consolidationSafetyGraph) ExtractTyped(text string) ([]EntityRef, []EntityLink, error) {
	id := consolidationSafetyEntityID(text)
	return []EntityRef{{ID: id, Label: text, EntityType: "concept"}}, []EntityLink{{From: id, To: "fixture", Relation: "fixture"}}, nil
}

func (g *consolidationSafetyGraph) UpsertNode(id, label, _ string) error {
	g.labels[id] = label
	return nil
}

func (g *consolidationSafetyGraph) NeighborTexts(_ string, _ int) ([]string, error) {
	return nil, nil
}

func (g *consolidationSafetyGraph) LinkEdges(links []EntityLink, factID string) error {
	for _, link := range links {
		g.sources[link.From] = factID
	}
	return nil
}

func (g *consolidationSafetyGraph) DecayGraph(time.Duration) error {
	g.decays++
	return nil
}
