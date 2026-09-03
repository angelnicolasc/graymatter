package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Instruction-file writers: drop a memory-usage block into the agent
// instruction files (CLAUDE.md, AGENTS.md) so MCP hosts actually *call* the
// graymatter tools. Having the server wired in .mcp.json only makes the tools
// available — nothing tells the model to use them (issue #3).

const (
	instrBeginMarker = "<!-- graymatter:instructions:begin — managed by `graymatter init`; edits inside this block are overwritten -->"
	instrEndMarker   = "<!-- graymatter:instructions:end -->"
)

// instructionsBlock is the canonical memory-usage briefing, including the
// begin/end markers.
//
// Three things this text has to get right, all learned the hard way from issue
// #14 ("everything is green but the agent never stored anything"):
//
//  1. It has to read as a procedure, not as API documentation. The earlier
//     version described the five tools and said to search "when prior context
//     might matter" — a condition a model can resolve to false every single
//     time. Triggers are now unconditional and tabulated.
//  2. The agent_id has to be derivable, not invented. A template like
//     `<project>-<role>` produces a different id per session, which scatters
//     facts across namespaces and looks exactly like memory not working.
//  3. A hook's routine recall and an MCP search must not duplicate each other.
//     The hook marker makes that condition observable without taking away
//     checkpoint resume or focused searches.
//
// Param names must match the MCP tool schemas
// (cmd/graymatter/internal/mcp/server.go).
//
// blockTmpl uses ~ where the rendered markdown needs a backtick: Go raw strings
// cannot contain one, and this text is mostly code spans.
const blockTmpl = `
## Memory (GrayMatter)

You have persistent memory through the ~graymatter~ MCP tools. Hooks and MCP are
complementary; neither replaces the other. MCP wiring alone only makes tools
available; this briefing and optional Claude Code hooks define when they run.

This block can be installed globally, so it may reach a project that has no
GrayMatter wired. If ~memory_search~ is not in your toolbelt for this session,
skip the rest of this section. That is a check on what tools exist, not a
judgement call about whether memory seems useful: if the tools are there,
everything below applies.

### Your identity

Your ~agent_id~ is the name of this repository's root directory, used verbatim
every session. Add a ~-<role>~ suffix only when several agents share the repo
(~myapp-backend~, ~myapp-frontend~). Inventing a new id per session scatters
your facts across namespaces and looks exactly like memory being broken.

Facts every agent in the project should see go to the reserved id ~__shared__~.

### Every session, without exception

1. **Resuming long-running work**: call ~checkpoint_resume~ first.
2. **Before your first substantive reply**, inspect only the newest hook block
   available for the session's initial turn; ignore quoted examples and blocks
   from older turns. A real recall block begins with a bracketed
   ~GrayMatter hook recall ran~ marker
   naming its ~agent_id~. If that id differs from the ~agent_id~ you would
   search, run both project and ~__shared__~ searches: cross-namespace dedup may
   have placed a shared duplicate under ~## Memory~. When the ids match, reuse
   each non-empty section and run the search for every missing section. Fold
   all results into your working context before answering.
3. A fresh marker suppresses only those matching routine searches. Keep using
   ~memory_search~ or ~memory_search_batch~ for focused, ad-hoc lookups; hooks
   do not replace MCP writes, reflections, aliases, or checkpoint tools.
4. **Before you stop**, store what you learned (see the table) and call
   ~checkpoint_save~ if the task is unfinished.

### What triggers a call

| When this happens | Call |
|---|---|
| The newest hook block has a different id | ~memory_search~ for both the project and ~__shared__~ |
| A same-id hook block lacks a non-empty section | ~memory_search~ for that missing scope |
| You need additional or focused context | ~memory_search~ or ~memory_search_batch~ |
| The user states a preference | ~memory_add~ |
| You discover a project convention | ~memory_add~ with ~agent_id: "__shared__"~ |
| You make a non-obvious decision | ~memory_add~, include the reasoning |
| You fix a non-trivial bug or find a workaround | ~memory_add~ |
| The user corrects you | ~memory_reflect~ with ~action="update"~ |
| A stored fact became wrong | ~memory_reflect~ with ~action="forget"~ |
| A search comes back with a **weak-match note** | Reformulate **once** with the note's suggested terms; if your wording and the store's differ, declare it with ~ALIAS_TOOL~ before trying more synonyms |

Err toward storing. A fact you never needed costs nothing. One you failed to
store costs the same mistake a second time.

### The tools

| Tool | Required | Optional |
|---|---|---|
| ~memory_search~ | ~agent_id~, ~query~ | ~top_k~ (default 8) |
| ~memory_add~ | ~agent_id~, ~text~ | |
| ~memory_reflect~ | ~action~, ~agent~ | ~text~, ~target~ |
| ~ALIAS_TOOL~ | ~agent_id~, ~term~, ~equivalents~ | teach the store a vocabulary bridge |
| ~checkpoint_save~ | ~agent_id~ | ~state~ |
| ~checkpoint_resume~ | ~agent_id~ | |

⚠ ~memory_reflect~ takes ~agent~, not ~agent_id~. Every other tool,
including ~ALIAS_TOOL~, takes ~agent_id~. Mixing them up fails validation.

### The store learns its own vocabulary

When a search misses because your wording and the store's differ, the store
learns that bridge from use: declare it once with ~ALIAS_TOOL~, or let two
sessions repeat the same unknown word and the store promotes the alias by
itself. ~graymatter alias list~ shows which aliases you taught (~authored~)
and which the store concluded from use (~usage~). A wrong one is revised like
any fact.

### Store conclusions, not transcripts

One idea per call. Skip anything already in the code or the README, anything
transient (that is what checkpoints are for), and never store secrets.
`

