package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// Two things this text has to get right, both learned the hard way from issue
// #14 ("everything is green but the agent never stored anything"):
//
//  1. It has to read as a procedure, not as API documentation. The earlier
//     version described the five tools and said to search "when prior context
//     might matter" — a condition a model can resolve to false every single
//     time. Triggers are now unconditional and tabulated.
//  2. The agent_id has to be derivable, not invented. A template like
//     `<project>-<role>` produces a different id per session, which scatters
//     facts across namespaces and looks exactly like memory not working.
//
// Param names must match the MCP tool schemas
// (cmd/graymatter/internal/mcp/server.go).
//
// blockTmpl uses ~ where the rendered markdown needs a backtick: Go raw strings
// cannot contain one, and this text is mostly code spans.
const blockTmpl = `
## Memory (GrayMatter)

You have persistent memory through the ~graymatter~ MCP tools. Wiring the MCP
server only makes them available; nothing calls them for you. That is your job,
every session.

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

1. **Before your first reply**, call ~memory_search~ with the user's request as
   the query. Then call it again with ~agent_id~ set to ~__shared__~. Fold both
   results into your working context before you answer.
2. **Resuming long-running work**: ~checkpoint_resume~ first.
3. **Before you stop**, store what you learned (see the table) and call
   ~checkpoint_save~ if the task is unfinished.

### What triggers a call

| When this happens | Call |
|---|---|
| You start any task | ~memory_search~ |
| The user states a preference | ~memory_add~ |
| You discover a project convention | ~memory_add~ with ~agent_id: "__shared__"~ |
| You make a non-obvious decision | ~memory_add~, include the reasoning |
| You fix a non-trivial bug or find a workaround | ~memory_add~ |
| The user corrects you | ~memory_reflect~ with ~action="update"~ |
| A stored fact became wrong | ~memory_reflect~ with ~action="forget"~ |

Err toward storing. A fact you never needed costs nothing. One you failed to
store costs the same mistake a second time.

### The tools

| Tool | Required | Optional |
|---|---|---|
| ~memory_search~ | ~agent_id~, ~query~ | ~top_k~ (default 8) |
| ~memory_add~ | ~agent_id~, ~text~ | |
| ~memory_reflect~ | ~action~, ~agent~ | ~text~, ~target~ |
| ~checkpoint_save~ | ~agent_id~ | ~state~ |
| ~checkpoint_resume~ | ~agent_id~ | |

⚠ ~memory_reflect~ takes ~agent~, not ~agent_id~. The other four take
~agent_id~. Mixing them up fails validation.

### Store conclusions, not transcripts

One idea per call. Skip anything already in the code or the README, anything
transient (that is what checkpoints are for), and never store secrets.
`

func instructionsBlock() string {
	return instrBeginMarker + strings.ReplaceAll(blockTmpl, "~", "`") + instrEndMarker + "\n"
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
		begin := strings.Index(content, instrBeginMarker)
		end := strings.Index(content, instrEndMarker)
		if begin >= 0 && end > begin {
			// Replace the managed block, keep everything around it.
			next = content[:begin] + strings.TrimSuffix(block, "\n") + content[end+len(instrEndMarker):]
		} else {
			next = strings.TrimRight(content, "\n") + "\n\n" + block
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

// writeInstructionFiles upserts the memory block into CLAUDE.md and AGENTS.md
// in projectDir. Returns one result per file, in a stable order.
func writeInstructionFiles(projectDir string) []writeResult {
	var out []writeResult
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
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
func globalInstructionPaths() ([]string, error) {
	home, err := resolveHome()
	if err != nil {
		return nil, err
	}
	opencodeDir, err := opencodeConfigDir()
	if err != nil {
		return nil, err
	}
	return []string{
		filepath.Join(home, ".claude", "CLAUDE.md"),
		filepath.Join(opencodeDir, "AGENTS.md"),
	}, nil
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
		fmt.Println("\nGlobal agent instructions (apply in every project):")
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
func printNextSteps() {
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  1. Restart your MCP client (editor, agent, or terminal session).\n")
	fmt.Printf("     Clients launch their MCP servers at startup, so the memory tools\n")
	fmt.Printf("     are not available in the session that just ran init.\n")
	fmt.Printf("  2. graymatter doctor   — verify the whole setup end to end\n")
	fmt.Printf("  3. Try it out:\n")
	fmt.Printf("       graymatter remember \"my-agent\" \"user prefers bullet points\"\n")
	fmt.Printf("       graymatter recall  \"my-agent\" \"how should I format this?\"\n")
}

// hasInstructionsBlock reports whether path contains the managed block (or at
// least mentions graymatter, for users who wrote their own briefing).
// Used by `graymatter doctor`.
func hasInstructionsBlock(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	content := strings.ToLower(string(data))
	return strings.Contains(content, strings.ToLower(instrBeginMarker)) ||
		strings.Contains(content, "graymatter")
}
