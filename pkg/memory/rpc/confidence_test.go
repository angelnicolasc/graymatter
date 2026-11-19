package rpc

import (
	"context"
	"errors"
	"math"
	netrpc "net/rpc"
	"net/rpc/jsonrpc"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestConfidenceRoundTripAndLegacyCompatibility(t *testing.T) {
	dir, srv := startServer(t, "")
	c := dialT(t, dir)
	ctx := context.Background()
	verified, unverified := "verified", "unverified"
	v, err := c.PutWithOptionsReturningFact(ctx, "a", "database configured for postgres", memory.WriteOptions{Confidence: &verified})
	if err != nil || v.Confidence != verified || v.ID == "" {
		t.Fatalf("verified write=%+v error=%v", v, err)
	}
	u, err := c.PutWithOptionsReturningFact(ctx, "a", "database configured for sqlite", memory.WriteOptions{Confidence: &unverified})
	if err != nil || u.Confidence != unverified {
		t.Fatalf("unverified write=%+v error=%v", u, err)
	}
	if _, err := c.PutWithOptionsReturningFact(ctx, memory.SharedAgentID, "database shared postgres policy", memory.WriteOptions{Confidence: &verified}); err != nil {
		t.Fatal(err)
	}
	sharedWrite, err := c.PutSharedWithOptionsReturningFact(ctx, "database explicit shared postgres policy", memory.WriteOptions{Confidence: &verified})
	if err != nil || sharedWrite.AgentID != memory.SharedAgentID || sharedWrite.Confidence != verified {
		t.Fatalf("new shared write=%+v %v", sharedWrite, err)
	}
	weight := 0.3
	opts := memory.RecallOptions{MinConfidence: &verified, ConfidenceWeight: &weight}
	result, err := c.RecallWithOptions(ctx, "a", "database configured", 8, opts)
	if err != nil || !reflect.DeepEqual(result.Facts, []string{v.Text}) {
		t.Fatalf("filtered result=%+v error=%v", result, err)
	}
	if result.Retrieval == nil || result.Retrieval.ConfidenceWeight != weight || *result.Retrieval.MinConfidence != verified {
		t.Fatalf("missing policy=%+v", result.Retrieval)
	}
	direct, err := srv.backend.(ConfidenceRecaller).RecallWithOptions(ctx, "a", "database configured", 8, opts)
	if err != nil || !reflect.DeepEqual(result, direct) {
		t.Fatalf("direct/RPC parity: rpc=%+v direct=%+v err=%v", result, direct, err)
	}
	explained, err := c.RecallExplainWithOptions(ctx, "a", "database configured", 8, opts)
	if err != nil || len(explained.Receipts) != 1 || explained.Receipts[0].Text != v.Text || explained.Retrieval == nil {
		t.Fatalf("explain=%+v error=%v", explained, err)
	}
	all, err := c.RecallAllWithOptions(ctx, "a", "database", 8, opts)
	if err != nil || len(all.Facts) != 3 || all.Retrieval == nil {
		t.Fatalf("all=%+v error=%v", all, err)
	}
	empty, err := c.RecallWithOptions(ctx, "empty", "database", 8, opts)
	if err != nil || len(empty.Facts) != 0 || empty.Retrieval == nil {
		t.Fatalf("empty receipt=%+v error=%v", empty, err)
	}

	// An old request has no confidence fields and must deliberately use zero.
	legacy, err := c.Recall(ctx, "a", "database configured", 8)
	if err != nil || len(legacy) != 2 {
		t.Fatalf("legacy=%v error=%v", legacy, err)
	}
	replacement, err := c.ReviseFactsWithOptions(ctx, "a", "database now configured for mysql", memory.WriteOptions{}, u)
	if err != nil || replacement.Confidence != unverified {
		t.Fatalf("revision=%+v error=%v", replacement, err)
	}
	facts, err := c.List("a")
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range facts {
		if fact.ID == u.ID && fact.SupersededBy != replacement.ID {
			t.Fatalf("incorrect lineage: %+v", fact)
		}
	}
	if err := c.SetPinned("a", true, v); err != nil {
		t.Fatal(err)
	}
	if err := c.Retire("a", v); err != nil {
		t.Fatal(err)
	}
	facts, err = c.List("a")
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range facts {
		if fact.ID == v.ID && (fact.Confidence != verified || !fact.Pinned || fact.SupersededBy != memory.SupersededByAgent) {
			t.Fatalf("metadata lost: %+v", fact)
		}
	}
}

func TestConfidenceValidationBeforeRPCEffects(t *testing.T) {
	dir, _ := startServer(t, "")
	c := dialT(t, dir)
	ctx := context.Background()
	empty := ""
	if _, err := c.PutWithOptionsReturningFact(ctx, "a", "must not land", memory.WriteOptions{Confidence: &empty}); err == nil {
		t.Fatal("explicit empty accepted")
	}
	// Bypass the typed client to establish independent server-side validation.
	var putResp PutWithOptionsResponse
	if err := c.call("PutWithOptionsReturningFact", &PutWithOptionsRequest{AgentID: "a", Text: "must not land", Options: memory.WriteOptions{Confidence: &empty}}, &putResp); err == nil {
		t.Fatal("server accepted invalid confidence")
	}
	for _, weight := range []float64{-0.1, 0.6, math.NaN(), math.Inf(1)} {
		if _, err := c.RecallWithOptions(ctx, "a", "fact", 8, memory.RecallOptions{ConfidenceWeight: &weight}); err == nil {
			t.Errorf("invalid weight %v accepted", weight)
		}
	}
	facts, err := c.List("a")
	if err != nil || len(facts) != 0 {
		t.Fatalf("invalid calls caused effects: %+v %v", facts, err)
	}
}

// LegacyProtocolService intentionally has only the previous daemon endpoints
// and a Ping response without capabilities, over the real JSON-RPC codec.
type LegacyProtocolService struct {
	calls atomic.Int64
	pings atomic.Int64
}
type LegacyPingResponse struct{ Protocol string }

func (s *LegacyProtocolService) Ping(req *PingRequest, resp *LegacyPingResponse) error {
	s.pings.Add(1)
	resp.Protocol = Protocol
	return nil
}
func (s *LegacyProtocolService) Put(*PutRequest, *PutResponse) error {
	s.calls.Add(1)
	return nil
}
func (s *LegacyProtocolService) Consolidate(*ConsolidateRequest, *ConsolidateResponse) error {
	s.calls.Add(1)
	return nil
}
func (s *LegacyProtocolService) PutReturningFact(req *PutReturningFactRequest, resp *PutReturningFactResponse) error {
	s.calls.Add(1)
	resp.Fact = memory.Fact{ID: "old-id", Text: req.Text}
	return nil
}
func (s *LegacyProtocolService) RecallDetailed(req *RecallDetailedRequest, resp *RecallDetailedResponse) error {
	s.calls.Add(1)
	resp.Facts = []string{"legacy fact"}
	return nil
}
func (s *LegacyProtocolService) RecallExplain(req *RecallExplainRequest, resp *RecallExplainResponse) error {
	s.calls.Add(1)
	resp.Receipts = []memory.RecallReceipt{{Text: "legacy fact"}}
	return nil
}
func (s *LegacyProtocolService) RecallAll(req *RecallAllRequest, resp *RecallAllResponse) error {
	s.calls.Add(1)
	resp.Facts = []string{"legacy fact"}
	return nil
}

func startLegacyProtocol(t *testing.T) (string, *LegacyProtocolService) {
	t.Helper()
	dir := t.TempDir()
	listener, cleanup, err := Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	svc := &LegacyProtocolService{}
	server := netrpc.NewServer()
	if err := server.RegisterName(ServiceName, svc); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.ServeCodec(jsonrpc.NewServerCodec(conn))
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-stopped })
	return dir, svc
}