func instructionsBlock() string {
	// The action name comes from memory.FeedbackAction — the same symbol the
	// weak-match block formats and the CLI registers as a command alias.
	// Hardcoding it here would leave this block naming a tool nobody answers
	// to the day that name changes, which is exactly the failure the
	// uninstructed-agent arms measured, in the one surface a user reads
	// first. The template writes ALIAS_TOOL; this substitutes it.
	body := strings.ReplaceAll(blockTmpl, "ALIAS_TOOL", memory.FeedbackAction)
	return instrBeginMarker + strings.ReplaceAll(body, "~", "`") + instrEndMarker + "\n"
}

// upsertInstructionsBlock writes the graymatter memory block into path:
//
//   - file missing            → created containing just the block
//   - file has the markers    → block between markers replaced in place
//   - file without markers    → block appended after the existing content
//
// The operation is idempotent: a second run over its own output is a no-op
// (changed=false). User content outside the markers is never touched.
func upsertInstructionsBlock(path string) (writeResult, error) {
	res := writeResult{path: path}
	block := instructionsBlock()

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("read %s: %w", path, err)
	}

	var next string
	switch {
	case len(data) == 0:
		next = block
	default:
		content := string(data)
		// Match the file's existing line endings before splicing. Writing LF
		// into a CRLF file left it with both, and once the block itself had
		// picked up CRLF — which is what a checkout with core.autocrlf=true
		// produces — the equality check below never held again: every run
		// rewrote the file, reported a change, and dirtied the tree.
		if usesCRLF(content) {
			block = toCRLF(block)
		}
		le := lineEnding(content)

		// Every managed block goes, and one canonical block comes back where
		// the first one was. Replacing only the first pair left a duplicate
		// behind still carrying the old text, which a check that reads the
		// first pair then happily reports as healthy.
		stripped, at := stripManagedBlocks(content)
		if at >= 0 {
			next = stripped[:at] + strings.TrimSuffix(block, le) + stripped[at:]
		} else {
			next = strings.TrimRight(stripped, "\r\n") + le + le + block
		}
	}

	if string(data) == next {
		return res, nil // already up to date
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return res, fmt.Errorf("write %s: %w", path, err)
	}
	res.changed = true
	return res, nil
}

// usesCRLF reports whether the file's dominant line ending is CRLF.
//
// Majority, not presence: one pasted CRLF line in an otherwise LF file must not
// drag the whole block over to CRLF. A file that is already mixed keeps what it
// has — the block follows the majority and the surrounding lines are left
// alone, which is what makes the result stable instead of trading one rewrite
// for another.
func usesCRLF(content string) bool {
	crlf := strings.Count(content, "\r\n")
	lines := strings.Count(content, "\n")
	return crlf*2 > lines
}

func lineEnding(content string) string {
	if usesCRLF(content) {
		return "\r\n"
	}
	return "\n"
}

func toCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// managedBlockSpans returns the [start,end) byte ranges of every managed block
// in content, in order.
//
// Pairing is non-greedy: a begin marker with another begin marker before the
// next end marker is orphaned — hand-mangled, half-pasted, whatever — and is
// skipped rather than paired with some later block's terminator. Pairing it
// would swallow everything in between, which is user content that was never
// inside a block.
//
// One scanner backs both the writer and the check in doctor, so the two cannot
// disagree about what counts as a block.
func managedBlockSpans(content string) [][2]int {
	var spans [][2]int
	for off := 0; off < len(content); {
		rel := strings.Index(content[off:], instrBeginMarker)
		if rel < 0 {
			break
		}
		begin := off + rel
		after := begin + len(instrBeginMarker)

		endRel := strings.Index(content[after:], instrEndMarker)
		if endRel < 0 {
			break // unterminated: nothing to pair here or after it
		}
		end := after + endRel + len(instrEndMarker)

		if nextRel := strings.Index(content[after:], instrBeginMarker); nextRel >= 0 && after+nextRel < end {
			off = after // this begin is orphaned; try the next one
			continue
		}
		spans = append(spans, [2]int{begin, end})
		off = end
	}
	return spans
}

// stripManagedBlocks removes every managed block from content and reports where
// the first one started in the result, or -1 when there was none.
func stripManagedBlocks(content string) (stripped string, insertAt int) {
	spans := managedBlockSpans(content)
	if len(spans) == 0 {
		return content, -1
	}
	var b strings.Builder
	prev := 0
	insertAt = -1
	for _, s := range spans {
		b.WriteString(content[prev:s[0]])
		if insertAt < 0 {
			insertAt = b.Len()
		}
		prev = s[1]
	}
	b.WriteString(content[prev:])
	return b.String(), insertAt
}

// writeInstructionFiles upserts the memory block into the given instruction
// files under projectDir. Returns one result per file, in the given order.
//
// The file list is the caller's to decide. Writing CLAUDE.md and AGENTS.md
// unconditionally is what put a CLAUDE.md in every OpenCode-only project: which
// files are needed depends on which agents are being wired, and only the caller
// knows that.
func writeInstructionFiles(projectDir string, names []string) []writeResult {
	var out []writeResult
	for _, name := range names {
		res, err := upsertInstructionsBlock(filepath.Join(projectDir, name))
		if err != nil {
			res.warn = fmt.Sprintf("could not update %s: %v", name, err)
		}
		out = append(out, res)
	}
	return out
}

