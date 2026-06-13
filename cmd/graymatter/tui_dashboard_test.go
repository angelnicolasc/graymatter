package main

import (
	"context"
	"strings"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
)

// TestDashboardRender_Empty verifies the dashboard renders an empty-state
// card (not panicking) when the store has no facts.
func TestDashboardRender_Empty(t *testing.T) {
	dir := t.TempDir()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dir
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	defer mem.Close()

	store := mem.Advanced()
	if store == nil {
		t.Fatal("nil advanced store")
	}

	m := tuiModel{store: &directStore{mem: mem, store: store}, width: 120, height: 32}
	// Execute the loader synchronously and dispatch the message.
	msg := m.loadDashboard()()
	loaded, ok := msg.(dashboardLoadedMsg)
	if !ok {
		t.Fatalf("expected dashboardLoadedMsg, got %T", msg)
	}
	m.dashboard = loaded.data

	out := m.renderDashboard(26)
	if out == "" {
		t.Fatal("empty dashboard render")
	}
	if !strings.Contains(out, "No memories stored") {
		t.Errorf("expected empty-state message, got: %q", out)
	}
}

// TestDashboardRender_WithFacts seeds a handful of real facts, loads the
// dashboard data, and checks that the computed aggregates and rendered body
// reflect the corpus (no mocks, no fabricated numbers).
func TestDashboardRender_WithFacts(t *testing.T) {
	dir := t.TempDir()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dir
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	defer mem.Close()

	store := mem.Advanced()
	if store == nil {
		t.Fatal("nil advanced store")
	}

	ctx := context.Background()
	seed := map[string][]string{
		"sales-closer": {
			"Acme signed the $240k contract on Q1 renewal.",
			"TechCorp expanded seats from 40 to 65.",
			"Globex renegotiated a 12% discount for multi-year commitment.",
		},
		"product-research": {
			"Users request dark mode in 68% of feedback submissions.",
			"Mobile onboarding drop-off concentrated on step 3.",
		},
	}
	for agent, facts := range seed {
		for _, f := range facts {
			if err := store.Put(ctx, agent, f); err != nil {
				t.Fatalf("put %s: %v", agent, err)
			}
		}
	}

	m := tuiModel{store: &directStore{mem: mem, store: store}, width: 140, height: 36}
	msg := m.loadDashboard()()
	loaded, ok := msg.(dashboardLoadedMsg)
	if !ok {
		t.Fatalf("expected dashboardLoadedMsg, got %T", msg)
	}
	d := loaded.data
	m.dashboard = d

	if d.AgentsN != 2 {
		t.Errorf("AgentsN = %d, want 2", d.AgentsN)
	}
	if d.FactsN != 5 {
		t.Errorf("FactsN = %d, want 5", d.FactsN)
	}
	if d.StorageB <= 0 {
		t.Errorf("StorageB = %d, want > 0", d.StorageB)
	}
	// Newest should land within the last 30 days (we just wrote).
	if time.Since(d.NewestAt) > time.Hour {
		t.Errorf("NewestAt too old: %v", d.NewestAt)
	}

	out := m.renderDashboard(28)
	for _, want := range []string{
		"FACTS", "MEMORY COST", "RECALLS", "HEALTH",
		"Agents · Inventory",
		"Weight Distribution",
		"Activity · Facts Created",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q", want)
		}
	}
}

// TestDashboardRender_WithTokens seeds the token_usage bucket directly and
// verifies the Token Cost panel renders a hero USD value, the cache-hit
// headline, and at least one model row — without fabricating numbers.
func TestDashboardRender_WithTokens(t *testing.T) {
	dir := t.TempDir()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dir
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	defer mem.Close()

	store := mem.Advanced()
	if store == nil {
		t.Fatal("nil advanced store")
	}

	// One fact so the dashboard leaves its empty-state path.
	if err := store.Put(context.Background(), "sales-closer", "baseline fact"); err != nil {
		t.Fatalf("put: %v", err)
	}

	// Seed a single realistic usage row (today, sonnet, healthy cache).
	if err := harness.RecordTokenUsage(store.DB(),
		"sales-closer", "claude-sonnet-4-6-20260301",
		10_000, 2_500, 20_000, 1_500); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	m := tuiModel{store: &directStore{mem: mem, store: store}, width: 140, height: 36}
	msg := m.loadDashboard()()
	loaded, ok := msg.(dashboardLoadedMsg)
	if !ok {
		t.Fatalf("expected dashboardLoadedMsg, got %T", msg)
	}
	m.dashboard = loaded.data

	if !m.dashboard.Tokens.Loaded {
		t.Fatal("Tokens.Loaded = false, want true")
	}
	if m.dashboard.Tokens.Requests != 1 {
		t.Errorf("Requests = %d, want 1", m.dashboard.Tokens.Requests)
	}
	if m.dashboard.Tokens.TotalUSD <= 0 {
		t.Errorf("TotalUSD = %f, want > 0", m.dashboard.Tokens.TotalUSD)
	}

	out := m.renderDashboard(30)
	for _, want := range []string{
		"Token Cost · 30d",
		"cache hit",
		"by model",
		"sonnet-4.6",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q", want)
		}
	}
}

// TestFormatUSD sanity-checks the USD formatter used by the Token Cost panel.
func TestFormatUSD(t *testing.T) {
	cases := map[float64]string{
		0:        "$0.00",
		0.05:     "$0.05",
		12.8:     "$12.80",
		999.99:   "$999.99",
		1500:     "$1.5K",
		2_500_000: "$2.5M",
	}
	for v, want := range cases {
		if got := formatUSD(v); got != want {
			t.Errorf("formatUSD(%g) = %q, want %q", v, got, want)
		}
	}
}

// TestFormatBytes sanity-checks the unit formatter.
func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:            "0 B",
		512:          "512 B",
		1024:         "1.0 KB",
		1024 * 1024:  "1.0 MB",
		1_500_000:    "1.4 MB",
	}
	for n, want := range cases {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestFormatCompact sanity-checks the integer compactor.
func TestFormatCompact(t *testing.T) {
	cases := map[int]string{
		0:         "0",
		999:       "999",
		1500:      "1.5K",
		2_300_000: "2.3M",
	}
	for n, want := range cases {
		if got := formatCompact(n); got != want {
			t.Errorf("formatCompact(%d) = %q, want %q", n, got, want)
		}
	}
}