func TestNewClientLegacyDaemonNegotiation(t *testing.T) {
	dir, svc := startLegacyProtocol(t)
	c := dialT(t, dir)
	ctx := context.Background()
	verified := "verified"
	zero := 0.0
	for _, opts := range []memory.RecallOptions{{MinConfidence: &verified}, {ConfidenceWeight: &zero}} {
		if _, err := c.RecallWithOptions(ctx, "a", "fact", 8, opts); !errors.Is(err, ErrUnsupportedCapability) {
			t.Fatalf("new recall options error=%v", err)
		}
		if _, err := c.RecallExplainWithOptions(ctx, "a", "fact", 8, opts); !errors.Is(err, ErrUnsupportedCapability) {
			t.Fatalf("new explain error=%v", err)
		}
		if _, err := c.RecallAllWithOptions(ctx, "a", "fact", 8, opts); !errors.Is(err, ErrUnsupportedCapability) {
			t.Fatalf("new all error=%v", err)
		}
	}
	if _, err := c.PutWithOptionsReturningFact(ctx, "a", "fact", memory.WriteOptions{Confidence: &verified}); !errors.Is(err, ErrMaintenancePolicyUnsupported) {
		t.Fatalf("new write error=%v", err)
	}
	if _, err := c.PutSharedWithOptionsReturningFact(ctx, "must not land", memory.WriteOptions{Confidence: &verified}); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("new shared write against previous daemon error=%v", err)
	}
	if _, err := c.ReviseWithOptions(ctx, "a", "old", "new", memory.WriteOptions{}); !errors.Is(err, ErrMaintenancePolicyUnsupported) {
		t.Fatalf("atomic revision error=%v", err)
	}
	if err := c.Put(ctx, "a", "fact"); !errors.Is(err, ErrMaintenancePolicyUnsupported) {
		t.Fatalf("typed legacy put error=%v", err)
	}
	if _, err := c.PutReturningFact(ctx, "a", "fact"); !errors.Is(err, ErrMaintenancePolicyUnsupported) {
		t.Fatalf("typed legacy returning put error=%v", err)
	}
	if _, err := c.PutWithOptionsReturningFact(ctx, "a", "fact", memory.WriteOptions{}); !errors.Is(err, ErrMaintenancePolicyUnsupported) {
		t.Fatalf("typed legacy default put error=%v", err)
	}
	if _, err := c.ReviseFactsWithOptions(ctx, "a", "new", memory.WriteOptions{}); !errors.Is(err, ErrMaintenancePolicyUnsupported) {
		t.Fatalf("typed legacy revision error=%v", err)
	}
	if err := c.Consolidate(ctx, "a"); !errors.Is(err, ErrMaintenancePolicyUnsupported) {
		t.Fatalf("typed legacy maintenance error=%v", err)
	}
	if svc.calls.Load() != 0 {
		t.Fatalf("unsupported operations reached old backend: %d", svc.calls.Load())
	}
	if svc.pings.Load() != 1 {
		t.Fatalf("capabilities not cached: %d pings", svc.pings.Load())
	}
	var raw PutReturningFactResponse
	if err := c.CallService(ServiceName, "PutReturningFact", &PutReturningFactRequest{AgentID: "a", Text: "raw"}, &raw, 0); err != nil || raw.Fact.ID != "old-id" {
		t.Fatalf("generic transport escape hatch = %+v, %v", raw, err)
	}
	plain, err := c.RecallWithOptions(ctx, "a", "fact", 8, memory.RecallOptions{})
	if err != nil || plain.Retrieval != nil || len(plain.Facts) != 1 {
		t.Fatalf("legacy fallback=%+v error=%v", plain, err)
	}
	if _, err := c.RecallExplainWithOptions(ctx, "a", "fact", 8, memory.RecallOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecallAllWithOptions(ctx, "a", "fact", 8, memory.RecallOptions{}); err != nil {
		t.Fatal(err)
	}
	if svc.calls.Load() != 4 {
		t.Fatalf("legacy requests=%d want 4", svc.calls.Load())
	}
}