// globalInstructionPaths returns the home-scoped instruction files agents read
// no matter which project they are working in. Writing the block here is what
// makes memory available in every repo without running `init` in each one
// (issue #17).
//
// Claude Code reads ~/.claude/CLAUDE.md. OpenCode renders the global
// ~/.config/opencode/AGENTS.md before any project-level AGENTS.md, so the block
// lands ahead of project content rather than replacing it. Both are plain
// markdown, so the same marker-based upsert applies and a user's own global
// instructions are left untouched.
//
// The paths come from knownAgents rather than being listed again here, so
// adding an agent with a global file is one edit and doctor learns about it at
// the same time.
func globalInstructionPaths() ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, a := range knownAgents(".") {
		if a.globalInstruction == nil {
			continue
		}
		p, err := a.globalInstruction()
		if err != nil {
			return nil, err
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// opencodeConfigDir resolves OpenCode's global config directory.
//
// It is ~/.config/opencode on every platform, Windows included: OpenCode does
// not use %APPDATA%. XDG_CONFIG_HOME takes precedence when set, which OpenCode
// supports but does not document on the rules page (anomalyco/opencode#6669) —
// hardcoding ~/.config would silently write where nothing reads.
func opencodeConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode"), nil
	}
	home, err := resolveHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

// writeGlobalInstructionFiles upserts the memory block into every path returned
// by globalInstructionPaths. Returns one result per file, in a stable order.
func writeGlobalInstructionFiles() []writeResult {
	paths, err := globalInstructionPaths()
	if err != nil {
		return []writeResult{{warn: fmt.Sprintf("could not resolve home directory: %v", err)}}
	}
	out := make([]writeResult, 0, len(paths))
	for _, p := range paths {
		res, err := upsertInstructionsBlock(p)
		if err != nil {
			res.warn = fmt.Sprintf("could not update %s: %v", p, err)
		}
		out = append(out, res)
	}
	return out
}

// installGlobalInstructions writes the block into the home-scoped files and
// prints what happened, returning any warnings for the caller to surface.
// Shared by the plain --global path and the wizard so the two cannot drift.
func installGlobalInstructions(quiet bool) []string {
	var warnings []string
	if !quiet {
		fmt.Println("\nGlobal agent instructions (active where GrayMatter MCP is wired):")
	}
	for _, res := range writeGlobalInstructionFiles() {
		if res.warn != "" {
			warnings = append(warnings, res.warn)
			continue
		}
		if !quiet {
			glyph, note := "✓", ""
			if !res.changed {
				glyph, note = "·", " (already present)"
			}
			fmt.Printf("  %s %s%s\n", glyph, res.path, note)
		}
	}
	return warnings
}

// printNextSteps closes both init paths with the same advice.
//
// The restart comes first because it is the step that makes a correct install
// look broken. MCP clients launch their servers at startup, so the tools do not
// exist in the session that ran init, and an agent that goes straight to
// calling them finds nothing however green doctor is. Shared between the plain
// and interactive paths so the two cannot drift.
// printNextSteps closes both init paths with the same advice.
//
// The restart comes first because it is the step that makes a correct install
// look broken. MCP clients launch their servers at startup, so the tools do not
// exist in the session that ran init, and an agent that goes straight to
// calling them finds nothing however green doctor is. Shared between the plain
// and interactive paths so the two cannot drift.
//
// pathChanged folds the Windows PATH note into the restart line instead of
// printing a second "restart" elsewhere: two restarts of two different things
// at the end of one install is exactly the dilution that gets both ignored.
func printNextSteps(kgAuto bool, pathChanged bool) {
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  1. Restart your MCP client (editor, agent, or terminal session)")
	if pathChanged {
		fmt.Printf(" — and PowerShell itself, which needs a restart to see the PATH entry this init added")
	}
	fmt.Printf(".\n")
	fmt.Printf("     Clients launch their MCP servers at startup, so the memory tools\n")
	fmt.Printf("     are not available in the session that just ran init.\n")
	if kgAuto {
		fmt.Printf("     Knowledge-graph auto-population is enabled (%s) and starts\n", daemon.KGSentinelFile)
		fmt.Printf("     with the same restart — first entities appear after ~20 facts.\n")
	}
	fmt.Printf("  2. graymatter doctor   — verify the whole setup end to end\n")
	fmt.Printf("  3. Try it out:\n")
	fmt.Printf("       graymatter remember \"my-agent\" \"user prefers bullet points\"\n")
	fmt.Printf("       graymatter recall  \"my-agent\" \"how should I format this?\"\n")
}

// normalizeEndings makes block comparisons independent of how a file was
// checked out or saved. Without it a CRLF file would compare unequal to the
// canonical block forever, and every check below would be a false alarm.
func normalizeEndings(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// blockStatus is what doctor needs to know about one instruction file.
type blockStatus int

const (
	blockAbsent  blockStatus = iota // no graymatter briefing at all
	blockCustom                     // mentions graymatter, but not our managed block
	blockStale                      // our markers, but not the text we ship today
	blockCurrent                    // our markers, current text
)

// inspectBlock classifies the managed block in path.
//
// The stale case is the one worth having. The briefing shipped before v0.7.0
// described the five tools and told the model to search "when prior context
// might matter" — a condition it can resolve to false every single time, which
// is the whole of issue #14. Its markers are byte-identical to today's, so a
// check that looks for a marker, or for the word "graymatter", reports it as
// healthy. Comparing the text is what separates "a briefing is present" from
// "the briefing that works is present", and it keeps working for the next
// revision without anyone remembering to bump a version.
//
// Every block in the file is checked, not just the first: a duplicate that
// still carries the old text is exactly the copy worth catching.
func inspectBlock(path string) blockStatus {
	data, err := os.ReadFile(path)
	if err != nil {
		return blockAbsent
	}
	content := normalizeEndings(string(data))
	want := normalizeEndings(strings.TrimSuffix(instructionsBlock(), "\n"))

	spans := managedBlockSpans(content)
	for _, s := range spans {
		if content[s[0]:s[1]] != want {
			return blockStale
		}
	}
	if len(spans) > 0 {
		return blockCurrent
	}

	// No managed block. A briefing someone wrote themselves still counts as
	// instructions — it is theirs, so it is never "stale".
	if strings.Contains(strings.ToLower(content), "graymatter") {
		return blockCustom
	}
	return blockAbsent
}
