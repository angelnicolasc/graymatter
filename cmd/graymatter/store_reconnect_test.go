package main

import (
	"context"
	"errors"
	"io"
	netrpc "net/rpc"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeStore is a cliStore that fails with a connection error until it is
// replaced. Only the methods the REST server uses are meaningful; the rest of
// the interface is satisfied by the embedded nil and never called.
type fakeStore struct {
	cliStore
	dead   atomic.Bool
	calls  atomic.Int64
	closed atomic.Bool
}

func (f *fakeStore) Remember(ctx context.Context, agentID, text string) error {
	f.calls.Add(1)
	if f.dead.Load() {
		return netrpc.ErrShutdown
	}
	return nil
}

func (f *fakeStore) ListAgents() ([]string, error) {
	f.calls.Add(1)
	if f.dead.Load() {
		return nil, netrpc.ErrShutdown
	}
	return []string{"a"}, nil
}

func (f *fakeStore) Close() error {
	f.closed.Store(true)
	return nil
}

func TestConnDead(t *testing.T) {
	for _, err := range []error{netrpc.ErrShutdown, io.EOF, io.ErrUnexpectedEOF} {
		if !connDead(err) {
			t.Errorf("connDead(%v) = false, want true", err)
		}
	}
	// A store error means the call reached the daemon and failed on its
	// merits. Retrying it could duplicate a write that already landed.
	for _, err := range []error{nil, errors.New("agent not found")} {
		if connDead(err) {
			t.Errorf("connDead(%v) = true, want false", err)
		}
	}
}

func TestReconnectingStore_RecoversFromDeadConnection(t *testing.T) {
	dead := &fakeStore{}
	dead.dead.Store(true)
	fresh := &fakeStore{}

	prev := reopenStore
	reopenStore = func() (cliStore, error) { return fresh, nil }
	t.Cleanup(func() { reopenStore = prev })

	rs := newReconnectingStore(dead)
	if err := rs.Remember(context.Background(), "a", "b"); err != nil {
		t.Fatalf("Remember should have recovered, got %v", err)
	}
	if !dead.closed.Load() {
		t.Error("the dead handle was not closed on reconnect")
	}
	if rs.snapshot() != cliStore(fresh) {
		t.Error("store was not swapped for the reconnected handle")
	}
}

func TestReconnectingStore_ReturnsRealErrors(t *testing.T) {
	// A reconnect must not be attempted for an ordinary failure.
	s := &fakeStore{}
	reopened := false
	prev := reopenStore
	reopenStore = func() (cliStore, error) { reopened = true; return s, nil }
	t.Cleanup(func() { reopenStore = prev })

	rs := newReconnectingStore(s)
	want := errors.New("boom")
	err := rs.do(func(cliStore) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
	if reopened {
		t.Error("reconnected on an error that was not a dead connection")
	}
}

func TestReconnectingStore_ReportsFailedReconnect(t *testing.T) {
	dead := &fakeStore{}
	dead.dead.Store(true)

	prev := reopenStore
	reopenStore = func() (cliStore, error) { return nil, errors.New("daemon unreachable") }
	t.Cleanup(func() { reopenStore = prev })

	rs := newReconnectingStore(dead)
	err := rs.Remember(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected an error when the reconnect fails")
	}
	// The original connection error stays wrapped so callers can still match it.
	if !errors.Is(err, netrpc.ErrShutdown) {
		t.Errorf("original error lost: %v", err)
	}
}

// TestReconnectingStore_ConcurrentRedial is aimed at the race detector: many
// goroutines hit a dead connection at once and must end up sharing a single
// replacement rather than each opening their own.
func TestReconnectingStore_ConcurrentRedial(t *testing.T) {
	dead := &fakeStore{}
	dead.dead.Store(true)
	fresh := &fakeStore{}

	var reopens atomic.Int64
	prev := reopenStore
	reopenStore = func() (cliStore, error) {
		reopens.Add(1)
		return fresh, nil
	}
	t.Cleanup(func() { reopenStore = prev })

	rs := newReconnectingStore(dead)

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = rs.Remember(context.Background(), "a", "b")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	if got := reopens.Load(); got != 1 {
		t.Errorf("reopened %d times, want exactly 1", got)
	}
}

func TestReconnectingStore_ReadyProbesTheStore(t *testing.T) {
	s := &fakeStore{}
	rs := newReconnectingStore(s)

	if err := rs.Ready(); err != nil {
		t.Fatalf("Ready on a live store: %v", err)
	}
	if s.calls.Load() == 0 {
		t.Error("Ready did not make a round-trip; it would report healthy for a store it never touched")
	}

	// A store that has gone away and cannot be replaced must report unready.
	s.dead.Store(true)
	prev := reopenStore
	reopenStore = func() (cliStore, error) { return nil, errors.New("gone") }
	t.Cleanup(func() { reopenStore = prev })

	if err := rs.Ready(); err == nil {
		t.Error("Ready returned nil for an unreachable store")
	}
}
