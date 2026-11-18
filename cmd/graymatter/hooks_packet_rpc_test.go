package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

type blockedPacketBackend struct {
	// The historical interface deliberately hides promoted optional methods.
	// Embedding Store would let the confidence-aware RPC server bypass Recall.
	rpc.Backend
	rpc.SubjectBackend
	release, entered, returned chan struct{}
	started, finished          sync.Once
	calls                      atomic.Int32
}

func (b *blockedPacketBackend) Recall(context.Context, string, string, int) ([]string, error) {
	b.calls.Add(1)
	b.started.Do(func() { close(b.entered) })
	<-b.release
	b.finished.Do(func() { close(b.returned) })
	return []string{"late project fact"}, nil
}

func (b *blockedPacketBackend) RecallShared(context.Context, string, int) ([]string, error) {
	b.calls.Add(1)
	return []string{"unexpected shared call"}, nil
}

// The hook now requests a version-aware projection. Keep the fixture's
// blocking seam explicit instead of promoting the real Store implementation.
func (*blockedPacketBackend) SupportsCitationEvidence() bool { return true }

func (*blockedPacketBackend) FetchCitation(context.Context, string, string, string) (memory.CitationResult, error) {
	return memory.CitationResult{}, memory.ErrFactNotFound
}

func (b *blockedPacketBackend) RecallForHooks(ctx context.Context, namespace, query string, k int) ([]string, error) {
	if namespace == memory.SharedAgentID {
		return b.RecallShared(ctx, query, k)
	}
	return b.Recall(ctx, namespace, query, k)
}

func (b *blockedPacketBackend) RecallForHooksWithOptions(ctx context.Context, namespace, query string, k int, options memory.RecallOptions) ([]string, error) {
	if options.ConfidenceWeight == nil || *options.ConfidenceWeight != 0 {
		return nil, fmt.Errorf("hook RPC did not preserve legacy zero confidence weight")
	}
	return b.RecallForHooks(ctx, namespace, query, k)
}

type blockedConfidencePacketBackend struct {
	*blockedPacketBackend
	recaller rpc.ConfidenceRecaller
}

func (b *blockedConfidencePacketBackend) RecallWithOptions(ctx context.Context, agent, query string, k int, opts memory.RecallOptions) (memory.RecallResult, error) {
	if opts.ConfidenceWeight == nil || *opts.ConfidenceWeight != 0 {
		return memory.RecallResult{}, fmt.Errorf("legacy hook RPC did not bind zero confidence weight")
	}
	var facts []string
	var err error
	if agent == memory.SharedAgentID {
		facts, err = b.RecallShared(ctx, query, k)
	} else {
		facts, err = b.Recall(ctx, agent, query, k)
	}
	return memory.RecallResult{Facts: facts}, err
}

func (b *blockedConfidencePacketBackend) RecallExplainWithOptions(ctx context.Context, agent, query string, k int, opts memory.RecallOptions) (memory.RecallExplainResult, error) {
	return b.recaller.RecallExplainWithOptions(ctx, agent, query, k, opts)
}

func (b *blockedConfidencePacketBackend) RecallAllWithOptions(ctx context.Context, agent, query string, k int, opts memory.RecallOptions) (memory.RecallResult, error) {
	return b.recaller.RecallAllWithOptions(ctx, agent, query, k, opts)
}

func TestHookPacketRPC_DeadlineClosesConnectionWithoutRetry(t *testing.T) {
	for _, confidenceAware := range []bool{false, true} {
		t.Run(fmt.Sprintf("confidence_capability_%v", confidenceAware), func(t *testing.T) {
			testHookPacketRPCDeadline(t, confidenceAware)
		})
	}
}

func testHookPacketRPCDeadline(t *testing.T, confidenceAware bool) {
	t.Helper()
	dir := t.TempDir()
	store, err := memory.Open(memory.StoreConfig{DataDir: dir, StrictWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	backend := &blockedPacketBackend{Backend: store, SubjectBackend: store, release: make(chan struct{}), entered: make(chan struct{}), returned: make(chan struct{})}
	var rpcBackend rpc.Backend = backend
	if confidenceAware {
		rpcBackend = &blockedConfidencePacketBackend{blockedPacketBackend: backend, recaller: store}
	}
	if _, supported := rpcBackend.(rpc.ConfidenceRecaller); supported != confidenceAware {
		t.Fatal("fixture exposed unintended optional retrieval capabilities")
	}
	server := rpc.NewServer(rpcBackend, nil)
	listener, cleanup, err := rpc.Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	defer func() {
		close(backend.release)
		select {
		case <-backend.entered:
			select {
			case <-backend.returned:
			case <-time.After(2 * time.Second):
				t.Error("blocked backend did not finish after release")
			}
		default:
		}
	}()
	client, err := rpc.Dial(rpc.DialOptions{DataDir: dir, PingOnDial: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	var reconnects atomic.Int32
	remote := newReconnectingStoreAt(daemonStore{Client: &daemon.Client{Client: client}}, func() (cliStore, error) {
		reconnects.Add(1)
		return nil, fmt.Errorf("unexpected hook redial")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	type outcome struct {
		block   string
		receipt hookPacketReceipt
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		block, receipt, err := hookRecallLexicalBlock(ctx, remote, "example", "project fact")
		done <- outcome{block: block, receipt: receipt, err: err}
	}()
	var result outcome
	completed := false
	select {
	case <-backend.entered:
	case result = <-done:
		// Both channels can be ready when the test goroutine is rescheduled.
		// Verify actual entry rather than depending on select's random choice.
		select {
		case <-backend.entered:
			completed = true
		default:
			t.Fatalf("hook returned before entering blocked RPC backend: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hook never entered RPC backend")
	}
	if !completed {
		select {
		case result = <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("hook did not stop at its deadline")
		}
	}
	block, receipt, err := result.block, result.receipt, result.err
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("hook used default RPC timeout instead of its deadline: %v", elapsed)
	}
	if err == nil || block != "" || receipt.Project.Effective != "none" || receipt.Shared.Effective != "none" {
		t.Fatalf("expired RPC result: block=%q receipt=%+v error=%v", block, receipt, err)
	}
	if backend.calls.Load() != 1 || reconnects.Load() != 0 {
		t.Fatalf("work continued after deadline: calls=%d reconnects=%d", backend.calls.Load(), reconnects.Load())
	}
	if err := client.Ping(); err == nil {
		t.Fatal("timed-out connection remained usable")
	}
}
