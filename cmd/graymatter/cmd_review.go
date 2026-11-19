package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func reviewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Inspect and manage the proposal and conflict journal",
		Long: `Review memory evidence through an explicit proposal and conflict journal.
Analysis sends eligible facts to the selected provider and stores review
records separately. Active facts and their recall eligibility are unchanged.

Inspect a record before recording a decision. Resolution and dismissal
require a decision and references. Purge applies only to closed records.`,
	}
	cmd.AddCommand(
		reviewAnalyzeCmd(), reviewListCmd(), reviewInspectCmd(),
		reviewActionCmd("reject", "Reject a candidate summary"),
		reviewActionCmd("resolve", "Record an explicit conflict resolution"),
		reviewActionCmd("dismiss", "Record an explicit conflict dismissal"),
		reviewActionCmd("purge", "Purge a closed review record"),
	)
	return cmd
}

func reviewAnalyzeCmd() *cobra.Command {
	var provider, model string
	cmd := &cobra.Command{
		Use:   "analyze <agent-id>",
		Short: "Send a bounded fact batch to a provider for review",
		Long: `Analyze eligible facts from one namespace. Select anthropic or ollama
with --provider. The store owner supplies provider settings and credentials.
The operation writes only review records and immutable evidence snapshots.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if provider != "anthropic" && provider != "ollama" {
				return fmt.Errorf("--provider must be anthropic or ollama")
			}
			store, err := openReviewWriter()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			rows, err := store.ReviewAnalyze(cmd.Context(), args[0], provider, model)
			if err != nil {
				return err
			}
			return writeReviewRows(cmd.OutOrStdout(), rows)
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "analysis provider (anthropic or ollama)")
	cmd.Flags().StringVar(&model, "model", "", "provider model (defaults to the store owner's configuration)")
	_ = cmd.MarkFlagRequired("provider")
	return cmd
}

func reviewListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <agent-id>",
		Short: "List review records without changing the store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openReviewReader()
			if errors.Is(err, errDoctorNoDatabase) {
				return writeReviewRows(cmd.OutOrStdout(), nil)
			}
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			rows, err := store.ReviewList(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeReviewRows(cmd.OutOrStdout(), rows)
		},
	}
}

func reviewInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <agent-id> <record-id>",
		Short: "Read one review record and its evidence as JSON",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openReviewReader()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			record, err := store.ReviewInspect(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return writeReviewJSON(cmd.OutOrStdout(), record)
		},
	}
}

func reviewActionCmd(action, description string) *cobra.Command {
	var decision string
	var references []string
	cmd := &cobra.Command{
		Use:   action + " <agent-id> <record-id>",
		Short: description,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openReviewWriter()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			if err := store.ReviewApply(cmd.Context(), args[0], args[1], memory.ReviewAction{
				Action: action, Decision: decision, References: references,
			}); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				return writeReviewJSON(out, struct {
					Namespace string `json:"namespace"`
					RecordID  string `json:"record_id"`
					Action    string `json:"action"`
				}{Namespace: args[0], RecordID: args[1], Action: action})
			}
			if !quiet {
				_, err = fmt.Fprintf(out, "Recorded %s for review %q.\n", action, args[1])
			}
			return err
		},
	}
	if action != "purge" {
		cmd.Flags().StringVar(&decision, "decision", "", "decision or reason recorded in the journal")
	}
	if action == "resolve" || action == "dismiss" {
		cmd.Flags().StringArrayVar(&references, "reference", nil, "decision reference (repeat for multiple references)")
		_ = cmd.MarkFlagRequired("decision")
		_ = cmd.MarkFlagRequired("reference")
	}
	return cmd
}

type reviewRow struct {
	ID           string    `json:"id"`
	Namespace    string    `json:"namespace"`
	Kind         string    `json:"kind"`
	Status       string    `json:"status,omitempty"`
	Resolution   string    `json:"resolution,omitempty"`
	SourceState  string    `json:"source_state"`
	Observations uint64    `json:"observations"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func writeReviewRows(out io.Writer, records []memory.ReviewRecord) error {
	if jsonOut {
		rows := make([]reviewRow, 0, len(records))
		for _, r := range records {
			rows = append(rows, reviewRow{
				ID: r.ID, Namespace: r.Namespace, Kind: r.Kind, Status: r.Status,
				Resolution: r.Resolution, SourceState: r.SourceState, Observations: r.Observations,
				CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			})
		}
		return writeReviewJSON(out, rows)
	}
	if len(records) == 0 {
		_, err := fmt.Fprintln(out, "No review records.")
		return err
	}
	for _, r := range records {
		state := r.Status
		if r.Resolution != "" {
			state = r.Resolution
		}
		if _, err := fmt.Fprintf(out, "%s  %s  %s  sources %s  observations %d\n",
			r.ID, r.Kind, state, r.SourceState, r.Observations); err != nil {
			return err
		}
	}
	return nil
}

func writeReviewJSON(out io.Writer, value any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
