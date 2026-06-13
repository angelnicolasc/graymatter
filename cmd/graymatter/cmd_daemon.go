package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func daemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the GrayMatter store daemon",
		Long: `The daemon is the single owner of the bbolt store for a data directory.
Every other graymatter process (TUI, MCP server, CLI commands) connects to
it over a local endpoint, which is what lets them run concurrently — bbolt
itself allows only one writer process.

You normally never run these commands by hand: clients start the daemon on
demand and it exits on its own after sitting idle. They exist for service
managers (systemd/launchd), debugging, and forced restarts.`,
	}
	cmd.AddCommand(daemonRunCmd(), daemonStatusCmd(), daemonStopCmd())
	return cmd
}

func daemonRunCmd() *cobra.Command {
	var idleExit time.Duration

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the store daemon in the foreground",
		Long: `Runs the daemon in the foreground until stopped (signal, ` + "`daemon stop`" + `,
or idle-exit). Acquires the store write lock strictly: if another process
holds it, the daemon fails fast instead of degrading to read-only.

Client processes spawn this automatically with --idle-exit ` + daemon.DefaultIdleExit.String() + `.
For service-manager setups, run it yourself with --idle-exit 0.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return daemon.Run(daemon.RunOptions{
				DataDir:  dataDir,
				IdleExit: idleExit,
			})
		},
	}
	cmd.Flags().DurationVar(&idleExit, "idle-exit", daemon.DefaultIdleExit,
		"exit after this long with no clients and no traffic (0 = never)")
	return cmd
}

func daemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether a daemon is serving this data directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := daemon.ConnectNoSpawn(dataDir)
			if err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "daemon: not running (dir %s)\n", dataDir)
				if pid := daemon.ReadPIDFile(dataDir); pid != 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "  stale pid file: %d (daemon likely crashed; safe to ignore)\n", pid)
				}
				return nil
			}
			defer func() { _ = c.Close() }()

			addr, _ := rpc.DiscoveryAddr(c.DataDir())
			fmt.Fprintf(cmd.OutOrStdout(), "daemon: running\n  dir:  %s\n  addr: %s\n", c.DataDir(), addr)
			if pid := daemon.ReadPIDFile(c.DataDir()); pid != 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  pid:  %d\n", pid)
			}
			return nil
		},
	}
}

func daemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon serving this data directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := daemon.ConnectNoSpawn(dataDir)
			if err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "daemon: not running (dir %s)\n", dataDir)
				return nil
			}
			defer func() { _ = c.Close() }()
			if err := c.Shutdown(); err != nil {
				return fmt.Errorf("shutdown: %w", err)
			}
			if !quiet {
				fmt.Fprintln(cmd.OutOrStdout(), "daemon: stopping")
			}
			return nil
		},
	}
}
