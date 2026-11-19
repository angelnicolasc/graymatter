package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	netrpc "net/rpc"
	"net/rpc/jsonrpc"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestUtilityRPCWireRejectsExplicitNullOptions(t *testing.T) {
	for _, body := range []string{`{"Facts":[{"text":"one","options":null}]}`, `{"Facts":[{"text":"one","options":{"confidence":null}}]}`} {
		var req PutManyRequest
		if err := json.Unmarshal([]byte(body), &req); err == nil {
			t.Fatalf("invalid batch options accepted: %s", body)
		}
	}
	for _, body := range []string{`{"Options":null}`, `{"Options":{"min_confidence":null}}`, `{"Options":{"confidence_weight":null}}`} {
		var req SearchScopedRequest
		if err := json.Unmarshal([]byte(body), &req); err == nil {
			t.Fatalf("invalid search options accepted: %s", body)
		}
	}
}

func TestUtilityRPCRejectsOldDaemonBeforeDispatch(t *testing.T) {
	dir, legacy := startLegacyProtocol(t)
	c := dialT(t, dir)
	if _, err := c.SearchScoped(context.Background(), "a", []string{"query"}, 8, true, memory.RecallOptions{}); !errors.Is(err, memory.ErrUtilityUnsupported) || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("missing scoped search capability: %v", err)
	}
	if _, err := c.PutManyReturningFacts(context.Background(), "a", []memory.WriteFact{{Text: "must not land"}}); !errors.Is(err, ErrMaintenancePolicyUnsupported) {
		t.Fatalf("unknown maintenance policy: %v", err)
	}
	if legacy.calls.Load() != 0 {
		t.Fatalf("old daemon received %d requests", legacy.calls.Load())
	}
}

