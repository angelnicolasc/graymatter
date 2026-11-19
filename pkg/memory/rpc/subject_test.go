package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	netrpc "net/rpc"
	"net/rpc/jsonrpc"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestSubjectRPCRoundTripReplacementAndID(t *testing.T) {
	dir, _ := startServer(t, "")
	c := dialT(t, dir)
	ctx := context.Background()
	subject, equivalent := "  CAFÉ\t政策 ", "cafe\u0301 政策"
	source, quote := "doc-é:1", "Literal 中文\r\n\tquote"
	old, err := c.PutManyReturningReceipts(ctx, "a", []memory.WriteFact{{Text: "Retention is 11 days", Options: memory.WriteOptions{Subject: &subject, Source: &source, Quote: &quote}}})
	if err != nil || len(old) != 1 {
		t.Fatalf("old receipt %+v %v", old, err)
	}
	next, err := c.PutManyReturningReceipts(ctx, "a", []memory.WriteFact{{Text: "Retention is 37 days", Options: memory.WriteOptions{Subject: &equivalent}}})
	if err != nil || len(next) != 1 || len(next[0].Replaced) != 1 || next[0].Replaced[0].ID != old[0].Fact.ID || next[0].Replaced[0].Text != old[0].Fact.Text {
		t.Fatalf("replacement receipt %+v %v", next, err)
	}
	if next[0].Fact.Evidence != nil || len(next[0].Subjects) != 1 || next[0].Subjects[0].Revisions != 1 {
		t.Fatalf("replacement inherited citation or lost subject: %+v", next[0])
	}
	found, err := c.SearchScoped(ctx, "a", []string{"Retention"}, 8, false, memory.RecallOptions{})
	if err != nil || len(found.Facts) != 1 || found.Facts[0].ID != next[0].Fact.ID || len(found.Facts[0].Subjects) != 1 {
		t.Fatalf("one live subject %+v %v", found, err)
	}
	selected, err := c.ResolveFactTargets(ctx, "a", next[0].Fact.ID, "id", false)
	if err != nil || len(selected) != 1 || selected[0].ID != next[0].Fact.ID {
		t.Fatalf("resolve ID %+v %v", selected, err)
	}
	before, _ := c.List("a")
	suggestion, err := c.SuggestSimilarFacts(ctx, "a", next[0].Fact.Text)
	after, _ := c.List("a")
	if err != nil || suggestion.Total != 1 || !reflect.DeepEqual(before, after) {
		t.Fatalf("suggestion mutated rows or lost match %+v %v", suggestion, err)
	}
	historical, err := c.FetchCitation(ctx, "a", old[0].Fact.ID, memory.NewEvidenceSnapshot(old[0].Fact).ContentHash)
	if err != nil || historical.Evidence == nil || historical.Evidence.Source != source || historical.Evidence.Quote != quote {
		t.Fatalf("old version evidence %+v %v", historical, err)
	}
	replaces, err := memory.NewReplacementIDs([]string{next[0].Fact.ID})
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := c.PutManyReturningReceipts(ctx, "a", []memory.WriteFact{{
		Text: "Retention is 45 days", Options: memory.WriteOptions{Replaces: replaces},
	}})
	if err != nil || len(explicit) != 1 || len(explicit[0].Replaced) != 1 || explicit[0].Replaced[0].ID != next[0].Fact.ID {
		t.Fatalf("explicit replacement lost across RPC: %+v %v", explicit, err)
	}
	found, err = c.SearchScoped(ctx, "a", []string{"Retention"}, 8, false, memory.RecallOptions{})
	if err != nil || len(found.Facts) != 1 || found.Facts[0].ID != explicit[0].Fact.ID {
		t.Fatalf("explicit replacement did not retire its target: %+v %v", found, err)
	}
}

