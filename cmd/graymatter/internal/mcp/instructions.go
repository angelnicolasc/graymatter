package mcp

import (
	"github.com/angelnicolasc/graymatter/internal/tokens"
)

// serverInstructions is returned to the client in the initialize handshake
// (mcp server.WithInstructions). Clients surface it to the model at the start
// of every session, which is what makes the memory tools get called even in
// hosts that never read CLAUDE.md or AGENTS.md — the failure mode behind
// issues #3 and #14.
//
// It is a compile-time constant on purpose. The initialize response is not a
// data channel: this string must never contain store contents, agent IDs, or
// anything derived from runtime state (see docs/threat-model.md on treating
// memory as untrusted input — recalled facts reach the model through tool
// results inside the session, never through this field).
//
// Budget: the text rides every initialize of every client, so its recurring
// token cost has to stay trivial against the savings it produces. The
// TestServerInstructionsBudget test pins it at 240 tokens via the same
// estimator every benchmark uses; raise the budget in that test, with
// reasoning, rather than growing the copy silently.
const serverInstructions = `GrayMatter gives you persistent memory across sessions.
Session protocol:
1. Before your first substantive reply, call memory_search for the task at hand, then call it again with agent_id "__shared__" for user-level preferences.
2. When you learn something durable — a user preference, a decision, a correction, a completed milestone — store it with memory_reflect action=add. Err toward storing.
3. Found a contradiction? memory_reflect action=update supersedes the stale fact; never leave both versions live.
4. Long session? checkpoint_save before context gets heavy, checkpoint_resume next time.
Full guide: docs/AGENTS.md in the GrayMatter repository.`

// Option customises the MCP server.
type Option func(*serverOptions)

type serverOptions struct {
	instructions string
}

// WithInstructions overrides the instructions announced in the initialize
// handshake. An empty string disables instruction injection entirely — for
// tests and for callers that manage agent briefing themselves. Production
// callers use the default and never construct this option.
func WithInstructions(text string) Option {
	return func(o *serverOptions) { o.instructions = text }
}

func defaultServerOptions() serverOptions {
	return serverOptions{instructions: serverInstructions}
}

// instructionTokenBudget is the ceiling enforced by
// TestServerInstructionsBudget. See the comment on serverInstructions.
const instructionTokenBudget = 240

// instructionTokens measures the recurring cost of the handshake copy with
// the shared word-based estimator, so the number here and the numbers in
// benchmarks and docs mean the same thing.
func instructionTokens() int { return tokens.Approx(serverInstructions) }
