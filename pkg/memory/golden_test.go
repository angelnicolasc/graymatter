package memory

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
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
)

// The anti-regression gate.
//
// v0.10.0 added two ranking knobs — SignalWeights and MinRelevance — whose
// defaults are supposed to reproduce v0.9.0 exactly. Unit tests assert that
// property one behaviour at a time, which catches a knob that is wired wrong
// but not a change in the ranking underneath them: a different RRF constant, a
// changed decay floor, a reordered tie-break would all leave those tests green
// while silently changing what every agent reads.
//
// This test freezes the observable output of the engine byte for byte against
// a committed fixture, with every knob at its zero value.
//
// Regenerate deliberately, never reflexively:
//
//	go test ./pkg/memory/ -run TestGolden -update
//
// A golden diff is the test doing its job. The question to answer before
// updating is always "did the recorded behaviour change on purpose?"
//
// # What each phase is for
//
// Every phase exists because mutation testing showed the previous version of
// this fixture missing a class of change. That record is kept here, because
// what a golden test cannot catch is the only interesting thing about it.
//
//	Phase A — ranking.    Records the fused RRF scores, not just the resulting
//	                      order. An order-only fixture passed with the default
//	                      recency weight moved 0.5 -> 0.6 and with the RRF
//	                      constant k moved 60 -> 50: both change every score,
//	                      neither reordered the head of this corpus.
//	Phase B — lifecycle.  One realistic recall, a long idle gap, then
//	                      Consolidate, sized so facts straddle the 0.01 prune
//	                      floor. An earlier version recalled everything at full
//	                      depth, which reset every AccessedAt and left nothing
//	                      near the floor — the prune threshold could then be
//	                      moved from 0.01 to 0.7 unnoticed.
//	Phase C — fallbacks.  Consolidate with a zero half-life, the only way to
//	                      reach the 720h default inside consolidate.go.
//	Phase D — tombstones. A superseded fact and two consecutive Consolidate
//	                      passes. Reaches the supersede filter in Recall and
//	                      the min() guard in the decay step, neither of which
//	                      any other phase can touch.

var updateGolden = flag.Bool("update", false, "rewrite the golden fixtures in testdata/golden")

const (
	goldenDir    = "testdata/golden"
	goldenAgent  = "golden-agent"
	goldenTopK   = 5
	goldenHalfLi = 720 * time.Hour

	// goldenIdleGap is how long the store sits untouched before consolidating.
	// Chosen so the corpus straddles the prune floor: facts touched by the
	// preceding recall survive, the oldest untouched ones do not.
	goldenIdleGap = 180 * 24 * time.Hour

	// The two corpus entries Phase D uses. Named so a corpus edit that removes
	// them fails loudly instead of quietly skipping the phase.
	goldenVictimText      = "release tags are signed with the team gpg key before publishing"
	goldenReplacementText = "the deployment freeze window runs from december 20th to january 2nd"
)

// goldenQueries spans the corpus's topics so the fixture exercises rankings
// where different signals dominate, rather than one query whose answer is
// obvious.
var goldenQueries = []string{
	"deployment pipeline release",
	"office facilities and food",
	"oncall rotation and incident handling",
	"gpg",         // matches exactly one fact
	"nonexistent", // matches none: pure recency ordering
}

// goldenEpoch is the fixed instant the scripted clock counts from. Any date
// works; it is pinned so the fixture never depends on when it was generated.
var goldenEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// corpusEntry is one line of testdata/golden/corpus.jsonl. OffsetHours places
// the fact on the timeline relative to goldenEpoch, so the corpus carries its
// own timestamps and the fixture does not depend on insertion timing.
type corpusEntry struct {
	OffsetHours int    `json:"offset_hours"`
	Text        string `json:"text"`
}

// scriptedClock reports goldenEpoch plus a caller-driven offset. It is a
// scripted clock rather than a frozen one on purpose.
//
// A constant clock gives every fact the same CreatedAt. That makes all recency
// scores equal, and recall.go ranks them with sort.Slice, which is not stable
// — so the tie order among identically-aged facts is unspecified and may vary
// between runs or Go versions. A frozen clock would therefore pin the wrong
// thing: a degenerate ranking regime that never occurs in practice, and it
// would be flaky while doing it.
type scriptedClock struct{ offset time.Duration }

