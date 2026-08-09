package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// testStdinReader is overridden in tests to provide canned input.
// Nil in production.
var testStdinReader *strings.Reader

func stdinReader() *bufio.Scanner {
	if testStdinReader != nil {
		return bufio.NewScanner(testStdinReader)
	}
	return bufio.NewScanner(os.Stdin)
}

// agentDef describes one known MCP agent for the interactive wizard.
type agentDef struct {
	id              string // claudecode, cursor, opencode, codex, antigravity
	name            string // display name
	configDesc      string // human-readable config-file description
	instructionFile string // instruction file it needs ("" if none)
	run             func() (writeResult, error)
}

// knownAgents returns the list of known agents, with project-scoped writers
// bound to the given project directory.
func knownAgents(projectDir string) []agentDef {
	return []agentDef{
		{
			id: "claudecode", name: "Claude Code", configDesc: ".mcp.json",
			instructionFile: "CLAUDE.md",
			run:             func() (writeResult, error) { return writeClaudeCodeProject(projectDir) },
		},
		{
			id: "cursor", name: "Cursor", configDesc: ".cursor/mcp.json",
			instructionFile: "AGENTS.md",
			run:             func() (writeResult, error) { return writeCursorProject(projectDir) },
		},
		{
			id: "opencode", name: "OpenCode", configDesc: "opencode.jsonc",
			instructionFile: "AGENTS.md",
			run:             func() (writeResult, error) { return writeOpencodeProject(projectDir) },
		},
		{
			id: "codex", name: "Codex", configDesc: "~/.codex/config.toml",
			instructionFile: "AGENTS.md",
			run:             writeCodexHome,
		},
		{
			id: "antigravity", name: "Antigravity", configDesc: "mcp_config.json",
			// Antigravity reads AGENTS.md (project root cross-tool standard)
			// alongside its own GEMINI.md. AGENTS.md is sufficient here — see
			// https://antigravity.codes/blog/antigravity-agents-md-guide
			instructionFile: "AGENTS.md",
			run:             func() (writeResult, error) { return writeAntigravityProject(projectDir) },
		},
	}
}

// runInteractiveWizard runs the interactive init wizard.
// It asks which agents to wire, then creates only the files needed for those.
// projectDir is the project root; writers and instruction files go there.
func runInteractiveWizard(dir, projectDir string, quiet bool) error {
	// 1. Create data dir + MEMORY.md (same as non-interactive init).
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

	agents := knownAgents(projectDir)

	// 2. Ask which agents to wire.
	selected := askForAgents(agents)

	if len(selected) == 0 {
		if !quiet {
			fmt.Printf("Initialised GrayMatter at %s\n", dir)
			fmt.Printf("  %s/gray.db       — bbolt database (created on first use)\n", dir)
			fmt.Printf("  %s/vectors/      — chromem-go vector index\n", dir)
			fmt.Printf("  %s/MEMORY.md     — human-readable index\n\n", dir)
			fmt.Println("No MCP clients selected. Run `graymatter init --interactive` again anytime to wire clients.")
		}
		maybeAddToPath(quiet)
		return nil
	}

	// 3. Build set of selected agent IDs.
	selSet := make(map[string]bool, len(selected))
	for _, id := range selected {
		selSet[id] = true
	}

	// 4. Build writer entries from selection (preserving knownAgents order).
	type writerJob struct {
		name string
		run  func() (writeResult, error)
	}
	var entries []writerJob
	for _, a := range agents {
		if selSet[a.id] {
			entries = append(entries, writerJob{name: a.name, run: a.run})
		}
	}

	// 5. Build ordered, deduped list of instruction files.
	var instrFiles []string
	instrSet := make(map[string]bool)
	for _, a := range agents {
		if selSet[a.id] && a.instructionFile != "" && !instrSet[a.instructionFile] {
			instrSet[a.instructionFile] = true
			instrFiles = append(instrFiles, a.instructionFile)
		}
	}

	// 6. Run writers and print results.
	if !quiet {
		fmt.Printf("Initialised GrayMatter at %s\n\n", dir)
		fmt.Println("Wired MCP for:")
	}
	var warnings []string
	for _, e := range entries {
		res, err := e.run()
		if err != nil {
			if !quiet {
				fmt.Printf("  ! %-14s %v\n", e.name, err)
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

	// 7. Write instruction files for selected agents.
	if len(instrFiles) > 0 {
		if !quiet {
			fmt.Println("\nAgent instructions (tells the model to actually use the tools):")
		}
		for _, name := range instrFiles {
			res, err := upsertInstructionsBlock(filepath.Join(projectDir, name))
			if err != nil {
				res.warn = fmt.Sprintf("could not update %s: %v", name, err)
			}
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

	maybeAddToPath(quiet)
	return nil
}

func maybeAddToPath(quiet bool) {
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
}

// askForAgents prints the interactive menu and returns the selected agent IDs.
// Returns an empty slice if the user selects "None" or enters nothing.
func askForAgents(agents []agentDef) []string {
	fmt.Println()
	fmt.Println("Which AI agent software do you use?")
	fmt.Println()
	fmt.Println("Select all that apply (enter numbers separated by commas or spaces):")
	fmt.Println()
	fmt.Println("  0  None — data directory only")
	for i, a := range agents {
		fmt.Printf("  %d  %s (%s)\n", i+1, a.name, a.configDesc)
	}
	fmt.Println()
	fmt.Print("Selection: ")

	scanner := stdinReader()
	if !scanner.Scan() {
		return nil
	}
	input := strings.TrimSpace(scanner.Text())
	if input == "" {
		return nil
	}

	// Tokenize by comma, space, or tab.
	parts := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})

	seen := make(map[string]bool)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil {
			continue
		}
		if n == 0 {
			return nil // "None" selected — clears all other selections
		}
		if n < 1 || n > len(agents) {
			continue // out of range — silently skip
		}
		id := agents[n-1].id
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result
}
