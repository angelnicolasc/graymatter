package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
)

func initCmd() *cobra.Command {
	var (
		interactive      bool
		global           bool
		skipCodex        bool
		skipOpencode     bool
		skipClaudeCode   bool
		skipCursor       bool
		withAntigravity  bool
		skipInstructions bool
		noPath           bool
		only             string
		enableKG         bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialise a .graymatter directory and auto-wire every supported MCP client",
		Long: `Creates the .graymatter data directory and wires GrayMatter as an MCP
server into every supported client config it finds.

Safe to run multiple times — existing MCP entries from other tools are
preserved and graymatter's own entry is upserted (never duplicated).

GrayMatter is a general-purpose MCP server. The clients listed below are
just the ones we auto-wire; any MCP-compatible client works over stdio
(` + "`graymatter mcp serve`" + `) or HTTP (` + "`graymatter mcp serve --http 127.0.0.1:8080`" + `).

On Windows, init appends the executable's directory to your user PATH
(HKCU\Environment). Pass --no-path to skip that: a PATH entry pointing at a
directory other people can write is a hijack vector for every process that
resolves a command through it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if interactive {
				if err := runInteractiveWizard(dataDir, ".", quiet); err != nil {
					return err
				}
				// --global is orthogonal to which clients the wizard wired, so
				// it applies to both paths rather than only the flag-driven one.
				if global {
					for _, w := range installGlobalInstructions(quiet) {
						fmt.Fprintf(os.Stderr, "\n%s\n", w)
					}
				}
				return nil
			}
			dir := dataDir
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create data dir: %w", err)
			}

			memoryMD := filepath.Join(dir, "MEMORY.md")
			if _, err := os.Stat(memoryMD); os.IsNotExist(err) {
				content := "# GrayMatter Memory\n\nThis directory is managed by GrayMatter.\nDo not edit gray.db manually.\n"
				if err := os.WriteFile(memoryMD, []byte(content), 0o644); err != nil {
					return fmt.Errorf("create MEMORY.md: %w", err)
				}
			}

			// --kg persists the opt-in as data-dir state so every future
			// daemon honours it — including the ones MCP clients spawn with
			// their own environment, which an exported GRAYMATTER_KG never
			// reaches. Removal is `rm <dir>/kg.auto`; documented as such.
			if enableKG {
				if err := os.WriteFile(daemon.KGSentinelPath(dir), nil, 0o644); err != nil {
					return fmt.Errorf("write kg sentinel: %w", err)
				}
			}

			// Build the list of writers to run, honoring --only / --skip-*.
			// Both the agent set and the instruction file each agent reads come
			// from knownAgents, so this path and the wizard cannot disagree.
			agents := knownAgents(".")

			onlySet, err := parseOnlyFlag(only, agents)
			if err != nil {
				return err
			}
			skipped := map[string]bool{
				"claudecode": skipClaudeCode,
				"cursor":     skipCursor,
				"codex":      skipCodex,
				"opencode":   skipOpencode,
			}
			optedIn := map[string]bool{"antigravity": withAntigravity}

			enabled := make(map[string]bool, len(agents))
			for _, a := range agents {
				switch {
				case len(onlySet) > 0:
					enabled[a.id] = onlySet[a.id]
				case a.optIn:
					enabled[a.id] = optedIn[a.id]
				default:
					enabled[a.id] = !skipped[a.id]
				}
			}

			if !quiet {
				fmt.Printf("Initialised GrayMatter at %s\n", dir)
				fmt.Printf("  %s/gray.db       — bbolt database (created on first use)\n", dir)
				fmt.Printf("  %s/vectors/      — chromem-go vector index\n", dir)
				fmt.Printf("  %s/MEMORY.md     — human-readable index\n\n", dir)
				fmt.Println("Wired MCP for:")
			}

			var warnings []string
			for _, e := range agents {
				if !enabled[e.id] {
					if !quiet {
						reason := "skipped"
						if e.optIn {
							reason = "skipped — pass --with-" + e.id + " to enable"
						}
						fmt.Printf("  · %-14s %s\n", e.name, reason)
					}
					continue
				}
				res, err := e.run()
				if err != nil {
					if !quiet {
						fmt.Printf("  ! %-14s %s — %v\n", e.name, res.path, err)
					}
					continue
				}
				if res.warn != "" {
					warnings = append(warnings, res.warn)
					if !quiet {
						fmt.Printf("  ! %-14s %s (see note below)\n", e.name, res.path)
					}
					continue
				}
				if !quiet {
					glyph := "✓"
					note := ""
					if !res.changed {
						glyph = "·"
						note = " (already wired)"
					}
					fmt.Printf("  %s %-14s %s%s\n", glyph, e.name, res.path, note)
				}
			}

			// Agent instruction files: wiring the MCP server only makes the
			// tools available — the model also needs to be told to use them
			// (issue #3). Upsert the memory block into CLAUDE.md / AGENTS.md.
			if !skipInstructions {
				if !quiet {
					fmt.Println("\nAgent instructions (tells the model to actually use the tools):")
				}
				// Only the files the wired agents actually read. Writing both
				// unconditionally is what put a CLAUDE.md in every
				// OpenCode-only project (issue #13).
				for _, res := range writeInstructionFiles(".", instructionFilesFor(agents, enabled)) {
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
			}

			// --global also writes the block into the home-scoped instruction
			// files, so agents pick up memory in projects that never ran init
			// (issue #17).
			if global {
				warnings = append(warnings, installGlobalInstructions(quiet)...)
			}

			if !quiet {
				for _, w := range warnings {
					fmt.Fprintf(os.Stderr, "\n%s\n", w)
				}
				fmt.Printf("\ngraymatter is a general-purpose MCP server. Any MCP-compatible client works.\n")
				printNextSteps(enableKG)
			}

			// Putting the executable's directory on the user PATH means every
			// later process resolves commands through it. That is fine for a
			// directory only you can write, and a hijack vector for one you
			// share — so it has to be refusable.
			if noPath {
				return nil
			}
			if added, pathErr := addExeDirToUserPath(); pathErr != nil {
				if !quiet {
					exe, _ := os.Executable()
					fmt.Fprintf(os.Stderr,
						"\n  Warning: could not add %s to PATH: %v\n  Add it manually so you can type 'graymatter' from any directory.\n",
						filepath.Dir(exe), pathErr)
				}
			} else if added && !quiet {
				exe, _ := os.Executable()
				fmt.Printf("\n  Added %s to your PATH — restart PowerShell to apply\n", filepath.Dir(exe))
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "interactive setup wizard (prompts for which agents to wire)")
	cmd.Flags().BoolVar(&skipClaudeCode, "skip-claudecode", false, "do not touch .mcp.json")
	cmd.Flags().BoolVar(&skipCursor, "skip-cursor", false, "do not touch .cursor/mcp.json")
	cmd.Flags().BoolVar(&skipCodex, "skip-codex", false, "do not touch ~/.codex/config.toml")
	cmd.Flags().BoolVar(&skipOpencode, "skip-opencode", false, "do not touch opencode.jsonc")
	cmd.Flags().BoolVar(&withAntigravity, "with-antigravity", false, "also wire mcp_config.json for Antigravity")
	cmd.Flags().BoolVar(&skipInstructions, "skip-instructions", false, "do not write the memory block into CLAUDE.md / AGENTS.md")
	cmd.Flags().BoolVar(&enableKG, "kg", false,
		"enable knowledge-graph auto-population: writes "+daemon.KGSentinelFile+" into the data dir so every future daemon extracts entities and co-mention edges (remove the file to turn off)")
	cmd.Flags().BoolVar(&global, "global", false, "also write the memory block into ~/.claude/CLAUDE.md and ~/.config/opencode/AGENTS.md, so agents use memory in every project")
	cmd.Flags().BoolVar(&noPath, "no-path", false,
		"do not add the executable's directory to your user PATH")
	cmd.Flags().StringVar(&only, "only", "", "CSV of writers to run (overrides skip flags, not --skip-instructions): claudecode,cursor,codex,opencode,antigravity")
	return cmd
}

// parseOnlyFlag parses --only and rejects ids that match no known agent.
//
// Silently ignoring a typo used to mean "wire nothing" while still writing both
// instruction files, which looked like a partial success. Now that the
// instruction files follow the selection, an unrecognised id would produce a
// run that writes nothing at all and still exits 0 — so it has to be an error.
func parseOnlyFlag(v string, agents []agentDef) (map[string]bool, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	known := make(map[string]bool, len(agents))
	valid := make([]string, 0, len(agents))
	for _, a := range agents {
		known[a.id] = true
		valid = append(valid, a.id)
	}

	out := map[string]bool{}
	var unknown []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "" {
			continue
		}
		if !known[p] {
			unknown = append(unknown, p)
			continue
		}
		out[p] = true
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("--only: unknown agent %s (valid: %s)",
			strings.Join(unknown, ", "), strings.Join(valid, ", "))
	}
	return out, nil
}
