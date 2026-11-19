package memory

import (
	"context"
	"errors"
)

type ConsolidationSummaryStatus string

const (
	ConsolidationNotAttempted ConsolidationSummaryStatus = "not_attempted"
	ConsolidationSkipped      ConsolidationSummaryStatus = "skipped"
)

type MaintenancePhaseStatus string

const (
	MaintenanceNotAttempted MaintenancePhaseStatus = "not_attempted"
	MaintenanceIncomplete   MaintenancePhaseStatus = "incomplete"
	MaintenanceSkipped      MaintenancePhaseStatus = "skipped"
	MaintenanceFailed       MaintenancePhaseStatus = "failed"
	MaintenanceComplete     MaintenancePhaseStatus = "complete"
)

// ConsolidationMaintenanceReport records the confirmed effects of one pass.
// A fact decayed and then pruned contributes to both counts. Incomplete phases
// may have committed earlier effects. Graph status covers configured work.
type ConsolidationMaintenanceReport struct {
	Decay        MaintenancePhaseStatus `json:"decay"`
	Prune        MaintenancePhaseStatus `json:"prune"`
	Graph        MaintenancePhaseStatus `json:"graph"`
	FactsDecayed int                    `json:"facts_decayed"`
	FactsPruned  int                    `json:"facts_pruned"`
}

func validMaintenanceReport(report *ConsolidationMaintenanceReport, success bool) bool {
	if report == nil || !validFactPhase(report.Decay, report.FactsDecayed) || !validFactPhase(report.Prune, report.FactsPruned) {
		return false
	}
	switch report.Graph {
	case MaintenanceNotAttempted, MaintenanceIncomplete:
		return !success
	case MaintenanceSkipped, MaintenanceFailed, MaintenanceComplete:
		return !success || report.Decay == MaintenanceComplete && report.Prune == MaintenanceComplete
	default:
		return false
	}
}

func validFactPhase(status MaintenancePhaseStatus, count int) bool {
	switch status {
	case MaintenanceNotAttempted:
		return count == 0
	case MaintenanceComplete, MaintenanceIncomplete:
		return count >= 0
	default:
		return false
	}
}

