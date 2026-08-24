package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/httpauth"
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
	var (
		httpAddr string
		token    string
		noAuth   bool
	)
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
  }

--http exposes the same five tools over StreamableHTTP. That transport carries
the whole memory surface, so it requires an HTTP bearer token (see
"graymatter server --help" for where the token lives) and should be pointed at
a loopback address:

  graymatter mcp serve --http 127.0.0.1:8080`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve auth before opening the store: a refused flag
			// combination should not leave a daemon connection behind.
			var httpOpts []gmcp.HTTPOption
			if httpAddr != "" {
				var err error
				httpOpts, err = resolveMCPHTTPAuth(cmd, httpAddr, token, noAuth)
				if err != nil {
					return err
				}
			}

			store, err := openStore()
			if err != nil {
				return fmt.Errorf("open memory: %w", err)
			}
			defer func() { _ = store.Close() }()

			srv := gmcp.New(store, version)

			if httpAddr != "" {
				return srv.ServeHTTP(httpAddr, httpOpts...)
			}

			if !quiet {
				fmt.Fprintln(os.Stderr, "graymatter MCP server ready (stdio)")
			}
			return srv.ServeStdio()
		},
	}
	cmd.Flags().StringVar(&httpAddr, "http", "",
		"serve StreamableHTTP on this address (e.g. 127.0.0.1:8080); stdio when empty")
	cmd.Flags().StringVar(&token, "token", "",
		"bearer token HTTP clients must present (default: read or create <data-dir>/"+httpauth.TokenFile+")")
	cmd.Flags().BoolVar(&noAuth, "no-auth", false,
		"serve HTTP without authentication (loopback addresses only)")
	return cmd
}

// resolveMCPHTTPAuth mirrors resolveServerAuth for the MCP transport: the two
// listeners share a token file, so a client configured for one already has the
// credential for the other.
func resolveMCPHTTPAuth(cmd *cobra.Command, addr, token string, noAuth bool) ([]gmcp.HTTPOption, error) {
	out := cmd.OutOrStderr()

	if noAuth {
		if !httpauth.IsLoopback(addr) {
			return nil, fmt.Errorf(
				"refusing --no-auth on %s: the MCP HTTP transport carries memory_add, memory_search "+
					"and memory_reflect, so an unauthenticated listener there hands the network "+
					"write access to every agent's memory. Bind 127.0.0.1, or drop --no-auth",
				addr)
		}
		fmt.Fprintln(out, "WARNING: --no-auth: any local process can read and rewrite memory over MCP.")
		return []gmcp.HTTPOption{gmcp.WithHTTPAnonymousAccess()}, nil
	}

	if token == "" {
		var (
			created bool
			err     error
		)
		token, created, err = httpauth.LoadOrCreateToken(dataDir)
		if err != nil {
			return nil, err
		}
		if created && !quiet {
			printTokenLocation(out, httpauth.TokenFilePath(dataDir))
		}
	}

	if w := httpauth.ExposureWarning(addr, true); w != "" && !quiet {
		fmt.Fprint(out, w)
	}
	return []gmcp.HTTPOption{gmcp.WithHTTPAuthToken(token)}, nil
}
