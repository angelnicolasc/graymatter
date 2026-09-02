// Command revision_currency measures the question a memory system is actually
// asked between sessions: after the same value was stated, corrected, and
// sometimes corrected again, does the caller get the one that holds?
//
// benchmarks/retrieval_quality answers "does it return the right facts" on a
// corpus whose tombstones are written by reaching into the store. That made it
// measure a configuration a user could not reach: until `graymatter revise`
// existed, nothing outside the library could create a supersede edge. This
// suite measures both sides of that line — the same corpus with and without
// the edges — through the same public write path the CLI uses.
//
// The endpoint is compound on purpose:
//
//	A  no retired sibling ranks above the current value
//	B  the current value is inside the injected top-8
//
// A alone is gameable and was gamed during development: weighting recency
// heavily scores a perfect A by pushing the whole family below the noise
// floor, where the caller sees neither value. A suite that reported A alone
// would have called that an improvement.
//
// Usage:
//
//	go run ./benchmarks/revision_currency
//
// No network, no LLM, no API key, no randomness. Keyword embedder, enumerated
// filler, scripted timeline. The predictions this is measured against were
// committed before it ran — see PREDICTIONS.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const (
	agentID     = "revision-bench"
	topK        = 8                // what the injection hook shows the agent
	corpusSize  = 600              // live facts in the flat arm
	factSpacing = 1 * time.Hour    // scripted gap between consecutive writes
	halfLife    = 8760 * time.Hour // a year: decay must not confound the currency question
)

