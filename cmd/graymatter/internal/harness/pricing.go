package harness

import (
	"sort"
	"strings"
)

// ModelPricing is USD per 1M tokens for a single model. Keep all four fields
// — the cache rates are what make the dashboard compelling, since prompt
// caching is where graymatter actually saves the user money.
//
// Prices are public list prices from Anthropic, last refreshed 2026-08-10.
// If Anthropic re-tiers or a new model ships, update ModelPrices below. The
// dashboard falls back to zero-cost (not a fabricated estimate) for any model
// not present in the table, and flags the total as partial.
//
// A missing model is the safe failure here; a stale one is not. Until this
// table gained the current generation, every Opus 4.6/4.7/4.8 run was costed
// against Opus 4's rates and every Haiku 4.5 run against Haiku 4's, both
// reported as though exact.
type ModelPricing struct {
	InputPer1M      float64 // per 1,000,000 input tokens
	OutputPer1M     float64 // per 1,000,000 output tokens
	CacheReadPer1M  float64 // prompt-cache read (the cheap hit)
	CacheWritePer1M float64 // prompt-cache creation (the expensive miss)
}

// ModelPrices is the canonical pricing map. Keys are matched longest-first with
// contains-semantics (see LookupPricing) so both full model IDs
// ("claude-haiku-4-5-20251001") and short aliases ("opus-5") resolve to the most
// specific row rather than to whichever shorter key happens to also match.
//
// Cache rates follow Anthropic's published multipliers rather than being quoted
// separately: a cache read is 0.1x the input rate, and a 5-minute cache write is
// 1.25x. The 1-hour TTL writes at 2x; the 5-minute figure is used here because
// it is the default.
var ModelPrices = map[string]ModelPricing{
	// Current generation.
	"claude-fable-5":   {InputPer1M: 10.00, OutputPer1M: 50.00, CacheReadPer1M: 1.00, CacheWritePer1M: 12.50},
	"claude-mythos-5":  {InputPer1M: 10.00, OutputPer1M: 50.00, CacheReadPer1M: 1.00, CacheWritePer1M: 12.50},
	"claude-opus-5":    {InputPer1M: 5.00, OutputPer1M: 25.00, CacheReadPer1M: 0.50, CacheWritePer1M: 6.25},
	"claude-opus-4-8":  {InputPer1M: 5.00, OutputPer1M: 25.00, CacheReadPer1M: 0.50, CacheWritePer1M: 6.25},
	"claude-opus-4-7":  {InputPer1M: 5.00, OutputPer1M: 25.00, CacheReadPer1M: 0.50, CacheWritePer1M: 6.25},
	"claude-opus-4-6":  {InputPer1M: 5.00, OutputPer1M: 25.00, CacheReadPer1M: 0.50, CacheWritePer1M: 6.25},
	"claude-sonnet-5":  {InputPer1M: 3.00, OutputPer1M: 15.00, CacheReadPer1M: 0.30, CacheWritePer1M: 3.75},
	"claude-haiku-4-5": {InputPer1M: 1.00, OutputPer1M: 5.00, CacheReadPer1M: 0.10, CacheWritePer1M: 1.25},

	// Older generations. These stay because a project's 30-day window can still
	// contain runs on them.
	"claude-sonnet-4": {InputPer1M: 3.00, OutputPer1M: 15.00, CacheReadPer1M: 0.30, CacheWritePer1M: 3.75},
	"claude-opus-4":   {InputPer1M: 15.00, OutputPer1M: 75.00, CacheReadPer1M: 1.50, CacheWritePer1M: 18.75},
	"claude-3-opus":   {InputPer1M: 15.00, OutputPer1M: 75.00, CacheReadPer1M: 1.50, CacheWritePer1M: 18.75},
	"claude-3-sonnet": {InputPer1M: 3.00, OutputPer1M: 15.00, CacheReadPer1M: 0.30, CacheWritePer1M: 3.75},
	"claude-3-haiku":  {InputPer1M: 0.25, OutputPer1M: 1.25, CacheReadPer1M: 0.03, CacheWritePer1M: 0.31},
}

// priceKeysByLength is ModelPrices' keys ordered longest first, so lookup always
// resolves to the most specific match.
var priceKeysByLength = func() []string {
	keys := make([]string, 0, len(ModelPrices))
	for k := range ModelPrices {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}()

// LookupPricing resolves a model identifier to its pricing row. Matching is
// case-insensitive and uses "contains" so that both the official model IDs
// returned by the Anthropic SDK and user-friendly shorthands work.
//
// Returns (ModelPricing{}, false) for unknown models — callers MUST render
// "—" instead of fabricating a cost.
func LookupPricing(model string) (ModelPricing, bool) {
	if model == "" {
		return ModelPricing{}, false
	}
	lower := strings.ToLower(model)
	// Longest key first. Map iteration would be both non-deterministic and
	// wrong: "claude-haiku-4-5-20251001" contains "claude-haiku-4", so a short
	// key can shadow a newer model and return a confidently incorrect price
	// with ok=true, which the "partial" flag does not catch. Specificity has to
	// win, and it has to win the same way every run.
	for _, key := range priceKeysByLength {
		if strings.Contains(lower, key) {
			return ModelPrices[key], true
		}
	}
	return ModelPricing{}, false
}

// CostUSD applies pricing to a token record and returns the total cost in
// dollars. Any leg whose model has no pricing contributes zero to the sum
// (signalled via the second return — false means "at least one leg was
// uncosted", so the UI can display a "partial" flag).
func (p ModelPricing) CostUSD(input, output, cacheRead, cacheWrite uint64) float64 {
	return float64(input)*p.InputPer1M/1_000_000 +
		float64(output)*p.OutputPer1M/1_000_000 +
		float64(cacheRead)*p.CacheReadPer1M/1_000_000 +
		float64(cacheWrite)*p.CacheWritePer1M/1_000_000
}
