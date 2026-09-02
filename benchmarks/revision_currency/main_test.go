package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The CI gate. Two of the claims here are structural rather than statistical,
// and those are the ones worth failing a build over: a retired fact must never
// reach the caller, and recording a revision must settle currency for every
// probe. A drop in either means the tombstone filter has a hole, which is a
// correctness bug wearing a benchmark's clothes.
//
// The A^B counts are left as a floor rather than an exact match. They depend
// on retrieval, and pinning them would turn an unrelated ranking improvement
// into a red build.
func TestRevisionCurrencyGate(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two 600-fact stores; skipped in -short")
	}
	var buf bytes.Buffer
	if err := run(&buf, true); err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, buf.String())
	}
	var rep report
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, buf.String())
	}

	if rep.Probes != len(families) {
		t.Fatalf("probes = %d, want %d", rep.Probes, len(families))
	}

	// Structural: recording the revision settles currency, every time.
	if rep.Revised.A != rep.Probes {
		t.Errorf("revised arm: current value outranked its retired siblings on %d/%d probes, want all — the tombstone filter has a hole",
			rep.Revised.A, rep.Probes)
	}
	// Structural: nothing retired is ever shown.
	if rep.Revised.StaleShown != 0 {
		t.Errorf("revised arm showed %d retired facts to the caller, want 0", rep.Revised.StaleShown)
	}
	// The flat arm is the control and must stay broken; if it stops being
	// broken, the corpus no longer poses the problem and the comparison is
	// measuring nothing.
	if rep.Flat.StaleShown == 0 {
		t.Error("flat arm showed no retired facts — the control no longer reproduces the problem")
	}

	// Statistical: the effect is real, with room to spare on the threshold.
	if rep.P >= 0.001 {
		t.Errorf("McNemar exact p = %g, want < 0.001 (b=%d c=%d)", rep.P, rep.Discordant[0], rep.Discordant[1])
	}
	if rep.Discordant[1] != 0 {
		t.Errorf("%d probes got worse under revision; expected none", rep.Discordant[1])
	}
	if rep.Revised.Primary <= rep.Flat.Primary {
		t.Errorf("revised %d/%d is not above flat %d/%d",
			rep.Revised.Primary, rep.Probes, rep.Flat.Primary, rep.Probes)
	}
}

// The corpus is the measurement's foundation: a probe token matching two
// statements scores the wrong one silently.
func TestCorpusIsScorable(t *testing.T) {
	corpus := buildCorpus()
	if got := len(corpus); got != corpusSize {
		t.Errorf("corpus size = %d, want %d", got, corpusSize)
	}
	if err := validate(corpus); err != nil {
		t.Error(err)
	}
	seen := make(map[string]bool, len(corpus))
	for _, line := range corpus {
		if seen[line] {
			t.Errorf("duplicate statement in corpus: %q", line)
		}
		seen[line] = true
	}
	// A correction must never be written before what it corrects.
	pos := make(map[string]int, len(corpus))
	for i, line := range corpus {
		pos[line] = i
	}
	for _, f := range families {
		for i := 1; i < len(f.Statements); i++ {
			if pos[f.Statements[i]] < pos[f.Statements[i-1]] {
				t.Errorf("%s: revision %d lands before the statement it revises", f.ID, i)
			}
		}
	}
}

// Determinism is the brand: the same corpus on every machine, every run.
func TestCorpusIsDeterministic(t *testing.T) {
	a, b := buildCorpus(), buildCorpus()
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatal("buildCorpus is not deterministic")
	}
}

// Both strata must be populated, or the paraphrased/literal split in the
// report is noise dressed as a finding.
func TestBothStrataArePopulated(t *testing.T) {
	para, lit := 0, 0
	for _, f := range families {
		if f.Paraphrased {
			para++
		} else {
			lit++
		}
	}
	if para < 10 || lit < 10 {
		t.Errorf("strata too small to report: paraphrased=%d literal=%d", para, lit)
	}
	shapes := make(map[string]int)
	for _, f := range families {
		shapes[f.Shape]++
	}
	if len(shapes) < 4 {
		t.Errorf("only %d revision shapes; a fix that works on one shape is not a fix", len(shapes))
	}
}

// Revise is the write path the CLI uses and the benchmark's treatment arm; if
// it stops retiring what it names, both stop meaning anything.
func TestReviseRetiresEveryNamedVictim(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "revbench-unit")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	store, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const dup = "the cache TTL is 60 seconds"
	for i := 0; i < 2; i++ {
		if err := store.Put(ctx, agentID, dup); err != nil {
			t.Fatal(err)
		}
	}
	facts, err := store.List(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revise(ctx, agentID, "cache entries now live 900 seconds", facts...); err != nil {
		t.Fatal(err)
	}
	after, err := store.List(agentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range after {
		if f.Text == dup && !f.IsSuperseded() {
			t.Errorf("a named victim survived the revision: %s", f.ID)
		}
	}
}
