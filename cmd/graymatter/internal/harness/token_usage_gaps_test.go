package harness

import (
	"encoding/json"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// The token ledger is what `status` and the TUI quote back to users as cost.
// These tests pin its arithmetic — accumulation, day-window cutoffs, pricing
// fallbacks and canonical model names — because a silently wrong number here
// is worse than no number: it looks authoritative.

func tokenTestDB(t *testing.T) *bolt.DB {
	t.Helper()
	return killTestDB(t, t.TempDir())
}

func TestRecordTokenUsage_RejectsNilDBAndEmptyIdentifiers(t *testing.T) {
	if err := RecordTokenUsage(nil, "a", "m", 1, 1, 0, 0); err == nil {
		t.Error("nil db accepted")
	}
	db := tokenTestDB(t)
	defer func() { _ = db.Close() }()
	if err := RecordTokenUsage(db, "", "m", 1, 1, 0, 0); err == nil {
		t.Error("empty agent accepted")
	}
	if err := RecordTokenUsage(db, "a", "", 1, 1, 0, 0); err == nil {
		t.Error("empty model accepted")
	}
}

func TestRecordTokenUsage_AccumulatesIntoOneRow(t *testing.T) {
	db := tokenTestDB(t)
	defer func() { _ = db.Close() }()

	for i := 0; i < 2; i++ {
		if err := RecordTokenUsage(db, "agent", "model", 100, 50, 10, 5); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	sum, err := LoadTokenUsageSummary(db, 30)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Input != 200 || sum.Output != 100 || sum.CacheRead != 20 || sum.CacheWrite != 10 {
		t.Errorf("totals = %+v, want doubled first call", sum)
	}
	if sum.Requests != 2 {
		t.Errorf("requests = %d, want 2", sum.Requests)
	}
}

func TestLoadTokenUsageSummary_EmptyLedgerIsLoadedZeroes(t *testing.T) {
	db := tokenTestDB(t)
	defer func() { _ = db.Close() }()
	if err := initTokenUsageBucket(db); err != nil {
		t.Fatal(err)
	}
	sum, err := LoadTokenUsageSummary(db, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Loaded || sum.Requests != 0 || len(sum.ByModel) != 0 {
		t.Errorf("empty ledger summary = %+v, want Loaded with zero counters", sum)
	}
}

func TestLoadTokenUsageSummary_NilDBAndDayWindowCutoffs(t *testing.T) {
	if _, err := LoadTokenUsageSummary(nil, 30); err == nil {
		t.Error("nil db accepted")
	}
	db := tokenTestDB(t)
	defer func() { _ = db.Close() }()
	if err := initTokenUsageBucket(db); err != nil {
		t.Fatal(err)
	}

	today := dayKey(time.Now())
	old := dayKey(time.Now().AddDate(0, 0, -40))
	rows := map[string]string{
		string(tokenKey("a", "m", today)): `{"agent_id":"a","model":"m","day":"` + today + `","input":10,"requests":1}`,
		string(tokenKey("a", "m", old)):   `{"agent_id":"a","model":"m","day":"` + old + `","input":999,"requests":9}`,
	}
	for k, v := range rows {
		v := v
		if err := db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(bucketTokenUsage).Put([]byte(k), []byte(v))
		}); err != nil {
			t.Fatal(err)
		}
	}

	sum, err := LoadTokenUsageSummary(db, 7) // explicit small window
	if err != nil {
		t.Fatal(err)
	}
	if sum.Input != 10 || sum.Requests != 1 {
		t.Errorf("window kept stale row: %+v", sum)
	}
}

func TestLoadTokenUsageSummary_PricingCanonicalNamesCacheRateAndSorting(t *testing.T) {
	db := tokenTestDB(t)
	defer func() { _ = db.Close() }()
	if err := initTokenUsageBucket(db); err != nil {
		t.Fatal(err)
	}

	pricedModel := ""
	for k := range ModelPrices {
		pricedModel = k
		break // any priced key: arithmetic must follow whatever the table says
	}
	day := dayKey(time.Now())
	putRow := func(agent, model string, in, out, cr, cw, req uint64, dayStr string) {
		t.Helper()
		key := tokenKey(agent, model, dayStr)
		row := TokenUsage{AgentID: agent, Model: model, Day: dayStr,
			Input: in, Output: out, CacheRead: cr, CacheWrite: cw, Requests: req}
		if err := db.Update(func(tx *bolt.Tx) error {
			raw, err := json.Marshal(row)
			if err != nil {
				return err
			}
			return tx.Bucket(bucketTokenUsage).Put([]byte(key), raw)
		}); err != nil {
			t.Fatal(err)
		}
	}

	putRow("a", pricedModel, 1_000_000, 100_000, 500_000, 0, 3, day)
	putRow("b", "totally-unknown-model", 1, 1, 0, 0, 1, day)
	putRow("c", "corrupt", 1, 1, 0, 0, 1, day)
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketTokenUsage).Put(
			[]byte(tokenKey("c", "corrupt", day)), []byte("{broken json"))
	}); err != nil {
		t.Fatal(err)
	}

	sum, err := LoadTokenUsageSummary(db, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Partial {
		t.Error("unpriced model did not flag Partial")
	}
	// Both rows count toward the input side: the priced row's fresh input
	// plus cache reads, and the unpriced row's single input token.
	wantRate := 500_000.0 / 1_500_001.0
	if diff := sum.CacheHitRate - wantRate; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("cache hit rate = %v, want %v", sum.CacheHitRate, wantRate)
	}
	if len(sum.ByModel) != 2 {
		t.Fatalf("by-model rows = %d, want 2 (priced + unknown; corrupt skipped)", len(sum.ByModel))
	}
	canonicalOK := false
	for _, mb := range sum.ByModel {
		if mb.Model == canonicalModelName(pricedModel) {
			canonicalOK = true
			p, _ := LookupPricing(pricedModel)
			want := p.CostUSD(1_000_000, 100_000, 500_000, 0)
			if diff := mb.CostUSD - want; diff < -1e-6 || diff > 1e-6 {
				t.Errorf("cost = %v, want %v", mb.CostUSD, want)
			}
		}
	}
	if !canonicalOK {
		t.Errorf("priced model not canonicalised: %+v", sum.ByModel)
	}
	if sum.TotalUSD <= 0 {
		t.Errorf("total USD = %v, want positive from the priced row", sum.TotalUSD)
	}
}

func TestCanonicalModelName_Table(t *testing.T) {
	tests := []struct{ in, want string }{
		{"claude-opus-4-20260101", "opus-4"},
		{"claude-sonnet-4-7-20260401", "sonnet-4.7"},
		{"Claude-Sonnet-4.7-XYZ", "sonnet-4.7"},
		{"claude-sonnet-4-6-20260301", "sonnet-4.6"},
		{"claude-sonnet-4-20260101", "sonnet-4"},
		{"gpt-42-turbo", "gpt-42-turbo"}, // unexpected ids pass through verbatim
	}
	for _, tc := range tests {
		if got := canonicalModelName(tc.in); got != tc.want {
			t.Errorf("canonicalModelName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
