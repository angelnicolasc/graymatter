package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func sessionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Manage background agent sessions",
		Long:  "List, inspect, and control background agent sessions.",
	}
	cmd.AddCommand(
		sessionsListCmd(),
		sessionsLogsCmd(),
		sessionsKillCmd(),
	)
	return cmd
}

func sessionsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			sessions, err := store.SessionsList()
			if err != nil {
				return fmt.Errorf("list sessions: %w", err)
			}

			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(sessions)
			}

			if len(sessions) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No sessions found.")
				return nil
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tAGENT\tSTATUS\tSTARTED\tPID")
			fmt.Fprintln(w, "--\t-----\t------\t-------\t---")
			for _, s := range sessions {
				pid := ""
				if s.PID > 0 {
					pid = fmt.Sprintf("%d", s.PID)
				}
				age := formatAge(s.StartedAt)
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					s.ID, s.AgentID, s.Status, age, pid)
			}
			return w.Flush()
		},
	}
}

func sessionsLogsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs <session-id>",
		Short: "Print the log of a background session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			sessions, err := store.SessionsList()
			if err != nil {
				return fmt.Errorf("list sessions: %w", err)
			}
			for _, s := range sessions {
				if s.ID != sessionID {
					continue
				}
				if s.LogFile == "" {
					return fmt.Errorf("session %q was not started in background mode (no log file)", sessionID)
				}
				data, err := os.ReadFile(s.LogFile)
				if err != nil {
					return fmt.Errorf("read log file %q: %w", s.LogFile, err)
				}
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			return fmt.Errorf("session %q not found", sessionID)
		},
	}
}

func sessionsKillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill <session-id>",
		Short: "Stop a running background session",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			sessionID := args[0]

			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			// Resolve "latest" to a concrete ID.
			if sessionID == "latest" {
				sessionID, err = store.SessionResolve("", "latest")
				if err != nil {
					return fmt.Errorf("resolve latest: %w", err)
				}
			}

			if err := store.SessionKill(sessionID); err != nil {
				return err
			}
			fmt.Printf("Session %s signalled.\n", sessionID)
			return nil
		},
	}
}

// formatAge returns a human-readable "started N ago" string.
func formatAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02 15:04")
	}
}

// Ensure os is used (needed for json output path via os.Stdout fallback).
var _ = os.Stdout
