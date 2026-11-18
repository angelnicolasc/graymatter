package rpc

import (
	"context"
	"errors"
	"io"
	"net"
	netrpc "net/rpc"
	"net/rpc/jsonrpc"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const confirmedSummary = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

type reportingTestBackend struct {
	failingBackend
	reports, legacy atomic.Int32
	probe           bool
	result          memory.ConsolidationReport
	err             error
	deadline        chan time.Time
}

func (b *reportingTestBackend) ConsolidateWithReport(ctx context.Context, _ string, _ memory.ConsolidateConfig) (memory.ConsolidationReport, error) {
	b.reports.Add(1)
	if b.deadline != nil {
		deadline, _ := ctx.Deadline()
		b.deadline <- deadline
	}
	return b.result, b.err
}

func (b *reportingTestBackend) Consolidate(context.Context, string, memory.ConsolidateConfig) error {
	b.legacy.Add(1)
	return nil
}

func (b *reportingTestBackend) SupportsConsolidationReport() bool { return b.probe }

func TestReportingServerPreflightAndCapability(t *testing.T) {
	backend := &reportingTestBackend{probe: true}
	for _, tc := range []struct {
		name    string
		backend Backend
		cfg     memory.ConsolidateConfig
	}{
		{"legacy backend", failingBackend{}, rpcTestConfig{}},
		{"missing config", backend, nil},
		{"typed nil config", backend, (*rpcTestConfig)(nil)},
		{"typed nil backend", (*reportingTestBackend)(nil), rpcTestConfig{}},
		{"negative delegated capability", &reportingTestBackend{}, rpcTestConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := NewServer(tc.backend, tc.cfg)
			var ping PingResponse
			if err := server.Ping(&PingRequest{}, &ping); err != nil || slices.Contains(ping.Capabilities, ConsolidationReportV1) {
				t.Fatalf("advertised unavailable capability: %+v %v", ping, err)
			}
			request := &ConsolidateWithReportRequest{AgentID: "fixture", DeadlineUnixNano: time.Now().Add(time.Second).UnixNano()}
			var resp ConsolidateWithReportResponse
			if err := server.ConsolidateWithReport(request, &resp); err != nil || resp.Report.Status != memory.ConsolidationNotAttempted || resp.Report.Reason != "operation_unavailable" || resp.Fatal == nil || resp.Fatal.Code != "unsupported_capability" {
				t.Fatalf("unavailable request not safely rejected: %+v %v", resp, err)
			}
		})
	}
	server := NewServer(backend, rpcTestConfig{})
	var ping PingResponse
	if err := server.Ping(&PingRequest{}, &ping); err != nil || !slices.Contains(ping.Capabilities, ConsolidationReportV1) {
		t.Fatalf("supported capability not advertised: %+v %v", ping, err)
	}
	for _, deadline := range []int64{0, -1, time.Now().Add(-time.Second).UnixNano()} {
		var resp ConsolidateWithReportResponse
		request := &ConsolidateWithReportRequest{AgentID: "fixture", DeadlineUnixNano: deadline}
		if err := server.ConsolidateWithReport(request, &resp); err != nil || resp.Report.Status != memory.ConsolidationNotAttempted || resp.Report.Reason != "aborted_before_summary" || resp.Fatal == nil || resp.Fatal.Code != "deadline_exceeded" {
			t.Fatalf("deadline not safely rejected: %+v %v", resp, err)
		}
	}
	if backend.reports.Load() != 0 || backend.legacy.Load() != 0 {
		t.Fatalf("rejected requests invoked backend: report=%d legacy=%d", backend.reports.Load(), backend.legacy.Load())
	}
}

