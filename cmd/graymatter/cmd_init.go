package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialise a .graymatter directory in the current project",
		Long:  "Creates the .graymatter data directory and a MEMORY.md index file.\nSafe to run multiple times — existing data is never overwritten.",
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

			mcpJSON := ".mcp.json"
			if _, err := os.Stat(mcpJSON); os.IsNotExist(err) {
				content := `{
  "mcpServers": {
    "graymatter": {
      "command": "graymatter",
      "args": ["mcp", "serve"],
      "description": "Persistent memory for AI agents"
    }
  }
}
`
				if err := os.WriteFile(mcpJSON, []byte(content), 0o644); err != nil {
					return fmt.Errorf("create .mcp.json: %w", err)
				}
			}

			if !quiet {
				fmt.Printf("Initialised GrayMatter at %s\n", dir)
				fmt.Printf("  %s/gray.db       — bbolt database (created on first use)\n", dir)
				fmt.Printf("  %s/vectors/      — chromem-go vector index\n", dir)
				fmt.Printf("  %s/MEMORY.md     — human-readable index\n", dir)
				fmt.Printf("  .mcp.json        — Claude Code MCP configuration\n")
				fmt.Printf("\nNext steps:\n")
				fmt.Printf("  graymatter remember \"my-agent\" \"user prefers bullet points\"\n")
				fmt.Printf("  graymatter recall  \"my-agent\" \"how should I format this?\"\n")
			}

			if added, pathErr := addExeDirToUserPath(); pathErr != nil {
				if !quiet {
					exe, _ := os.Executable()
					fmt.Fprintf(os.Stderr,
						"  Warning: could not add %s to PATH: %v\n  Add it manually so you can type 'graymatter' from any directory.\n",
						filepath.Dir(exe), pathErr)
				}
			} else if added && !quiet {
				exe, _ := os.Executable()
				fmt.Printf("  Added %s to your PATH — restart PowerShell to apply\n", filepath.Dir(exe))
			}

			return nil
		},
	}
}
