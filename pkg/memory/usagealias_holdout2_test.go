package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Usage-alias learning measured as a RESULT rather than a mechanism, over a
// second evaluation corpus built for the case the first one could not
// isolate: the absent-morphology stratum, 10 families whose probe word does
// not exist in the corpus (df=0) but shares a prefix of at least 3 with the
// corpus word. Before learning, the target fact sits outside the top-3. If
// the store's own promoted aliases move it into the top-3 with no agent
// action, the mechanism has a measurable effect; if not, it is a mechanism
// without a result and the approach gets re-specified or dropped. Both
// outcomes were declared before the run.
//
// Like the first corpus, this one lives outside the repository and the test
// skips without GRAYMATTER_HOLDOUT2_DIR (holdout2_corpus.txt, one fact per
// line, and holdout2_probes.json in the shape of holdout2Family below).

const holdout2EnvVar = "GRAYMATTER_HOLDOUT2_DIR"

type holdout2Family struct {
	ID     string `json:"id"`
	Y      string `json:"y"`
	X      string `json:"x"`
	Probe  string `json:"probe"`
	Reform string `json:"reform"`
	Target string `json:"target"`
}

func loadHoldout2(t *testing.T) (corpus []string, fams []holdout2Family) {
	t.Helper()
	dir := os.Getenv(holdout2EnvVar)
	if dir == "" {
		t.Skipf("%s not set; holdout v2 corpus not available", holdout2EnvVar)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "holdout2_corpus.txt"))
	if err != nil {
		t.Skipf("v2 corpus unreadable: %v", err)
	}
	for _, l := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			corpus = append(corpus, l)
		}
	}
	pb, err := os.ReadFile(filepath.Join(dir, "holdout2_probes.json"))
	if err != nil {
		t.Skipf("v2 probes unreadable: %v", err)
	}
	var doc struct {
		Families []holdout2Family `json:"families"`
	}
	if err := json.Unmarshal(pb, &doc); err != nil {
		t.Fatalf("decode v2 probes: %v", err)
	}
	return corpus, doc.Families
}

// targetRank returns the 0-based position of the target fact in results, or
// -1 when absent.
func targetRank(results []string, target string) int {
	for i, r := range results {
		if r == target {
			return i
		}
	}
	return -1
}

func TestUsageAliasLearningOnHoldoutV2(t *testing.T) {
	corpus, fams := loadHoldout2(t)
	s, err := Open(StoreConfig{
		DataDir:            t.TempDir(),
		DecayHalfLife:      8760 * time.Hour,
		UsageAliasLearning: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// Scripted clock: one hour per fact, so ingest order = recency order and
	// each family's target (first line of its block) is the oldest of its
	// subject matches.
	clock := &explainClock{}
	s.now = clock.now
	ctx := context.Background()
	for _, line := range corpus {
		clock.offset += time.Hour
		if err := s.Put(ctx, "holdout2", line); err != nil {
			t.Fatal(err)
		}
	}

	targetRankPre := make([]int, len(fams))
	for i, f := range fams {
		results, err := s.Recall(ctx, "holdout2", f.Probe, 8)
		if err != nil {
			t.Fatal(err)
		}
		targetRankPre[i] = targetRank(results, f.Target)
	}

	// Two learning sessions: probe (weak — the trigger fires on the absent
	// word) then the reformulated query (strong — the corpus word). No alias
	// is written by the test; the only writer is the store.
	for session := 0; session < 2; session++ {
		for _, f := range fams {
			if _, _, err := s.RecallDetailed(ctx, "holdout2", f.Probe, 8); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.RecallDetailed(ctx, "holdout2", f.Reform, 8); err != nil {
				t.Fatal(err)
			}
		}
		if session == 0 {
			if n := s.countUsageAliases("holdout2"); n != 0 {
				t.Fatalf("PRE-REGISTERED MISS (gate 1): %d aliases promoted after a single observation, want 0", n)
			}
		}
	}

	// Gate 1: one promoted alias per affined (unknown-word, working-word)
	// pair. 10 families, but V04's probe carries TWO unknown words with
	// affined working words (archived→archive, encrypted→encryption), so the
	// derived total is 11. The first draft of this gate said 10 — it missed
	// V04's second pair, exactly the kind of derivation slip the miss
	// reporting exists to catch.
	n := s.countUsageAliases("holdout2")
	if n != len(fams)+1 {
		t.Fatalf("PRE-REGISTERED MISS (gate 1): %d usage aliases after two sessions, want %d (10 families + V04's second affined pair)", n, len(fams)+1)
	}

	// Gate 2: affinity ≥ 3 and source=usage on every promoted alias.
	for _, pair := range s.usageAliasTerms("holdout2") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			t.Fatalf("malformed usage alias %q", pair)
		}
		if commonPrefixLen(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])) < 3 {
			t.Errorf("PRE-REGISTERED MISS (gate 2): promoted pair %q has affinity < 3", pair)
		}
	}

	// Gate 3: the flywheel's mechanical effect — the target fact moves into
	// the top-3 with no agent action.
	post := 0
	for i, f := range fams {
		results, err := s.Recall(ctx, "holdout2", f.Probe, 8)
		if err != nil {
			t.Fatal(err)
		}
		rank := targetRank(results, f.Target)
		if rank >= 0 && rank <= 2 {
			post++
		} else {
			t.Errorf("family %s: target rank after learning = %d (pre %d), want <= 2", f.ID, rank, targetRankPre[i])
		}
	}
	if post < 9 {
		t.Fatalf("PRE-REGISTERED MISS (gate 3): target-in-top-3 after learning = %d/10, want >= 9", post)
	}

	pre := 0
	for _, r := range targetRankPre {
		if r >= 0 && r <= 2 {
			pre++
		}
	}
	t.Logf("target-in-top-3: pre %d/10 -> post %d/10 · aliases the store promoted: %d", pre, post, n)
}
