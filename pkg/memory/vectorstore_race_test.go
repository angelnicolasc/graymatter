package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestChromemVectorStore_ConcurrentEnsureCollection is the regression test for
// the unsynchronised collections map (H-03).
//
// Before the fix this failed two ways: `go test -race` flagged the read/write
// pair in EnsureCollection, and often the runtime's own map check killed the
// process outright with "concurrent map read and map write" — a fatal error,
// not a panic, so nothing downstream could recover from it. The daemon reaches
// this code from every RPC goroutine, so a fatal here is a DoS for every
// client sharing the store.
//
// The shape matters: writers keep growing the map with fresh collections while
// readers hammer the lookup path, which is what Store.Put and the background
// reconciler do to each other in production.
func TestChromemVectorStore_ConcurrentEnsureCollection(t *testing.T) {
	vs, err := newChromemVectorStore(t.TempDir())
	if err != nil {
		t.Fatalf("new chromem store: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	const (
		writers        = 16
		readers        = 16
		perWriter      = 16
		readIterations = 20000
	)

	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	errs := make(chan error, writers*perWriter+readers)

	for w := 0; w < writers; w++ {
		done.Add(1)
		go func(w int) {
			defer done.Done()
			start.Wait() // release everyone at once, to maximise overlap
			for i := 0; i < perWriter; i++ {
				name := fmt.Sprintf("agent-%d-%d", w, i)
				if err := vs.EnsureCollection(name); err != nil {
					errs <- fmt.Errorf("ensure %s: %w", name, err)
					return
				}
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		done.Add(1)
		go func(r int) {
			defer done.Done()
			start.Wait()
			for i := 0; i < readIterations; i++ {
				// Hits the read-hit path once the writers have created it, and
				// the create path exactly once — either way it is a concurrent
				// map access against the writers above.
				if err := vs.EnsureCollection(fmt.Sprintf("agent-%d-0", r)); err != nil {
					errs <- fmt.Errorf("reader ensure: %w", err)
					return
				}
			}
		}(r)
	}

	start.Done()
	done.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if got := len(vs.collections); got != writers*perWriter {
		t.Errorf("collections = %d, want %d", got, writers*perWriter)
	}
}

// TestChromemVectorStore_ConcurrentAddAndQuery covers the other half of H-03:
// AddDocument and Query used to look the collection up in the bare map after
// calling EnsureCollection, so they raced with each other too — the exact
// interleaving reached from Store.Put and the background reconciler.
func TestChromemVectorStore_ConcurrentAddAndQuery(t *testing.T) {
	vs, err := newChromemVectorStore(t.TempDir())
	if err != nil {
		t.Fatalf("new chromem store: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	ctx := context.Background()
	const goroutines = 32
	emb := []float32{1, 0, 0}

	// Readers need a collection with something in it: chromem rejects a query
	// for more results than the collection holds.
	const seeded = "seeded"
	if err := vs.AddDocument(ctx, seeded, "seed", "hello", emb, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	errs := make(chan error, goroutines*2)

	for i := 0; i < goroutines; i++ {
		done.Add(2)
		go func(i int) {
			defer done.Done()
			start.Wait()
			col := fmt.Sprintf("writer-%d", i)
			if err := vs.AddDocument(ctx, col, fmt.Sprintf("id-%d", i), "hello", emb, nil); err != nil {
				errs <- fmt.Errorf("add %s: %w", col, err)
			}
		}(i)
		go func(i int) {
			defer done.Done()
			start.Wait()
			if _, err := vs.Query(ctx, seeded, emb, 1); err != nil {
				errs <- fmt.Errorf("query: %w", err)
			}
		}(i)
	}

	start.Done()
	done.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
