package main

import (
	"context"
	"errors"
	"net/rpc"
	"sync/atomic"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	memoryrpc "github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

type preLineageBackend struct {
	*memory.Store
	writes atomic.Int32
}

func (*preLineageBackend) SupportsAuthoritativeLineage() bool { return false }
func (*preLineageBackend) MaintenanceOnly() bool              { return true }

func (b *preLineageBackend) UpdateFact(string, memory.Fact) error {
	b.writes.Add(1)
	return nil
}

func (b *preLineageBackend) ReviseWithOptions(context.Context, string, string, string, memory.WriteOptions) (memory.Fact, error) {
	b.writes.Add(1)
	return memory.Fact{}, nil
}

func (b *preLineageBackend) ReviseFactsWithOptions(context.Context, string, string, memory.WriteOptions, ...memory.Fact) (memory.Fact, error) {
	b.writes.Add(1)
	return memory.Fact{}, nil
}

func (b *preLineageBackend) CurateFact(context.Context, string, string, string, string, memory.Fact) (memory.Fact, error) {
	b.writes.Add(1)
	return memory.Fact{}, nil
}

func listenLineageBackend(t *testing.T, backend memoryrpc.Backend) string {
	t.Helper()
	dir := t.TempDir()
	listener, cleanup, err := memoryrpc.Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	server := memoryrpc.NewServer(backend, nil)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return dir
}

func TestLineageL08RejectsPreLineageDaemonAtStartupAndReconnect(t *testing.T) {
	core, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	ctx := context.Background()
	fact, err := core.PutReturningFact(ctx, "a", "existing policy")
	if err != nil {
		t.Fatal(err)
	}
	legacy := &preLineageBackend{Store: core}
	oldDir := listenLineageBackend(t, legacy)
	newDir := listenLineageBackend(t, core)

	for _, connection := range []string{"startup", "reconnect"} {
		t.Run(connection, func(t *testing.T) {
			connectOld := func() (cliStore, error) {
				client, err := daemon.Connect(oldDir)
				if err != nil {
					return nil, err
				}
				return daemonStore{Client: client}, nil
			}
			initial := connectOld
			if connection == "reconnect" {
				initial = func() (cliStore, error) {
					client, err := daemon.Connect(newDir)
					if err != nil {
						return nil, err
					}
					if supported, known := client.Capability(memoryrpc.AuthoritativeLineageV1); !known || !supported {
						_ = client.Close()
						t.Fatal("initial peer did not negotiate lineage")
					}
					_ = client.Close()
					return daemonStore{Client: client}, nil
				}
			}
			for _, operation := range []struct {
				name string
				run  func(*reconnectingStore) error
			}{
				{"full update", func(s *reconnectingStore) error { return s.UpdateFact("a", fact) }},
				{"text revision", func(s *reconnectingStore) error {
					_, err := s.ReviseWithOptions(ctx, "a", fact.Text, "revised policy", memory.WriteOptions{})
					return err
				}},
				{"fact revision", func(s *reconnectingStore) error {
					_, err := s.ReviseFactsWithOptions(ctx, "a", "revised policy", memory.WriteOptions{}, fact)
					return err
				}},
				{"curation revision", func(s *reconnectingStore) error {
					_, err := s.CurateFact(ctx, "a", fact.ID, "revise", "revised policy", fact)
					return err
				}},
				{"lineage inspection", func(s *reconnectingStore) error {
					_, err := s.InspectLineage(ctx, "a", fact.ID, "")
					return err
				}},
				{"lineage export", func(s *reconnectingStore) error {
					_, err := s.ExportLineage(ctx, "a")
					return err
				}},
			} {
				t.Run(operation.name, func(t *testing.T) {
					handle, err := initial()
					if err != nil {
						t.Fatal(err)
					}
					s := newReconnectingStoreAt(handle, connectOld)
					t.Cleanup(func() { _ = s.Close() })
					if err := operation.run(s); !errors.Is(err, memoryrpc.ErrLineageUnsupported) {
						t.Fatalf("peer incompatibility: %v", err)
					}
					if facts, err := s.List("a"); err != nil || len(facts) != 1 || facts[0].ID != fact.ID {
						t.Fatalf("legacy read control: %+v, %v", facts, err)
					}
				})
			}
		})
	}
	if writes := legacy.writes.Load(); writes != 0 {
		t.Fatalf("old peer received %d writes", writes)
	}
}

type lostLineageUpdate struct {
	*fakeStore
	updates atomic.Int32
}

func (s *lostLineageUpdate) UpdateFact(string, memory.Fact) error {
	s.updates.Add(1)
	return rpc.ErrShutdown
}

func TestLineageL08DoesNotReplayDispatchedFullUpdate(t *testing.T) {
	initial := &lostLineageUpdate{fakeStore: &fakeStore{}}
	s := newReconnectingStoreAt(initial, func() (cliStore, error) {
		t.Fatal("full update was replayed after an ambiguous response")
		return nil, nil
	})
	if err := s.UpdateFact("a", memory.Fact{ID: "head"}); !errors.Is(err, memory.ErrMutationOutcomeUnknown) {
		t.Fatalf("lost write acknowledgement: %v", err)
	}
	if initial.updates.Load() != 1 {
		t.Fatalf("initial update dispatches: %d", initial.updates.Load())
	}
}
