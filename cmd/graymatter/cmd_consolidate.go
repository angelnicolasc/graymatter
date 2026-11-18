package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/angelnicolasc/graymatter/internal/providerurl"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/spf13/cobra"
)

var errConsolidationReported = errors.New("consolidation result already reported")

// consolidateCmd runs one consolidation cycle for an agent. It is a normal
// CLI command (usable directly) and the entry point the session-end hook
// spawns detached: consolidation may proxy an LLM round-trip, which is why it
// runs outside the hook's time budget rather than inside it.
func consolidateCmd() *cobra.Command {
	return consolidateCmdWithStore(openReportingStore)
}

func consolidateCmdWithStore(open func(context.Context) (reportingStore, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "consolidate <agent-id>",
		Short: "Run one memory consolidation cycle for an agent",
		Long: `Consolidates the agent's memories: summarisation (when an LLM is
configured — Anthropic key or Ollama), decay application, and pruning, with
superseded originals kept as tombstone receipts.

Runs through the store daemon, so it applies the daemon's consolidation
policy and is safe alongside the TUI, MCP servers, and hooks. The report covers
the summary phase. Decay, pruning, and graph maintenance can still run when the
summary phase is skipped or fails.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Argument and flag errors happen before this point and keep
			// Cobra's ordinary rendering.
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			agentID := args[0]
			var report memory.ConsolidationReport
			store, err := open(cmd.Context())
			if err == nil {
				defer func() { _ = store.Close() }()
				report, err = store.ConsolidateWithReport(cmd.Context(), agentID)
			}
			result := commandConsolidationResult(agentID, report, err)
			if err := result.write(cmd.OutOrStdout(), cmd.ErrOrStderr(), jsonOut, quiet); err != nil {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Could not write consolidation result.")
				return errConsolidationReported
			}
			if !result.success() {
				return errConsolidationReported
			}
			return nil
		},
	}
}

type consolidationCLIResult struct {
	SchemaVersion int                             `json:"schema_version"`
	AgentID       string                          `json:"agent_id"`
	Summary       *memory.ConsolidationReport     `json:"summary"`
	Error         *memory.ConsolidationDiagnostic `json:"error"`
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
	result.Summary, result.Error = &report, fatal
	return result
}

func consolidationCommandDiagnostic(err error) *memory.ConsolidationDiagnostic {
	if err == nil {
		return nil
	}
	var safe *memory.ConsolidationError
	if !errors.As(err, &safe) {
		var configErr *providerurl.ConfigError
		code := "store_error"
		switch {
		case errors.Is(err, memory.ErrMutationOutcomeUnknown):
			code = "outcome_unknown"
		case errors.Is(err, context.Canceled):
			code = "canceled"
		case errors.Is(err, context.DeadlineExceeded):
			code = "deadline_exceeded"
		case errors.As(err, &configErr):
			code = "config_invalid_url"
		}
		safe = memory.NewConsolidationError(code, err).(*memory.ConsolidationError)
	}
	d := safe.Diagnostic()
	return &d
}

func (r consolidationCLIResult) success() bool {
	return r.Summary != nil && r.Error == nil &&
		(r.Summary.Status == memory.ConsolidationApplied || r.Summary.Status == memory.ConsolidationSkipped)
}

func (r consolidationCLIResult) write(out, errOut io.Writer, asJSON, isQuiet bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(r)
	}
	if r.success() {
		if isQuiet {
			return nil
		}
		_, err := io.WriteString(out, r.human())
		return err
	}
	_, err := io.WriteString(errOut, r.human())
	return err
}

func (r consolidationCLIResult) human() string {
	if r.Summary == nil {
		return r.Error.Message + "\n"
	}
	s := r.Summary
	agent := r.AgentID
	switch s.Status {
	case memory.ConsolidationApplied:
		summary := fmt.Sprintf("Summary %q committed for %q (%d source retirements confirmed).\n", s.SummaryFactID, agent, s.SourcesRetired)
		if r.Error != nil {
			return fmt.Sprintf("Consolidation stopped for %q: %s\n", agent, r.Error.Message) + summary
		}
		return fmt.Sprintf("Summary applied for %q: %d source facts retired (summary %q).\n", agent, s.SourcesRetired, s.SummaryFactID)
	case memory.ConsolidationSkipped:
		reason := skippedConsolidationReason(s.Reason)
		if r.Error != nil {
			return fmt.Sprintf("Consolidation stopped for %q: %s\nSummary was skipped: %s.\n", agent, r.Error.Message, reason)
		}
		return fmt.Sprintf("Summary skipped for %q: %s.\n", agent, reason)
	case memory.ConsolidationFailed:
		failure := fmt.Sprintf("Summary failed for %q: %s\n", agent, s.Diagnostic.Message)
		if r.Error != nil {
			failure = fmt.Sprintf("Consolidation stopped for %q: %s\n", agent, r.Error.Message) + failure
		}
		return failure + "Maintenance effects may have occurred.\n"
	case memory.ConsolidationPartial:
		return fmt.Sprintf("Summary %q committed for %q (%d source retirements confirmed).\n%s\n", s.SummaryFactID, agent, s.SourcesRetired, r.Error.Message)
	case memory.ConsolidationUncertain:
		if s.Reason == "source_retirement_uncertain" {
			return fmt.Sprintf("Summary %q committed for %q. %d source retirements confirmed; additional retirements may have committed.\n%s\n",
				s.SummaryFactID, agent, s.SourcesRetired, r.Error.Message)
		}
		return fmt.Sprintf("Summary outcome for %q: %s\n", agent, r.Error.Message)
	default: // not_attempted
		msg := fmt.Sprintf("Consolidation stopped for %q: %s\n", agent, r.Error.Message)
		if s.Reason == "aborted_before_summary" {
			msg += "Earlier maintenance effects may have occurred.\n"
		}
		return msg
	}
}

func skippedConsolidationReason(reason string) string {
	switch reason {
	case "no_facts":
		return "store has no facts"
	case "disabled":
		return "summarization is disabled"
	case "threshold_not_exceeded":
		return "summary threshold was not exceeded"
	default: // no_eligible_batch
		return "no eligible summary batch"
	}
}
