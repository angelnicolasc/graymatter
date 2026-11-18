package main

import (
	"fmt"
	"net"
	"os"
	"strconv"

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
		httpAddr     string
		token        string
		noAuth       bool
		noCreate     bool
		toolsProfile string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server (stdio by default)",
		Args:  cobra.NoArgs,
		Long: `Start GrayMatter as a Model Context Protocol server.

By default it uses stdio transport, which is what Claude Code and Cursor expect.
Stdio selects CLAUDE_PROJECT_DIR when present, otherwise the process directory;
--dir explicitly selects a store without changing a client's agent_id.
Use --http to expose an HTTP endpoint instead. HTTP selects its store from
the service's own working directory or an explicit --dir.

Claude Code setup — add to your project's .mcp.json:

  {
    "mcpServers": {
      "graymatter": {
        "command": "graymatter",
        "args": ["mcp", "serve"]
      }
    }
  }

--http serves the same callable tools and selected catalog over StreamableHTTP. That transport carries
the whole memory surface, so it requires an HTTP bearer token (see
"graymatter server --help" for where the token lives) and should be pointed at
a loopback address:

  graymatter mcp serve --http 127.0.0.1:8080

--no-create requires an existing regular gray.db or MEMORY.md before starting.
It does not make the store read-only. A rejected server writes no token, starts
no daemon, and serves no tools. Prepare with init --store-only, then reconnect.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, err := gmcp.ParseToolsProfile(toolsProfile, true)
			if err != nil {
				return err
			}
			// Reject invalid options and unsafe leaves before token generation,
			// daemon startup, or a listener can change the selected store.
			if err := validateMCPHTTPOptions(httpAddr, noAuth); err != nil {
				return err
			}
			cwd, cwdErr := os.Getwd()
			claudeRoot, claudeRootPresent := os.LookupEnv("CLAUDE_PROJECT_DIR")
			transport := runtimeMCPStdio
			if httpAddr != "" {
				transport = runtimeMCPHTTP
			}
			dirFlag := cmd.Flag("dir")
			route, err := resolveRuntimeContext(runtimeContextInput{
				configuredDir: dataDir, dirChanged: dirFlag != nil && dirFlag.Changed,
				capturedCWD: cwd, cwdErr: cwdErr,
				claudeProjectDir: claudeRoot, claudeProjectDirPresent: claudeRootPresent,
				transport: transport,
			})
			if err != nil {
				return fmt.Errorf("MCP project route: %w", err)
			}
			if _, err := inspectRuntimeStore(route.storeDir, noCreate); err != nil {
				return fmt.Errorf("MCP store %s: %w", route.storeDir, err)
			}

			var httpOpts []gmcp.HTTPOption
			if httpAddr != "" {
				var err error
				httpOpts, err = resolveMCPHTTPAuthAt(cmd, httpAddr, token, noAuth, route.storeDir)
				if err != nil {
					return err
				}
			}

			store, err := openStoreAt(route.storeDir)
			if err != nil {
				return fmt.Errorf("open memory: %w", err)
			}
			defer func() { _ = store.Close() }()

			var serverOpts []gmcp.Option
			if profile == gmcp.ToolsFull {
				serverOpts = append(serverOpts, gmcp.WithFullCatalog())
			} else if httpAddr == "" {
				serverOpts = append(serverOpts, gmcp.WithLocalProjectStartup(route.agentID))
			}
			srv := gmcp.New(store, version, serverOpts...)

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
	cmd.Flags().BoolVar(&noCreate, "no-create", false,
		"require an existing regular gray.db or MEMORY.md before opening the store")
	cmd.Flags().StringVar(&toolsProfile, "tools", gmcp.ToolsCore,
		"catalog profile: core (default) or full")
	return cmd
}

// validateMCPHTTPOptions performs syntax and exposure checks without touching
// the token file or store. A successful bind is intentionally not promised.
func validateMCPHTTPOptions(addr string, noAuth bool) error {
	if addr == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid MCP HTTP listen address %q: %w", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("invalid MCP HTTP listen port in %q", addr)
	}
	if noAuth && !httpauth.IsLoopback(addr) {
		return fmt.Errorf("refusing --no-auth on %s: bind loopback or drop --no-auth", addr)
	}
	return nil
}

// resolveMCPHTTPAuth mirrors resolveServerAuth for the MCP transport: the two
// listeners share a token file, so a client configured for one already has the
// credential for the other.
func resolveMCPHTTPAuth(cmd *cobra.Command, addr, token string, noAuth bool) ([]gmcp.HTTPOption, error) {
	return resolveMCPHTTPAuthAt(cmd, addr, token, noAuth, dataDir)
}

func resolveMCPHTTPAuthAt(cmd *cobra.Command, addr, token string, noAuth bool, selectedDir string) ([]gmcp.HTTPOption, error) {
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
		token, created, err = httpauth.LoadOrCreateToken(selectedDir)
		if err != nil {
			return nil, err
		}
		if created && !quiet {
			printTokenLocation(out, httpauth.TokenFilePath(selectedDir))
		}
	}

	if w := httpauth.ExposureWarning(addr, true); w != "" && !quiet {
		fmt.Fprint(out, w)
	}
	return []gmcp.HTTPOption{gmcp.WithHTTPAuthToken(token)}, nil
}
