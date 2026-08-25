package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// The W3 consolidation contract, exercised end to end against fake Ollama
// servers: propose/apply discipline (ADR-011), tombstone receipts that stay
// auditable (ADR-007), pinned facts never entering the prompt (invariant
// I-1), and the extraction watermark (A7).

// ollamaFixture is a store plus a scripted Ollama endpoint. The handler
// receives every generate call with its parsed prompt payload so tests can
// inspect what the model was shown and answer with proposals built from the
// very IDs the prompt carried.
type ollamaFixture struct {
	store *Store
	srv   *httptest.Server

	ch        chan *ollamaRequest
	responder func(r *ollamaRequest) string
}

type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Format string `json:"format"`
}

var promptIDRe = regexp.MustCompile(`^- ([0-9A-Z]{26}): `)

// idsInPrompt extracts the fact IDs the model was shown, in order.
func idsInPrompt(prompt string) []string {
	var ids []string
	for _, line := range strings.Split(prompt, "\n") {
		if m := promptIDRe.FindStringSubmatch(line); m != nil {
			ids = append(ids, m[1])
		}
	}
	return ids
}

func newOllamaFixture(t *testing.T, responder func(r *ollamaRequest) string) *ollamaFixture {
	t.Helper()
	f := &ollamaFixture{ch: make(chan *ollamaRequest, 64), responder: responder}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var r ollamaRequest
		_ = json.NewDecoder(req.Body).Decode(&r)
		select {
		case f.ch <- &r:
		default:
		}
		fmt.Fprint(w, responder(&r))
	}))
	t.Cleanup(f.srv.Close)

	dir := t.TempDir()
	s, err := Open(StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f.store = s
	return f
}

func (f *ollamaFixture) drain() []*ollamaRequest {
	close(f.ch)
	var reqs []*ollamaRequest
	for r := range f.ch {
		reqs = append(reqs, r)
	}
	return reqs
}

func (f *ollamaFixture) cfg(threshold int) *testConsolidateCfg {
	return &testConsolidateCfg{threshold: threshold, halfLife: 720 * time.Hour,
		llm: "ollama", ollamaURL: f.srv.URL, ollamaMdl: "test-model"}
}

// jsonProposal builds an Ollama /api/generate body whose .Response field is a
// JSON proposal — mirroring what "format":"json" yields.
func jsonProposal(summary string, consumes []string) string {
	inner, _ := json.Marshal(map[string]any{"summary": summary, "consumes": consumes})
	resp, _ := json.Marshal(map[string]any{"response": string(inner)})
	return string(resp)
}

func findByText(t *testing.T, facts []Fact, text string) Fact {
	t.Helper()
	for _, f := range facts {
		if f.Text == text {
			return f
		}
	}
	t.Fatalf("fact %q not found", text)
	return Fact{}
}

const (
	pinnedText    = "ARCHITECTURE DECISION: the ledger is append-only forever"
	weakA         = "weak observation alpha with filler detail one"
	weakB         = "weak observation beta with filler detail two"
	weakC         = "weak observation gamma with filler detail three"
	strongD       = "strong observation delta with crucial detail four"
	unpinnedOther = "medium observation epsilon with detail five"
)