func (c *scriptedClock) now() time.Time      { return goldenEpoch.Add(c.offset) }
func (c *scriptedClock) set(d time.Duration) { c.offset = d }

func TestGolden_EngineAtDefaults(t *testing.T) {
	compareGolden(t, "engine.golden", buildGoldenFixture(t, true))
}

// TestGolden_IsStableAcrossRuns proves the fixture is reproducible rather than
// merely recorded. A golden that drifts between runs is worse than no golden:
// it trains people to regenerate it without reading the diff.
func TestGolden_IsStableAcrossRuns(t *testing.T) {
	first := buildGoldenFixture(t, false)
	for i := 2; i <= 5; i++ {
		if got := buildGoldenFixture(t, false); got != first {
			t.Fatalf("run %d differs from run 1; the fixture is not reproducible.\n%s",
				i, firstDiff(first, got))
		}
	}
}

func buildGoldenFixture(t *testing.T, withHeader bool) string {
	t.Helper()
	corpus := loadGoldenCorpus(t)
	ctx := context.Background()

	var b strings.Builder
	if withHeader {
		b.WriteString("# GrayMatter engine golden fixture\n")
		b.WriteString("#\n")
		b.WriteString("# Regenerate: go test ./pkg/memory/ -run TestGolden -update\n")
		b.WriteString("# A diff here means the default-configuration output of the engine\n")
		b.WriteString("# changed. That is either a bug or a decision; it is never noise.\n")
		b.WriteString("#\n")
		fmt.Fprintf(&b, "# epoch:     %s\n", goldenEpoch.Format(time.RFC3339))
		fmt.Fprintf(&b, "# half-life: %s\n", goldenHalfLi)
		b.WriteString("# knobs:     SignalWeights=nil (defaults), MinRelevance=0\n")
		b.WriteString("#\n")
		b.WriteString("# Fact IDs are ULIDs seeded from the real clock and random entropy, so\n")
		b.WriteString("# they cannot be pinned. Facts appear by List position or by text;\n")
		b.WriteString("# every other value is the stored one, verbatim and unrounded.\n")
		b.WriteString("\n")
	}

	b.WriteString(goldenPhaseA(t, ctx, corpus))
	b.WriteString(goldenPhaseB(t, ctx, corpus))
	b.WriteString(goldenPhaseC(t, ctx, corpus))
	b.WriteString(goldenPhaseD(t, ctx, corpus))
	return b.String()
}

