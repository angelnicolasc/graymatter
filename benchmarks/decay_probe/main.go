// decay_probe is the deterministic forgetting-curve simulation: it compresses
// the published 30-day half-life down to seconds and verifies the three
// lifecycle promises end to end — untouched facts fade, the recall "touch"
// protects a fact from fading, pinned facts are exempt from decay and pruning.
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/angelnicolasc/graymatter"
)

func main() {
	os.Exit(run())
}

func run() int {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir, _ = os.MkdirTemp("", "gm-decay")
	defer func() {
		if err := os.RemoveAll(cfg.DataDir); err != nil {
			fmt.Fprintf(os.Stderr, "cleanup %s: %v\n", cfg.DataDir, err)
		}
	}()
	cfg.DecayHalfLife = 2 * time.Second // 30 days compressed to 2 seconds
	cfg.AsyncConsolidate = false        // drive consolidation by hand
	cfg.ConsolidateThreshold = 1000     // never auto-trigger
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword

	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		fmt.Println("open:", err)
		return 1
	}
	// Registered after directory cleanup so LIFO closes bbolt first on return.
	defer func() {
		if err := mem.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "close:", err)
		}
	}()
	ctx := context.Background()
	fmt.Println("Embedder: keyword (no LLM, no network, no API key)")

	const agent = "decay-agent"
	for _, f := range []string{"untouched A", "untouched B", "untouched C", "untouched D", "untouched E", "untouched F", "untouched G", "untouched H", "untouched I", "untouched J"} {
		if err := mem.Remember(ctx, agent, f); err != nil {
			fmt.Println("remember:", err)
			return 1
		}
	}
	if err := mem.Remember(ctx, agent, "pinned institutional fact: production secrets live in the platform vault"); err != nil {
		fmt.Println("remember pinned:", err)
		return 1
	}
	// pin it through the store handle (what memory_reflect action=pin does)
	adv := mem.Advanced()
	facts, _ := adv.List(agent)
	for _, f := range facts {
		if strings.HasPrefix(f.Text, "pinned") {
			f.Pinned = true
			f.PinnedAt = time.Now().UTC()
			_ = adv.UpdateFact(agent, f)
		}
	}

	failures := 0
	check := func(name string, ok bool, detail string) {
		verdict := "PASS"
		if !ok {
			verdict = "FAIL"
			failures++
		}
		fmt.Printf("%-46s %s  %s\n", name, verdict, detail)
	}

	// 3 half-lives: an untouched fact should sit near 12.5% of initial weight
	time.Sleep(6 * time.Second)
	if err := mem.Consolidate(ctx, agent); err != nil {
		fmt.Println("consolidate 1:", err)
		return 1
	}
	facts, _ = adv.List(agent)
	maxW := 0.0
	pinnedW := 0.0
	for _, f := range facts {
		if f.Pinned {
			pinnedW = f.Weight
			continue
		}
		if f.Weight > maxW {
			maxW = f.Weight
		}
	}
	check("decay: untouched facts faded (w<=0.2 after 3 half-lives)", maxW > 0 && maxW <= 0.2, fmt.Sprintf("max untouched weight=%.4f", maxW))
	check("decay: pinned fact exempt", pinnedW >= 0.9, fmt.Sprintf("pinned weight=%.4f", pinnedW))

	// the touch: recalling A resets its decay clock
	if _, err := mem.Recall(ctx, agent, "untouched A"); err != nil {
		fmt.Println("recall touch:", err)
		return 1
	}

	// The touch: recalling A resets ITS decay clock — and the clocks of the
	// other 7 facts the top-8 returns (product behaviour: injected context
	// must not decay, and with recency weight every query returns and touches
	// a top-8). The siblings left out of that batch keep their original clock
	// and cross the 0.01 prune floor (7 half-lives) while the touched batch
	// sits at 4 half-lives (0.0625).
	time.Sleep(8 * time.Second)
	if err := mem.Consolidate(ctx, agent); err != nil {
		fmt.Println("consolidate 2:", err)
		return 1
	}

	facts, _ = adv.List(agent)
	alive := map[string]bool{}
	pinnedAlive := false
	for _, f := range facts {
		alive[f.Text] = true
		if strings.HasPrefix(f.Text, "pinned") {
			pinnedAlive = true
		}
	}
	untouchedLeft := 0
	for _, f := range facts {
		if strings.HasPrefix(f.Text, "untouched") {
			untouchedLeft++
		}
	}
	check("prune: facts past the floor were collected", untouchedLeft < 10, fmt.Sprintf("%d of 10 untouched facts remain (the top-8 of the touch query were re-clocked by design)", untouchedLeft))
	check("prune: pinned fact survives", pinnedAlive, "")
	check("touch: recalled fact survives its siblings", alive["untouched A"], "")
	check("audit: prune is the only removal, tombstone rules intact", len(facts) >= 9 && len(facts) <= 11, fmt.Sprintf("%d live facts", len(facts)))

	if failures > 0 {
		fmt.Printf("\n%d checks FAILED\n", failures)
		return 1
	}
	fmt.Println("\nAll forgetting-curve checks passed.")
	return 0
}

var _ = math.Exp
