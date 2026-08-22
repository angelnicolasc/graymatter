package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/httpauth"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/server"
)

func serverCmd() *cobra.Command {
	var (
		addr   string
		token  string
		noAuth bool
	)

	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the GrayMatter REST API server",
		Long: `Start an HTTP server that exposes GrayMatter memory operations
over a JSON REST API. Useful for integrating non-Go agents (Python, Shell, etc.)
with the same persistent memory store.

Routes:
  POST   /remember        {"agent":"<id>","text":"<text>"}
  GET    /recall           ?agent=<id>&q=<query>[&k=<int>]
  POST   /consolidate      {"agent":"<id>"}
  GET    /facts            ?agent=<id>[&limit=<int>]
  DELETE /forget           {"agent":"<id>","id":"<fact-id>"}
                           or {"agent":"<id>","query":"<q>","confirm":true}
  DELETE /forget/{id}      ?agent=<id>
  GET    /healthz

Every route except /healthz requires an HTTP bearer token:

  TOKEN=$(cat .graymatter/graymatter.http-token)
  curl -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8080/facts?agent=alice"

The token is generated on first run and stored in <data-dir>/graymatter.http-token.
Override it with --token or the GRAYMATTER_HTTP_TOKEN environment variable.
Pass --no-auth to serve without one; that is only allowed on a loopback address.

The server binds 127.0.0.1 by default. Memory holds whatever your agents were
told, so widen the bind deliberately, not by accident.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := slog.New(slog.NewTextHandler(cmd.OutOrStderr(), nil))

			opts, err := resolveServerAuth(cmd, addr, token, noAuth)
			if err != nil {
				return err
			}

			// Reach the store the same way every other command does. Opening
			// bbolt here would lose the race against the daemon that owns it
			// and leave the API up but serving 503s (issue #19). Failing now
			// is better than starting a server that cannot answer anything.
			store, err := openStore()
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			// openStore already returns a handle that survives the daemon being
			// stopped, crashing or upgraded underneath it, which is what a
			// process this long-lived needs. Wrapping again here would only
			// add a redundant layer.
			defer func() { _ = store.Close() }()

			srv := server.New(addr, store, logger, opts...)

			// Graceful shutdown on SIGINT / SIGTERM.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			errCh := make(chan error, 1)
			go func() { errCh <- srv.ListenAndServe() }()

			select {
			case <-ctx.Done():
				if !quiet {
					fmt.Fprintln(cmd.OutOrStderr(), "shutting down...")
				}
				return srv.Shutdown(context.Background())
			case err := <-errCh:
				return err
			}
		},
	}

	cmd.Flags().StringVarP(&addr, "addr", "a", "127.0.0.1:8080", "listen address (host:port)")
	cmd.Flags().StringVar(&token, "token", "",
		"bearer token clients must present (default: read or create <data-dir>/"+httpauth.TokenFile+")")
	cmd.Flags().BoolVar(&noAuth, "no-auth", false,
		"serve without authentication (loopback addresses only)")
	return cmd
}

// resolveServerAuth turns the auth flags into server options, and refuses the
// one combination that caused the original finding: no credential on an
// address the rest of the network can reach.
//
// It also prints the token the first time one is minted. Printing it on every
// start would scatter the credential through logs and terminal scrollback;
// printing it never would leave the user with a server they cannot call.
func resolveServerAuth(cmd *cobra.Command, addr, token string, noAuth bool) ([]server.Option, error) {
	out := cmd.OutOrStderr()

	if noAuth {
		if !httpauth.IsLoopback(addr) {
			return nil, fmt.Errorf(
				"refusing --no-auth on %s: an unauthenticated listener on a non-loopback address "+
					"exposes every agent's memory to the network. Bind 127.0.0.1, or drop --no-auth",
				addr)
		}
		fmt.Fprintln(out, "WARNING: --no-auth: any local process can read, write and delete memory.")
		return []server.Option{server.WithAnonymousAccess()}, nil
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
			fmt.Fprintf(out, "Generated API token (stored in %s):\n\n  %s\n\n",
				httpauth.TokenFilePath(dataDir), token)
		}
	}

	if w := httpauth.ExposureWarning(addr, true); w != "" && !quiet {
		fmt.Fprint(out, w)
	}
	return []server.Option{server.WithAuthToken(token)}, nil
}