// goldenPhaseA records the fused score of every candidate for every query,
// which is what makes the gate sensitive to the ranking knobs themselves.
func goldenPhaseA(t *testing.T, ctx context.Context, corpus []corpusEntry) string {
	t.Helper()
	s, clock := openGoldenStore(t)
	defer s.Close()
	seedGoldenCorpus(t, s, clock, corpus)

	byID := factsByID(t, s)
	var b strings.Builder
	b.WriteString("== PHASE A: fused ranking (all candidates, scores verbatim) ==\n\n")
	for _, q := range goldenQueries {
		var ranked []scored
		s.debugRanking = func(_ string, r []scored) { ranked = r }
		got, err := s.Recall(ctx, goldenAgent, q, goldenTopK)
		if err != nil {
			t.Fatalf("Recall(%q): %v", q, err)
		}
		s.wg.Wait()
		s.debugRanking = nil

		fmt.Fprintf(&b, "-- query %q\n", q)
		for i, sc := range ranked {
			fmt.Fprintf(&b, "  rank=%02d score=%.17g  %s\n", i+1, sc.score, byID[sc.id])
		}
		fmt.Fprintf(&b, "  returned at topK=%d: %d\n", goldenTopK, len(got))
		for i, text := range got {
			fmt.Fprintf(&b, "    %02d %s\n", i+1, text)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// goldenPhaseB is the realistic lifecycle: one recall, a long idle gap, then
// Consolidate with facts straddling the prune floor.
func goldenPhaseB(t *testing.T, ctx context.Context, corpus []corpusEntry) string {
	t.Helper()
	s, clock := openGoldenStore(t)
	defer s.Close()
	seedGoldenCorpus(t, s, clock, corpus)

	if _, err := s.Recall(ctx, goldenAgent, goldenQueries[0], goldenTopK); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	s.wg.Wait()

	var b strings.Builder
	b.WriteString("== PHASE B: store state after one recall ==\n")
	b.WriteString(snapshotStore(t, s))

	clock.set(clock.offset + goldenIdleGap)
	if err := s.Consolidate(ctx, goldenAgent, goldenConsolidateCfg{halfLife: goldenHalfLi}); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	s.wg.Wait()

	fmt.Fprintf(&b, "\n== PHASE B: after %s idle, then consolidate ==\n", goldenIdleGap)
	b.WriteString(snapshotStore(t, s))
	return b.String()
}

// goldenPhaseC reaches the 720h fallback inside consolidate.go, which is only
// used when the config reports a zero half-life.
func goldenPhaseC(t *testing.T, ctx context.Context, corpus []corpusEntry) string {
	t.Helper()
	s, clock := openGoldenStore(t)
	defer s.Close()
	seedGoldenCorpus(t, s, clock, corpus)
	clock.set(clock.offset + goldenIdleGap)

	if err := s.Consolidate(ctx, goldenAgent, goldenConsolidateCfg{halfLife: 0}); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	s.wg.Wait()

	var b strings.Builder
	b.WriteString("\n== PHASE C: consolidate with half-life 0 (uses the 720h fallback) ==\n")
	b.WriteString(snapshotStore(t, s))
	return b.String()
}

// goldenPhaseD covers the two paths no other phase reaches: the supersede
// filter in Recall, and the min() guard in the decay step, which only differs
// from plain assignment once a fact has been decayed or had its weight zeroed.
func goldenPhaseD(t *testing.T, ctx context.Context, corpus []corpusEntry) string {
	t.Helper()
	s, clock := openGoldenStore(t)
	defer s.Close()
	seedGoldenCorpus(t, s, clock, corpus)

	facts, err := s.List(goldenAgent)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var victim, replacement *Fact
	for i := range facts {
		switch facts[i].Text {
		case goldenVictimText:
			victim = &facts[i]
		case goldenReplacementText:
			replacement = &facts[i]
		}
	}
	if victim == nil || replacement == nil {
		t.Fatalf("golden corpus no longer contains the facts phase D supersedes:\n  victim=%q\n  replacement=%q",
			goldenVictimText, goldenReplacementText)
	}

	// Retire it the way memory_reflect does: tombstone pointing at the
	// replacement, weight zeroed so ordinary pruning collects it.
	victim.SupersededBy = replacement.ID
	victim.Weight = 0
	if err := s.UpdateFact(goldenAgent, *victim); err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}

	got, err := s.Recall(ctx, goldenAgent, goldenQueries[0], goldenTopK)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	s.wg.Wait()

	var b strings.Builder
	b.WriteString("\n== PHASE D: recall with one fact tombstoned ==\n")
	for i, text := range got {
		fmt.Fprintf(&b, "  %02d %s\n", i+1, text)
	}

	// Consolidate twice at the same instant. Decay is defined to be
	// idempotent, so the second pass must change nothing; without the min()
	// guard the tombstoned fact's zeroed weight is also handed back.
	clock.set(clock.offset + goldenIdleGap)
	for pass := 1; pass <= 2; pass++ {
		if err := s.Consolidate(ctx, goldenAgent, goldenConsolidateCfg{halfLife: goldenHalfLi}); err != nil {
			t.Fatalf("Consolidate pass %d: %v", pass, err)
		}
		s.wg.Wait()
		fmt.Fprintf(&b, "\n== PHASE D: after consolidate pass %d ==\n", pass)
		b.WriteString(snapshotStore(t, s))
	}
	return b.String()
}

// openGoldenStore opens a store with every knob at its zero value. That is the
// whole point: this fixture records what a caller who configures nothing gets.
func openGoldenStore(t *testing.T) (*Store, *scriptedClock) {
	t.Helper()
	s, err := Open(StoreConfig{
		DataDir:       t.TempDir(),
		Embedder:      embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}),
		DecayHalfLife: goldenHalfLi,
		// SignalWeights: nil
		// MinRelevance:  0
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock := &scriptedClock{}
	s.now = clock.now
	return s, clock
}

// seedGoldenCorpus writes the corpus on its own timeline, so each fact has a
// distinct age, then advances the clock a week past the last write.
func seedGoldenCorpus(t *testing.T, s *Store, clock *scriptedClock, corpus []corpusEntry) {
	t.Helper()
	ctx := context.Background()
	for _, e := range corpus {
		clock.set(time.Duration(e.OffsetHours) * time.Hour)
		if err := s.Put(ctx, goldenAgent, e.Text); err != nil {
			t.Fatalf("Put %q: %v", e.Text, err)
		}
	}
	clock.set(time.Duration(corpus[len(corpus)-1].OffsetHours)*time.Hour + 168*time.Hour)
}

func factsByID(t *testing.T, s *Store) map[string]string {
	t.Helper()
	facts, err := s.List(goldenAgent)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	m := make(map[string]string, len(facts))
	for _, f := range facts {
		m[f.ID] = f.Text
	}
	return m
}

// snapshotStore renders every stored field a caller can observe. Fact IDs are
// omitted because ULIDs are not reproducible; every other field is the stored
// value at full precision.
func snapshotStore(t *testing.T, s *Store) string {
	t.Helper()
	facts, err := s.List(goldenAgent)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) == 0 {
		return "  (store empty)\n"
	}
	var b strings.Builder
	for i, f := range facts {
		superseded := "-"
		if f.IsSuperseded() {
			superseded = "yes"
		}
		fmt.Fprintf(&b, "[%02d] created=%s accessed=%s count=%d weight=%.17g superseded=%s embedding=%d\n",
			i,
			f.CreatedAt.UTC().Format(time.RFC3339Nano),
			f.AccessedAt.UTC().Format(time.RFC3339Nano),
			f.AccessCount,
			f.Weight,
			superseded,
			len(f.Embedding),
		)
		fmt.Fprintf(&b, "     text=%s\n", f.Text)
	}
	return b.String()
}

func loadGoldenCorpus(t *testing.T) []corpusEntry {
	t.Helper()
	fh, err := os.Open(filepath.Join(goldenDir, "corpus.jsonl"))
	if err != nil {
		t.Fatalf("open golden corpus: %v", err)
	}
	defer fh.Close()

	var out []corpusEntry
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var e corpusEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("corpus line %q: %v", line, err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read golden corpus: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("golden corpus is empty")
	}
	// Written oldest first; the timeline must be monotonic or the fixture's
	// meaning depends on file order rather than on the declared offsets.
	if !sort.SliceIsSorted(out, func(i, j int) bool {
		return out[i].OffsetHours < out[j].OffsetHours
	}) {
		t.Fatal("golden corpus offsets are not strictly increasing")
	}
	return out
}

func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir, name)

	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("golden fixture rewritten: %s", path)
		return
	}

	wantBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v\nGenerate it with: go test ./pkg/memory/ -run TestGolden -update", path, err)
	}
	// Normalise line endings: the repo checks out CRLF on Windows, and the
	// fixture's content is what is under test, not its line terminators.
	want := strings.ReplaceAll(string(wantBytes), "\r\n", "\n")
	got = strings.ReplaceAll(got, "\r\n", "\n")

	if got == want {
		return
	}
	t.Errorf("default-configuration engine output changed.\n\n%s\n\n"+
		"If this change is intended, regenerate the fixture in the same commit so a\n"+
		"reviewer sees the behavioural diff:\n"+
		"    go test ./pkg/memory/ -run TestGolden -update",
		firstDiff(want, got))
}

// firstDiff reports the first differing line, because a full dump of two
// multi-hundred-line fixtures is unreadable in CI output.
func firstDiff(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return fmt.Sprintf("first difference at line %d:\n  want | %s\n  got  | %s\n(%d golden lines, %d produced)",
				i+1, w, g, len(wl), len(gl))
		}
	}
	return "files differ only in trailing content"
}

// goldenConsolidateCfg drives Consolidate deterministically: decay and prune,
// no LLM, so the fixture needs no network and no key.
type goldenConsolidateCfg struct{ halfLife time.Duration }

func (goldenConsolidateCfg) GetAnthropicAPIKey() string        { return "" }
func (goldenConsolidateCfg) GetConsolidateLLM() string         { return "" }
func (goldenConsolidateCfg) GetConsolidateModel() string       { return "" }
func (goldenConsolidateCfg) GetConsolidateThreshold() int      { return 100 }
func (c goldenConsolidateCfg) GetDecayHalfLife() time.Duration { return c.halfLife }
