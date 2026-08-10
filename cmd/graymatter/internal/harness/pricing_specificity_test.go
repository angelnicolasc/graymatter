package harness

import "testing"

// TestLookupPricing_MostSpecificKeyWins is the regression this file exists for.
//
// Matching was a map range over "contains", which is both non-deterministic and
// wrong for any model whose ID extends a shorter key. Haiku 4.5 resolved to the
// Haiku 4 row and under-reported cost 4x; Opus 4.8 resolved to Opus 4 and
// over-reported 3x. Both returned ok=true, so the dashboard's "partial" flag
// never fired: the panel showed a confident, incorrect number.
func TestLookupPricing_MostSpecificKeyWins(t *testing.T) {
	cases := []struct {
		model     string
		wantInput float64
	}{
		// Each of these extends a shorter key that must not win.
		{"claude-haiku-4-5-20251001", 1.00}, // not claude-haiku-4 at 0.25
		{"claude-opus-4-8", 5.00},           // not claude-opus-4 at 15.00
		{"claude-opus-4-7", 5.00},           // not claude-opus-4 at 15.00
		{"claude-opus-4-6", 5.00},           // not claude-opus-4 at 15.00
		{"claude-sonnet-5", 3.00},
		{"claude-opus-5", 5.00},
		{"claude-fable-5", 10.00},
		// Older models still resolve to their own rows.
		{"claude-opus-4-20250514", 15.00},
		{"claude-3-haiku-20240307", 0.25},
	}

	for _, c := range cases {
		p, ok := LookupPricing(c.model)
		if !ok {
			t.Errorf("%s: no pricing row found", c.model)
			continue
		}
		if p.InputPer1M != c.wantInput {
			t.Errorf("%s: input = $%.2f/1M, want $%.2f/1M", c.model, p.InputPer1M, c.wantInput)
		}
	}
}

// TestLookupPricing_Deterministic guards the ordering itself. A map range would
// pass a single run and fail intermittently.
func TestLookupPricing_Deterministic(t *testing.T) {
	first, ok := LookupPricing("claude-haiku-4-5")
	if !ok {
		t.Fatal("no pricing for claude-haiku-4-5")
	}
	for i := 0; i < 200; i++ {
		got, _ := LookupPricing("claude-haiku-4-5")
		if got != first {
			t.Fatalf("lookup is not deterministic: got %+v, first %+v", got, first)
		}
	}
}

// TestModelPrices_CacheRatesFollowMultipliers pins the relationship rather than
// the numbers: cache reads are 0.1x input, 5-minute cache writes 1.25x. A row
// added by hand with a mistyped cache rate fails here.
func TestModelPrices_CacheRatesFollowMultipliers(t *testing.T) {
	const epsilon = 0.005
	for name, p := range ModelPrices {
		if wantRead := p.InputPer1M * 0.1; abs(p.CacheReadPer1M-wantRead) > epsilon {
			t.Errorf("%s: cache read $%.2f, want ~$%.2f (0.1x input)", name, p.CacheReadPer1M, wantRead)
		}
		if wantWrite := p.InputPer1M * 1.25; abs(p.CacheWritePer1M-wantWrite) > epsilon {
			t.Errorf("%s: cache write $%.2f, want ~$%.2f (1.25x input)", name, p.CacheWritePer1M, wantWrite)
		}
		if p.OutputPer1M <= p.InputPer1M {
			t.Errorf("%s: output $%.2f is not above input $%.2f", name, p.OutputPer1M, p.InputPer1M)
		}
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
