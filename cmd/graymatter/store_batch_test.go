package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/spf13/cobra"
)

type failingScopedBatchStore struct {
	*fakeStore
	batches int
}

func (s *failingScopedBatchStore) BatchRecallScoped(context.Context, string, []string, int, bool, *memory.RecallOptions) ([]memory.BatchResult, error) {
	s.batches++
	return nil, io.EOF
}

func TestScopedBatchReconnectDoesNotReplay(t *testing.T) {
	initial := &failingScopedBatchStore{fakeStore: &fakeStore{}}
	reopens := 0
	store := newReconnectingStoreAt(initial, func() (cliStore, error) {
		reopens++
		return &failingScopedBatchStore{fakeStore: &fakeStore{}}, nil
	})
	_, err := cliRecallBatch(context.Background(), store, "agent", []string{"one", "two"}, 5, recallSettings{legacy: true})
	if !errors.Is(err, io.EOF) || initial.batches != 1 || reopens != 0 {
		t.Fatalf("err=%v initial batches=%d reconnects=%d", err, initial.batches, reopens)
	}
}

func TestScopedBatchCLITransportFailureKeepsQueryRows(t *testing.T) {
	store := &failingScopedBatchStore{fakeStore: &fakeStore{}}
	previousReopen, previousNoDaemon, previousJSON := reopenStore, noDaemon, jsonOut
	t.Cleanup(func() { reopenStore, noDaemon, jsonOut = previousReopen, previousNoDaemon, previousJSON })
	reopens := 0
	reopenStore = func() (cliStore, error) {
		reopens++
		return store, nil
	}
	t.Setenv("GRAYMATTER_NO_DAEMON", "")
	noDaemon, jsonOut = false, true
	var err error
	raw := captureStdout(t, func() {
		err = runRecallBatch(&cobra.Command{}, "agent", []string{"one", "two"}, 5)
	})
	if err == nil || err.Error() != "all recall queries failed: EOF; EOF" || store.batches != 1 || reopens != 1 {
		t.Fatalf("err=%v batches=%d opens=%d", err, store.batches, reopens)
	}
	var payload struct {
		Queries  int `json:"queries"`
		PerQuery []struct {
			Query string `json:"query"`
			Error string `json:"error"`
		} `json:"per_query"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("invalid batch JSON: %v (%s)", err, raw)
	}
	if payload.Queries != 2 || len(payload.PerQuery) != 2 ||
		payload.PerQuery[0].Query != "one" || payload.PerQuery[0].Error != "EOF" ||
		payload.PerQuery[1].Query != "two" || payload.PerQuery[1].Error != "EOF" {
		t.Fatalf("batch JSON lost failed queries: %s", raw)
	}
}
