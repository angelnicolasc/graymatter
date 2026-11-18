package contextblock

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/internal/tokens"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestPlanTK1BudgetAndSelection(t *testing.T) {
	const budget = DefaultBudgetTokens
	const byteLimit = 3 * budget
	bodyOverhead := len(RenderBody(nil)) + len("- \n")
	exact := strings.Repeat("x", byteLimit-bodyOverhead)
	words := strings.Repeat("a ", budget)
	tests := []struct {
		name  string
		facts []memory.Fact
		want  []string
	}{
		{"exact byte boundary", []memory.Fact{fact("exact", 1, 0, exact)}, []string{"exact"}},
		{"final newline counts", []memory.Fact{fact("large", 1, 0, exact+"x"), fact("small", .5, 0, "small")}, []string{"small"}},
		{"long identifiers", []memory.Fact{fact("long", 1, 0, strings.Repeat("partition_audit_key_", 100)), fact("short", .5, 0, "short")}, []string{"short"}},
		{"UTF-8 bytes", []memory.Fact{fact("wide", 1, 0, strings.Repeat("界", budget))}, nil},
		{"approximation still binds", []memory.Fact{fact("many", 1, 0, words), fact("few", .5, 0, "few")}, []string{"few"}},
		{"empty selection", nil, nil},
		{"very large budget", []memory.Fact{fact("short", 1, 0, "short")}, []string{"short"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limit := budget
			if tt.name == "very large budget" {
				limit = math.MaxInt
			}
			selected, stats := Select(tt.facts, limit)
			body := RenderBody(selected)
			if got := ids(selected); !slices.Equal(got, tt.want) {
				t.Errorf("selected %v, want %v", got, tt.want)
			}
			approx := tokens.Approx(body)
			if approx > limit || (len(body)+2)/3 > limit {
				t.Errorf("body costs %d estimated tokens and %d bytes at budget %d", approx, len(body), limit)
			}
			if stats.TokensUsed != approx {
				t.Errorf("Stats.TokensUsed = %d, want legacy approximate count %d", stats.TokensUsed, approx)
			}
		})
	}
}

func FuzzPlanTK1Budget(f *testing.F) {
	f.Add(strings.Repeat("deployment_partition_", 90), "short", DefaultBudgetTokens)
	f.Add(strings.Repeat("界", DefaultBudgetTokens), strings.Repeat("a ", 200), DefaultBudgetTokens)
	f.Fuzz(func(t *testing.T, first, second string, budget int) {
		if budget < MinBudgetTokens || budget > 1<<16 {
			budget = DefaultBudgetTokens
		}
		facts := []memory.Fact{fact("first", 1, 0, first), fact("second", .5, 0, second)}
		selected, stats := Select(facts, budget)
		body := RenderBody(selected)
		approx := tokens.Approx(body)
		if approx > budget || (len(body)+2)/3 > budget || stats.TokensUsed != approx {
			t.Fatalf("body uses %d estimated tokens, %d bytes and reports %d at budget %d", approx, len(body), stats.TokensUsed, budget)
		}
	})
}

func TestPlanTK1LegacyFixtureByteBoundary(t *testing.T) {
	large := fact("large", 1, 0, "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen")
	small := fact("small", .5, 0, "short")
	if body := RenderBody([]memory.Fact{large}); len(body) != 130 || !strings.HasSuffix(body, "\n") {
		t.Fatalf("first fact body = %q (%d bytes), want 130 bytes including final newline", body, len(body))
	}
	for _, tt := range []struct {
		budget int
		facts  []memory.Fact
		want   []string
	}{
		{30, []memory.Fact{large, small}, []string{"small"}},
		{43, []memory.Fact{large}, nil},
		{44, []memory.Fact{large}, []string{"large"}},
	} {
		selected, stats := Select(tt.facts, tt.budget)
		body := RenderBody(selected)
		if got := ids(selected); !slices.Equal(got, tt.want) {
			t.Errorf("budget %d selected %v, want %v", tt.budget, got, tt.want)
		}
		approx := tokens.Approx(body)
		if approx > tt.budget || len(body) > 3*tt.budget || stats.TokensUsed != approx {
			t.Errorf("budget %d: body uses %d estimated tokens and %d bytes, reports %d", tt.budget, approx, len(body), stats.TokensUsed)
		}
	}
}
