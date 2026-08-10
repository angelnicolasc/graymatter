package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// doctor: end-to-end setup verification. Closes the issue #3 failure mode
// ("MCP connected but nothing ever gets written") by checking every link of
// the chain: binary → data dir → store → MCP wiring → agent instructions.

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | info | warn | fail
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the GrayMatter setup in this directory",
		Long: `Checks every link in the chain that makes agent memory work:

  1. graymatter binary reachable on PATH
  2. data directory exists and is writable
  3. store opens; fact/agent counts; lock state (single-writer detection)
  4. MCP server wired into at least one client config
  5. CLAUDE.md / AGENTS.md tell the model to use the memory tools

Exit code is 1 only when a check fails outright; warnings exit 0.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			checks := []checkResult{
				checkBinaryOnPath(),
				checkDataDir(dataDir),
				checkStore(dataDir),
				checkMCPWiring("."),
				checkInstructions("."),
			}

			if jsonOut {
				ok := true
				for _, c := range checks {
					if c.Status == "fail" {
						ok = false
					}
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(map[string]any{
					"data_dir": dataDir,
					"ok":       ok,
					"checks":   checks,
				}); err != nil {
					return err
				}
				if !ok {
					os.Exit(1)
				}
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(), "GrayMatter doctor — data dir %q\n\n", dataDir)
			var fails, warns int
			for _, c := range checks {
				glyph := map[string]string{"ok": "✓", "info": "·", "warn": "!", "fail": "✗"}[c.Status]
				fmt.Fprintf(cmd.OutOrStdout(), "  %s %-14s %s\n", glyph, c.Name, c.Detail)
				if c.Hint != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "    → %s\n", c.Hint)
				}
				switch c.Status {
				case "fail":
					fails++
				case "warn":
					warns++
				}
			}

			fmt.Fprintln(cmd.OutOrStdout())
			switch {
			case fails > 0:
				fmt.Fprintf(cmd.OutOrStdout(), "%d check(s) failed.\n", fails)
				os.Exit(1)
			case warns > 0:
				fmt.Fprintf(cmd.OutOrStdout(), "%d warning(s) — memory may not be used by your agent. See hints above.\n", warns)
			default:
				fmt.Fprintln(cmd.OutOrStdout(), "Everything looks good.")
			}
			return nil
		},
	}
}

func checkBinaryOnPath() checkResult {
	c := checkResult{Name: "binary"}
	path, err := exec.LookPath("graymatter")
	switch {
	case err == nil:
		c.Status, c.Detail = "ok", "graymatter on PATH ("+path+")"
	case errors.Is(err, exec.ErrDot):
		c.Status = "warn"
		c.Detail = "graymatter found only in the current directory, not on PATH"
		c.Hint = "MCP clients launch `graymatter` by name — move the binary onto PATH or re-run `graymatter init` (Windows: it registers the directory for you)"
	default:
		c.Status = "warn"
		c.Detail = "graymatter is not on PATH"
		c.Hint = "MCP clients launch `graymatter` by name; install with `go install github.com/angelnicolasc/graymatter/cmd/graymatter@latest` or move the binary into a PATH directory"
	}
	return c
}

func checkDataDir(dir string) checkResult {
	c := checkResult{Name: "data dir"}
	info, err := os.Stat(dir)
	if err != nil {
		c.Status = "warn"
		c.Detail = fmt.Sprintf("%s does not exist", dir)
		c.Hint = "run `graymatter init` to initialise this project"
		return c
	}
	if !info.IsDir() {
		c.Status, c.Detail = "fail", dir+" exists but is not a directory"
		return c
	}
	probe := filepath.Join(dir, ".doctor_probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		c.Status = "fail"
		c.Detail = fmt.Sprintf("%s is not writable: %v", dir, err)
		return c
	}
	_ = os.Remove(probe)
	c.Status, c.Detail = "ok", dir+" exists and is writable"
	return c
}

// staleAfter is how long a project may sit initialised with nothing stored
// before doctor stops calling that normal. Long enough that a genuinely new
// project stays quiet, short enough that a broken setup surfaces inside a
// working day.
const staleAfter = 24 * time.Hour

// projectAge reports how long ago the project was initialised, using MEMORY.md
// as the anchor: init writes it once and nothing else touches it, unlike the
// data directory whose mtime moves every time the daemon starts.
func projectAge(dir string) (time.Duration, bool) {
	info, err := os.Stat(filepath.Join(dir, "MEMORY.md"))
	if err != nil {
		return 0, false
	}
	return time.Since(info.ModTime()), true
}

// flagIfUnused downgrades an otherwise-healthy store check to a warning when a
// project has been set up for a while and still holds nothing.
//
// This is the gap issue #14 fell through: wiring, instructions and store can
// all be green while the agent never calls a single tool, and the old summary
// still read "Everything looks good". A user had no way to tell a fresh install
// apart from a week of silence, so the failure produced no bug reports, just
// people quietly giving up.
func flagIfUnused(c checkResult, dir string, facts int) checkResult {
	if facts > 0 || (c.Status != "ok" && c.Status != "info") {
		return c
	}
	age, ok := projectAge(dir)
	if !ok || age < staleAfter {
		return c
	}
	c.Status = "warn"
	c.Detail = fmt.Sprintf("initialised %d day(s) ago and still holds no facts", int(age.Hours()/24))
	c.Hint = "the tools are wired but nothing is calling them; confirm CLAUDE.md / AGENTS.md carry the memory block (re-run `graymatter init` to refresh it) and that your agent loads that file, or use `graymatter init --global` to install it for every project"
	return c
}

func checkStore(dir string) checkResult {
	c := checkResult{Name: "store"}
	dbPath := filepath.Join(dir, "gray.db")
	if _, err := os.Stat(dbPath); err != nil {
		c.Status, c.Detail = "info", "no database yet (gray.db is created on first write)"
		// Anyone reading this line right after `init` is one step from the most
		// common false alarm: the client has not been restarted, so the tools
		// are not loaded yet and nothing has had a chance to write. The hint
		// disappears as soon as a single fact exists.
		c.Hint = "if your agent cannot see the memory tools, restart your MCP client; clients launch their servers at startup, so a session that predates `graymatter init` never picks them up"
		return flagIfUnused(c, dir, 0)
	}

	// Preferred path: ask the daemon, which owns the store in normal
	// operation. This is also what proves the daemon is healthy end to end.
	if dc, err := daemon.ConnectNoSpawn(dir); err == nil {
		defer func() { _ = dc.Close() }()
		agents, err := dc.ListAgents()
		if err != nil {
			c.Status, c.Detail = "fail", fmt.Sprintf("daemon up but listing agents failed: %v", err)
			return c
		}
		facts := 0
		for _, a := range agents {
			if st, err := dc.Stats(a); err == nil {
				facts += st.FactCount
			}
		}
		pending, _ := dc.PendingVectorCount()
		c.Status = "ok"
		c.Detail = fmt.Sprintf("served by daemon — %d fact(s) across %d agent(s)", facts, len(agents))
		if pending > 0 {
			c.Status = "warn"
			c.Detail += fmt.Sprintf(", %d pending vector write(s)", pending)
			c.Hint = "pending vectors in a quiescent system mean the embedding backend is failing — check your embedding configuration (Ollama URL / API keys)"
		}
		return flagIfUnused(c, dir, facts)
	}

	// No daemon: read-only probe. Lock contention here means some non-daemon
	// process (e.g. a Go program embedding the library, or a stale daemon)
	// holds the write lock.
	store, err := memory.Open(memory.StoreConfig{DataDir: dir, ReadOnly: true})
	if err != nil {
		if strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "timeout") {
			c.Status = "warn"
			c.Detail = "gray.db is held by a non-daemon process (bbolt is single-writer)"
			c.Hint = "another program is holding the store directly — a Go app embedding the library, or `graymatter ... --no-daemon`; close it and clients will start their own daemon" + lsofHint(dbPath)
			return c
		}
		c.Status, c.Detail = "fail", fmt.Sprintf("store failed to open: %v", err)
		return c
	}
	defer func() { _ = store.Close() }()

	agents, err := store.ListAgents()
	if err != nil {
		c.Status, c.Detail = "fail", fmt.Sprintf("store opened but listing agents failed: %v", err)
		return c
	}
	facts := 0
	for _, a := range agents {
		if st, err := store.Stats(a); err == nil {
			facts += st.FactCount
		}
	}
	pending := store.PendingVectorCount()
	c.Status = "ok"
	c.Detail = fmt.Sprintf("no daemon running — %d fact(s) across %d agent(s) (direct read)", facts, len(agents))
	if pending > 0 {
		c.Status = "warn"
		c.Detail += fmt.Sprintf(", %d pending vector write(s)", pending)
		c.Hint = "pending vectors in a quiescent system mean the embedding backend is failing — check your embedding configuration (Ollama URL / API keys)"
	}
	return flagIfUnused(c, dir, facts)
}

// wiredAgents returns the known agents whose MCP config in this project
// references graymatter. Paths come from knownAgents, the same table the init
// writers use, so doctor cannot look somewhere the writer never wrote.
func wiredAgents(projectDir string) []agentDef {
	var out []agentDef
	for _, a := range knownAgents(projectDir) {
		if a.configPath == nil {
			continue
		}
		p, err := a.configPath(projectDir)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// String containment is deliberately tolerant: it covers JSON, JSONC
		// (comments) and TOML without needing three parsers here.
		if strings.Contains(string(data), "graymatter") {
			out = append(out, a)
		}
	}
	return out
}

func checkMCPWiring(projectDir string) checkResult {
	c := checkResult{Name: "mcp wiring"}
	var wired []string
	for _, a := range wiredAgents(projectDir) {
		p, _ := a.configPath(projectDir)
		wired = append(wired, fmt.Sprintf("%s (%s)", a.name, p))
	}
	if len(wired) == 0 {
		c.Status = "warn"
		c.Detail = "no MCP client config references graymatter"
		c.Hint = "run `graymatter init` to wire Claude Code, Cursor, Codex, and OpenCode automatically"
		return c
	}
	c.Status, c.Detail = "ok", strings.Join(wired, ", ")
	return c
}

// checkInstructions asks the question that matters: for each client actually
// wired here, does a briefing reach it?
//
// It used to look only at the project's CLAUDE.md and AGENTS.md, which made it
// contradict `graymatter init --global` — the very command its own hint
// recommends. A global install writes the block where Claude Code and OpenCode
// read it in every project, and doctor reported that as missing (issue #17).
// The inverse error is just as bad: crediting a global block to Cursor, which
// has no global file graymatter writes, would hide a real gap. So coverage is
// resolved per agent, from the same table that decides where init writes.
func checkInstructions(projectDir string) checkResult {
	c := checkResult{Name: "instructions"}

	agents := wiredAgents(projectDir)
	if len(agents) == 0 {
		// Nothing wired, so there is no agent whose needs we can check against.
		// Demanding a file for all five here would double-report the missing
		// wiring that `mcp wiring` already warns about, so this degrades to the
		// older question — is there a briefing at all — while still reporting a
		// stale one.
		return checkAnyInstructions(projectDir)
	}

	// An agent whose only briefing is stale is "uncovered", but reporting it in
	// both lists says the same thing twice and buries the actionable half. It
	// is counted as outdated only.
	var covered, uncovered, stale []string
	seenStale := map[string]bool{}
	for _, a := range agents {
		if a.instructionFile == "" {
			continue
		}
		// Every source is inspected, not just the first that covers the agent.
		// A project block does not shadow the global one — Claude Code loads
		// the user file and the project file, so an outdated block in either
		// is still being fed to the model and has to be reported.
		hit, outdated := "", false
		for _, cand := range instructionSources(projectDir, a) {
			switch inspectBlock(cand.path) {
			case blockCurrent, blockCustom:
				if hit == "" {
					hit = cand.label
				}
			case blockStale:
				outdated = true
				if !seenStale[cand.path] {
					seenStale[cand.path] = true
					stale = append(stale, cand.label)
				}
			}
		}
		switch {
		case hit != "":
			covered = append(covered, fmt.Sprintf("%s → %s", a.name, hit))
		case outdated:
			// already accounted for in `stale`
		default:
			uncovered = append(uncovered, a.name)
		}
	}

	switch {
	case len(uncovered) > 0 && len(stale) > 0:
		c.Status = "warn"
		c.Detail = fmt.Sprintf("nothing tells %s to use the memory tools, and %s %s an outdated block",
			strings.Join(uncovered, ", "), strings.Join(stale, ", "), carries(len(stale)))
		c.Hint = "re-run `graymatter init` — it adds what is missing and replaces the managed block in place, leaving everything outside the markers alone"
	case len(uncovered) > 0:
		c.Status = "warn"
		c.Detail = "nothing tells " + strings.Join(uncovered, ", ") + " to use the memory tools"
		c.Hint = "an MCP connection only makes tools *available* — without instructions the model never calls them; run `graymatter init` (or `--global` for every project)"
	case len(stale) > 0:
		// Everything is covered, but by a briefing from before v0.7.0 — the one
		// that told the model to search "when prior context might matter",
		// which is a condition it can resolve to false every time (issue #14).
		c.Status = "warn"
		c.Detail = strings.Join(stale, ", ") + " " + carries(len(stale)) + " a memory block from an older version"
		c.Hint = "the old block described the tools instead of prescribing a procedure, which is why agents did not call them; re-run `graymatter init` to replace it in place"
	default:
		c.Status, c.Detail = "ok", strings.Join(covered, ", ")
	}
	return c
}

// carries agrees the verb with the number of files, so a single outdated file
// does not read as a grammar slip in the one message meant to be trusted.
func carries(n int) string {
	if n == 1 {
		return "carries"
	}
	return "carry"
}

// checkAnyInstructions answers "is there a briefing anywhere" for a project
// with no MCP client wired yet. Staleness is still reported: an old block is
// worth flagging whether or not anything is wired.
func checkAnyInstructions(projectDir string) checkResult {
	c := checkResult{Name: "instructions"}
	var present, stale []string
	for _, a := range knownAgents(projectDir) {
		if a.instructionFile == "" {
			continue
		}
		for _, cand := range instructionSources(projectDir, a) {
			switch inspectBlock(cand.path) {
			case blockCurrent, blockCustom:
				if !contains(present, cand.label) {
					present = append(present, cand.label)
				}
			case blockStale:
				if !contains(stale, cand.label) {
					stale = append(stale, cand.label)
				}
			}
		}
	}
	switch {
	case len(present) == 0 && len(stale) == 0:
		c.Status = "warn"
		c.Detail = "neither CLAUDE.md nor AGENTS.md tells the model to use the memory tools"
		c.Hint = "an MCP connection only makes tools *available* — without instructions the model never calls them; run `graymatter init` to add the memory block"
	case len(present) == 0:
		c.Status = "warn"
		c.Detail = strings.Join(stale, ", ") + " " + carries(len(stale)) + " a memory block from an older version"
		c.Hint = "the old block described the tools instead of prescribing a procedure, which is why agents did not call them; re-run `graymatter init` to replace it in place"
	default:
		c.Status, c.Detail = "ok", strings.Join(present, ", ")+" mention the memory tools"
		if len(stale) > 0 {
			c.Status = "warn"
			c.Detail += "; " + strings.Join(stale, ", ") + " " + map[bool]string{true: "is", false: "are"}[len(stale) == 1] + " outdated"
			c.Hint = "re-run `graymatter init` to replace the managed block in place"
		}
	}
	return c
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// instructionSources lists where a briefing for this agent could live, project
// file first: a project block overrides, and is what the user most likely means
// when both exist.
func instructionSources(projectDir string, a agentDef) []struct{ path, label string } {
	out := []struct{ path, label string }{
		{filepath.Join(projectDir, a.instructionFile), a.instructionFile},
	}
	if a.globalInstruction != nil {
		if p, err := a.globalInstruction(); err == nil {
			out = append(out, struct{ path, label string }{p, p + " (global)"})
		}
	}
	return out
}

// lsofHint suggests the lock-holder lookup command on platforms that have one.
func lsofHint(dbPath string) string {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		return fmt.Sprintf(" (find the holder with `lsof %s`)", dbPath)
	}
	return ""
}
