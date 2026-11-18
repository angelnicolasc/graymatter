package mcp

import (
	"context"
	"fmt"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/internal/tokens"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// serverInstructions is returned to the client in the initialize handshake
// (mcp server.WithInstructions). Clients surface it to the model at the start
// of every session, which is what makes the memory tools get called even in
// hosts that never read CLAUDE.md or AGENTS.md — the failure mode behind
// issues #3 and #14.
//
// It is built from compile-time constants. The initialize response never
// includes store contents or agent IDs. Core stdio can use a fixed first line
// for empty local memory. Recalled facts reach the client through tool results.
// The one interpolation is memory.FeedbackAction, itself a source constant: the
// briefing names the same action the block prints and the CLI registers, so
// the three surfaces cannot drift apart.
//
// Budget: the text rides every initialize of every client, so its recurring
// token cost has to stay trivial against the savings it produces. The
// TestServerInstructionsBudget test pins it at 240 tokens via the same
// estimator every benchmark uses; raise the budget in that test, with
// reasoning, rather than growing the copy silently.
//
// Step 6 exists because two measured arms proved the weak-match block alone
// is not a teaching channel: with the block firing and the command
// resolving, two model-instances ran 217 recalls and wrote zero aliases —
// 98 and 119 calls against the instructed baseline of 6. The protocol had
// to move to where the agent is already looking: the session briefing.
var serverInstructions = fmt.Sprintf(`GrayMatter persists session memory.
1. Resume work with checkpoint_resume first.
2. Before your first substantive reply, inspect the newest hook block from the session's initial turn; ignore examples and older turns. Marker agent_id differs? run both project and __shared__ searches: shared duplicates may appear under ## Memory. Matching identity? Reuse non-empty sections; search every missing scope.
3. Hooks and MCP are complementary: memory_search/memory_search_batch serve focused, ad-hoc lookups.
4. memory_add stores durable preferences, decisions and milestones; memory_reflect action=update replaces stale versions.
5. checkpoint_save before context overload or stopping mid-task.
6. A weak-match note means vocabulary gaps: reformulate once with its terms; declare differing vocabulary with %s before more synonyms.
Confidence is writer-declared: verified/inferred/unverified, never automated verification. Add omission is inferred; update omission caps weakest target at inferred. Search/batch: min_confidence, confidence_weight [0,0.5], default 0; zero keeps filters. Filters suppress graph hints; explain shows final scores. Unsupported options require daemon upgrade/restart.
Guide: docs/AGENTS.md.`, memory.FeedbackAction)

// Option customises the MCP server.
type Option func(*serverOptions)

type serverOptions struct {
	instructions    string
	instructionsSet bool
	fullCatalog     bool
	localProject    string
}

// WithInstructions overrides the instructions announced in the initialize
// handshake. An empty string disables instruction injection entirely — for
// tests and for callers that manage agent briefing themselves. Production
// callers use the default and never construct this option.
func WithInstructions(text string) Option {
	return func(o *serverOptions) { o.instructions = text; o.instructionsSet = true }
}

// WithLocalProjectStartup enables the empty-memory briefing for a core stdio
// server whose project identity was captured at startup.
func WithLocalProjectStartup(project string) Option {
	return func(o *serverOptions) { o.localProject = project }
}

func localProjectMemoryEmpty(ctx context.Context, backend Backend, project string) bool {
	peeker, ok := backend.(interface {
		CheckpointPeekPending(string) (*session.PendingCheckpoint, error)
	})
	if !ok || ctx.Err() != nil {
		return false
	}
	for _, namespace := range []string{project, memory.SharedAgentID} {
		pending, err := peeker.CheckpointPeekPending(namespace)
		if err != nil || pending != nil {
			return false
		}
		facts, err := backend.List(namespace)
		if err != nil || ctx.Err() != nil {
			return false
		}
		for _, fact := range facts {
			if !fact.IsSuperseded() && !fact.IsAlias() {
				return false
			}
		}
	}
	return true
}

func defaultServerOptions() serverOptions {
	return serverOptions{instructions: coreServerInstructions}
}

// instructionTokenBudget is the ceiling enforced by
// TestServerInstructionsBudget. See the comment on serverInstructions.
// Confidence and conservative revision guidance share the existing budget;
// detailed examples remain in the full guide.
const instructionTokenBudget = 240

// instructionTokens measures the recurring cost of the handshake copy with
// the shared word-based estimator, so the number here and the numbers in
// benchmarks and docs mean the same thing.
func instructionTokens() int { return tokens.Approx(serverInstructions) }