func main() {
	jsonOut := flag.Bool("json", false, "emit the result as JSON")
	flag.Parse()
	if err := run(os.Stdout, *jsonOut); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// ---- corpus ----------------------------------------------------------------

// buildCorpus returns every statement in chronological order. Revision
// statements keep their relative order — a correction can never land before
// what it corrects — and the filler is enumerated, never sampled.
// buildCorpus builds the published 600-fact corpus. buildCorpusOfSize is the
// same construction at any size: only the filler grows, so the 35 families and
// their probes stay byte-identical and the scale points stay comparable.
func buildCorpus() []string { return buildCorpusOfSize(corpusSize) }

func buildCorpusOfSize(corpusSize int) []string {
	probeTokens := make([]string, 0, 128)
	for _, f := range families {
		probeTokens = append(probeTokens, strings.ToLower(f.Correct))
		for _, s := range f.Stale {
			probeTokens = append(probeTokens, strings.ToLower(s))
		}
	}
	clean := func(s string) bool {
		low := strings.ToLower(s)
		for _, tok := range probeTokens {
			if strings.Contains(low, tok) {
				return false
			}
		}
		return true
	}

	var timeline []string
	for _, f := range families {
		timeline = append(timeline, f.Statements...)
	}

	filler := make([]string, 0, corpusSize)
	seen := make(map[string]bool)
	add := func(s string) {
		if !seen[s] && clean(s) {
			seen[s] = true
			filler = append(filler, s)
		}
	}
	for _, s := range nearMiss {
		add(s)
	}
	// Enumerated, not sampled: template × service × person × a counter that
	// never collides with a probe token.
	for n := 2; len(filler) < corpusSize-len(timeline); n++ {
		for _, svc := range fillServices {
			p := fillPeople[n%len(fillPeople)]
			add(fmt.Sprintf("the %s service runs %d replicas", svc, n))
			add(fmt.Sprintf("%s documented the %s runbook", p, svc))
			add(fmt.Sprintf("%s logs rotate after %d days", svc, n))
			add(fmt.Sprintf("the %s health endpoint is on port %d", svc, n))
			add(fmt.Sprintf("the %s dashboard refreshes every %d seconds", svc, n))
			if len(filler) >= corpusSize-len(timeline) {
				break
			}
		}
		if n > 5000 {
			break // enumeration exhausted; the corpus is short but deterministic
		}
	}
	filler = filler[:corpusSize-len(timeline)]

	out := make([]string, 0, corpusSize)
	fi, step := 0, len(filler)/len(timeline)
	for _, stmt := range timeline {
		end := fi + step
		if end > len(filler) {
			end = len(filler)
		}
		out = append(out, filler[fi:end]...)
		fi = end
		out = append(out, stmt)
	}
	return append(out, filler[fi:]...)
}

// validate refuses to measure a corpus that cannot be scored honestly: every
// probe token must identify exactly one statement.
func validate(corpus []string) error {
	for _, f := range families {
		for _, tok := range append([]string{f.Correct}, f.Stale...) {
			n := 0
			for _, line := range corpus {
				if strings.Contains(strings.ToLower(line), strings.ToLower(tok)) {
					n++
				}
			}
			if n != 1 {
				return fmt.Errorf("%s: token %q matches %d statements, want exactly 1", f.ID, tok, n)
			}
		}
	}
	return nil
}

// ---- the two arms ----------------------------------------------------------

func openStore(dir string) (*memory.Store, error) {
	return memory.Open(memory.StoreConfig{
		DataDir:       dir,
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: halfLife,
	})
}

// scriptTimeline rewrites CreatedAt so every fact sits at a known point on a
// fixed timeline, in corpus order. Without it, facts written inside one clock
// tick share a timestamp and tie-break by random ULID, and the recency signal
// — the one this suite is about — would differ between runs.
func scriptTimeline(store *memory.Store, corpus []string) error {
	order := make(map[string]int, len(corpus))
	for i, line := range corpus {
		order[line] = i
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	facts, err := store.List(agentID)
	if err != nil {
		return err
	}
	for _, f := range facts {
		idx, ok := order[f.Text]
		if !ok {
			return fmt.Errorf("fact not in corpus: %q", f.Text)
		}
		f.CreatedAt = base.Add(time.Duration(idx) * factSpacing)
		if err := store.UpdateFact(agentID, f); err != nil {
			return err
		}
	}
	return nil
}

// buildFlat is the store an agent produces today when it writes each
// correction as an independent fact: every version live, nothing linked.
func buildFlat(ctx context.Context, dir string, corpus []string) (*memory.Store, error) {
	store, err := openStore(dir)
	if err != nil {
		return nil, err
	}
	for _, line := range corpus {
		if err := store.Put(ctx, agentID, line); err != nil {
			return nil, err
		}
	}
	return store, scriptTimeline(store, corpus)
}

// buildRevised is the same history recorded as revisions. Only the first
// statement of each family is seeded; every later version enters through
// Store.Revise, the call `graymatter revise` makes. Fact count is identical to
// the flat arm — the only difference is that the edges exist.
func buildRevised(ctx context.Context, dir string, corpus []string) (*memory.Store, error) {
	later := make(map[string]bool)
	for _, f := range families {
		for _, s := range f.Statements[1:] {
			later[s] = true
		}
	}
	store, err := openStore(dir)
	if err != nil {
		return nil, err
	}
	for _, line := range corpus {
		if later[line] {
			continue
		}
		if err := store.Put(ctx, agentID, line); err != nil {
			return nil, err
		}
	}
	for _, f := range families {
		for i := 0; i < len(f.Statements)-1; i++ {
			old, next := f.Statements[i], f.Statements[i+1]
			facts, err := store.List(agentID)
			if err != nil {
				return nil, err
			}
			var victims []memory.Fact
			for _, cand := range facts {
				if cand.Text == old && !cand.IsSuperseded() {
					victims = append(victims, cand)
				}
			}
			if len(victims) == 0 {
				return nil, fmt.Errorf("%s: nothing to revise for %q", f.ID, old)
			}
			if _, err := store.Revise(ctx, agentID, next, victims...); err != nil {
				return nil, fmt.Errorf("%s: %w", f.ID, err)
			}
		}
	}
	return store, scriptTimeline(store, corpus)
}

// ---- measurement -----------------------------------------------------------

type armResult struct {
	Name        string  `json:"name"`
	A           int     `json:"current_outranks_stale"`
	B           int     `json:"current_injected"`
	Primary     int     `json:"primary"`
	StaleShown  int     `json:"stale_facts_returned"`
	UsefulAt8   float64 `json:"useful_at_8"`
	Paraphrased float64 `json:"primary_paraphrased"`
	Literal     float64 `json:"primary_literal"`
	perProbe    []int
}

func measure(ctx context.Context, name string, store *memory.Store) (armResult, error) {
	r := armResult{Name: name, perProbe: make([]int, 0, len(families))}
	var useful, paraOK, litOK, paraN, litN int
	for _, f := range families {
		// A is a question about the ranking, B about the injected slice, so the
		// full ranking has to be pulled and the top-k taken from it. Asking for
		// top-k alone collapses A into B: a current value ranked 11th reads as
		// "outranked by nothing" simply because nothing else came back either.
		receipts, err := store.RecallExplain(ctx, agentID, f.Query, corpusSize)
		if err != nil {
			return r, err
		}
		curAt, staleAt := -1, -1
		for i, rec := range receipts {
			low := strings.ToLower(rec.Text)
			if curAt < 0 && strings.Contains(low, strings.ToLower(f.Correct)) {
				curAt = i
			}
			for _, s := range f.Stale {
				if strings.Contains(low, strings.ToLower(s)) && staleAt < 0 {
					staleAt = i
				}
			}
			if i < topK {
				if strings.Contains(low, strings.ToLower(f.Correct)) {
					continue
				}
				for _, s := range f.Stale {
					if strings.Contains(low, strings.ToLower(s)) {
						r.StaleShown++ // a retired fact inside the injected block
						break
					}
				}
			}
		}
		for i, rec := range receipts {
			if i >= topK {
				break
			}
			if rec.Ranks.KeywordRank > 0 || rec.Ranks.VectorRank > 0 {
				useful++
			}
		}
		a := curAt >= 0 && (staleAt < 0 || curAt < staleAt)
		b := curAt >= 0 && curAt < topK
		if a {
			r.A++
		}
		if b {
			r.B++
		}
		ok := 0
		if a && b {
			r.Primary++
			ok = 1
		}
		r.perProbe = append(r.perProbe, ok)
		if f.Paraphrased {
			paraN++
			paraOK += ok
		} else {
			litN++
			litOK += ok
		}
	}
	r.UsefulAt8 = float64(useful) / float64(len(families))
	r.Paraphrased = float64(paraOK) / float64(paraN)
	r.Literal = float64(litOK) / float64(litN)
	return r, nil
}

// ---- statistics ------------------------------------------------------------

// wilson is the interval for a proportion at small n. A bare "26 of 35" invites
// a claim the sample cannot support.
func wilson(k, n int) (float64, float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.96
	p := float64(k) / float64(n)
	fn := float64(n)
	d := 1 + z*z/fn
	c := (p + z*z/(2*fn)) / d
	rad := z * math.Sqrt(p*(1-p)/fn+z*z/(4*fn*fn)) / d
	return math.Max(0, c-rad), math.Min(1, c+rad)
}

// mcnemarExact compares the two arms on the probes where they disagree. The
// probes both arms get right, or both get wrong, carry no information about
// which arm is better, so they are not counted.
func mcnemarExact(b, c int) float64 {
	n := b + c
	if n == 0 {
		return 1
	}
	lo := b
	if c < lo {
		lo = c
	}
	sum := 0.0
	for i := 0; i <= lo; i++ {
		sum += float64(binom(n, i))
	}
	p := 2 * sum * math.Pow(0.5, float64(n))
	return math.Min(1, p)
}

func binom(n, k int) int {
	if k > n-k {
		k = n - k
	}
	out := 1
	for i := 0; i < k; i++ {
		out = out * (n - i) / (i + 1)
	}
	return out
}

// ---- report ----------------------------------------------------------------

type report struct {
	Corpus     int       `json:"corpus_facts"`
	Probes     int       `json:"probes"`
	Flat       armResult `json:"flat"`
	Revised    armResult `json:"revised"`
	Discordant [2]int    `json:"discordant_b_c"`
	P          float64   `json:"mcnemar_exact_p"`
}

func run(w io.Writer, jsonOut bool) error {
	ctx := context.Background()
	corpus := buildCorpus()
	if err := validate(corpus); err != nil {
		return err
	}

	flatDir, err := os.MkdirTemp("", "revbench-flat")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(flatDir) }()
	revDir, err := os.MkdirTemp("", "revbench-rev")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(revDir) }()

	flatStore, err := buildFlat(ctx, flatDir, corpus)
	if err != nil {
		return fmt.Errorf("flat arm: %w", err)
	}
	defer func() { _ = flatStore.Close() }()
	revStore, err := buildRevised(ctx, revDir, corpus)
	if err != nil {
		return fmt.Errorf("revised arm: %w", err)
	}
	defer func() { _ = revStore.Close() }()

	flat, err := measure(ctx, "flat (no supersede edges)", flatStore)
	if err != nil {
		return err
	}
	rev, err := measure(ctx, "revised (edges via Store.Revise)", revStore)
	if err != nil {
		return err
	}

	b, c := 0, 0
	for i := range flat.perProbe {
		switch {
		case rev.perProbe[i] == 1 && flat.perProbe[i] == 0:
			b++
		case rev.perProbe[i] == 0 && flat.perProbe[i] == 1:
			c++
		}
	}
	p := mcnemarExact(b, c)

	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(report{len(corpus), len(families), flat, rev, [2]int{b, c}, p})
	}

	n := len(families)
	fmt.Fprintf(w, "# Revision currency — %d facts, %d revision families, top-k %d\n\n", len(corpus), n, topK)
	fmt.Fprintf(w, "Endpoint: the current value outranks every retired sibling AND lands in the top-%d.\n", topK)
	fmt.Fprintf(w, "Keyword embedder, scripted timeline, no network, no LLM, no randomness.\n\n")
	fmt.Fprintf(w, "  %-34s %8s %16s %8s %8s %10s\n", "arm", "A^B", "95% CI", "A", "B", "useful@8")
	fmt.Fprintf(w, "  %s\n", strings.Repeat("-", 88))
	for _, a := range []armResult{flat, rev} {
		lo, hi := wilson(a.Primary, n)
		fmt.Fprintf(w, "  %-34s %8s %16s %8s %8s %10.2f\n", a.Name,
			fmt.Sprintf("%d/%d", a.Primary, n), fmt.Sprintf("[%.2f, %.2f]", lo, hi),
			fmt.Sprintf("%d/%d", a.A, n), fmt.Sprintf("%d/%d", a.B, n), a.UsefulAt8)
	}
	fmt.Fprintf(w, "\n  McNemar exact, paired: b=%d c=%d  p=%.7f\n", b, c, p)
	fmt.Fprintf(w, "  retired facts shown to the caller: %d -> %d\n", flat.StaleShown, rev.StaleShown)
	fmt.Fprintf(w, "\n  %-34s %14s %10s\n", "stratum", "paraphrased", "literal")
	for _, a := range []armResult{flat, rev} {
		fmt.Fprintf(w, "  %-34s %13.0f%% %9.0f%%\n", a.Name, a.Paraphrased*100, a.Literal*100)
	}
	fmt.Fprintf(w, "\n  A = %d/%d in the revised arm means currency is settled; what caps A^B is\n", rev.A, n)
	fmt.Fprintf(w, "  retrieval — the current value not reaching the top-%d at all.\n", topK)
	return nil
}
