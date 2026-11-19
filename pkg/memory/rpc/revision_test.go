package rpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	netrpc "net/rpc"
	"net/rpc/jsonrpc"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// A pre-R daemon advertises the older confidence and curation contracts but
// cannot guarantee atomic revision.
type preAtomicRevisionService struct {
	calls atomic.Int32
}

func (s *preAtomicRevisionService) Ping(_ *PingRequest, out *PingResponse) error {
	out.Protocol = Protocol
	out.Capabilities = []string{ConfidenceWriteV1, CurationV1, InspectionContextV1, MaintenanceOnlyV1}
	return nil
}

func (s *preAtomicRevisionService) ReviseWithOptions(_ *ReviseWithOptionsRequest, _ *ReviseWithOptionsResponse) error {
	s.calls.Add(1)
	return nil
}

func (s *preAtomicRevisionService) ReviseFactsWithOptions(_ *ReviseFactsWithOptionsRequest, _ *ReviseWithOptionsResponse) error {
	s.calls.Add(1)
	return nil
}

func (s *preAtomicRevisionService) CurateFact(_ *CurateFactRequest, _ *CurateFactResponse) error {
	s.calls.Add(1)
	return nil
}

func TestAtomicRevisionRejectsPreRDaemonBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	legacy := &preAtomicRevisionService{}
	server := netrpc.NewServer()
	if err := server.RegisterName(ServiceName, legacy); err != nil {
		t.Fatal(err)
	}
	go server.ServeCodec(jsonrpc.NewServerCodec(serverConn))
	client := &Client{rpc: netrpc.NewClientWithCodec(jsonrpc.NewClientCodec(clientConn)), conn: clientConn, callTimeout: time.Second}
	t.Cleanup(func() { _ = client.Close() })

	if _, err := client.ReviseWithOptions(ctx, "a", "old", "new", memory.WriteOptions{}); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("text revision: %v", err)
	}
	if _, err := client.ReviseFactsWithOptions(ctx, "a", "new", memory.WriteOptions{}); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("ID revision: %v", err)
	}
	if _, err := client.CurateFact(ctx, "a", "id", "revise", "new", memory.Fact{}); !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("curation revision: %v", err)
	}
	if got := legacy.calls.Load(); got != 0 {
		t.Fatalf("dispatched %d unsafe requests", got)
	}
	if _, err := client.CurateFact(ctx, "a", "id", "pin", "", memory.Fact{}); err != nil {
		t.Fatalf("legacy pin: %v", err)
	}
	if got := legacy.calls.Load(); got != 1 {
		t.Fatalf("expected a single pin request, got %d", got)
	}
}

type nonAtomicRevisionBackend struct {
	*memory.Store
	calls atomic.Int32
}

func (*nonAtomicRevisionBackend) AtomicRevisionSupported() bool { return false }

func (b *nonAtomicRevisionBackend) ReviseWithOptions(ctx context.Context, agent, target, text string, opts memory.WriteOptions) (memory.Fact, error) {
	b.calls.Add(1)
	return b.Store.ReviseWithOptions(ctx, agent, target, text, opts)
}

func TestAtomicRevisionServerRejectsNonAtomicBackendEvenForOldWire(t *testing.T) {
	ctx := context.Background()
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	backend := &nonAtomicRevisionBackend{Store: store}
	client := confidenceClientForBackend(t, backend)
	if err := store.Put(ctx, "a", "old text"); err != nil {
		t.Fatal(err)
	}
	req := struct{ AgentID, Target, Text string }{"a", "old text", "new text"}
	var resp ReviseWithOptionsResponse
	err = client.CallService(ServiceName, "ReviseWithOptions", &req, &resp, time.Second)
	if !errors.Is(mapCapabilityError(err), ErrUnsupportedCapability) || backend.calls.Load() != 0 {
		t.Fatalf("legacy wire reached non-atomic backend: calls=%d err=%v", backend.calls.Load(), err)
	}
}

