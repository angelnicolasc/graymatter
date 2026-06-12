package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func initCmd() *cobra.Command {
	var (
		skipCodex        bool
		skipOpencode     bool
		skipClaudeCode   bool
		skipCursor       bool
		withAntigravity  bool
		skipInstructions bool
		only             string
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
(` + "`graymatter mcp serve`" + `) or HTTP (` + "`graymatter mcp serve --http :8080`" + `).`,
		RunE: func(cmd *cobra.Command, args []string) error {
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

			// Build the list of writers to run, honoring --only / --skip-*.
			type writerEntry struct {
				name    string
				id      string
				run     func() (writeResult, error)
				skip    bool
				optIn   bool
				enabled bool
			}

			onlySet := parseOnlyFlag(only)
			entries := []writerEntry{
				{name: "Claude Code", id: "claudecode", run: func() (writeResult, error) { return writeClaudeCodeProject(".") }, skip: skipClaudeCode},
				{name: "Cursor", id: "cursor", run: func() (writeResult, error) { return writeCursorProject(".") }, skip: skipCursor},
				{name: "Codex", id: "codex", run: writeCodexHome, skip: skipCodex},
				{name: "OpenCode", id: "opencode", run: func() (writeResult, error) { return writeOpencodeProject(".") }, skip: skipOpencode},
				{name: "Antigravity", id: "antigravity", run: func() (writeResult, error) { return writeAntigravityProject(".") }, optIn: !withAntigravity},
			}

			for i := range entries {
				e := &entries[i]
				if len(onlySet) > 0 {
					e.enabled = onlySet[e.id]
					continue
				}
				e.enabled = !e.skip && !e.optIn
			}

			if !quiet {
				fmt.Printf("Initialised GrayMatter at %s\n", dir)
				fmt.Printf("  %s/gray.db       — bbolt database (created on first use)\n", dir)
				fmt.Printf("  %s/vectors/      — chromem-go vector index\n", dir)
				fmt.Printf("  %s/MEMORY.md     — human-readable index\n\n", dir)
				fmt.Println("Wired MCP for:")
			}

			var warnings []string
			for _, e := range entries {
				if !e.enabled {
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
				for _, res := range writeInstructionFiles(".") {
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

			if !quiet {
				for _, w := range warnings {
					fmt.Fprintf(os.Stderr, "\n%s\n", w)
				}
				fmt.Printf("\ngraymatter is a general-purpose MCP server. Any MCP-compatible client works.\n")
				fmt.Printf("\nNext steps:\n")
				fmt.Printf("  graymatter doctor   — verify the whole setup end to end\n")
				fmt.Printf("  graymatter remember \"my-agent\" \"user prefers bullet points\"\n")
				fmt.Printf("  graymatter recall  \"my-agent\" \"how should I format this?\"\n")
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

	cmd.Flags().BoolVar(&skipClaudeCode, "skip-claudecode", false, "do not touch .mcp.json")
	cmd.Flags().BoolVar(&skipCursor, "skip-cursor", false, "do not touch .cursor/mcp.json")
	cmd.Flags().BoolVar(&skipCodex, "skip-codex", false, "do not touch ~/.codex/config.toml")
	cmd.Flags().BoolVar(&skipOpencode, "skip-opencode", false, "do not touch opencode.jsonc")
	cmd.Flags().BoolVar(&withAntigravity, "with-antigravity", false, "also wire mcp_config.json for Antigravity")
	cmd.Flags().BoolVar(&skipInstructions, "skip-instructions", false, "do not write the memory block into CLAUDE.md / AGENTS.md")
	cmd.Flags().StringVar(&only, "only", "", "CSV of writers to run (overrides skip flags, not --skip-instructions): claudecode,cursor,codex,opencode,antigravity")
	return cmd
}

func parseOnlyFlag(v string) map[string]bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	out := map[string]bool{}
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(strings.ToLower(p))
		if p != "" {
			out[p] = true
		}
	}
	return out
}
