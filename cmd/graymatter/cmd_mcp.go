package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	gmcp "github.com/angelnicolasc/graymatter/cmd/graymatter/internal/mcp"
)

func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCP server commands",
	}
	cmd.AddCommand(mcpServeCmd())
	return cmd
}

func mcpServeCmd() *cobra.Command {
	var httpAddr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server (stdio by default)",
		Long: `Start GrayMatter as a Model Context Protocol server.

By default it uses stdio transport, which is what Claude Code and Cursor expect.
Use --http to expose an HTTP endpoint instead.

Claude Code setup — add to your project's .mcp.json:

  {
    "mcpServers": {
      "graymatter": {
        "command": "graymatter",
        "args": ["mcp", "serve"]
      }
    }
  }`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openStore()
			if err != nil {
				return fmt.Errorf("open memory: %w", err)
			}
			defer func() { _ = store.Close() }()

			srv := gmcp.New(store)

			if httpAddr != "" {
				return srv.ServeHTTP(httpAddr)
			}

			if !quiet {
				fmt.Fprintln(os.Stderr, "graymatter MCP server ready (stdio)")
			}
			return srv.ServeStdio()
		},
	}
	cmd.Flags().StringVar(&httpAddr, "http", "", "serve HTTP on this address (e.g. :8080)")
	return cmd
}
