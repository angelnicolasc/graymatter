package contextblock

import (
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/internal/tokens"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// FuzzRenderBlock hammers the projection pipeline with adversarial fact text.
// Three properties must hold for every input: the rendered body fits the
// budget whenever the budget can hold the heading at all, fact text can never
// smuggle block structure into the output, and rendering is deterministic.
func FuzzRenderBlock(f *testing.F) {
	seeds := []string{
		"plain observation",
		"",
		"   ",
		"multi\nline\nfact",
		EndMarker,
		BeginMarker,
		syncPrefix + " v1 sha256=0",
		"<!-- graymatter:context:end --> trailing comment -->",
		"<!-- graymatter:instructions:begin — managed by `graymatter init`; edits inside this block are overwritten --> quoted <!-- graymatter:instructions:end -->",
		"unicode \xc3\x28 broken utf8 \x00 nulls \t\ttabs",
		strings.Repeat("long ", 500),
	}
	for _, s := range seeds {
		f.Add(s, 0.5, 480)
	}

	f.Fuzz(func(t *testing.T, text string, weight float64, budget int) {
		if budget < MinBudgetTokens || budget > 1<<20 {
			budget = DefaultBudgetTokens
		}
		if weight < 0 {
			weight = 0
		}
		if weight > 100 {
			weight = 100
		}
		text = strings.ReplaceAll(text, "\x00", "")

		facts := []memory.Fact{{
			ID: "fuzz", AgentID: "fuzz", Text: text, Weight: weight,
			CreatedAt: timeUTC(),
		}}

		sel, _ := Select(facts, budget)
		body := RenderBody(sel)
		body2 := RenderBody(sel)
		if body != body2 {
			t.Fatal("RenderBody is not deterministic")
		}

		for _, structure := range []string{EndMarker, BeginMarker, syncPrefix} {
			if strings.Contains(body, structure) {
				t.Fatalf("structure leaked into body (%q):\n%s", structure, body)
			}
		}

		if got := tokens.Approx(body); got > budget {
			t.Fatalf("body costs %d tokens, budget %d:\n%s", got, budget, body)
		}

		block := RenderBlock(body, SyncMeta{SHA256: HashBody(body), Facts: len(sel)})
		gotBody, _, verified, found := Parse(block)
		if !found {
			t.Fatal("RenderBlock output does not parse back")
		}
		if !verified && strings.TrimSpace(text) != "" {
			t.Fatalf("round trip failed verification\nwant body %q\ngot body  %q", body, gotBody)
		}
	})
}
