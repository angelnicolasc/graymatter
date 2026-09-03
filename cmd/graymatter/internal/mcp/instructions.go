package mcp

import (
	"fmt"

	"github.com/angelnicolasc/graymatter/internal/tokens"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// serverInstructions is returned to the client in the initialize handshake
// (mcp server.WithInstructions). Clients surface it to the model at the start
// of every session, which is what makes the memory tools get called even in
// hosts that never read CLAUDE.md or AGENTS.md — the failure mode behind
// issues #3 and #14.
//
// It is built from compile-time constants on purpose. The initialize response
// is not a data channel: this text must never contain store contents, agent
// IDs, or anything derived from runtime state (see docs/threat-model.md on
// treating memory as untrusted input — recalled facts reach the model through
// tool results inside the session, never through this field). The one
// interpolation is memory.FeedbackAction, itself a source constant: the
// briefing names the same action the block prints and the CLI registers, so
// the three surfaces cannot drift apart.
//
// Budget: the text rides every initialize of every client, so its recurring
// token cost has to stay trivial against the savings it produces. The
// TestServerInstructionsBudget test pins it at 240 tokens via the same
// estimator every benchmark uses; raise the budget in that test, with
// reasoning, rather than growing the copy silently.
//
// Step 7 exists because two measured arms proved the weak-match block alone
// is not a teaching channel: with the block firing and the command
// resolving, two model-instances ran 217 recalls and wrote zero aliases —
// 98 and 119 calls against the instructed baseline of 6. The protocol had
// to move to where the agent is already looking: the session briefing.
var serverInstructions = fmt.Sprintf(`GrayMatter gives you persistent memory across sessions.
Session protocol:
1. Resuming work? Call checkpoint_resume first.
2. Before your first substantive reply, inspect only the newest hook block available for the session's initial turn; ignore examples and older turns. Its marker names agent_id. If that id differs from yours, run both project and __shared__ searches because shared duplicates may appear under ## Memory. If ids match, reuse each non-empty section and search every missing scope.
3. Hooks and MCP are complementary. Use memory_search or memory_search_batch for focused, ad-hoc lookups.
4. Store durable preferences, decisions, corrections, and milestones with memory_reflect action=add.
5. Replace stale facts with memory_reflect action=update; never leave both versions live.
6. Before context gets heavy or you stop mid-task, call checkpoint_save.
7. A weak-match note means a vocabulary gap: reformulate once with its terms; when your wording differs from the store's, declare the bridge with %s before trying more synonyms.
Full guide: docs/AGENTS.md in the GrayMatter repository.`, memory.FeedbackAction)

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