// seedSixWeights plants six facts with distinct weights; the three weakest
// unpinned ones form the summarisation batch of a 6-fact store.
func seedSixWeights(t *testing.T, s *Store, agent string) {
	t.Helper()
	ctx := context.Background()
	for _, text := range []string{weakA, weakB, weakC, strongD, unpinnedOther, pinnedText} {
		if err := s.Put(ctx, agent, text); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	weights := map[string]float64{
		weakA: 0.05, weakB: 0.06, weakC: 0.07, // the batch
		strongD:       0.95,
		unpinnedOther: 0.50,
		pinnedText:    0.01, // lowest weight overall — the trap for an unfiltered batch
	}
	facts, err := s.List(agent)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := range facts {
		w, ok := weights[facts[i].Text]
		if !ok {
			t.Fatalf("unseeded fact %q", facts[i].Text)
		}
		facts[i].Weight = w
		if facts[i].Text == pinnedText {
			now := time.Now().UTC()
			facts[i].Pinned = true
			facts[i].PinnedAt = now
			facts[i].AccessedAt = now.Add(-400 * time.Hour)
		}
		if err := s.UpdateFact(agent, facts[i]); err != nil {
			t.Fatalf("UpdateFact: %v", err)
		}
	}
}

// TestOllamaConsolidation_AppliesValidProposalWithReceipts drives the happy
// path end to end: the model proposes consuming two of the three shown facts;
// exactly those are tombstoned toward the real summary fact, receipts stay
// listed and auditable with their weight intact, the unconsumed batch fact
// stays live, the pinned fact never reached the prompt, and the counters
// moved by one cycle and two facts.
func TestOllamaConsolidation_AppliesValidProposalWithReceipts(t *testing.T) {
	var consume []string
	f := newOllamaFixture(t, func(r *ollamaRequest) string {
		if r.Format != "json" {
			t.Errorf("request did not ask Ollama for JSON output: format=%q", r.Format)
		}
		ids := idsInPrompt(r.Prompt)
		if len(ids) < 2 {
			t.Errorf("prompt showed %d facts, want >=2:\n%s", len(ids), r.Prompt)
		}
		consume = ids[:2]
		return jsonProposal("merged summary of the weak observations.", consume)
	})

	const agent = "receipts-agent"
	seedSixWeights(t, f.store, agent)

	ctx := context.Background()
	before, _ := f.store.List(agent)
	if err := f.store.Consolidate(ctx, agent, f.cfg(4)); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	after, _ := f.store.List(agent)
	summary := findByText(t, after, "merged summary of the weak observations.")

	for _, id := range consume {
		var tomb *Fact
		for i := range after {
			if after[i].ID == id {
				tomb = &after[i]
				break
			}
		}
		if tomb == nil {
			t.Fatalf("consumed fact %s vanished entirely; receipt lost", id)
		}
		if !tomb.IsSuperseded() || tomb.SupersededBy != summary.ID {
			t.Errorf("fact %s: SupersededBy=%q, want summary ID %q", id, tomb.SupersededBy, summary.ID)
		}
		var preWeight float64
		for _, b := range before {
			if b.ID == id {
				preWeight = b.Weight
			}
		}
		if tomb.Weight != preWeight {
			t.Errorf("tombstone %s weight %.4f != pre-decay %.4f: zeroing it would let the same cycle's prune eat the receipt", id, tomb.Weight, preWeight)
		}
	}

	// The third batch fact was not consumed and must be untouched and live.
	for _, fct := range after {
		if fct.Text == weakC && fct.IsSuperseded() {
			t.Error("unconsumed batch fact was tombstoned anyway")
		}
	}
	// Pinned trap survived byte-for-byte.
	pin := findByText(t, after, pinnedText)
	if !pin.Pinned || pin.Weight != 0.01 {
		t.Errorf("pinned fact mutated during consolidation: %+v", pin)
	}

	cycles, factsConsumed := f.store.ConsolidationCounters()
	if cycles != 1 || factsConsumed != len(consume) {
		t.Errorf("counters = (%d cycles, %d facts), want (%d, %d)", cycles, factsConsumed, 1, len(consume))
	}
}

// TestOllamaConsolidation_PinnedNeverEntersPrompt pins the I-1 guarantee at
// the protocol boundary: even the weakest fact in the store must be absent
// from the payload when it is pinned.
func TestOllamaConsolidation_PinnedNeverEntersPrompt(t *testing.T) {
	f := newOllamaFixture(t, func(r *ollamaRequest) string {
		return jsonProposal("digest.", idsInPrompt(r.Prompt))
	})
	seedSixWeights(t, f.store, "pin-prompt-agent")

	if err := f.store.Consolidate(context.Background(), "pin-prompt-agent", f.cfg(4)); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, r := range f.drain() {
		if strings.Contains(r.Prompt, pinnedText) {
			t.Errorf("pinned fact leaked into the summarisation prompt:\n%s", r.Prompt)
		}
	}
}

// TestOllamaConsolidation_MalformedProposalDiscarded: a syntactically valid
// HTTP exchange carrying garbage instead of a JSON object must leave the
// store byte-intact, fire the hook with ErrInvalidProposal, and move no
// counter.
func TestOllamaConsolidation_MalformedProposalDiscarded(t *testing.T) {
	var reported []error
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
		OnConsolidateError: func(_ string, err error) {
			reported = append(reported, err)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		fmt.Fprint(w, `{"response":"this is prose, definitely not the JSON object we demanded"}`)
	}))
	defer srv.Close()

	const agent = "malformed-agent"
	seedSixWeights(t, s, agent)
	cfg := &testConsolidateCfg{threshold: 4, halfLife: 720 * time.Hour,
		llm: "ollama", ollamaURL: srv.URL}

	if err := s.Consolidate(context.Background(), agent, cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	found := false
	for _, e := range reported {
		if errors.Is(e, ErrInvalidProposal) {
			found = true
		}
	}
	if !found {
		t.Errorf("discarded proposal reported as %v, want ErrInvalidProposal", reported)
	}
	facts, _ := s.List(agent)
	if len(facts) != 6 {
		t.Fatalf("store mutated by a discarded proposal: %d facts, want 6", len(facts))
	}
	for _, fct := range facts {
		if fct.IsSuperseded() {
			t.Errorf("fact %q tombstoned by a discarded proposal", fct.Text)
		}
	}
	if cycles, consumed := s.ConsolidationCounters(); cycles != 0 || consumed != 0 {
		t.Errorf("counters moved on a discarded proposal: (%d,%d)", cycles, consumed)
	}
	_ = calls
}

