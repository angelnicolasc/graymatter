package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
	"github.com/spf13/cobra"
)

// consolidateCmd runs one maintenance cycle for an agent. The session-end
// hook runs the same command detached to keep maintenance out of its budget.
func consolidateCmd() *cobra.Command {
	return consolidateCmdWithStore(openReportingStore)
}

func consolidateCmdWithStore(open func(context.Context) (reportingStore, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "consolidate <agent-id>",
		Short: "Run memory maintenance for an agent",
		Long: `Applies decay and pruning, and extracts knowledge-graph entities when
auto-population is enabled. Generative compaction is disabled, including
when Anthropic or Ollama is configured.

Uses the store owner's maintenance policy. The daemon coordinates access
with the TUI, MCP servers, and hooks.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			if jsonOut {
				return runConsolidationReport(cmd, agentID, open)
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			if err := store.Consolidate(cmd.Context(), agentID); err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Maintenance complete for %q. Generative compaction is disabled.\n", agentID)
			}
			return nil
		},
	}
}

var errConsolidationReported = errors.New("consolidation result already reported")

type consolidationCLIResult struct {
	SchemaVersion int                                    `json:"schema_version"`
	AgentID       string                                 `json:"agent_id"`
	Summary       *memory.ConsolidationReport            `json:"summary"`
	Maintenance   *memory.ConsolidationMaintenanceReport `json:"maintenance"`
	Error         *memory.ConsolidationDiagnostic        `json:"error"`
}

func runConsolidationReport(cmd *cobra.Command, agentID string, open func(context.Context) (reportingStore, error)) error {
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	ctx, cancel := rpc.ConsolidationContext(cmd.Context())
	defer cancel()

	var report memory.ConsolidationReport
	store, err := open(ctx)
	if err == nil {
		defer func() { _ = store.Close() }()
		report, err = store.ConsolidateWithReport(ctx, agentID)
	}
	result := commandConsolidationResult(agentID, report, err)
	if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Could not write consolidation result.")
		return errConsolidationReported
	}
	if result.Error != nil {
		return errConsolidationReported
	}
	return nil
}

func commandConsolidationResult(agentID string, report memory.ConsolidationReport, err error) consolidationCLIResult {
	result := consolidationCLIResult{SchemaVersion: 1, AgentID: agentID, Error: consolidationCommandDiagnostic(err)}
	if report == (memory.ConsolidationReport{}) && result.Error != nil {
		return result
	}
	report, fatal, invalid := memory.ValidateConsolidationResult(report, result.Error)
	if invalid != nil {
		result.Error = consolidationCommandDiagnostic(memory.NewConsolidationError("outcome_unknown", invalid))
		return result
	}
	result.Maintenance, report.Maintenance = report.Maintenance, nil
	result.Summary, result.Error = &report, fatal
	return result
}

func consolidationCommandDiagnostic(err error) *memory.ConsolidationDiagnostic {
	if err == nil {
		return nil
	}
	var safe *memory.ConsolidationError
	if !errors.As(err, &safe) {
		code := "store_error"
		switch {
		case errors.Is(err, memory.ErrMutationOutcomeUnknown):
			code = "outcome_unknown"
		case errors.Is(err, context.Canceled):
			code = "canceled"
		case errors.Is(err, context.DeadlineExceeded):
			code = "deadline_exceeded"
		}
		safe = memory.NewConsolidationError(code, err).(*memory.ConsolidationError)
	}
	diagnostic := safe.Diagnostic()
	return &diagnostic
}