func TestUtilityRPCRoundTrip(t *testing.T) {
	dir, _ := startServer(t, "")
	c := dialT(t, dir)
	ctx := context.Background()
	verified := "verified"
	written, err := c.PutManyReturningFacts(ctx, "a", []memory.WriteFact{{Text: "deployment alpha"}, {Text: "deployment beta", Options: memory.WriteOptions{Confidence: &verified}}})
	if err != nil || len(written) != 2 || written[0].ID == written[1].ID || written[1].Confidence != verified {
		t.Fatalf("batch receipt=%+v err=%v", written, err)
	}
	if _, err := c.PutManyReturningFacts(ctx, memory.SharedAgentID, []memory.WriteFact{{Text: "deployment shared"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutManyReturningFacts(ctx, "b", []memory.WriteFact{{Text: "deployment forbidden"}}); err != nil {
		t.Fatal(err)
	}
	got, err := c.SearchScoped(ctx, "a", []string{"deployment", "alpha"}, 8, true, memory.RecallOptions{})
	if err != nil || len(got.Facts) != 3 || len(got.PerQuery) != 2 {
		t.Fatalf("search=%+v err=%v", got, err)
	}
	for _, fact := range got.Facts {
		if fact.ID == "" || fact.Namespace == "b" || fact.Text == "deployment forbidden" {
			t.Fatalf("invalid identity %+v", fact)
		}
		if fact.Receipt != nil && fact.Receipt.Ranking != nil {
			t.Fatalf("omitted legacy confidence changed metadata: %+v", fact.Receipt)
		}
	}
	zero := 0.0
	explicit, err := c.SearchScoped(ctx, "a", []string{"deployment"}, 8, true, memory.RecallOptions{ConfidenceWeight: &zero})
	if err != nil || explicit.Retrieval == nil || len(explicit.Facts) != 3 {
		t.Fatalf("explicit options result=%+v err=%v", explicit, err)
	}
	for _, fact := range explicit.Facts {
		if fact.Receipt == nil || fact.Receipt.Ranking == nil {
			t.Fatalf("explicit confidence receipt absent: %+v", fact)
		}
	}
	if _, err := c.PutManyReturningFacts(ctx, "invalid", []memory.WriteFact{{Text: "partial"}, {Text: " "}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	rows, err := c.List("invalid")
	if err != nil || len(rows) != 0 {
		t.Fatalf("partial rows %+v %v", rows, err)
	}
}

func TestVC301RPCVocabularyCorpus(t *testing.T) {
	dir, _ := startServer(t, "")
	client := dialT(t, dir)
	ctx := context.Background()
	const project = "vc-vocabulary"
	writes := []memory.WriteFact{
		{Text: "Calibration safety uses a sealed enclosure."},
		{Text: "Calibration access uses a keyed interlock."},
		{Text: "Calibration inspection uses an isolated motor."},
	}
	if _, err := client.PutManyReturningFacts(ctx, project, writes); err != nil {
		t.Fatal(err)
	}
	for _, topK := range []int{1, 8} {
		result, err := client.SearchScoped(ctx, project, []string{"unmatchedtoken"}, topK, false, memory.RecallOptions{})
		if err != nil {
			t.Fatal(err)
		}
		shown := min(topK, len(writes))
		if len(result.Facts) != shown || result.Feedback == "" {
			t.Fatalf("top_k=%d: expected feedback and %d facts, got %q and %d", topK, shown, result.Feedback, len(result.Facts))
		}
		want := shown < len(writes)
		t.Logf("top_k=%d live=%d shown=%d library_feedback=true has_unshown_facts=%t want=%t",
			topK, len(writes), shown, result.HasUnshownFacts, want)
		if result.HasUnshownFacts != want {
			t.Errorf("top_k=%d: has_unshown_facts=%t, want %t", topK, result.HasUnshownFacts, want)
		}
	}
}

type lostUtilityReply struct {
	store *memory.Store
	conn  net.Conn
	calls atomic.Int32
}

func (s *lostUtilityReply) Ping(_ *PingRequest, out *PingResponse) error {
	out.Protocol = Protocol
	out.Capabilities = []string{UtilityV1, MaintenanceOnlyV1, InspectionContextV1}
	return nil
}

func (s *lostUtilityReply) PutManyReturningFacts(req *PutManyRequest, out *PutManyResponse) error {
	s.calls.Add(1)
	facts, err := s.store.PutManyReturningFacts(context.Background(), req.AgentID, req.Facts)
	if err != nil {
		return err
	}
	out.Facts = facts
	return s.conn.Close() // Commit exists; the client never receives the receipt.
}

func TestUtilityRPCReplyLossDoesNotReplay(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.Open(memory.StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	clientConn, serverConn := net.Pipe()
	service := &lostUtilityReply{store: store, conn: serverConn}
	srv := netrpc.NewServer()
	if err := srv.RegisterName(ServiceName, service); err != nil {
		t.Fatal(err)
	}
	go srv.ServeCodec(jsonrpc.NewServerCodec(serverConn))
	c := &Client{rpc: netrpc.NewClientWithCodec(jsonrpc.NewClientCodec(clientConn)), conn: clientConn, callTimeout: time.Second}
	t.Cleanup(func() { _ = c.Close(); _ = serverConn.Close() })
	facts, err := c.PutManyReturningFacts(context.Background(), "a", []memory.WriteFact{{Text: "one"}, {Text: "two"}})
	if !errors.Is(err, memory.ErrMutationOutcomeUnknown) || len(facts) != 0 || service.calls.Load() != 1 {
		t.Fatalf("response loss facts=%+v err=%v calls=%d", facts, err, service.calls.Load())
	}
	rows, err := store.List("a")
	if err != nil || len(rows) != 2 {
		t.Fatalf("committed rows %+v %v", rows, err)
	}
	ids := []string{rows[0].ID, rows[1].ID}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = memory.Open(memory.StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err = store.List("a")
	if err != nil || len(rows) != 2 || !reflect.DeepEqual(ids, []string{rows[0].ID, rows[1].ID}) {
		t.Fatalf("reopen changed committed batch %+v %v", rows, err)
	}
}