// TestOllamaConsolidation_FencedJSONAccepted: models wrap JSON in code fences
// even when told not to; the fence stripper must absorb that without turning
// a good proposal into a discard.
func TestOllamaConsolidation_FencedJSONAccepted(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
		OnConsolidateError: func(_ string, err error) {
			t.Errorf("valid fenced proposal reported: %v", err)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A valid proposal wrapped in the code fences models insist on.
		inner := "```json\n{\"summary\":\"fenced digest.\",\"consumes\":[\"ANY\"]}\n```"
		env, _ := json.Marshal(map[string]any{"response": inner})
		_, _ = w.Write(env)
	}))
	defer srv.Close()

	const agent = "fenced-agent"
	seedSixWeights(t, s, agent)
	cfg := &testConsolidateCfg{threshold: 4, halfLife: 720 * time.Hour,
		llm: "ollama", ollamaURL: srv.URL}

	if err := s.Consolidate(context.Background(), agent, cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if id := s.factIDByText(agent, "fenced digest."); id == "" {
		t.Error("fenced valid proposal was not applied")
	}
	// "ANY" is not a batch ID: clamped to nothing, so nothing was tombstoned
	// and no counter moved — the summary alone entered the store.
	if _, consumed := s.ConsolidationCounters(); consumed != 0 {
		t.Errorf("hallucinated consumes moved the receipt counter: %d", consumed)
	}
}

// TestOllamaConsolidation_TransientFailureRetriesThenFallsBack: an endpoint
// that accepts connections but never answers must be tried exactly twice
// (one patient retry), then degrade silently-but-audibly to decay+prune with
// the batch intact.
func TestOllamaConsolidation_TransientFailureRetriesThenFallsBack(t *testing.T) {
	oldTimeout := ollamaHTTPTimeout
	ollamaHTTPTimeout = 80 * time.Millisecond
	defer func() { ollamaHTTPTimeout = oldTimeout }()

	var reported []error
	dir := t.TempDir()
	s, err := Open(StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: 720 * time.Hour,
		OnConsolidateError: func(_ string, err error) {
			reported = append(reported, err)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	attempts := make(chan struct{}, 8)
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts <- struct{}{}
		<-block // hold until the client gives up
	}))
	defer func() { close(block); srv.Close() }()

	const agent = "timeout-agent"
	seedSixWeights(t, s, agent)
	cfg := &testConsolidateCfg{threshold: 4, halfLife: 720 * time.Hour,
		llm: "ollama", ollamaURL: srv.URL}

	start := time.Now()
	if err := s.Consolidate(context.Background(), agent, cfg); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("fallback took %s; the caller's patience must bound retries", elapsed)
	}
	if n := len(attempts); n != 2 {
		t.Errorf("endpoint hit %d times, want exactly 2 (initial + one retry)", n)
	}
	if len(reported) == 0 {
		t.Error("silent fallback: OnConsolidateError never fired")
	}
	facts, _ := s.List(agent)
	if len(facts) != 6 {
		t.Errorf("fallback lost data: %d facts, want 6", len(facts))
	}
}

