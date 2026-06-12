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

	"github.com/spf13/cobra"

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

func checkStore(dir string) checkResult {
	c := checkResult{Name: "store"}
	dbPath := filepath.Join(dir, "gray.db")
	if _, err := os.Stat(dbPath); err != nil {
		c.Status, c.Detail = "info", "no database yet (gray.db is created on first write)"
		return c
	}

	// Read-only probe: side-effect free, and lock contention tells us
	// another process is actively holding the store.
	store, err := memory.Open(memory.StoreConfig{DataDir: dir, ReadOnly: true})
	if err != nil {
		if strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "timeout") {
			c.Status = "warn"
			c.Detail = "gray.db is in use by another process (bbolt is single-writer)"
			c.Hint = "this is normal while an MCP client or the TUI is running; if you started `graymatter mcp serve` manually in a terminal, kill it — MCP clients spawn their own instance" + lsofHint(dbPath)
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
	c.Detail = fmt.Sprintf("%d fact(s) across %d agent(s)", facts, len(agents))
	if pending > 0 {
		c.Status = "warn"
		c.Detail += fmt.Sprintf(", %d pending vector write(s)", pending)
		c.Hint = "pending vectors in a quiescent system mean the embedding backend is failing — check your embedding configuration (Ollama URL / API keys)"
	}
	return c
}

// mcpClientConfigs mirrors the paths used by the init writers
// (cmd_init_writers.go). Codex is home-scoped; the rest are project-scoped.
func mcpClientConfigs(projectDir string) []struct{ client, path string } {
	out := []struct{ client, path string }{
		{"Claude Code", filepath.Join(projectDir, ".mcp.json")},
		{"Cursor", filepath.Join(projectDir, ".cursor", "mcp.json")},
		{"OpenCode", filepath.Join(projectDir, "opencode.jsonc")},
		{"Antigravity", filepath.Join(projectDir, "mcp_config.json")},
	}
	if codexPath, err := codexConfigPath(); err == nil {
		out = append(out, struct{ client, path string }{"Codex", codexPath})
	}
	return out
}

func checkMCPWiring(projectDir string) checkResult {
	c := checkResult{Name: "mcp wiring"}
	var wired []string
	for _, cc := range mcpClientConfigs(projectDir) {
		data, err := os.ReadFile(cc.path)
		if err != nil {
			continue
		}
		// String containment is deliberately tolerant: it covers JSON, JSONC
		// (comments) and TOML without needing three parsers here.
		if strings.Contains(string(data), "graymatter") {
			wired = append(wired, fmt.Sprintf("%s (%s)", cc.client, cc.path))
		}
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

func checkInstructions(projectDir string) checkResult {
	c := checkResult{Name: "instructions"}
	var present []string
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		if hasInstructionsBlock(filepath.Join(projectDir, name)) {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		c.Status = "warn"
		c.Detail = "neither CLAUDE.md nor AGENTS.md tells the model to use the memory tools"
		c.Hint = "an MCP connection only makes tools *available* — without instructions the model never calls them; run `graymatter init` to add the memory block"
		return c
	}
	c.Status, c.Detail = "ok", strings.Join(present, ", ")+" mention the memory tools"
	return c
}

// lsofHint suggests the lock-holder lookup command on platforms that have one.
func lsofHint(dbPath string) string {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		return fmt.Sprintf(" (find the holder with `lsof %s`)", dbPath)
	}
	return ""
}
