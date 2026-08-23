// Command retrieval_quality measures whether GrayMatter returns the *right*
// facts, against the baseline production actually uses.
//
// benchmarks/token_count answers "how many tokens does this save against
// injecting everything", which is the weakest comparison available: a system
// returning eight facts at random scores the same 90% reduction, because
// nothing there checks relevance. This benchmark checks relevance, and
// compares against a real sliding window rather than against full-history
// injection alone.
//
// Usage:
//
//	go run ./benchmarks/retrieval_quality
//
// No network, no LLM, no API key. Keyword embedder only, fixtures frozen,
// insertion order fixed by the corpus file. The predictions this is measured
// against were committed before it existed — see benchmarks/RESULTS.md and
// `git log --follow benchmarks/RESULTS.md`.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/angelnicolasc/graymatter/benchmarks/internal/tokens"
	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const (
	agentID = "bench-agent"
	budget  = 8 // fixed-K budget: GrayMatter TopK and the sliding window size

	// sessionSpan is how far apart two consecutive sessions are placed on the
	// timeline. One day per session puts the oldest planted fact ~99 days
	// before the queries, which is what makes recency and relevance disagree.
	sessionSpan = 24 * time.Hour

	// adaptiveMinRelevance drives the adaptive protocol. Results from this
	// mode are never compared against a fixed-K baseline: that would measure
	// the budget rather than the ranking.
	adaptiveMinRelevance = 0.5
)

