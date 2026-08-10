package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/server"
)

func serverCmd() *cobra.Command {
	var addr string

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
  DELETE /forget           {"agent":"<id>","query":"<query>"}
  GET    /healthz`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := slog.New(slog.NewTextHandler(cmd.OutOrStderr(), nil))

			// Reach the store the same way every other command does. Opening
			// bbolt here would lose the race against the daemon that owns it
			// and leave the API up but serving 503s (issue #19). Failing now
			// is better than starting a server that cannot answer anything.
			store, err := openStore()
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			// Unlike a CLI command, this handle has to survive the daemon being
			// stopped, crashing, or upgraded underneath it.
			rs := newReconnectingStore(store)
			defer func() { _ = rs.Close() }()

			srv := server.New(addr, rs, logger)

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

	cmd.Flags().StringVarP(&addr, "addr", "a", ":8080", "listen address (host:port)")
	return cmd
}