type ConsolidationDiagnostic struct {
	Code       string `json:"code"`
	Provider   string `json:"provider,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Message    string `json:"message"`
}

// ConsolidationReport describes the synthesis phase of a maintenance run.
// G1 keeps synthesis disabled. The summary identity and retirement fields
// remain empty and zero in this version of the report.
type ConsolidationReport struct {
	Status         ConsolidationSummaryStatus      `json:"status"`
	Reason         string                          `json:"reason"`
	Provider       string                          `json:"provider,omitempty"`
	SummaryFactID  string                          `json:"summary_fact_id,omitempty"`
	SourcesRetired int                             `json:"sources_retired"`
	Diagnostic     *ConsolidationDiagnostic        `json:"diagnostic,omitempty"`
	Maintenance    *ConsolidationMaintenanceReport `json:"maintenance,omitempty"`
}

// ReportingConsolidator is an optional extension to the store API.
type ReportingConsolidator interface {
	ConsolidateWithReport(context.Context, string, ConsolidateConfig) (ConsolidationReport, error)
}

// ConsolidationError exposes a fixed diagnostic. The cause is available to Go callers.
type ConsolidationError struct {
	diagnostic ConsolidationDiagnostic
	cause      error
}

func (e *ConsolidationError) Error() string                       { return e.diagnostic.Message }
func (e *ConsolidationError) Diagnostic() ConsolidationDiagnostic { return e.diagnostic }
func (e *ConsolidationError) Unwrap() error                       { return e.cause }

var consolidationDiagnosticMessages = map[string]string{
	"canceled":               "The operation was canceled.",
	"deadline_exceeded":      "The operation exceeded its deadline.",
	"store_error":            "A memory-store operation failed.",
	"operation_unavailable":  "The memory store is not operational.",
	"unsupported_capability": "Maintenance reporting requires an updated daemon and a supported store.",
	"outcome_unknown":        "The connection ended before the maintenance outcome was known; inspect the store before retrying.",
}

func safeConsolidationDiagnostic(d ConsolidationDiagnostic) (ConsolidationDiagnostic, bool) {
	message, ok := consolidationDiagnosticMessages[d.Code]
	if !ok || d.Provider != "" || d.HTTPStatus != 0 {
		return ConsolidationDiagnostic{}, false
	}
	return ConsolidationDiagnostic{Code: d.Code, Message: message}, true
}

func NewConsolidationError(code string, cause error) error {
	diagnostic, ok := safeConsolidationDiagnostic(ConsolidationDiagnostic{Code: code})
	if !ok {
		diagnostic, _ = safeConsolidationDiagnostic(ConsolidationDiagnostic{Code: "store_error"})
	}
	switch diagnostic.Code {
	case "canceled":
		cause = errors.Join(cause, context.Canceled)
	case "deadline_exceeded":
		cause = errors.Join(cause, context.DeadlineExceeded)
	case "outcome_unknown":
		cause = errors.Join(cause, ErrMutationOutcomeUnknown)
	}
	return &ConsolidationError{diagnostic: diagnostic, cause: cause}
}

func NewConsolidationErrorFromDiagnostic(d ConsolidationDiagnostic) error {
	return NewConsolidationError(d.Code, nil)
}

var errInvalidConsolidationResult = errors.New("invalid consolidation result")

// ValidateConsolidationResult validates a known report and its fatal diagnostic
// together. It rejects reported synthesis and normalizes messages.
func ValidateConsolidationResult(report ConsolidationReport, fatal *ConsolidationDiagnostic) (ConsolidationReport, *ConsolidationDiagnostic, error) {
	if report.Provider != "" || report.SummaryFactID != "" || report.SourcesRetired != 0 || report.Diagnostic != nil {
		return ConsolidationReport{}, nil, errInvalidConsolidationResult
	}
	code := ""
	if fatal != nil {
		fixed, ok := safeConsolidationDiagnostic(*fatal)
		if !ok {
			return ConsolidationReport{}, nil, errInvalidConsolidationResult
		}
		code, fatal = fixed.Code, &fixed
	}

	var valid bool
	switch report.Status {
	case ConsolidationSkipped:
		valid = report.Reason == "disabled" && validMaintenanceReport(report.Maintenance, code == "") &&
			(code == "" || code == "store_error" || code == "canceled" || code == "deadline_exceeded")
	case ConsolidationNotAttempted:
		if report.Maintenance != nil {
			return ConsolidationReport{}, nil, errInvalidConsolidationResult
		}
		switch report.Reason {
		case "operation_unavailable":
			valid = code == "operation_unavailable" || code == "unsupported_capability"
		case "aborted_before_summary":
			valid = code == "canceled" || code == "deadline_exceeded" || code == "store_error"
		}
	}
	if !valid {
		return ConsolidationReport{}, nil, errInvalidConsolidationResult
	}
	return report, fatal, nil
}

func consolidationStoreError(err error) error {
	code := "store_error"
	switch {
	case errors.Is(err, context.Canceled):
		code = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "deadline_exceeded"
	}
	return NewConsolidationError(code, err)
}

// ConsolidateWithReport reports the synthesis phase of one maintenance run.
// A maintenance error may accompany the report after earlier effects committed.
func (s *Store) ConsolidateWithReport(ctx context.Context, agentID string, cfg ConsolidateConfig) (ConsolidationReport, error) {
	if s.readOnly {
		return ConsolidationReport{Status: ConsolidationNotAttempted, Reason: "operation_unavailable"},
			NewConsolidationError("operation_unavailable", ErrStoreReadOnly)
	}
	if cfg == nil {
		return ConsolidationReport{Status: ConsolidationNotAttempted, Reason: "operation_unavailable"},
			NewConsolidationError("operation_unavailable", nil)
	}
	if err := ctx.Err(); err != nil {
		return ConsolidationReport{Status: ConsolidationNotAttempted, Reason: "aborted_before_summary"},
			consolidationStoreError(err)
	}
	effects := &ConsolidationMaintenanceReport{
		Decay: MaintenanceNotAttempted, Prune: MaintenanceNotAttempted, Graph: MaintenanceNotAttempted,
	}
	report := ConsolidationReport{Status: ConsolidationSkipped, Reason: "disabled", Maintenance: effects}
	if err := s.consolidate(ctx, agentID, cfg, effects); err != nil {
		return report, consolidationStoreError(err)
	}
	return report, nil
}
