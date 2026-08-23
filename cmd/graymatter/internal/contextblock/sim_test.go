package contextblock

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/internal/tokens"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// TestSimulatedMonth drives the projection through a synthetic month of store
// activity and holds it against the acceptance criteria: the block fits its
// budget every single day, a superseded fact leaves the block the day the
// tombstone lands and never returns, output is deterministic, and the median
// daily churn stays under five changed lines so git history of the host file
// stays reviewable.
//
// Decay is applied by the documented half-life model rather than by calling
// Consolidate on purpose: this test targets the projection layer, and the
// engine's own clock-seamed tests already own the decay arithmetic.
func TestSimulatedMonth(t *testing.T) {
	const (
		days           = 30
		newFactsPerDay = 2
	)
	base := timeUTC()
	started := base.AddDate(0, 0, -days) // month begins before day zero

	var facts []memory.Fact
	bodies := make(map[int]string)

	add := func(d int, i int, text string) {
		facts = append(facts, memory.Fact{
			ID:        fmt.Sprintf("d%02d-%d", d, i),
			AgentID:   "sim",
			Text:      text,
			CreatedAt: started.AddDate(0, 0, d),
			Weight:    1,
		})
	}

	for d := 0; d < days; d++ {
		if d == 0 {
			add(0, 0, "genesis observation: the original architecture decision everything else amends")
		}
		for i := 0; i < newFactsPerDay; i++ {
			add(d, i+1, fmt.Sprintf("day %02d observation %d: durable project knowledge worth keeping", d, i))
		}
		if d == 18 {
			// The oldest fact is contradicted mid-month. Its replacement is
			// just another fact; what matters is that the tombstoned one
			// vanishes from the projection the same day and stays gone.
			for i := range facts {
				if strings.Contains(facts[i].Text, "genesis") {
					facts[i].SupersededBy = memory.SupersededByAgent
				}
			}
		}
		// Documented decay: weight halves every 30 days of age.
		redecay(facts, started.AddDate(0, 0, d))
		// The genesis fact is recalled every day, so its weight stays topped
		// up: when the tombstone lands on day 18 its disappearance must be
		// attributable to the tombstone alone, not to natural eviction.
		for i := range facts {
			if strings.Contains(facts[i].Text, "genesis") && !facts[i].IsSuperseded() {
				facts[i].Weight = math.Max(facts[i].Weight, 0.95)
			}
		}

		sel, _ := Select(facts, DefaultBudgetTokens)
		body := RenderBody(sel)
		bodies[d] = body
		if got := tokens.Approx(body); got > DefaultBudgetTokens {
			t.Fatalf("day %d: body costs %d tokens, budget %d", d, got, DefaultBudgetTokens)
		}
		if !strings.Contains(body, heading) {
			t.Fatalf("day %d: heading missing from body", d)
		}
		if again := RenderBody(sel); again != body {
			t.Fatalf("day %d: projection is not deterministic", d)
		}
		if d >= 18 && strings.Contains(body, "genesis") {
			t.Fatalf("day %d: tombstoned fact still projected", d)
		}
		if d == 17 && !strings.Contains(body, "genesis") {
			t.Fatalf("day 17: genesis fact missing while still live with top weight")
		}
	}

	// The newest facts always carry full weight: the last day must show them.
	if last := bodies[days-1]; !strings.Contains(last, fmt.Sprintf("day %02d observation 1", days-1)) {
		t.Error("final projection dropped a same-day fact")
	}
	// Early in the month, before crowding, the genesis fact must be there.
	if early := bodies[5]; !strings.Contains(early, "genesis") {
		t.Error("genesis fact missing before any contradiction existed")
	}

	// Daily churn: lines added plus lines removed between consecutive bodies.
	var churns []int
	for d := 1; d < days; d++ {
		churns = append(churns, changedLines(bodies[d-1], bodies[d]))
	}
	sort.Ints(churns)
	median := churns[len(churns)/2]
	if median > 5 {
		t.Errorf("median daily churn = %d lines, want <= 5 (distribution: %v)", median, churns)
	}
}

// redecay applies the documented forgetting curve as of `now`.
func redecay(facts []memory.Fact, now time.Time) {
	for i := range facts {
		ageDays := now.Sub(facts[i].CreatedAt).Hours() / 24
		facts[i].Weight = math.Pow(0.5, math.Max(0, ageDays)/30.0)
	}
}

// changedLines counts added plus removed lines between two bodies, order
// -insensitive within the multiset: projection churn is about content turnover,
// not about which line moved position.
func changedLines(prev, cur string) int {
	prevSet := map[string]int{}
	for _, l := range strings.Split(strings.TrimRight(prev, "\n"), "\n") {
		prevSet[l]++
	}
	curSet := map[string]int{}
	for _, l := range strings.Split(strings.TrimRight(cur, "\n"), "\n") {
		curSet[l]++
	}
	removed, added := 0, 0
	for l, n := range prevSet {
		if curSet[l] < n {
			removed += n - curSet[l]
		}
	}
	for l, n := range curSet {
		if prevSet[l] < n {
			added += n - prevSet[l]
		}
	}
	return removed + added
}