func TestConfidenceNegotiationWithoutPingOnDial(t *testing.T) {
	dir, svc := startLegacyProtocol(t)
	c, err := Dial(DialOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	zero := 0.0
	_, err = c.RecallWithOptions(context.Background(), "a", "fact", 8, memory.RecallOptions{ConfidenceWeight: &zero})
	if !errors.Is(err, ErrUnsupportedCapability) || svc.pings.Load() != 1 || svc.calls.Load() != 0 {
		t.Fatalf("lazy negotiation err=%v pings=%d calls=%d", err, svc.pings.Load(), svc.calls.Load())
	}
}

type legacyOnlyBackend struct{ Backend }

func TestNewServerLegacyBackendRejectsBeforeMutation(t *testing.T) {
	_, srv := startServer(t, "")
	old := &legacyOnlyBackend{Backend: srv.backend}
	wrapped := NewServer(old, nil)
	var ping PingResponse
	if err := wrapped.Ping(&PingRequest{}, &ping); err != nil || len(ping.Capabilities) != 0 {
		t.Fatalf("advertised unsupported backend: %+v %v", ping, err)
	}
	label := "verified"
	var result PutWithOptionsResponse
	err := wrapped.PutWithOptionsReturningFact(&PutWithOptionsRequest{AgentID: "a", Text: "not stored", Options: memory.WriteOptions{Confidence: &label}}, &result)
	if !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("unsupported=%v", err)
	}
	facts, err := old.List("a")
	if err != nil || len(facts) != 0 {
		t.Fatalf("unsupported mutated: %v %v", facts, err)
	}
	if err := wrapped.Put(&PutRequest{AgentID: "a", Text: "legacy stored"}, &PutResponse{}); err != nil {
		t.Fatal(err)
	}
}

