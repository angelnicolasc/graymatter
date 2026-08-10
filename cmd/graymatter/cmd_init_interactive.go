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

// agentDef describes one known MCP agent.
//
// This table is the single source of truth for "which agent needs which file".
// It used to feed only the interactive wizard, while `init`'s flag path and
// `doctor` each carried their own parallel list. That drift is what let
// `--only opencode` keep writing CLAUDE.md (issue #13) and what let `doctor`
// report missing instructions for a project covered by `init --global`
// (issue #17): three lists, one of them updated.
type agentDef struct {
	id              string // claudecode, cursor, opencode, codex, antigravity
	name            string // display name
	configDesc      string // human-readable config-file description
	instructionFile string // project instruction file it reads ("" if none)
	optIn           bool   // wired only when explicitly asked for
	run             func() (writeResult, error)

	// configPath resolves the MCP config this agent actually reads, so doctor
	// looks exactly where the writer wrote.
	configPath func(projectDir string) (string, error)

	// globalInstruction is the home-scoped instruction file this agent reads in
	// every project, or nil when the agent has none that `init --global`
	// writes. Only these agents can be covered by a global install; for the
	// rest a project file is the only delivery path, and doctor must not credit
	// them for a global block they never read.
	globalInstruction func() (string, error)
}

// knownAgents returns the list of known agents, with project-scoped writers
// bound to the given project directory.
func knownAgents(projectDir string) []agentDef {
	return []agentDef{
		{
			id: "claudecode", name: "Claude Code", configDesc: ".mcp.json",
			// Claude Code reads CLAUDE.md and explicitly does not read
			// AGENTS.md; its user-scope file is ~/.claude/CLAUDE.md.
			instructionFile: "CLAUDE.md",
			run:             func() (writeResult, error) { return writeClaudeCodeProject(projectDir) },
			configPath: func(dir string) (string, error) {
				return filepath.Join(dir, ".mcp.json"), nil
			},
			globalInstruction: func() (string, error) {
				home, err := resolveHome()
				if err != nil {
					return "", err
				}
				return filepath.Join(home, ".claude", "CLAUDE.md"), nil
			},
		},
		{
			id: "cursor", name: "Cursor", configDesc: ".cursor/mcp.json",
			instructionFile: "AGENTS.md",
			run:             func() (writeResult, error) { return writeCursorProject(projectDir) },
			configPath: func(dir string) (string, error) {
				return filepath.Join(dir, ".cursor", "mcp.json"), nil
			},
		},
		{
			id: "opencode", name: "OpenCode", configDesc: "opencode.jsonc",
			instructionFile: "AGENTS.md",
			run:             func() (writeResult, error) { return writeOpencodeProject(projectDir) },
			configPath: func(dir string) (string, error) {
				return filepath.Join(dir, "opencode.jsonc"), nil
			},
			globalInstruction: func() (string, error) {
				d, err := opencodeConfigDir()
				if err != nil {
					return "", err
				}
				return filepath.Join(d, "AGENTS.md"), nil
			},
		},
		{
			id: "codex", name: "Codex", configDesc: "~/.codex/config.toml",
			instructionFile: "AGENTS.md",
			run:             writeCodexHome,
			configPath:      func(string) (string, error) { return codexConfigPath() },
		},
		{
			id: "antigravity", name: "Antigravity", configDesc: "mcp_config.json",
			// Antigravity reads AGENTS.md (project root cross-tool standard)
			// alongside its own GEMINI.md. AGENTS.md is sufficient here — see
			// https://antigravity.codes/blog/antigravity-agents-md-guide
			instructionFile: "AGENTS.md",
			optIn:           true,
			run:             func() (writeResult, error) { return writeAntigravityProject(projectDir) },
			configPath: func(dir string) (string, error) {
				return filepath.Join(dir, "mcp_config.json"), nil
			},
		},
	}
}

// instructionFilesFor returns the project instruction files the selected agents
// need, deduped, in knownAgents order. An OpenCode-only selection yields just
// AGENTS.md; a Claude-Code-only selection just CLAUDE.md.
func instructionFilesFor(agents []agentDef, selected map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range agents {
		if !selected[a.id] || a.instructionFile == "" || seen[a.instructionFile] {
			continue
		}
		seen[a.instructionFile] = true
		out = append(out, a.instructionFile)
	}
	return out
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
	instrFiles := instructionFilesFor(agents, selSet)

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
		printNextSteps()
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
