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
// begin/end markers. Param names here must match the MCP tool schemas
// (cmd/graymatter/internal/mcp/server.go) — notably memory_reflect's `agent`
// vs everyone else's `agent_id`.
func instructionsBlock() string {
	return instrBeginMarker + `
## Memory (GrayMatter)

This project has persistent agent memory via the ` + "`graymatter`" + ` MCP tools:

- ` + "`memory_search`" + ` (` + "`agent_id`, `query`" + `) — call at the **start of a task** when prior context might matter.
- ` + "`memory_add`" + ` (` + "`agent_id`, `text`" + `) — call whenever you learn something **durable**: user preferences, decisions, conventions, gotchas.
- ` + "`memory_reflect`" + ` (` + "`action`, `agent`, `text`/`target`" + `) — update or forget stale facts. ⚠ takes ` + "`agent`" + `, not ` + "`agent_id`" + `.
- ` + "`checkpoint_save`" + ` / ` + "`checkpoint_resume`" + ` (` + "`agent_id`" + `) — snapshot/restore session state before major refactors or across restarts.

Use a stable ` + "`agent_id`" + ` of the form ` + "`<project>-<role>`" + ` (e.g. ` + "`myapp-backend`" + `). Store conclusions, not conversation logs. Err on the side of remembering.
` + instrEndMarker + "\n"
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