func TestAtomicRevisionPreservesOldWireRequest(t *testing.T) {
	store, _, client, _ := inspectionEmbedderServer(t)
	ctx := context.Background()
	old, err := store.PutReturningFact(ctx, "a", "old text")
	if err != nil {
		t.Fatal(err)
	}
	// The previous request has no Context field.
	req := struct{ AgentID, Target, Text string }{"a", old.Text, "new text"}
	var resp ReviseWithOptionsResponse
	if err := client.CallService(ServiceName, "ReviseWithOptions", &req, &resp, time.Second); err != nil || resp.Error != "" {
		t.Fatalf("old wire request: %+v %v", resp, err)
	}
	if resp.Fact.ID == "" || resp.Fact.Text != "new text" {
		t.Fatalf("missing committed receipt: %+v", resp)
	}
	invalid := struct{ AgentID, Target, Text string }{"a", "missing source", "must not persist"}
	var invalidResp ReviseWithOptionsResponse
	if err := client.CallService(ServiceName, "ReviseWithOptions", &invalid, &invalidResp, time.Second); err == nil {
		t.Fatal("old wire request lost the RPC-level failure")
	}
	facts, err := store.List("a")
	if err != nil || len(facts) != 2 {
		t.Fatalf("canonical facts: %+v %v", facts, err)
	}
	for _, fact := range facts {
		if fact.ID == old.ID && fact.SupersededBy == resp.Fact.ID {
			return
		}
	}
	t.Fatalf("old wire request did not retire source toward %q: %+v", resp.Fact.ID, facts)
}

func TestAtomicRevisionWireCancelAcknowledgesRollback(t *testing.T) {
	store, server, client, _ := inspectionEmbedderServer(t)
	ctx := context.Background()
	old, err := store.PutReturningFact(ctx, "a", "old text")
	if err != nil {
		t.Fatal(err)
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	meta, err := newInspectionContext(callCtx)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CallService(ServiceName, "CancelInspection", &CancelInspectionRequest{Context: meta}, &CancelInspectionResponse{}, time.Second); err != nil {
		t.Fatal(err)
	}
	var resp ReviseWithOptionsResponse
	req := &ReviseFactsWithOptionsRequest{Context: meta, AgentID: "a", Text: "new text", Victims: []memory.Fact{old}}
	err = client.CallService(ServiceName, "ReviseFactsWithOptions", req, &resp, time.Second)
	fact, err := revisionResult(resp, err)
	if fact.ID != "" || !errors.Is(err, context.Canceled) || errors.Is(err, memory.ErrMutationOutcomeUnknown) {
		t.Fatalf("expected acknowledged cancellation with no commit: %+v %v", fact, err)
	}
	awaitInspectionCleanup(t, server)
	facts, err := store.List("a")
	if err != nil || !reflect.DeepEqual(facts, []memory.Fact{old}) {
		t.Fatalf("acknowledged rollback changed facts: %+v %v", facts, err)
	}
}

func TestAtomicRevisionPreservesWrappedStoreErrors(t *testing.T) {
	ctx := context.Background()
	for _, cause := range []error{memory.ErrFactChanged, memory.ErrStoreReadOnly, context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			backend := &confidenceFailureBackend{failure: fmt.Errorf("revise transaction: %w", cause)}
			client := confidenceClientForBackend(t, backend)
			fact, err := client.ReviseWithOptions(ctx, "a", "old", "new", memory.WriteOptions{})
			if fact.ID != "" || !errors.Is(err, cause) || errors.Is(err, memory.ErrMutationOutcomeUnknown) {
				t.Fatalf("wrapped error lost its cause: %+v %v", fact, err)
			}
		})
	}
}

func TestAtomicRevisionRPCCancelStopsBothWriterForms(t *testing.T) {
	for _, form := range []string{"text", "ids"} {
		t.Run(form, func(t *testing.T) {
			store, server, client, embedder := inspectionEmbedderServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			old, err := store.PutReturningFact(ctx, "a", "old policy")
			if err != nil {
				t.Fatal(err)
			}
			embedder.block.Store(true)
			done := make(chan error, 1)
			go func() {
				if form == "text" {
					_, err := client.ReviseWithOptions(ctx, "a", old.Text, "new policy", memory.WriteOptions{})
					done <- err
					return
				}
				_, err := client.ReviseFactsWithOptions(ctx, "a", "new policy", memory.WriteOptions{}, old)
				done <- err
			}()
			awaitInspectionSignal(t, embedder.started, "revision embedding")
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, memory.ErrMutationOutcomeUnknown) && !errors.Is(err, context.Canceled) {
					t.Fatalf("unexpected caller outcome: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled revision waited for its full timeout")
			}
			awaitInspectionSignal(t, embedder.canceled, "server revision cancellation")
			awaitInspectionCleanup(t, server)
			facts, err := store.List("a")
			if err != nil || !reflect.DeepEqual(facts, []memory.Fact{old}) {
				t.Fatalf("canceled revision changed facts: %+v %v", facts, err)
			}
			if err := client.Ping(); err != nil {
				t.Fatalf("cancellation closed shared connection: %v", err)
			}
		})
	}
}
