package rpc

import (
	"context"
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

func TestInspectionRoundTripParity(t *testing.T) {
	dir, server := startServer(t, "")
	c := dialT(t, dir)
	s := server.backend.(*memory.Store)
	ctx := context.Background()
	f, err := s.PutReturningFact(ctx, "a", "database production policy")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.List("a")
	directPage, err := s.ListFacts(ctx, "a", "all", "DATABASE", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	remotePage, err := c.ListFacts(ctx, "a", "all", "DATABASE", "", 1)
	if err != nil || !reflect.DeepEqual(directPage, remotePage) {
		t.Fatalf("page parity: %+v %+v %v", directPage, remotePage, err)
	}
	direct, err := s.RecallPreview(ctx, "a", "database", 8)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := c.InspectRecall(ctx, "a", "database", 8)
	if err != nil || len(remote) != len(direct) || len(remote) != 1 || remote[0].Provenance.FactID != direct[0].Provenance.FactID || remote[0].Text != direct[0].Text {
		t.Fatalf("recall parity: %+v %+v %v", direct, remote, err)
	}
	after, _ := s.List("a")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("RPC preview changed access state")
	}
	pinned, err := c.CurateFact(ctx, "a", f.ID, "pin", "", f)
	if err != nil || !pinned.Pinned {
		t.Fatalf("pin: %+v %v", pinned, err)
	}
	if _, err := c.CurateFact(ctx, "a", f.ID, "revise", "stale", f); !errors.Is(err, memory.ErrFactChanged) {
		t.Fatalf("lost conflict type: %v", err)
	}
	if _, err := c.CurateFact(ctx, "other", f.ID, "retire", "", pinned); !errors.Is(err, memory.ErrFactChanged) {
		t.Fatalf("namespace: %v", err)
	}
	replacement, err := c.CurateFact(ctx, "a", f.ID, "revise", "replacement", pinned)
	if err != nil || replacement.ID == f.ID || replacement.Pinned {
		t.Fatalf("revise: %+v %v", replacement, err)
	}
	retired, err := c.ListFacts(ctx, "a", "retired", "", "", 1)
	if err != nil || retired.Total != 1 || retired.Facts[0].SupersededBy != replacement.ID {
		t.Fatalf("lineage: %+v %v", retired, err)
	}
	health, err := c.InspectHealth(ctx)
	if err != nil || health.ProviderConfigured || health.ProviderReachability != "not applicable" {
		t.Fatalf("health: %+v %v", health, err)
	}
}

func TestInspectionLegacyDaemonDoesNotFallback(t *testing.T) {
	dir, service := startLegacyProtocol(t)
	c := dialT(t, dir)
	ctx := context.Background()
	_, previewErr := c.RecallPreview(ctx, "a", "fact", 8)
	_, listErr := c.ListFacts(ctx, "a", "all", "", "", 10)
	_, curateErr := c.CurateFact(ctx, "a", "id", "pin", "", memory.Fact{})
	_, healthErr := c.InspectHealth(ctx)
	for _, err := range []error{previewErr, listErr, curateErr, healthErr} {
		if !errors.Is(err, memory.ErrInspectionUnsupported) {
			t.Fatalf("unsupported: %v", err)
		}
	}
	if service.calls.Load() != 0 {
		t.Fatal("inspection fell back to side-effecting legacy operation")
	}
}

// LostCurationResponse commits successfully and drops the connection before
// the response arrives, modeling a crash between commit and acknowledgement.
type LostCurationResponse struct {
	store *memory.Store
	conn  net.Conn
	calls atomic.Int64
}

func (s *LostCurationResponse) CurateFact(req *CurateFactRequest, resp *CurateFactResponse) error {
	s.calls.Add(1)
	f, err := s.store.CurateFact(context.Background(), req.AgentID, req.FactID, req.Action, req.Text, req.Expected)
	resp.Fact = f
	_ = s.conn.Close()
	return err
}

func TestCurationDroppedResponseIsAmbiguousAndNeverReplayed(t *testing.T) {
	s, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f, err := s.PutReturningFact(context.Background(), "a", "old text")
	if err != nil {
		t.Fatal(err)
	}
	clientConn, serverConn := net.Pipe()
	svc := &LostCurationResponse{store: s, conn: serverConn}
	server := netrpc.NewServer()
	if err := server.RegisterName(ServiceName, svc); err != nil {
		t.Fatal(err)
	}
	go server.ServeCodec(jsonrpc.NewServerCodec(serverConn))
	c := &Client{rpc: netrpc.NewClientWithCodec(jsonrpc.NewClientCodec(clientConn)), conn: clientConn, callTimeout: time.Second, capKnown: true, capabilities: map[string]bool{CurationV1: true, InspectionContextV1: true, AtomicRevisionV1: true, AuthoritativeLineageV1: true}}
	defer c.Close()
	_, err = c.CurateFact(context.Background(), "a", f.ID, "revise", "new text", f)
	if !errors.Is(err, memory.ErrMutationOutcomeUnknown) {
		t.Fatalf("missing ambiguity: %v", err)
	}
	if svc.calls.Load() != 1 {
		t.Fatalf("write called %d times", svc.calls.Load())
	}
	page, err := s.ListFacts(context.Background(), "a", "all", "", "", 10)
	if err != nil || page.Total != 2 {
		t.Fatalf("expected exactly one committed revision: %+v %v", page, err)
	}
}