func TestSubjectRPCRejectsOldCapabilityBeforeMutation(t *testing.T) {
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	clientConn, serverConn := net.Pipe()
	service := &lostUtilityReply{store: store, conn: serverConn}
	srv := netrpc.NewServer()
	if err := srv.RegisterName(ServiceName, service); err != nil {
		t.Fatal(err)
	}
	go srv.ServeCodec(jsonrpc.NewServerCodec(serverConn))
	c := &Client{rpc: netrpc.NewClientWithCodec(jsonrpc.NewClientCodec(clientConn)), conn: clientConn, callTimeout: time.Second}
	t.Cleanup(func() { c.Close(); serverConn.Close() })
	subject := "policy"
	_, err = c.PutManyReturningReceipts(context.Background(), "a", []memory.WriteFact{{Text: "never silently add", Options: memory.WriteOptions{Subject: &subject}}})
	if !errors.Is(err, memory.ErrSubjectUnsupported) || service.calls.Load() != 0 {
		t.Fatalf("unsupported daemon mutated: %v calls=%d", err, service.calls.Load())
	}
	_, err = c.PutManyReturningFacts(context.Background(), "a", []memory.WriteFact{{Text: "legacy endpoint must reject subject", Options: memory.WriteOptions{Subject: &subject}}})
	if !errors.Is(err, memory.ErrSubjectUnsupported) || service.calls.Load() != 0 {
		t.Fatalf("legacy endpoint dropped subject: %v calls=%d", err, service.calls.Load())
	}
}

func TestSubjectRPCRejectsNullIntent(t *testing.T) {
	for _, field := range []string{"subject", "replaces"} {
		for _, dst := range []any{&PutWithOptionsRequest{}, &ReviseWithOptionsRequest{}, &ReviseFactsWithOptionsRequest{}} {
			if err := json.Unmarshal([]byte(`{"Options":{"`+field+`":null}}`), dst); err == nil {
				t.Fatalf("null %s accepted by %T", field, dst)
			}
		}
		for _, dst := range []any{&PutManyRequest{}, &PutManyReceiptsRequest{}} {
			if err := json.Unmarshal([]byte(`{"Facts":[{"text":"x","options":{"`+field+`":null}}]}`), dst); err == nil {
				t.Fatalf("null %s accepted by %T", field, dst)
			}
		}
	}
}

type lostSubjectReply struct {
	store *memory.Store
	conn  net.Conn
	calls atomic.Int32
}

func (s *lostSubjectReply) Ping(_ *PingRequest, out *PingResponse) error {
	out.Protocol = Protocol
	out.Capabilities = []string{SubjectHeadsV1, MaintenanceOnlyV1, InspectionContextV1}
	return nil
}

func (s *lostSubjectReply) PutManyReturningReceipts(req *PutManyReceiptsRequest, out *PutManyReceiptsResponse) error {
	s.calls.Add(1)
	var err error
	out.Receipts, err = s.store.PutManyReturningReceipts(context.Background(), req.AgentID, req.Facts)
	if err != nil {
		return err
	}
	return s.conn.Close()
}