type legacyZeroRecorder struct {
	Backend
	calls []memory.RecallOptions
}

func (s *legacyZeroRecorder) RecallWithOptions(ctx context.Context, a, q string, k int, o memory.RecallOptions) (memory.RecallResult, error) {
	s.calls = append(s.calls, o)
	return memory.RecallResult{}, nil
}
func (s *legacyZeroRecorder) RecallAllWithOptions(ctx context.Context, a, q string, k int, o memory.RecallOptions) (memory.RecallResult, error) {
	return s.RecallWithOptions(ctx, a, q, k, o)
}
func (s *legacyZeroRecorder) RecallExplainWithOptions(ctx context.Context, a, q string, k int, o memory.RecallOptions) (memory.RecallExplainResult, error) {
	_, err := s.RecallWithOptions(ctx, a, q, k, o)
	return memory.RecallExplainResult{}, err
}

func TestLegacyEndpointsPinZeroExplicitly(t *testing.T) {
	b := &legacyZeroRecorder{}
	s := NewServer(b, nil)
	if err := s.Recall(&RecallRequest{}, &RecallResponse{}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecallShared(&RecallSharedRequest{}, &RecallSharedResponse{}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecallDetailed(&RecallDetailedRequest{}, &RecallDetailedResponse{}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecallAll(&RecallAllRequest{}, &RecallAllResponse{}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecallExplain(&RecallExplainRequest{}, &RecallExplainResponse{}); err != nil {
		t.Fatal(err)
	}
	if len(b.calls) != 5 {
		t.Fatalf("legacy endpoints dispatch count=%d", len(b.calls))
	}
	for _, opts := range b.calls {
		if opts.ConfidenceWeight == nil || *opts.ConfidenceWeight != 0 || opts.MinConfidence != nil {
			t.Fatalf("legacy policy not explicit zero: %+v", opts)
		}
	}
}

// Keep the old response type independent of PingResponse in this test.
func TestOldClientNewDaemonPingAndMethods(t *testing.T) {
	dir, _ := startServer(t, "")
	addr, _, err := readDiscovery(dir)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialAddr(addr, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := netrpc.NewClientWithCodec(jsonrpc.NewClientCodec(conn))
	t.Cleanup(func() { _ = old.Close() })
	var ping LegacyPingResponse
	if err := old.Call(ServiceName+".Ping", &PingRequest{}, &ping); err != nil || ping.Protocol != Protocol {
		t.Fatalf("old ping=%+v %v", ping, err)
	}
	if err := old.Call(ServiceName+".Put", &PutRequest{AgentID: "a", Text: "legacy fact"}, &PutResponse{}); err != nil {
		t.Fatal(err)
	}
	var result RecallResponse
	if err := old.Call(ServiceName+".Recall", &RecallRequest{AgentID: "a", Query: "legacy", TopK: 8}, &result); err != nil || len(result.Facts) != 1 {
		t.Fatalf("old recall=%+v %v", result, err)
	}
}

func TestRawStoreLegacyZeroTopKAcrossEndpoints(t *testing.T) {
	dir, _ := startServer(t, "")
	c := dialT(t, dir)
	ctx := context.Background()
	if err := c.Put(ctx, "a", "legacy database policy"); err != nil {
		t.Fatal(err)
	}
	if err := c.PutShared(ctx, "shared database policy"); err != nil {
		t.Fatal(err)
	}
	plain, err := c.Recall(ctx, "a", "database", 0)
	if err != nil || len(plain) != 0 {
		t.Fatalf("legacy raw Recall zero=%v %v", plain, err)
	}
	shared, err := c.RecallShared(ctx, "database", 0)
	if err != nil || len(shared) != 0 {
		t.Fatalf("legacy raw Shared zero=%v %v", shared, err)
	}
	all, err := c.RecallAll(ctx, "a", "database", 0)
	if err != nil || len(all) != 0 {
		t.Fatalf("legacy raw All zero=%v %v", all, err)
	}
	explain, err := c.RecallExplain(ctx, "a", "database", 0)
	if err != nil || len(explain) != 0 {
		t.Fatalf("legacy raw Explain zero=%v %v", explain, err)
	}
	plain, _, err = c.RecallDetailed(ctx, "a", "database", 0)
	if err != nil || len(plain) != 0 {
		t.Fatalf("legacy raw Detailed zero=%v %v", plain, err)
	}
	result, err := c.RecallWithOptions(ctx, "a", "database", 0, memory.RecallOptions{})
	if err != nil || len(result.Facts) != 1 || result.Retrieval != nil {
		t.Fatalf("new method must default: %+v %v", result, err)
	}
	explain, err = c.RecallExplain(ctx, "a", "database", 8)
	if err != nil || len(explain) != 1 || explain[0].Ranking != nil {
		t.Fatalf("legacy receipt gained new ranking metadata: %+v %v", explain, err)
	}
}

func TestConfidenceWireRejectsExplicitNullAndInvalidTypes(t *testing.T) {
	dir, _ := startServer(t, "")
	c := dialT(t, dir)
	for _, value := range []any{nil, "", 123, true, []string{"verified"}, " verified", "unknown"} {
		request := map[string]any{"AgentID": "a", "Text": "must not land", "Options": map[string]any{"confidence": value}}
		if err := c.call("PutWithOptionsReturningFact", request, &PutWithOptionsResponse{}); err == nil {
			t.Errorf("wire confidence %v accepted", value)
		}
	}
	for _, method := range []string{"RecallWithOptions", "RecallExplainWithOptions", "RecallAllWithOptions"} {
		for _, field := range []string{"min_confidence", "confidence_weight"} {
			request := map[string]any{"AgentID": "a", "Query": "query", "TopK": 8, "Options": map[string]any{field: nil}}
			if err := c.call(method, request, &RecallWithOptionsResponse{}); err == nil {
				t.Errorf("%s %s=null accepted", method, field)
			}
		}
	}
	for _, method := range []string{"PutWithOptionsReturningFact", "ReviseWithOptions", "ReviseFactsWithOptions"} {
		request := map[string]any{"AgentID": "a", "Text": "must not land", "Options": map[string]any{"confidence": nil}}
		if err := c.call(method, request, &PutWithOptionsResponse{}); err == nil {
			t.Errorf("%s null accepted", method)
		}
	}
	facts, err := c.List("a")
	if err != nil || len(facts) != 0 {
		t.Fatalf("invalid wire caused effects: %+v %v", facts, err)
	}
}

type confidenceFailureBackend struct {
	Backend
	fact    memory.Fact
	failure error
}

func (*confidenceFailureBackend) MaintenanceOnly() bool              { return true }
func (*confidenceFailureBackend) AtomicRevisionSupported() bool      { return true }
func (*confidenceFailureBackend) SupportsAuthoritativeLineage() bool { return true }
func (*confidenceFailureBackend) InspectLineage(context.Context, string, string, string) (memory.LineageInspection, error) {
	return memory.LineageInspection{}, memory.ErrFactNotFound
}
func (*confidenceFailureBackend) ExportLineage(context.Context, string) (memory.LineageArchive, error) {
	return memory.LineageArchive{}, nil
}

func (b *confidenceFailureBackend) PutWithOptionsReturningFact(context.Context, string, string, memory.WriteOptions) (memory.Fact, error) {
	return b.fact, b.failure
}
func (b *confidenceFailureBackend) ReviseWithOptions(context.Context, string, string, string, memory.WriteOptions) (memory.Fact, error) {
	return b.fact, b.failure
}
func (b *confidenceFailureBackend) ReviseFactsWithOptions(context.Context, string, string, memory.WriteOptions, ...memory.Fact) (memory.Fact, error) {
	return b.fact, b.failure
}

func confidenceClientForBackend(t *testing.T, backend Backend) *Client {
	t.Helper()
	dir := t.TempDir()
	listener, cleanup, err := Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	srv := NewServer(backend, nil)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(srv.Stop)
	return dialT(t, dir)
}

func TestConfidenceRPCPreservesKnownCommittedFactOnLaterError(t *testing.T) {
	ctx := context.Background()
	for _, knownCommit := range []bool{false, true} {
		backend := &confidenceFailureBackend{failure: errors.New("backend follow-up failed")}
		if knownCommit {
			backend.fact = memory.Fact{ID: "committed-exact-id", Confidence: "unverified", Text: "replacement"}
		}
		c := confidenceClientForBackend(t, backend)
		for _, operation := range []string{"put", "revise", "revise-facts"} {
			var fact memory.Fact
			var err error
			switch operation {
			case "put":
				fact, err = c.PutWithOptionsReturningFact(ctx, "a", "replacement", memory.WriteOptions{})
			case "revise":
				fact, err = c.ReviseWithOptions(ctx, "a", "old", "replacement", memory.WriteOptions{})
			case "revise-facts":
				fact, err = c.ReviseFactsWithOptions(ctx, "a", "replacement", memory.WriteOptions{})
			}
			if err == nil || err.Error() != backend.failure.Error() || !reflect.DeepEqual(fact, backend.fact) {
				t.Fatalf("%s known=%v fact=%+v error=%v", operation, knownCommit, fact, err)
			}
		}
	}
}

func TestConfiguredConfidenceDefaultNegotiatesAndMatchesDirect(t *testing.T) {
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), StrictWrite: true, ConfidenceWeight: 0.3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for _, label := range []string{"verified", "unverified"} {
		if _, err := store.PutWithOptionsReturningFact(ctx, "a", label+" database policy", memory.WriteOptions{Confidence: &label}); err != nil {
			t.Fatal(err)
		}
	}
	c := confidenceClientForBackend(t, store)
	if c.DefaultConfidenceWeight() != 0.3 {
		t.Fatalf("configured default not negotiated: %g", c.DefaultConfidenceWeight())
	}
	actual, err := c.RecallWithOptions(ctx, "a", "database policy", 8, memory.RecallOptions{})
	if err != nil || actual.Retrieval == nil || actual.Retrieval.ConfidenceWeight != 0.3 {
		t.Fatalf("configured default lost: %+v %v", actual, err)
	}
	expected, err := store.RecallWithOptions(ctx, "a", "database policy", 8, memory.RecallOptions{})
	if err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("configured direct/RPC mismatch: actual=%+v expected=%+v err=%v", actual, expected, err)
	}
	zero := 0.0
	actual, err = c.RecallWithOptions(ctx, "a", "database policy", 8, memory.RecallOptions{ConfidenceWeight: &zero})
	if err != nil || actual.Retrieval == nil || actual.Retrieval.ConfidenceWeight != 0 {
		t.Fatalf("configured default zero opt-out lost: %+v %v", actual, err)
	}
	legacy, err := c.RecallExplain(ctx, "a", "database policy", 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range legacy {
		if receipt.Ranking != nil {
			t.Fatalf("legacy endpoint emitted ranking metadata: %+v", receipt)
		}
	}
}

func TestClientPositiveDefaultAgainstLegacyRequiresCapability(t *testing.T) {
	dir, backend := startLegacyProtocol(t)
	positive := 0.2
	c, err := Dial(DialOptions{DataDir: dir, PingOnDial: true, DefaultConfidenceWeight: &positive})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_, err = c.RecallWithOptions(context.Background(), "a", "fact", 8, memory.RecallOptions{})
	if !errors.Is(err, ErrUnsupportedCapability) || backend.calls.Load() != 0 || c.DefaultConfidenceWeight() != positive {
		t.Fatalf("positive default silently downgraded: err=%v calls=%d default=%g", err, backend.calls.Load(), c.DefaultConfidenceWeight())
	}
	zero := 0.0
	_, err = c.RecallWithOptions(context.Background(), "a", "fact", 8, memory.RecallOptions{ConfidenceWeight: &zero})
	if !errors.Is(err, ErrUnsupportedCapability) || backend.calls.Load() != 0 {
		t.Fatalf("explicit zero policy receipt fabricated by legacy: %v calls=%d", err, backend.calls.Load())
	}
}

type invalidDefaultRecaller struct{ legacyZeroRecorder }

func (b *invalidDefaultRecaller) DefaultConfidenceWeight() float64 { return 0.6 }

func TestInvalidNegotiatedDefaultNeverCachesUsableCapabilities(t *testing.T) {
	dir := t.TempDir()
	listener, cleanup, err := Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	backend := &invalidDefaultRecaller{}
	server := NewServer(backend, nil)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	client, err := Dial(DialOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	for range 2 {
		_, err := client.RecallWithOptions(context.Background(), "a", "query", 8, memory.RecallOptions{})
		if err == nil || !strings.Contains(err.Error(), "invalid daemon default policy") {
			t.Fatalf("invalid default negotiation became usable: %v", err)
		}
	}
	if len(backend.calls) != 0 {
		t.Fatalf("invalid default caused operations: %d", len(backend.calls))
	}
}
