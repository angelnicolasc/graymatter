package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/angelnicolasc/graymatter/internal/providerhttp"
	"github.com/angelnicolasc/graymatter/internal/providerurl"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/oklog/ulid/v2"
	bolt "go.etcd.io/bbolt"
)

type ConsolidationSummaryStatus string

const (
	ConsolidationNotAttempted ConsolidationSummaryStatus = "not_attempted"
	ConsolidationSkipped      ConsolidationSummaryStatus = "skipped"
	ConsolidationApplied      ConsolidationSummaryStatus = "applied"
	ConsolidationFailed       ConsolidationSummaryStatus = "failed"
	ConsolidationPartial      ConsolidationSummaryStatus = "partial"
	ConsolidationUncertain    ConsolidationSummaryStatus = "uncertain"
)

type ConsolidationDiagnostic struct {
	Code       string `json:"code"`
	Provider   string `json:"provider,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Message    string `json:"message"`
}

// ConsolidationReport describes the summary phase. It can accompany an error.
// SourcesRetired counts acknowledged retirements and is a lower bound if their
// outcome is uncertain. A SummaryFactID identifies an acknowledged commit.
type ConsolidationReport struct {
	Status         ConsolidationSummaryStatus `json:"status"`
	Reason         string                     `json:"reason"`
	Provider       string                     `json:"provider,omitempty"`
	SummaryFactID  string                     `json:"summary_fact_id,omitempty"`
	SourcesRetired int                        `json:"sources_retired"`
	Diagnostic     *ConsolidationDiagnostic   `json:"diagnostic,omitempty"`
}

type ReportingConsolidator interface {
	ConsolidateWithReport(context.Context, string, ConsolidateConfig) (ConsolidationReport, error)
}

// ConsolidationError renders only a fixed diagnostic. Unwrap is for Go callers
// that need the in-process cause, not for rendering output.
type ConsolidationError struct {
	diagnostic ConsolidationDiagnostic
	cause      error
}

func (e *ConsolidationError) Error() string                       { return e.diagnostic.Message }
func (e *ConsolidationError) Diagnostic() ConsolidationDiagnostic { return e.diagnostic }
func (e *ConsolidationError) Unwrap() error                       { return e.cause }

func NewConsolidationError(code string, cause error) error {
	return consolidationError(ConsolidationDiagnostic{Code: code}, cause)
}

func NewConsolidationErrorFromDiagnostic(d ConsolidationDiagnostic) error {
	return consolidationError(d, nil)
}

func consolidationError(d ConsolidationDiagnostic, cause error) error {
	safe, ok := safeDiagnostic(d)
	if !ok {
		safe, _ = safeDiagnostic(ConsolidationDiagnostic{Code: "store_error"})
	}
	switch safe.Code {
	case "canceled":
		cause = errors.Join(cause, context.Canceled)
	case "deadline_exceeded":
		cause = errors.Join(cause, context.DeadlineExceeded)
	case "outcome_unknown", "summary_commit_uncertain":
		cause = errors.Join(cause, ErrMutationOutcomeUnknown)
	case "source_retirement_uncertain":
		cause = errors.Join(cause, ErrMutationOutcomeUnknown, ErrConsolidationRetirement)
	case "source_retirement_failed":
		cause = errors.Join(cause, ErrConsolidationRetirement)
	}
	return &ConsolidationError{diagnostic: safe, cause: cause}
}

func summaryProvider(provider string) bool { return provider == "anthropic" || provider == "ollama" }

var diagnosticMessages = map[string]string{
	"unsupported_provider":        "The configured summary provider is unsupported.",
	"invalid_proposal":            "The proposed summary was not applied.",
	"summary_write_failed":        "The summary could not be committed.",
	"summary_commit_uncertain":    "The summary commit could not be confirmed; it may have been stored. Inspect the store before retrying.",
	"source_retirement_failed":    "The summary was committed but source retirement failed.",
	"source_retirement_uncertain": "The summary was committed, but some source retirements could not be confirmed. Inspect the store before retrying.",
	"canceled":                    "The operation was canceled.",
	"deadline_exceeded":           "The operation exceeded its deadline.",
	"store_error":                 "A memory-store operation failed.",
	"operation_unavailable":       "The memory store is not operational.",
	"unsupported_capability":      "Consolidation reporting is unavailable; check daemon version, configuration, and backend.",
	"outcome_unknown":             "The connection ended before the consolidation outcome was known; inspect the store before retrying.",
}

func safeDiagnostic(d ConsolidationDiagnostic) (ConsolidationDiagnostic, bool) {
	if d.Code != "provider_http_error" && d.HTTPStatus != 0 {
		return ConsolidationDiagnostic{}, false
	}
	switch d.Code {
	case "provider_http_error":
		if !summaryProvider(d.Provider) || d.HTTPStatus < 100 || d.HTTPStatus > 599 {
			return ConsolidationDiagnostic{}, false
		}
		d.Message = fmt.Sprintf("%s summary request returned HTTP %d.", d.Provider, d.HTTPStatus)
		return d, true
	case "config_invalid_url", "provider_timeout", "provider_transport_error", "provider_response_invalid", "provider_redirect_blocked":
		if !summaryProvider(d.Provider) && (d.Code != "config_invalid_url" || d.Provider != "") {
			return ConsolidationDiagnostic{}, false
		}
		switch d.Code {
		case "config_invalid_url":
			d.Message = "Invalid provider base URL configuration."
		case "provider_timeout":
			d.Message = d.Provider + " summary request exceeded its deadline."
		case "provider_transport_error":
			d.Message = d.Provider + " summary request could not be completed."
		case "provider_response_invalid":
			d.Message = d.Provider + " returned an unusable response."
		case "provider_redirect_blocked":
			if d.Provider != "anthropic" {
				return ConsolidationDiagnostic{}, false
			}
			d.Message = d.Provider + " summary redirect was blocked."
		}
		return d, true
	}
	if d.Code == "unsupported_provider" {
		if d.Provider != "unknown" {
			return ConsolidationDiagnostic{}, false
		}
	} else if d.Provider != "" {
		return ConsolidationDiagnostic{}, false
	}
	d.Message = diagnosticMessages[d.Code]
	return d, d.Message != ""
}

func diagnostic(code, provider string, status int) *ConsolidationDiagnostic {
	d, _ := safeDiagnostic(ConsolidationDiagnostic{Code: code, Provider: provider, HTTPStatus: status})
	return &d
}

var invalidConsolidationResult = errors.New("invalid consolidation result")

// ValidateConsolidationResult checks the public report and its associated fatal
// error as one result. Caller-supplied messages are replaced with fixed text.
func ValidateConsolidationResult(report ConsolidationReport, fatal *ConsolidationDiagnostic) (ConsolidationReport, *ConsolidationDiagnostic, error) {
	if report.SourcesRetired < 0 || !validSummaryIdentity(report.SummaryFactID) {
		return ConsolidationReport{}, nil, invalidConsolidationResult
	}
	for _, target := range []**ConsolidationDiagnostic{&report.Diagnostic, &fatal} {
		if *target == nil {
			continue
		}
		fixed, ok := safeDiagnostic(**target)
		if !ok {
			return ConsolidationReport{}, nil, invalidConsolidationResult
		}
		*target = &fixed
	}
	if !validConsolidationTuple(report, fatal) {
		return ConsolidationReport{}, nil, invalidConsolidationResult
	}
	return report, fatal, nil
}

func validSummaryIdentity(id string) bool {
	if id == "" {
		return true
	}
	if len(id) != 26 || id[0] > '7' {
		return false
	}
	parsed, err := ulid.ParseStrict(id)
	return err == nil && parsed.String() == id
}

func validConsolidationTuple(r ConsolidationReport, fatal *ConsolidationDiagnostic) bool {
	d, f := diagnosticCode(r.Diagnostic), diagnosticCode(fatal)
	withoutSummary := r.SummaryFactID == "" && r.SourcesRetired == 0
	knownProvider := summaryProvider(r.Provider)
	maintenance := f == "" || f == "store_error"
	switch r.Status {
	case ConsolidationNotAttempted:
		if !withoutSummary || d != "" || r.Provider != "" {
			return false
		}
		return r.Reason == "operation_unavailable" && (f == "operation_unavailable" || f == "unsupported_capability") ||
			r.Reason == "aborted_before_summary" && (f == "canceled" || f == "deadline_exceeded" || f == "store_error")
	case ConsolidationSkipped:
		if !withoutSummary || d != "" || r.Provider != "" {
			return false
		}
		switch r.Reason {
		case "no_facts":
			return f == ""
		case "disabled", "threshold_not_exceeded", "no_eligible_batch":
			return maintenance
		}
	case ConsolidationFailed:
		if !withoutSummary || !maintenance || r.Diagnostic == nil {
			return false
		}
		switch r.Reason {
		case "unsupported_provider":
			return r.Provider == "unknown" && d == r.Reason
		case "provider_error":
			if !knownProvider || r.Diagnostic.Provider != r.Provider {
				return false
			}
			switch d {
			case "config_invalid_url", "provider_http_error", "provider_timeout", "provider_transport_error", "provider_response_invalid", "provider_redirect_blocked":
				return true
			}
		case "invalid_proposal", "summary_write_failed":
			return knownProvider && d == r.Reason
		}
	case ConsolidationApplied:
		return r.Reason == "summary_applied" && knownProvider && r.SummaryFactID != "" && r.SourcesRetired > 0 && d == "" && maintenance
	case ConsolidationPartial:
		return r.Reason == "source_retirement_failed" && knownProvider && r.SummaryFactID != "" && d == r.Reason && f == r.Reason
	case ConsolidationUncertain:
		if !knownProvider || d != r.Reason || f != r.Reason {
			return false
		}
		return r.Reason == "summary_commit_uncertain" && withoutSummary || r.Reason == "source_retirement_uncertain" && r.SummaryFactID != ""
	}
	return false
}

func diagnosticCode(d *ConsolidationDiagnostic) string {
	if d == nil {
		return ""
	}
	return d.Code
}

var errMissingProviderMessage = errors.New("anthropic returned a null message")

func summaryFailure(provider string, err error, responseStatus int) ConsolidationReport {
	r := ConsolidationReport{Status: ConsolidationFailed, Reason: "provider_error", Provider: provider}
	var config *providerurl.ConfigError
	var blocked *providerhttp.RedirectBlockedError
	var anthropicStatus *anthropic.Error
	var networkError net.Error
	var syntax *json.SyntaxError
	var invalidType *json.UnmarshalTypeError
	switch {
	case errors.Is(err, ErrInvalidProposal):
		r.Reason = "invalid_proposal"
		r.Diagnostic = diagnostic(r.Reason, "", 0)
	case errors.As(err, &blocked):
		r.Diagnostic = diagnostic("provider_redirect_blocked", provider, 0)
	case errors.As(err, &config):
		r.Diagnostic = diagnostic("config_invalid_url", provider, 0)
	case errors.As(err, &anthropicStatus):
		r.Diagnostic = providerStatus(provider, anthropicStatus.StatusCode)
	case responseStatus != 0 && (provider == "ollama" || responseStatus >= 300):
		r.Diagnostic = providerStatus(provider, responseStatus)
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout():
		r.Diagnostic = diagnostic("provider_timeout", provider, 0)
	case errors.Is(err, context.Canceled):
		r.Diagnostic = diagnostic("provider_transport_error", provider, 0)
	case responseStatus >= 200 || errors.Is(err, errMissingProviderMessage) || errors.As(err, &syntax) || errors.As(err, &invalidType):
		r.Diagnostic = diagnostic("provider_response_invalid", provider, 0)
	default:
		r.Diagnostic = diagnostic("provider_transport_error", provider, 0)
	}
	return r
}

func providerStatus(provider string, status int) *ConsolidationDiagnostic {
	if status < 100 || status > 599 {
		return diagnostic("provider_response_invalid", provider, 0)
	}
	return diagnostic("provider_http_error", provider, status)
}

// ConsolidateWithReport runs the legacy maintenance pipeline and reports the
// summary phase. The deadline is cooperative for provider requests.
func (s *Store) ConsolidateWithReport(ctx context.Context, agentID string, cfg ConsolidateConfig) (ConsolidationReport, error) {
	return s.consolidateWithReport(ctx, agentID, cfg, consolidationWrites{})
}

func (s *Store) consolidateWithReport(ctx context.Context, agentID string, cfg ConsolidateConfig, writes consolidationWrites) (ConsolidationReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if cfg == nil || s == nil {
		return ConsolidationReport{Status: ConsolidationNotAttempted, Reason: "operation_unavailable"}, NewConsolidationError("operation_unavailable", nil)
	}
	if err := ctx.Err(); err != nil {
		code := "canceled"
		if errors.Is(err, context.DeadlineExceeded) {
			code = "deadline_exceeded"
		}
		return ConsolidationReport{Status: ConsolidationNotAttempted, Reason: "aborted_before_summary"}, NewConsolidationError(code, err)
	}
	outcome, err := s.consolidate(ctx, agentID, cfg, writes)
	var fatal *ConsolidationDiagnostic
	switch outcome.report.Status {
	case ConsolidationUncertain:
		fatal = outcome.report.Diagnostic
		err = consolidationError(*fatal, errors.Join(outcome.cause, err))
	case ConsolidationPartial:
		fatal = outcome.report.Diagnostic
		err = consolidationError(*fatal, err)
	default:
		if err != nil {
			fatal = diagnostic("store_error", "", 0)
			err = consolidationError(*fatal, err)
		}
	}
	report, _, invalid := ValidateConsolidationResult(outcome.report, fatal)
	if invalid != nil {
		return ConsolidationReport{}, NewConsolidationError("outcome_unknown", invalid)
	}
	return report, err
}

type commitReceipt uint8

const (
	notCommitted commitReceipt = iota
	commitAcknowledged
	commitUnconfirmed
)

type commitUpdate func(func(*bolt.Tx) error) error

type consolidationWrites struct {
	summary    commitUpdate
	retirement commitUpdate
}

// trackCommit records whether the managed transaction reached the commit
// boundary. An Update error after that point does not prove the write is absent.
func (s *Store) trackCommit(update commitUpdate, modify func(*bolt.Tx) error) (commitReceipt, error) {
	if update == nil {
		update = s.db.Update
	}
	prepared := false
	err := update(func(tx *bolt.Tx) error {
		err := modify(tx)
		prepared = err == nil
		return err
	})
	if !prepared {
		return notCommitted, err
	}
	if err != nil {
		return commitUnconfirmed, err
	}
	return commitAcknowledged, nil
}

type proposalApplication struct {
	summaryID             string
	retired               int
	summaryUnconfirmed    bool
	retirementUnconfirmed bool
}

type consolidationOutcome struct {
	report ConsolidationReport
	cause  error
}

func skippedConsolidation(reason string) ConsolidationReport {
	return ConsolidationReport{Status: ConsolidationSkipped, Reason: reason}
}

func (s *Store) consolidateSummary(ctx context.Context, agentID string, facts []Fact, cfg ConsolidateConfig, writes consolidationWrites) (consolidationOutcome, error) {
	provider := cfg.GetConsolidateLLM()
	if provider == "" {
		return consolidationOutcome{report: skippedConsolidation("disabled")}, nil
	}
	// The summary phase uses a strictly greater threshold, unlike scheduling.
	if len(facts) <= cfg.GetConsolidateThreshold() {
		return consolidationOutcome{report: skippedConsolidation("threshold_not_exceeded")}, nil
	}
	batch := summarisationBatch(facts)
	if len(batch) == 0 {
		return consolidationOutcome{report: skippedConsolidation("no_eligible_batch")}, nil
	}
	if !summaryProvider(provider) {
		return consolidationOutcome{report: ConsolidationReport{
			Status: ConsolidationFailed, Reason: "unsupported_provider", Provider: "unknown",
			Diagnostic: diagnostic("unsupported_provider", "unknown", 0),
		}}, nil
	}
	var responseStatus int
	proposal, err := summariseFacts(ctx, batch, provider, cfg, &responseStatus)
	if err != nil {
		if s.cfg.OnConsolidateError != nil {
			s.cfg.OnConsolidateError(agentID, err)
		}
		return consolidationOutcome{report: summaryFailure(provider, err, responseStatus), cause: err}, nil
	}
	application, err := s.applyProposalWithReceipt(ctx, agentID, batch, proposal, writes)
	if application.retired > 0 {
		if counterErr := recordConsolidation(s.db, application.retired); counterErr != nil && s.cfg.OnConsolidateError != nil {
			s.cfg.OnConsolidateError(agentID, fmt.Errorf("record consolidation counters: %w", counterErr))
		}
	}
	if err != nil && s.cfg.OnConsolidateError != nil {
		s.cfg.OnConsolidateError(agentID, err)
	}
	out := consolidationOutcome{report: application.report(provider, err), cause: err}
	if errors.Is(err, ErrConsolidationRetirement) {
		return out, err
	}
	return out, nil
}

func (a proposalApplication) report(provider string, err error) ConsolidationReport {
	r := ConsolidationReport{Provider: provider, SummaryFactID: a.summaryID, SourcesRetired: a.retired}
	switch {
	case a.summaryUnconfirmed:
		r.Status, r.Reason = ConsolidationUncertain, "summary_commit_uncertain"
	case a.retirementUnconfirmed:
		r.Status, r.Reason = ConsolidationUncertain, "source_retirement_uncertain"
	case errors.Is(err, ErrConsolidationRetirement):
		r.Status, r.Reason = ConsolidationPartial, "source_retirement_failed"
	case errors.Is(err, ErrInvalidProposal):
		r.Status, r.Reason = ConsolidationFailed, "invalid_proposal"
	case err != nil:
		r.Status, r.Reason = ConsolidationFailed, "summary_write_failed"
	default:
		r.Status, r.Reason = ConsolidationApplied, "summary_applied"
		return r
	}
	r.Diagnostic = diagnostic(r.Reason, "", 0)
	return r
}