// TestOllamaConsolidation_ServerErrorRetriesClientErrorDoesNot pins the retry
// policy split: 5xx is transient and earns the one retry; a 4xx is a
// deterministic rejection where retrying can only reproduce it.
func TestOllamaConsolidation_ServerErrorRetriesClientErrorDoesNot(t *testing.T) {
	openWithHook := func(hook func(string, error)) *Store {
		t.Helper()
		s, err := Open(StoreConfig{
			DataDir:            t.TempDir(),
			Embedder:           embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
			DecayHalfLife:      720 * time.Hour,
			OnConsolidateError: hook,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	t.Run("500-then-200-retries", func(t *testing.T) {
		var calls int
		s := openWithHook(func(string, error) {})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			if calls == 1 {
				http.Error(w, "warming up", http.StatusInternalServerError)
				return
			}
			fmt.Fprint(w, `{"response":"{\"summary\":\"retry arrived.\",\"consumes\":[]}"}`)
		}))
		defer srv.Close()

		const agent = "retry-5xx"
		seedSixWeights(t, s, agent)
		cfg := &testConsolidateCfg{threshold: 4, halfLife: 720 * time.Hour,
			llm: "ollama", ollamaURL: srv.URL}
		if err := s.Consolidate(context.Background(), agent, cfg); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Errorf("5xx earned %d attempts, want 2 (initial + one retry)", calls)
		}
		// The retried proposal carries an empty consumes list, so it must be
		// discarded at validation — proving validation runs after retries.
		if id := s.factIDByText(agent, "retry arrived."); id != "" {
			t.Error("empty-consumes proposal was applied after retry")
		}
	})

	t.Run("400-single-attempt", func(t *testing.T) {
		var reported []error
		var calls int
		s := openWithHook(func(_ string, err error) { reported = append(reported, err) })
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			http.Error(w, "bad request", http.StatusBadRequest)
		}))
		defer srv.Close()

		const agent = "no-retry-4xx"
		seedSixWeights(t, s, agent)
		cfg := &testConsolidateCfg{threshold: 4, halfLife: 720 * time.Hour,
			llm: "ollama", ollamaURL: srv.URL}
		if err := s.Consolidate(context.Background(), agent, cfg); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Errorf("4xx earned %d attempts, want exactly 1", calls)
		}
		if len(reported) == 0 {
			t.Error("4xx fallback was silent")
		}
	})
}

