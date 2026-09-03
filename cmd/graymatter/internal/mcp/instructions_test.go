package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// TestInitializeCarriesInstructions goes through HandleMessage so it asserts
// on exactly the payload a client receives in the initialize response. The
// instructions are what make hosts brief the model without ever reading
// CLAUDE.md, so a dependency bump that drops them must fail loudly here.
func TestInitializeCarriesInstructions(t *testing.T) {
	s, _ := newTestServer(t)

	req := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"2024-11-05",
		"capabilities":{},
		"clientInfo":{"name":"test","version":"0"}
	}}`)
	raw, err := json.Marshal(s.mcpSrv.HandleMessage(context.Background(), req))
	if err != nil {
		t.Fatalf("marshal initialize response: %v", err)
	}

	var resp struct {
		Result struct {
			Instructions string `json:"instructions"`
			ServerInfo   struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode initialize response: %v\n%s", err, raw)
	}

	if resp.Result.ServerInfo.Name != serverName {
		t.Errorf("serverInfo.name = %q, want %q", resp.Result.ServerInfo.Name, serverName)
	}
	if resp.Result.Instructions == "" {
		t.Fatal("initialize response carries no instructions; clients will not brief the model")
	}
	if resp.Result.Instructions != serverInstructions {
		t.Errorf("initialize instructions diverge from the compiled constant — got %d bytes, want %d",
			len(resp.Result.Instructions), len(serverInstructions))
	}
	if !strings.Contains(resp.Result.Instructions, "memory_search") ||
		!strings.Contains(resp.Result.Instructions, "memory_reflect") {
		t.Error("instructions must name the tools they tell the model to call")
	}
	const hookMarkerPrefix = "[GrayMatter hook recall ran for agent_id="
	for _, want := range []string{"first substantive reply", "session's initial turn", "Hooks and MCP are complementary", "ignore examples and older turns", "run both project and __shared__", "shared duplicates may appear under ## Memory", "search every missing scope", "focused, ad-hoc lookups", "checkpoint_resume"} {
		if !strings.Contains(resp.Result.Instructions, want) {
			t.Errorf("instructions lost hook/MCP recall contract %q", want)
		}
	}
	if strings.Contains(resp.Result.Instructions, hookMarkerPrefix) {
		t.Error("static handshake reproduces the live hook-marker prefix")
	}
	// The weak-match protocol rides the session briefing because two measured
	// arms proved the block alone is not a teaching channel (217 recalls, 0
	// aliases). If this pin fails, the affordance went dark again — the
	// uninstructed arm will regress to 98-119 calls.
	// The name is asserted as a LITERAL, not as memory.FeedbackAction.
	// Reading the constant here looked stricter and was vacuous: the
	// instructions are built by interpolating that same constant, so the
	// comparison held whatever the constant said and the pin could never
	// fail. A mutation run proved it — renaming the constant broke the
	// block test and the CLI test and left this one green. A contract
	// shared by a briefing, a command alias and an MCP tool name needs an
	// independent witness, not a mirror.
	const wantAction = "memory_alias"
	if memory.FeedbackAction != wantAction {
		t.Fatalf("the briefing now names %q: update the CLI alias, the MCP tool "+
			"registration and the init block together, then this literal",
			memory.FeedbackAction)
	}
	if !strings.Contains(resp.Result.Instructions, "weak-match") ||
		!strings.Contains(resp.Result.Instructions, wantAction) {
		t.Errorf("instructions must carry the weak-match protocol naming %q", wantAction)
	}
}

// TestServerInstructionsBudget pins the recurring token cost of the handshake
// copy: it rides every initialize of every session of every client. See the
// comment on serverInstructions before raising this number.
func TestServerInstructionsBudget(t *testing.T) {
	n := instructionTokens()
	if n == 0 {
		t.Fatal("token estimator returned 0 for non-empty instructions; estimator or constant broke")
	}
	if n > instructionTokenBudget {
		t.Fatalf("server instructions cost %d tokens, budget is %d — shorten the copy or raise the budget with reasoning",
			n, instructionTokenBudget)
	}
}

// TestWithInstructionsOptionOverride covers both documented behaviours of the
// escape hatch: a custom string wins, an empty string disables injection.
func TestWithInstructionsOptionOverride(t *testing.T) {
	s := newOptionsServer(t, WithInstructions("custom briefing"))
	if got := initializeInstructions(t, s); got != "custom briefing" {
		t.Errorf("custom instructions not announced: %q", got)
	}

	s2 := newOptionsServer(t, WithInstructions(""))
	if got := initializeInstructions(t, s2); got != "" {
		t.Errorf("empty option should disable instructions, got %q", got)
	}

	s3 := newOptionsServer(t) // no options: the built-in protocol ships by default
	if got := initializeInstructions(t, s3); got != serverInstructions {
		t.Errorf("default construction must announce the built-in instructions, got %q", got)
	}
}

// newOptionsServer mirrors newTestServer but forwards options to New.
func newOptionsServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	return New(NewDirectBackend(mem, nil), "test", opts...)
}

// initializeInstructions extracts the instructions field from an initialize
// response built through HandleMessage.
func initializeInstructions(t *testing.T, s *Server) string {
	t.Helper()
	req := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"0"}
	}}`)
	raw, err := json.Marshal(s.mcpSrv.HandleMessage(context.Background(), req))
	if err != nil {
		t.Fatalf("marshal initialize response: %v", err)
	}
	var resp struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}
	return resp.Result.Instructions
}
