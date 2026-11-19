package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/internal/tokens"
)

// U0 uses sorted JSON keys and a space after each comma and colon. Count the
// tools array from tools/list, including output schemas, not only descriptions.
func measuredSurface(t *testing.T, s *Server) int {
	t.Helper()
	raw, err := json.Marshal(s.mcpSrv.HandleMessage(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Result struct {
			Tools any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(wire.Result.Tools)
	if err != nil {
		t.Fatal(err)
	}
	var spaced strings.Builder
	quoted, escaped := false, false
	for _, ch := range string(canonical) {
		spaced.WriteRune(ch)
		if escaped {
			escaped = false
			continue
		}
		if quoted && ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			quoted = !quoted
		}
		if !quoted && (ch == ',' || ch == ':') {
			spaced.WriteByte(' ')
		}
	}
	return tokens.Approx(initializeInstructions(t, s) + "\n" + spaced.String())
}

func TestCoreSurfaceBudget(t *testing.T) {
	s := newOptionsServer(t)
	if got := measuredSurface(t, s); got > 1400 {
		t.Fatalf("fixed surface = %d estimated tokens, budget 1400", got)
	}
}

func TestCoreSurfaceBudgetDetectsInflation(t *testing.T) {
	s := newOptionsServer(t, WithInstructions(strings.Repeat("inflated instruction ", 1000)))
	if got := measuredSurface(t, s); got <= 1400 {
		t.Fatalf("inflated negative control unexpectedly passed: %d", got)
	}
}