// TestConsolidation_TombstonePropertyAcrossCycles is playbook property test:
// across many cycles nobody disappears without a receipt. Any fact present in
// snapshot k but gone from k+1 must already have been superseded in k, and
// its receipt target must have existed. Pinned facts survive byte-identical.
func TestConsolidation_TombstonePropertyAcrossCycles(t *testing.T) {
	f := newOllamaFixture(t, func(r *ollamaRequest) string {
		return jsonProposal(fmt.Sprintf("consolidated digest %d.", time.Now().UnixNano()), idsInPrompt(r.Prompt))
	})
	const agent = "property-agent"

	ctx := context.Background()
	base := time.Now().UTC().Add(-24 * 30 * time.Hour)
	current := base
	f.store.now = func() time.Time { return current }

	texts := make([]string, 0, 10)
	for i := 0; i < 9; i++ {
		txt := fmt.Sprintf("cyclic observation number %d about topic %d", i, i%3)
		texts = append(texts, txt)
		if err := f.store.Put(ctx, agent, txt); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Put(ctx, agent, pinnedText); err != nil {
		t.Fatal(err)
	}
	facts, _ := f.store.List(agent)
	for i := range facts {
		if facts[i].Text == pinnedText {
			facts[i].Pinned = true
			facts[i].PinnedAt = current
			if err := f.store.UpdateFact(agent, facts[i]); err != nil {
				t.Fatal(err)
			}
		} else {
			// Spread ages so each cycle's decay produces a fresh weak half.
			facts[i].CreatedAt = current.Add(-time.Duration(i+1) * 24 * time.Hour)
			facts[i].AccessedAt = facts[i].CreatedAt
			facts[i].Weight = 0.9 - 0.05*float64(i)
			if err := f.store.UpdateFact(agent, facts[i]); err != nil {
				t.Fatal(err)
			}
		}
	}

	snapshot := func() map[string]Fact {
		fs, _ := f.store.List(agent)
		m := make(map[string]Fact, len(fs))
		for _, x := range fs {
			m[x.ID] = x
		}
		return m
	}

	prev := snapshot()
	for cycle := 0; cycle < 8; cycle++ {
		current = current.Add(20 * 24 * time.Hour)
		if err := f.store.Consolidate(ctx, agent, f.cfg(3)); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		next := snapshot()

		for id, old := range prev {
			if _, alive := next[id]; alive {
				continue
			}
			if !old.IsSuperseded() {
				t.Fatalf("cycle %d: fact %s (%q) disappeared without ever being superseded", cycle, id, old.Text)
			}
			if _, ok := prev[old.SupersededBy]; !ok {
				t.Fatalf("cycle %d: receipt of %s pointed at %s which did not exist", cycle, id, old.SupersededBy)
			}
		}
		// Invariant I-1 holds across the whole horizon.
		for id, cur := range next {
			if cur.Text == pinnedText && (!cur.Pinned || cur.Weight <= 0) {
				t.Fatalf("cycle %d: pinned fact mutated: %+v", cycle, cur)
			}
			_ = id
		}
		prev = next
	}

	cycles, _ := f.store.ConsolidationCounters()
	if cycles == 0 {
		t.Fatal("no consolidation ever applied; property vacuously true")
	}
}

// countingTypedExtractor and countingGraph feed the watermark tests.
type countingTypedExtractor struct {
	typedCalls int
	lastText   string
}

func (e *countingTypedExtractor) ExtractIDs(text string) ([]string, error) {
	e.typedCalls++
	e.lastText = text
	return []string{"entity-x"}, nil
}

func (e *countingTypedExtractor) ExtractTyped(text string) ([]EntityRef, []EntityLink, error) {
	e.typedCalls++
	e.lastText = text
	return []EntityRef{{ID: "entity-x", Label: "Entity X", EntityType: "concept"}}, nil, nil
}

type countingGraph struct {
	upserts int
}

func (g *countingGraph) UpsertNode(id, label, entityType string) error {
	g.upserts++
	return nil
}

func (g *countingGraph) NeighborTexts(string, int) ([]string, error) { return nil, nil }

// TestExtraction_WatermarkSkipsUnchangedAndSuperseded covers A7 plus the
// tombstone filter: unchanged live facts are extracted once and never again;
// editing a fact re-extracts exactly that one; retired facts are never fed
// to the extractor at all.
func TestExtraction_WatermarkSkipsUnchangedAndSuperseded(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ex := &countingTypedExtractor{}
	g := &countingGraph{}
	s.SetKG(g, ex)

	ctx := context.Background()
	const agent = "wm-agent"
	for _, txt := range []string{"watermark fact one", "watermark fact two", "watermark fact three"} {
		if err := s.Put(ctx, agent, txt); err != nil {
			t.Fatal(err)
		}
	}
	cfg := defaultTestCfg()

	if err := s.Consolidate(ctx, agent, cfg); err != nil {
		t.Fatal(err)
	}
	if ex.typedCalls != 3 {
		t.Fatalf("first pass extracted %d facts, want 3", ex.typedCalls)
	}

	if err := s.Consolidate(ctx, agent, cfg); err != nil {
		t.Fatal(err)
	}
	if ex.typedCalls != 3 {
		t.Errorf("unchanged store re-extracted: %d calls total, want 3", ex.typedCalls)
	}

	// Edit one fact's text; retire another. Next pass: exactly one extract,
	// none for the tombstone.
	facts, _ := s.List(agent)
	var edited, retired Fact
	n := 0
	for _, fct := range facts {
		switch n {
		case 0:
			edited = fct
		case 1:
			retired = fct
		}
		n++
	}
	edited.Text += " (revised)"
	if err := s.UpdateFact(agent, edited); err != nil {
		t.Fatal(err)
	}
	retired.SupersededBy = SupersededByAgent
	if err := s.UpdateFact(agent, retired); err != nil {
		t.Fatal(err)
	}

	if err := s.Consolidate(ctx, agent, cfg); err != nil {
		t.Fatal(err)
	}
	if ex.typedCalls != 4 {
		t.Errorf("third pass made %d total calls, want 4 (only the edited fact)", ex.typedCalls)
	}
	if ex.lastText != edited.Text {
		t.Errorf("extractor saw %q, want the revised text", ex.lastText)
	}
}

// TestDelete_ClearsExtractionWatermark: a deleted fact's signature must go
// with it, so re-adding the same text later extracts afresh under its new ID.
func TestDelete_ClearsExtractionWatermark(t *testing.T) {
	s, cleanup := openTestStore(t)
	defer cleanup()
	ex := &countingTypedExtractor{}
	s.SetKG(&countingGraph{}, ex)

	ctx := context.Background()
	const agent = "del-wm-agent"
	if err := s.Put(ctx, agent, "ephemeral fact"); err != nil {
		t.Fatal(err)
	}
	if err := s.Consolidate(ctx, agent, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	facts, _ := s.List(agent)
	id := facts[0].ID
	if _, ok := s.extractedSignature(agent, id); !ok {
		t.Fatal("signature not recorded on first extraction")
	}

	if err := s.Delete(agent, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.extractedSignature(agent, id); ok {
		t.Error("watermark outlived its fact")
	}

	// Same text, new ULID: must be treated as unseen.
	if err := s.Put(ctx, agent, "ephemeral fact"); err != nil {
		t.Fatal(err)
	}
	if err := s.Consolidate(ctx, agent, defaultTestCfg()); err != nil {
		t.Fatal(err)
	}
	if ex.typedCalls != 2 {
		t.Errorf("extractor called %d times, want 2 (once per incarnation)", ex.typedCalls)
	}
}
