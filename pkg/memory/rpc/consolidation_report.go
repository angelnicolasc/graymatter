package rpc

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// ConsolidationReportV1 identifies the bounded reporting RPC, which never falls
// back to the legacy Consolidate method.
const ConsolidationReportV1 = "consolidation-report-v1"

// ReportingConsolidator is an optional backend capability.
type ReportingConsolidator = memory.ReportingConsolidator

// ConsolidateWithReportRequest carries the original caller's absolute deadline.
type ConsolidateWithReportRequest struct {
	AgentID          string
	DeadlineUnixNano int64
}

// ConsolidateWithReportResponse carries a known domain result and any fatal
// diagnostic together. Neither field may contain a raw error message.
type ConsolidateWithReportResponse struct {
	Report memory.ConsolidationReport
	Fatal  *memory.ConsolidationDiagnostic
}

// ConsolidationContext supplies the single operation-wide budget. Call it at
// entry to a reporting operation, before attempting to acquire its connection.
// An earlier caller deadline always takes precedence.
func ConsolidationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, consolidateTimeout)
}

// SupportsConsolidationReporting checks an optional capability on the actual
// backend. An adapter may also expose a delegated support probe.
func SupportsConsolidationReporting(backend any) bool {
	reporter, ok := backend.(ReportingConsolidator)
	if !ok || isNilValue(reporter) {
		return false
	}
	if probe, ok := backend.(interface{ SupportsConsolidationReport() bool }); ok {
		return probe.SupportsConsolidationReport()
	}
	return true
}

func (s *Server) supportsConsolidationReporting() bool {
	return s.maintenanceOnly() && !isNilValue(s.consolidateCfg) && SupportsConsolidationReporting(s.backend)
}

func isNilValue(value any) bool {
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Invalid:
		return true
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// ConsolidateWithReport runs the configured backend and validates its result
// before exposing it over JSON-RPC. Known pre-entry rejections are structured
// domain results. An invalid backend result is an opaque protocol error.
func (s *Server) ConsolidateWithReport(req *ConsolidateWithReportRequest, resp *ConsolidateWithReportResponse) error {
	started := time.Now()
	defer s.touch()
	if !s.supportsConsolidationReporting() {
		return setConsolidationResult(resp, memory.ConsolidationReport{
			Status: memory.ConsolidationNotAttempted, Reason: "operation_unavailable",
		}, memory.NewConsolidationError("unsupported_capability", nil))
	}
	if req == nil || req.DeadlineUnixNano <= 0 {
		return rejectReportEntry(resp, "deadline_exceeded", context.DeadlineExceeded)
	}
	deadline := time.Unix(0, req.DeadlineUnixNano)
	if limit := started.Add(consolidateTimeout); deadline.After(limit) {
		deadline = limit
	}
	select {
	case <-s.stop:
		return rejectReportEntry(resp, "canceled", context.Canceled)
	default:
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return rejectReportEntry(resp, "deadline_exceeded", err)
	}
	report, err := s.backend.(ReportingConsolidator).ConsolidateWithReport(ctx, req.AgentID, s.consolidateCfg)
	return setConsolidationResult(resp, report, err)
}

func rejectReportEntry(resp *ConsolidateWithReportResponse, code string, cause error) error {
	return setConsolidationResult(resp, memory.ConsolidationReport{
		Status: memory.ConsolidationNotAttempted, Reason: "aborted_before_summary",
	}, memory.NewConsolidationError(code, cause))
}

func setConsolidationResult(resp *ConsolidateWithReportResponse, report memory.ConsolidationReport, err error) error {
	var fatal *memory.ConsolidationDiagnostic
	if err != nil {
		var reported *memory.ConsolidationError
		if !errors.As(err, &reported) {
			return errors.New("rpc: invalid consolidation result")
		}
		diagnostic := reported.Diagnostic()
		fatal = &diagnostic
	}
	validated, safeFatal, validationErr := memory.ValidateConsolidationResult(report, fatal)
	if validationErr != nil {
		return errors.New("rpc: invalid consolidation result")
	}
	*resp = ConsolidateWithReportResponse{Report: validated, Fatal: safeFatal}
	return nil
}

// ConsolidateWithReport consumes a connection opened by DialContext and sends
// at most one report mutation. This method always closes its dedicated client.
func (c *Client) ConsolidateWithReport(ctx context.Context, agentID string) (memory.ConsolidationReport, error) {
	ctx, cancel := ConsolidationContext(ctx)
	defer cancel()
	if !c.owned || !c.reportStarted.CompareAndSwap(false, true) {
		return memory.ConsolidationReport{}, memory.NewConsolidationError("operation_unavailable", errDedicatedRequired)
	}
	defer c.Close()
	if err := ctx.Err(); err != nil {
		return memory.ConsolidationReport{}, reportReadinessError(err)
	}
	c.capMu.Lock()
	known := c.capKnown
	c.capMu.Unlock()
	if !known {
		if err := c.PingContext(ctx); err != nil {
			return memory.ConsolidationReport{}, reportReadinessError(err)
		}
	}
	c.capMu.Lock()
	supported := c.capabilities[ConsolidationReportV1] && c.capabilities[MaintenanceOnlyV1]
	c.capMu.Unlock()
	if !supported {
		return memory.ConsolidationReport{}, memory.NewConsolidationError("unsupported_capability", unsupported(ConsolidationReportV1))
	}
	deadline, _ := ctx.Deadline()
	request := &ConsolidateWithReportRequest{AgentID: agentID, DeadlineUnixNano: deadline.UnixNano()}
	response, dispatched, err := ownedCall[ConsolidateWithReportResponse](c, ctx, "ConsolidateWithReport", request, true)
	if err != nil {
		if dispatched {
			return memory.ConsolidationReport{}, memory.NewConsolidationError("outcome_unknown", err)
		}
		return memory.ConsolidationReport{}, reportReadinessError(err)
	}
	report, fatal, err := memory.ValidateConsolidationResult(response.Report, response.Fatal)
	if err != nil {
		return memory.ConsolidationReport{}, memory.NewConsolidationError("outcome_unknown", err)
	}
	if fatal != nil {
		if fatal.Code == "unsupported_capability" {
			return report, memory.NewConsolidationError(fatal.Code, unsupported(ConsolidationReportV1))
		}
		return report, memory.NewConsolidationErrorFromDiagnostic(*fatal)
	}
	return report, nil
}

func reportReadinessError(err error) error {
	code := "operation_unavailable"
	switch {
	case errors.Is(err, context.Canceled):
		code = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "deadline_exceeded"
	}
	return memory.NewConsolidationError(code, err)
}