// fact is one line of benchmarks/fixtures/corpus-v1.jsonl.
type fact struct {
	ID      string `json:"id"`
	Session int    `json:"session"`
	Domain  string `json:"domain"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
}

// query is one line of benchmarks/fixtures/queries-v1.jsonl.
type query struct {
	ID        string   `json:"id"`
	Domain    string   `json:"domain"`
	AskedAt   int      `json:"asked_at_session"`
	Text      string   `json:"text"`
	Gold      []string `json:"gold"`
	Forbidden []string `json:"forbidden"`
}

// result is one system's measured behaviour over the whole query set.
type result struct {
	System      string
	Mode        string // "fixed-K" or "adaptive"
	HitRate     float64
	DeadRate    float64
	AvgTokens   float64
	AvgReturned float64
	P50, P95    time.Duration
	PerQuery    []queryOutcome
	Returned    map[string][]string // query id -> returned fact ids
}

// queryOutcome is one query under one system, kept so the report can say which
// query missed rather than only that something did.
type queryOutcome struct {
	QueryID string
	Hit     bool
	Dead    bool
}

func main() {
	fixtures := flag.String("fixtures", "benchmarks/fixtures", "directory holding corpus-v1.jsonl and queries-v1.jsonl")
	flag.Parse()

	suite, err := runAll(*fixtures)
	if err != nil {
		fail("%v", err)
	}
	report(suite)
}

// suite is everything one full run produces. Returned rather than printed so
// the test that gates the published tables can call it.
type suite struct {
	Corpus   []fact
	Queries  []query
	FixedK   []result
	Adaptive result
}

// byName returns the measured result for one system, so callers do not index
// into FixedK by position.
func (s suite) byName(name string) (result, bool) {
	for _, r := range s.FixedK {
		if r.System == name {
			return r, true
		}
	}
	if s.Adaptive.System == name {
		return s.Adaptive, true
	}
	return result{}, false
}

// runAll loads the fixtures and measures every system.
func runAll(fixtureDir string) (suite, error) {
	corpus, err := loadCorpus(filepath.Join(fixtureDir, "corpus-v1.jsonl"))
	if err != nil {
		return suite{}, fmt.Errorf("load corpus: %w", err)
	}
	queries, err := loadQueries(filepath.Join(fixtureDir, "queries-v1.jsonl"))
	if err != nil {
		return suite{}, fmt.Errorf("load queries: %w", err)
	}
	if err := validateFixtures(corpus, queries); err != nil {
		return suite{}, fmt.Errorf("fixtures are inconsistent: %w", err)
	}

	out := suite{Corpus: corpus, Queries: queries}
	out.FixedK = []result{
		runFullHistory(corpus, queries),
		runWindow(corpus, queries, budget),
	}

	gm, err := runGrayMatter(corpus, queries, "graymatter-fixed-k", "fixed-K", memory.StoreConfig{})
	if err != nil {
		return suite{}, fmt.Errorf("graymatter fixed-k: %w", err)
	}
	out.FixedK = append(out.FixedK, gm)

	recency, err := runGrayMatter(corpus, queries, "graymatter-recency-only", "fixed-K", memory.StoreConfig{
		SignalWeights: &memory.SignalWeights{Vector: 0, Keyword: 0, Recency: 1},
	})
	if err != nil {
		return suite{}, fmt.Errorf("graymatter recency-only: %w", err)
	}
	out.FixedK = append(out.FixedK, recency)

	out.Adaptive, err = runGrayMatter(corpus, queries, "graymatter-adaptive", "adaptive", memory.StoreConfig{
		MinRelevance: adaptiveMinRelevance,
	})
	if err != nil {
		return suite{}, fmt.Errorf("graymatter adaptive: %w", err)
	}
	return out, nil
}

// ---- systems ---------------------------------------------------------------

// runFullHistory injects everything. Kept for continuity with token_count.
func runFullHistory(corpus []fact, queries []query) result {
	return measure("full-history", "fixed-K", queries, func(query) ([]string, time.Duration) {
		start := time.Now()
		ids := make([]string, 0, len(corpus))
		for _, f := range corpus {
			ids = append(ids, f.ID)
		}
		return ids, time.Since(start)
	}, corpus)
}

// runWindow is a real sliding window: the last k facts by insertion order,
// nothing else. Implemented directly rather than simulated with recency-only
// signal weights, because the recency ablation is used to *check* the claim
// that a window is a special case of the ranking. Simulating the baseline with
// the thing under test would assume the conclusion.
func runWindow(corpus []fact, queries []query, k int) result {
	ordered := make([]fact, len(corpus))
	copy(ordered, corpus)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Session < ordered[j].Session })

	return measure(fmt.Sprintf("window-%d", k), "fixed-K", queries, func(query) ([]string, time.Duration) {
		start := time.Now()
		from := len(ordered) - k
		if from < 0 {
			from = 0
		}
		ids := make([]string, 0, k)
		for _, f := range ordered[from:] {
			ids = append(ids, f.ID)
		}
		return ids, time.Since(start)
	}, corpus)
}

// runGrayMatter builds a store from the corpus and measures Recall.
func runGrayMatter(corpus []fact, queries []query, name, mode string, cfg memory.StoreConfig) (result, error) {
	dir, err := os.MkdirTemp("", "graymatter-quality-*")
	if err != nil {
		return result{}, err
	}
	defer os.RemoveAll(dir)

	cfg.DataDir = dir
	cfg.Embedder = embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword})
	if cfg.DecayHalfLife == 0 {
		cfg.DecayHalfLife = 720 * time.Hour
	}

	store, err := memory.Open(cfg)
	if err != nil {
		return result{}, err
	}
	defer store.Close()

	ctx := context.Background()
	textToID, err := seed(ctx, store, corpus)
	if err != nil {
		return result{}, err
	}
	if err := applySupersede(store, corpus, textToID); err != nil {
		return result{}, err
	}

	return measure(name, mode, queries, func(q query) ([]string, time.Duration) {
		start := time.Now()
		got, rerr := store.Recall(ctx, agentID, q.Text, budget)
		elapsed := time.Since(start)
		if rerr != nil {
			fail("recall %q: %v", q.ID, rerr)
		}
		ids := make([]string, 0, len(got))
		for _, text := range got {
			if id, ok := textToID[text]; ok {
				ids = append(ids, id)
			}
		}
		return ids, elapsed
	}, corpus), nil
}

// seed writes the corpus and backdates each fact to its session on the
// timeline. Timestamps go through the public API — Put, then List and
// UpdateFact — so this benchmark needs no access to store internals and makes
// no change to the engine.
func seed(ctx context.Context, store *memory.Store, corpus []fact) (map[string]string, error) {
	ordered := make([]fact, len(corpus))
	copy(ordered, corpus)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Session < ordered[j].Session })

	latest := ordered[len(ordered)-1].Session
	now := time.Now().UTC()

	for _, f := range ordered {
		if err := store.Put(ctx, agentID, f.Text); err != nil {
			return nil, fmt.Errorf("put %s: %w", f.ID, err)
		}
	}

	stored, err := store.List(agentID)
	if err != nil {
		return nil, err
	}
	byText := make(map[string]memory.Fact, len(stored))
	for _, sf := range stored {
		byText[sf.Text] = sf
	}

	textToID := make(map[string]string, len(corpus))
	for _, f := range ordered {
		sf, ok := byText[f.Text]
		if !ok {
			return nil, fmt.Errorf("fact %s did not survive the write", f.ID)
		}
		// Session N sits (latest-N) spans before the query time.
		at := now.Add(-time.Duration(latest-f.Session) * sessionSpan)
		sf.CreatedAt = at
		sf.AccessedAt = at
		if err := store.UpdateFact(agentID, sf); err != nil {
			return nil, fmt.Errorf("backdate %s: %w", f.ID, err)
		}
		textToID[f.Text] = f.ID
	}
	return textToID, nil
}

// applySupersede tombstones every kind=stale fact, pointing at the
// kind=replacement fact, exactly as memory_reflect does.
func applySupersede(store *memory.Store, corpus []fact, textToID map[string]string) error {
	var replacementText string
	for _, f := range corpus {
		if f.Kind == "replacement" {
			replacementText = f.Text
		}
	}
	stored, err := store.List(agentID)
	if err != nil {
		return err
	}
	byText := make(map[string]memory.Fact, len(stored))
	for _, sf := range stored {
		byText[sf.Text] = sf
	}
	replacementID := ""
	if replacementText != "" {
		if sf, ok := byText[replacementText]; ok {
			replacementID = sf.ID
		}
	}

	for _, f := range corpus {
		if f.Kind != "stale" {
			continue
		}
		sf, ok := byText[f.Text]
		if !ok {
			return fmt.Errorf("stale fact %s not found in store", f.ID)
		}
		sf.SupersededBy = replacementID
		if sf.SupersededBy == "" {
			sf.SupersededBy = memory.SupersededByAgent
		}
		sf.Weight = 0
		if err := store.UpdateFact(agentID, sf); err != nil {
			return fmt.Errorf("supersede %s: %w", f.ID, err)
		}
	}
	return nil
}

// ---- measurement -----------------------------------------------------------

func measure(name, mode string, queries []query, run func(query) ([]string, time.Duration), corpus []fact) result {
	byID := make(map[string]fact, len(corpus))
	for _, f := range corpus {
		byID[f.ID] = f
	}

	var hits, dead, totalTokens, totalReturned int
	latencies := make([]time.Duration, 0, len(queries))
	outcomes := make([]queryOutcome, 0, len(queries))
	returned := make(map[string][]string, len(queries))

	for _, q := range queries {
		ids, elapsed := run(q)
		latencies = append(latencies, elapsed)

		texts := make([]string, 0, len(ids))
		for _, id := range ids {
			texts = append(texts, byID[id].Text)
		}
		totalTokens += tokens.ApproxAll(texts)
		totalReturned += len(ids)

		got := make(map[string]bool, len(ids))
		for _, id := range ids {
			got[id] = true
		}
		outcome := queryOutcome{QueryID: q.ID}
		for _, g := range q.Gold {
			if got[g] {
				hits++
				outcome.Hit = true
				break
			}
		}
		for _, bad := range q.Forbidden {
			if got[bad] {
				dead++
				outcome.Dead = true
				break
			}
		}
		outcomes = append(outcomes, outcome)
		returned[q.ID] = ids
	}

	n := float64(len(queries))
	p50, p95 := percentiles(latencies)
	return result{
		System:      name,
		Mode:        mode,
		HitRate:     float64(hits) / n * 100,
		DeadRate:    float64(dead) / n * 100,
		AvgTokens:   float64(totalTokens) / n,
		AvgReturned: float64(totalReturned) / n,
		P50:         p50,
		P95:         p95,
		PerQuery:    outcomes,
		Returned:    returned,
	}
}

func percentiles(d []time.Duration) (p50, p95 time.Duration) {
	if len(d) == 0 {
		return 0, 0
	}
	s := make([]time.Duration, len(d))
	copy(s, d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)*50/100], s[min(len(s)*95/100, len(s)-1)]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- reporting -------------------------------------------------------------

func report(s suite) {
	corpus, queries, fixedK, adaptive := s.Corpus, s.Queries, s.FixedK, s.Adaptive
	var gold, stale int
	for _, f := range corpus {
		switch f.Kind {
		case "gold":
			gold++
		case "stale":
			stale++
		}
	}

	fmt.Println()
	fmt.Println("GrayMatter Retrieval Quality Benchmark")
	fmt.Println()
	fmt.Printf("Corpus:   %d facts, %d planted early as gold, %d superseded\n", len(corpus), gold, stale)
	fmt.Printf("Queries:  %d across %d domains, all asked at the newest session\n", len(queries), countDomains(queries))
	fmt.Printf("Budget:   %d facts (GrayMatter TopK and sliding-window size)\n", budget)
	fmt.Printf("Embedder: keyword (no LLM, no network, no API key)\n")
	fmt.Println()

	fmt.Println("── fixed-K protocol: equal budget, apples to apples ──")
	fmt.Println()
	header()
	for _, r := range fixedK {
		row(r)
	}
	fmt.Println()

	fmt.Println("── adaptive protocol: reported separately, never compared above ──")
	fmt.Println()
	fmt.Printf("MinRelevance=%.2f returns a variable number of facts, so comparing it\n", adaptiveMinRelevance)
	fmt.Println("against a fixed-K baseline would measure the budget, not the ranking.")
	fmt.Println()
	header()
	row(adaptive)
	fmt.Println()

	perQuery(fixedK, queries)
	ablationCheck(fixedK)

	fmt.Println("Tokens are words × 1.33, the same approximation ./benchmarks/token_count uses.")
	fmt.Println("HitRate: queries where a planted gold fact was returned.")
	fmt.Println("Dead:    queries that returned a fact the store knows is superseded.")
	fmt.Println()
}

func header() {
	fmt.Printf("%-26s  %-9s  %8s  %6s  %9s  %8s  %9s  %9s\n",
		"System", "Mode", "HitRate", "Dead", "Tokens/q", "Facts/q", "p50", "p95")
	fmt.Println(strings.Repeat("─", 104))
}

func row(r result) {
	fmt.Printf("%-26s  %-9s  %7.0f%%  %5.0f%%  %9.0f  %8.1f  %9s  %9s\n",
		r.System, r.Mode, r.HitRate, r.DeadRate, r.AvgTokens, r.AvgReturned,
		r.P50.Round(time.Microsecond), r.P95.Round(time.Microsecond))
}

func countDomains(queries []query) int {
	seen := map[string]bool{}
	for _, q := range queries {
		seen[q.Domain] = true
	}
	return len(seen)
}

// ---- fixtures --------------------------------------------------------------

func loadCorpus(path string) ([]fact, error) {
	var out []fact
	err := eachJSONLine(path, func(line []byte) error {
		var f fact
		if err := json.Unmarshal(line, &f); err != nil {
			return err
		}
		out = append(out, f)
		return nil
	})
	return out, err
}

func loadQueries(path string) ([]query, error) {
	var out []query
	err := eachJSONLine(path, func(line []byte) error {
		var q query
		if err := json.Unmarshal(line, &q); err != nil {
			return err
		}
		out = append(out, q)
		return nil
	})
	return out, err
}

func eachJSONLine(path string, fn func([]byte) error) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := fn([]byte(line)); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return sc.Err()
}

// validateFixtures refuses to run against a corpus that cannot answer its own
// questions. Silently measuring a HitRate of zero because a gold ID was
// renamed would look like a finding rather than a broken fixture.
func validateFixtures(corpus []fact, queries []query) error {
	ids := make(map[string]bool, len(corpus))
	texts := make(map[string]bool, len(corpus))
	for _, f := range corpus {
		if ids[f.ID] {
			return fmt.Errorf("duplicate fact id %s", f.ID)
		}
		if texts[f.Text] {
			return fmt.Errorf("duplicate fact text for %s; ids are recovered by text", f.ID)
		}
		ids[f.ID] = true
		texts[f.Text] = true
	}
	if len(queries) < 5 {
		return fmt.Errorf("need at least 5 queries, have %d", len(queries))
	}
	if d := countDomains(queries); d < 3 {
		return fmt.Errorf("need queries from at least 3 domains, have %d", d)
	}
	for _, q := range queries {
		if len(q.Gold) == 0 {
			return fmt.Errorf("query %s has no gold facts", q.ID)
		}
		for _, g := range q.Gold {
			if !ids[g] {
				return fmt.Errorf("query %s references unknown gold fact %s", q.ID, g)
			}
		}
		for _, b := range q.Forbidden {
			if !ids[b] {
				return fmt.Errorf("query %s references unknown forbidden fact %s", q.ID, b)
			}
		}
	}
	return nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// perQuery prints the hit/miss grid. An aggregate HitRate says something
// missed; this says which, which is the part that directs engineering.
func perQuery(results []result, queries []query) {
	fmt.Println("── per query (· hit, x miss, D returned a superseded fact) ──")
	fmt.Println()
	fmt.Printf("%-26s", "System")
	for _, q := range queries {
		fmt.Printf("  %-4s", q.ID)
	}
	fmt.Println()
	fmt.Println(strings.Repeat("─", 26+6*len(queries)))
	for _, r := range results {
		fmt.Printf("%-26s", r.System)
		for _, o := range r.PerQuery {
			mark := "x"
			if o.Hit {
				mark = "·"
			}
			if o.Dead {
				mark += "D"
			}
			fmt.Printf("  %-4s", mark)
		}
		fmt.Println()
	}
	fmt.Println()
	for _, q := range queries {
		fmt.Printf("  %-4s [%s] %q\n", q.ID, q.Domain, q.Text)
	}
	fmt.Println()
}

// ablationCheck tests ADR-006 empirically. The ADR claims a sliding window is
// the special case of this ranking with all weight on recency. window-8 is
// implemented independently, so if the claim holds the two must return the
// same facts for every query — and if they diverge, the ADR is wrong and says
// so here rather than in prose nobody re-checks.
func ablationCheck(results []result) {
	var window, ablation *result
	for i := range results {
		switch results[i].System {
		case "window-8":
			window = &results[i]
		case "graymatter-recency-only":
			ablation = &results[i]
		}
	}
	if window == nil || ablation == nil {
		return
	}

	fmt.Println("── ADR-006 check: is a sliding window the recency-only special case? ──")
	fmt.Println()
	agree := true
	for qid, wids := range window.Returned {
		aids, ok := ablation.Returned[qid]
		if !ok || !sameSet(wids, aids) {
			agree = false
			fmt.Printf("  %s: window and recency-only ablation returned different facts\n", qid)
		}
	}
	if agree {
		fmt.Println("  CONFIRMED: for every query, SignalWeights{0,0,1} returns exactly the")
		fmt.Println("  facts an independently implemented sliding window returns.")
	} else {
		fmt.Println("  NOT CONFIRMED. ADR-006 overstates the equivalence and needs correcting.")
	}
	fmt.Println()
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
		if m[x] < 0 {
			return false
		}
	}
	return true
}