func TestReportServerRejectsInvalidBackendResultWithoutEcho(t *testing.T) {
	backend := &reportingTestBackend{probe: true, result: memory.ConsolidationReport{
		Status: memory.ConsolidationApplied, Reason: "summary_applied", Provider: "ollama",
		SummaryFactID: "invalid summary CANARY", SourcesRetired: 1,
	}}
	server := NewServer(backend, rpcTestConfig{})
	var response ConsolidateWithReportResponse
	err := server.ConsolidateWithReport(&ConsolidateWithReportRequest{
		AgentID: "fixture", DeadlineUnixNano: time.Now().Add(time.Second).UnixNano(),
	}, &response)
	if err == nil || strings.Contains(err.Error(), "CANARY") || response.Report.Status != "" || backend.reports.Load() != 1 {
		t.Fatalf("response=%+v error=%v backend calls=%d", response, err, backend.reports.Load())
	}
}

func TestContextDialReportAndLegacyMethods(t *testing.T) {
	backend := &reportingTestBackend{probe: true, deadline: make(chan time.Time, 2), result: memory.ConsolidationReport{
		Status: memory.ConsolidationApplied, Reason: "summary_applied", Provider: "anthropic", SummaryFactID: confirmedSummary, SourcesRetired: 2,
	}}
	server := NewServer(backend, rpcTestConfig{})
	dir, token := t.TempDir(), mustToken(t)
	server.SetAuthToken(token)
	listener, cleanup, err := Listen(dir, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	legacy, err := Dial(DialOptions{DataDir: dir, PingOnDial: true})
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.ConsolidateWithReport(context.Background(), "legacy"); err == nil {
		t.Fatal("shared legacy connection accepted a report mutation")
	}
	if err := legacy.Consolidate(context.Background(), "legacy"); err != nil {
		t.Fatal(err)
	}
	readiness, cancelReadiness := context.WithTimeout(context.Background(), 3*time.Second)
	client, err := DialContext(readiness, DialOptions{DataDir: dir, PingOnDial: true})
	cancelReadiness() // Daemon ConnectContext cancels its setup context on return.
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	parent, cancelParent := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancelParent()
	wantDeadline, _ := parent.Deadline()
	result, err := client.ConsolidateWithReport(parent, "fixture")
	if err != nil || result != backend.result || backend.reports.Load() != 1 || backend.legacy.Load() != 1 {
		t.Fatalf("report=%+v err=%v report calls=%d legacy calls=%d", result, err, backend.reports.Load(), backend.legacy.Load())
	}
	if got := <-backend.deadline; !got.Equal(wantDeadline) {
		t.Fatalf("server got deadline %v, original=%v", got, wantDeadline)
	}
	if _, err := client.ConsolidateWithReport(parent, "fixture"); err == nil || backend.reports.Load() != 1 {
		t.Fatal("a consumed connection replayed a report")
	}
	client.closeMu.Lock()
	closed := client.closed
	client.closeMu.Unlock()
	if !closed || legacy.Ping() != nil {
		t.Fatal("the report did not close only its own connection")
	}
}

// LegacyReportPeer models a server which has only pre-report wire methods.
type LegacyReportPeer struct {
	pingStarted, pingRelease chan struct{}
	legacy                   atomic.Int32
	caps                     []string
}

func (p *LegacyReportPeer) Ping(_ *PingRequest, resp *PingResponse) error {
	if p.pingStarted != nil {
		close(p.pingStarted)
		<-p.pingRelease
	}
	*resp = PingResponse{Protocol: Protocol, Capabilities: p.caps}
	return nil
}

func (p *LegacyReportPeer) Consolidate(_ *ConsolidateRequest, _ *ConsolidateResponse) error {
	p.legacy.Add(1)
	return nil
}

type ReportWirePeer struct {
	*LegacyReportPeer
	reports atomic.Int32
	handle  func(*ConsolidateWithReportResponse) error
}

func (p *ReportWirePeer) ConsolidateWithReport(_ *ConsolidateWithReportRequest, resp *ConsolidateWithReportResponse) error {
	p.reports.Add(1)
	return p.handle(resp)
}

func reportPipeClient(t *testing.T, service any) *Client {
	t.Helper()
	client, _ := reportPipeConnection(t, service)
	return client
}

func reportPipeConnection(t *testing.T, service any) (*Client, net.Conn) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server := netrpc.NewServer()
	if err := server.RegisterName(ServiceName, service); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		server.ServeCodec(jsonrpc.NewServerCodec(serverConn))
	}()
	client := newClient(clientConn, "pipe", DialOptions{CallTimeout: 3 * time.Second})
	client.owned = true
	t.Cleanup(func() {
		_ = client.Close()
		_ = serverConn.Close()
		waitReportSignal(t, closed, "wire server termination")
	})
	return client, serverConn
}

func waitReportSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", message)
	}
}

func TestReportAgainstLegacyPeerNeverFallsBack(t *testing.T) {
	peer := &LegacyReportPeer{}
	client := reportPipeClient(t, peer)
	result, err := client.ConsolidateWithReport(context.Background(), "fixture")
	var known *memory.ConsolidationError
	if result.Status != "" || !errors.As(err, &known) || known.Diagnostic().Code != "unsupported_capability" || peer.legacy.Load() != 0 {
		t.Fatalf("result=%+v error=%v legacy=%d", result, err, peer.legacy.Load())
	}
}

func TestReportCancellationWhilePingingNeverSendsMutation(t *testing.T) {
	peer := &ReportWirePeer{LegacyReportPeer: &LegacyReportPeer{
		caps: []string{ConsolidationReportV1}, pingStarted: make(chan struct{}), pingRelease: make(chan struct{}),
	}}
	client := reportPipeClient(t, peer)
	t.Cleanup(func() { close(peer.pingRelease) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.ConsolidateWithReport(ctx, "fixture"); done <- err }()
	waitReportSignal(t, peer.pingStarted, "Ping dispatch")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errors.Is(err, memory.ErrMutationOutcomeUnknown) || peer.reports.Load() != 0 || peer.legacy.Load() != 0 {
			t.Fatalf("cancellation did not prevent report: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled Ping did not unblock")
	}
}

func TestReportDeadlineWhilePingingNeverSendsMutation(t *testing.T) {
	peer := &ReportWirePeer{LegacyReportPeer: &LegacyReportPeer{
		caps: []string{ConsolidationReportV1}, pingStarted: make(chan struct{}), pingRelease: make(chan struct{}),
	}}
	client := reportPipeClient(t, peer)
	t.Cleanup(func() { close(peer.pingRelease) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.ConsolidateWithReport(ctx, "fixture"); done <- err }()
	waitReportSignal(t, peer.pingStarted, "Ping dispatch")
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, memory.ErrMutationOutcomeUnknown) || peer.reports.Load() != 0 || peer.legacy.Load() != 0 {
			t.Fatalf("deadline did not prevent report: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled Ping did not respect the caller's deadline")
	}
}

func TestReportDisconnectAfterPeerCommitDoesNotReplay(t *testing.T) {
	committed, releaseResponse := make(chan struct{}), make(chan struct{})
	peer := &ReportWirePeer{LegacyReportPeer: &LegacyReportPeer{caps: []string{ConsolidationReportV1}}, handle: func(out *ConsolidateWithReportResponse) error {
		out.Report = memory.ConsolidationReport{Status: memory.ConsolidationApplied, Reason: "summary_applied", Provider: "ollama", SummaryFactID: confirmedSummary, SourcesRetired: 1}
		close(committed)
		<-releaseResponse
		return nil
	}}
	client, serverConn := reportPipeConnection(t, peer)
	t.Cleanup(func() { close(releaseResponse) })
	result := make(chan error, 1)
	go func() {
		report, err := client.ConsolidateWithReport(context.Background(), "fixture")
		if report.Status != "" {
			t.Errorf("disconnected report should be unknown: %+v", report)
		}
		result <- err
	}()
	waitReportSignal(t, committed, "peer commit receipt")
	_ = serverConn.Close()
	select {
	case err := <-result:
		if !errors.Is(err, memory.ErrMutationOutcomeUnknown) || peer.reports.Load() != 1 || peer.legacy.Load() != 0 {
			t.Fatalf("disconnected result: error=%v reports=%d legacy=%d", err, peer.reports.Load(), peer.legacy.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not release the report caller")
	}
}

type writeObservedConn struct {
	net.Conn
	entered, released chan struct{}
	writes            atomic.Int32
}

func (c *writeObservedConn) Write(b []byte) (int, error) {
	if c.writes.Add(1) == 1 {
		close(c.entered)
		defer close(c.released)
	}
	return c.Conn.Write(b)
}

func TestReportBlockedWriterClosesAndIsUnknownWithoutReplay(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	conn := &writeObservedConn{Conn: left, entered: make(chan struct{}), released: make(chan struct{})}
	client := newClient(conn, "pipe", DialOptions{CallTimeout: time.Second})
	client.owned = true
	defer client.Close()
	if err := client.acceptPing(PingResponse{Protocol: Protocol, Capabilities: []string{ConsolidationReportV1}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		result, err := client.ConsolidateWithReport(ctx, "fixture")
		if result.Status != "" {
			t.Errorf("blocked writer returned a known report: %+v", result)
		}
		done <- err
	}()
	waitReportSignal(t, conn.entered, "blocked mutation Write")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, memory.ErrMutationOutcomeUnknown) || !errors.Is(err, context.Canceled) {
			t.Fatalf("post-dispatch error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked report write did not return")
	}
	waitReportSignal(t, conn.released, "worker release after owned connection close")
	if conn.writes.Load() != 1 {
		t.Fatalf("report attempted %d writes", conn.writes.Load())
	}
}

type cancelAtDeadlineConn struct {
	*writeObservedConn
	cancel context.CancelFunc
}

func (c *cancelAtDeadlineConn) SetDeadline(deadline time.Time) error {
	c.cancel()
	return c.Conn.SetDeadline(deadline)
}

func TestReportCancellationBeforeDispatchAttemptsNoWrite(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &cancelAtDeadlineConn{cancel: cancel, writeObservedConn: &writeObservedConn{
		Conn: left, entered: make(chan struct{}), released: make(chan struct{}),
	}}
	client := newClient(conn, "pipe", DialOptions{CallTimeout: time.Second})
	client.owned = true
	defer client.Close()
	if err := client.acceptPing(PingResponse{Protocol: Protocol, Capabilities: []string{ConsolidationReportV1}}); err != nil {
		t.Fatal(err)
	}
	result, err := client.ConsolidateWithReport(ctx, "fixture")
	if result.Status != "" || !errors.Is(err, context.Canceled) || errors.Is(err, memory.ErrMutationOutcomeUnknown) || conn.writes.Load() != 0 {
		t.Fatalf("predispatch cancellation: result=%+v error=%v writes=%d", result, err, conn.writes.Load())
	}
}

type shortAuthenticationConn struct{ net.Conn }

func (c shortAuthenticationConn) Write([]byte) (int, error) { return 1, nil }

func TestAuthenticationWriteHonorsContextAndRejectsPartialPreamble(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	defer left.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := authenticateContext(ctx, shortAuthenticationConn{left}, "synthetic"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short preamble: %v", err)
	}
	conn := &writeObservedConn{Conn: left, entered: make(chan struct{}), released: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- authenticateContext(ctx, conn, "synthetic") }()
	waitReportSignal(t, conn.entered, "blocked authentication Write")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stalled auth error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authentication ignored cancellation")
	}
	waitReportSignal(t, conn.released, "authentication write release")
}

func TestReportWireKeepsKnownFatalAndRejectsInvalidResponsesAfterDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, expected string
		report         memory.ConsolidationReport
		fatal          *memory.ConsolidationDiagnostic
		wireError      error
	}{
		{"uncertain summary", "summary_commit_uncertain", memory.ConsolidationReport{
			Status: memory.ConsolidationUncertain, Reason: "summary_commit_uncertain", Provider: "anthropic", Diagnostic: &memory.ConsolidationDiagnostic{Code: "summary_commit_uncertain", Message: "diagnostic CANARY"},
		}, &memory.ConsolidationDiagnostic{Code: "summary_commit_uncertain", Message: "fatal CANARY"}, nil},
		{"hostile ID", "outcome_unknown", memory.ConsolidationReport{
			Status: memory.ConsolidationApplied, Reason: "summary_applied", Provider: "anthropic", SummaryFactID: "hostile ID CANARY\n", SourcesRetired: 1,
		}, nil, nil},
		{"lowercase ID", "outcome_unknown", memory.ConsolidationReport{
			Status: memory.ConsolidationApplied, Reason: "summary_applied", Provider: "anthropic", SummaryFactID: strings.ToLower(confirmedSummary), SourcesRetired: 1,
		}, nil, nil},
		{"overflow ID", "outcome_unknown", memory.ConsolidationReport{
			Status: memory.ConsolidationApplied, Reason: "summary_applied", Provider: "anthropic", SummaryFactID: "8" + confirmedSummary[1:], SourcesRetired: 1,
		}, nil, nil},
		{"overlong ID", "outcome_unknown", memory.ConsolidationReport{
			Status: memory.ConsolidationApplied, Reason: "summary_applied", Provider: "anthropic", SummaryFactID: confirmedSummary + "A", SourcesRetired: 1,
		}, nil, nil},
		{"negative retirement", "outcome_unknown", memory.ConsolidationReport{
			Status: memory.ConsolidationApplied, Reason: "summary_applied", Provider: "anthropic", SummaryFactID: confirmedSummary, SourcesRetired: -1,
		}, nil, nil},
		{"skipped with provider", "outcome_unknown", memory.ConsolidationReport{
			Status: memory.ConsolidationSkipped, Reason: "disabled", Provider: "anthropic",
		}, nil, nil},
		{"invalid HTTP diagnostic", "outcome_unknown", memory.ConsolidationReport{
			Status: memory.ConsolidationFailed, Reason: "provider_error", Provider: "anthropic",
			Diagnostic: &memory.ConsolidationDiagnostic{Code: "provider_http_error", Provider: "anthropic", HTTPStatus: 999},
		}, nil, nil},
		{"impossible tuple", "outcome_unknown", memory.ConsolidationReport{
			Status: memory.ConsolidationPartial, Reason: "source_retirement_failed", Provider: "anthropic", SummaryFactID: confirmedSummary, Diagnostic: &memory.ConsolidationDiagnostic{Code: "source_retirement_failed"},
		}, nil, nil},
		{"unexpected RPC error after possible commit", "outcome_unknown", memory.ConsolidationReport{}, nil, errors.New("raw server CANARY")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := &ReportWirePeer{LegacyReportPeer: &LegacyReportPeer{caps: []string{ConsolidationReportV1}}, handle: func(resp *ConsolidateWithReportResponse) error {
				*resp = ConsolidateWithReportResponse{Report: tc.report, Fatal: tc.fatal}
				return tc.wireError
			}}
			client := reportPipeClient(t, peer)
			result, err := client.ConsolidateWithReport(context.Background(), "fixture")
			var known *memory.ConsolidationError
			if !errors.As(err, &known) || known.Diagnostic().Code != tc.expected || !errors.Is(err, memory.ErrMutationOutcomeUnknown) || strings.Contains(err.Error(), "CANARY") || peer.reports.Load() != 1 || peer.legacy.Load() != 0 {
				t.Fatalf("result=%+v error=%v report=%d legacy=%d", result, err, peer.reports.Load(), peer.legacy.Load())
			}
			if tc.expected == "outcome_unknown" && result.Status != "" || tc.expected == "summary_commit_uncertain" && (result.Status != memory.ConsolidationUncertain || result.Diagnostic == nil || strings.Contains(result.Diagnostic.Message, "CANARY")) {
				t.Fatalf("incorrect handling of report: %+v", result)
			}
		})
	}
}