func TestSubjectRPCReplyLossDoesNotReplayReplacement(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.Open(memory.StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	subject := "retention"
	old, err := store.PutManyReturningReceipts(context.Background(), "a", []memory.WriteFact{{Text: "eleven days", Options: memory.WriteOptions{Subject: &subject}}})
	if err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := net.Pipe()
	service := &lostSubjectReply{store: store, conn: serverConn}
	srv := netrpc.NewServer()
	if err := srv.RegisterName(ServiceName, service); err != nil {
		t.Fatal(err)
	}
	go srv.ServeCodec(jsonrpc.NewServerCodec(serverConn))
	c := &Client{rpc: netrpc.NewClientWithCodec(jsonrpc.NewClientCodec(clientConn)), conn: clientConn, callTimeout: time.Second}
	t.Cleanup(func() { _ = c.Close(); _ = serverConn.Close() })
	receipts, err := c.PutManyReturningReceipts(context.Background(), "a", []memory.WriteFact{{Text: "thirty-seven days", Options: memory.WriteOptions{Subject: &subject}}})
	if !errors.Is(err, memory.ErrMutationOutcomeUnknown) || len(receipts) != 0 || service.calls.Load() != 1 {
		t.Fatalf("lost reply receipts=%+v err=%v calls=%d", receipts, err, service.calls.Load())
	}
	assertRows := func() {
		t.Helper()
		rows, err := store.List("a")
		if err != nil || len(rows) != 2 {
			t.Fatalf("committed rows=%+v err=%v", rows, err)
		}
		live, retired := 0, 0
		for _, row := range rows {
			if row.IsSuperseded() {
				retired++
				if row.ID != old[0].Fact.ID {
					t.Fatalf("wrong retired row: %+v", row)
				}
			} else {
				live++
				if row.Text != "thirty-seven days" {
					t.Fatalf("wrong live row: %+v", row)
				}
			}
		}
		if live != 1 || retired != 1 {
			t.Fatalf("live=%d retired=%d", live, retired)
		}
	}
	assertRows()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = memory.Open(memory.StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	assertRows()
}

type malformedSubjectReply struct{ receipts []memory.WriteReceipt }

func (*malformedSubjectReply) Ping(_ *PingRequest, out *PingResponse) error {
	out.Protocol = Protocol
	out.Capabilities = []string{SubjectHeadsV1, MaintenanceOnlyV1, InspectionContextV1}
	return nil
}
func (s *malformedSubjectReply) PutManyReturningReceipts(_ *PutManyReceiptsRequest, out *PutManyReceiptsResponse) error {
	out.Receipts = s.receipts
	out.Error = "follow-up failed"
	return nil
}

func TestSubjectRPCValidatesIdentityEvenOnFollowupError(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	srv := netrpc.NewServer()
	service := &malformedSubjectReply{receipts: []memory.WriteReceipt{{Fact: memory.Fact{ID: "wrong", AgentID: "other", Text: "assertion"}}}}
	if err := srv.RegisterName(ServiceName, service); err != nil {
		t.Fatal(err)
	}
	go srv.ServeCodec(jsonrpc.NewServerCodec(serverConn))
	c := &Client{rpc: netrpc.NewClientWithCodec(jsonrpc.NewClientCodec(clientConn)), conn: clientConn, callTimeout: time.Second}
	t.Cleanup(func() { _ = c.Close(); _ = serverConn.Close() })
	receipts, err := c.PutManyReturningReceipts(context.Background(), "a", []memory.WriteFact{{Text: "assertion"}})
	if !errors.Is(err, memory.ErrMutationOutcomeUnknown) || len(receipts) != 0 {
		t.Fatalf("unverified partial receipt leaked: %+v %v", receipts, err)
	}
}

type subjectWithoutOptionWriter struct{ calls atomic.Int32 }

func (*subjectWithoutOptionWriter) Ping(_ *PingRequest, out *PingResponse) error {
	out.Protocol = Protocol
	out.Capabilities = []string{SubjectHeadsV1, MaintenanceOnlyV1, FactIdentityV1}
	return nil
}
func (s *subjectWithoutOptionWriter) PutReturningFact(_ *PutReturningFactRequest, out *PutReturningFactResponse) error {
	s.calls.Add(1)
	out.Fact = memory.Fact{ID: "lost-options"}
	return nil
}
func (s *subjectWithoutOptionWriter) Put(_ *PutRequest, _ *PutResponse) error {
	s.calls.Add(1)
	return nil
}

func TestSubjectRPCNeverDowngradesToIdentityOnlyWrite(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	srv := netrpc.NewServer()
	service := &subjectWithoutOptionWriter{}
	if err := srv.RegisterName(ServiceName, service); err != nil {
		t.Fatal(err)
	}
	go srv.ServeCodec(jsonrpc.NewServerCodec(serverConn))
	c := &Client{rpc: netrpc.NewClientWithCodec(jsonrpc.NewClientCodec(clientConn)), conn: clientConn, callTimeout: time.Second}
	t.Cleanup(func() { _ = c.Close(); _ = serverConn.Close() })
	subject := "topic"
	replaces, err := memory.NewReplacementIDs([]string{"id"})
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []memory.WriteOptions{{Subject: &subject}, {Replaces: replaces}} {
		if _, err := c.PutWithOptionalIdentity(context.Background(), "a", "new", opts); err == nil {
			t.Fatal("identity fallback accepted subject intent")
		}
		if _, err := c.PutWithOptionsReturningFact(context.Background(), "a", "new", opts); err == nil {
			t.Fatal("options fallback accepted subject intent")
		}
	}
	if service.calls.Load() != 0 {
		t.Fatalf("legacy writes=%d", service.calls.Load())
	}
}

func TestSubjectRPCRejectsContradictoryRevisionBeforeDispatch(t *testing.T) {
	c := &Client{}
	subject := "topic"
	replaces, err := memory.NewReplacementIDs([]string{"id"})
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []memory.WriteOptions{{Subject: &subject}, {Replaces: replaces}} {
		if _, err := c.ReviseWithOptions(context.Background(), "a", "old", "new", opts); err == nil {
			t.Fatal("revision accepted subject intent")
		}
		if _, err := c.ReviseFactsWithOptions(context.Background(), "a", "new", opts, memory.Fact{ID: "old"}); err == nil {
			t.Fatal("revision facts accepted subject intent")
		}
	}
}
