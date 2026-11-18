package rpc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type identityLegacyBackend struct {
	Backend
	puts atomic.Int32
}

func (*identityLegacyBackend) MaintenanceOnly() bool { return true }

func (b *identityLegacyBackend) Put(ctx context.Context, agent, text string) error {
	b.puts.Add(1)
	return b.Backend.Put(ctx, agent, text)
}

type identityReturningBackend struct {
	*identityLegacyBackend
	writer returningFactWriter
}

func (b *identityReturningBackend) PutReturningFact(ctx context.Context, agent, text string) (memory.Fact, error) {
	return b.writer.PutReturningFact(ctx, agent, text)
}

func TestR4IdentityRPCPolicyAndLegacyNegotiation(t *testing.T) {
	ctx := context.Background()
	dir, unknown := startLegacyProtocol(t)
	c := dialT(t, dir)
	if f, err := c.PutWithOptionalIdentity(ctx, "a", "must not land", memory.WriteOptions{}); !errors.Is(err, ErrMaintenancePolicyUnsupported) || f.ID != "" || unknown.calls.Load() != 0 {
		t.Fatalf("unverified daemon received a write: %+v / %v, calls=%d", f, err, unknown.calls.Load())
	}

	for _, hasIdentity := range []bool{false, true} {
		name := "safe-write-only"
		if hasIdentity {
			name = "safe-returning"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := memory.Open(memory.StoreConfig{DataDir: dir, StrictWrite: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			legacy := &identityLegacyBackend{Backend: store}
			var backend Backend = legacy
			if hasIdentity {
				backend = &identityReturningBackend{identityLegacyBackend: legacy, writer: store}
			}
			s := NewServer(backend, nil)
			listener, cleanup, err := Listen(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(cleanup)
			go func() { _ = s.Serve(listener) }()
			t.Cleanup(s.Stop)
			c := dialT(t, dir)
			const text = "confirmed legacy-compatible fact"
			label := "verified"
			f, err := c.PutWithOptionalIdentity(ctx, "a", "explicit confidence is unavailable", memory.WriteOptions{Confidence: &label})
			if !errors.Is(err, ErrUnsupportedCapability) || f.ID != "" {
				t.Fatalf("explicit options silently fell back: %+v / %v", f, err)
			}
			facts, err := c.List("a")
			if err != nil || len(facts) != 0 {
				t.Fatalf("failed option changed facts: %+v / %v", facts, err)
			}
			f, err = c.PutWithOptionalIdentity(ctx, "a", text, memory.WriteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			facts, err = c.List("a")
			if err != nil || len(facts) != 1 || facts[0].Text != text || facts[0].Confidence != "" {
				t.Fatalf("legacy write state: %+v / %v", facts, err)
			}
			if hasIdentity {
				if f.ID != facts[0].ID || legacy.puts.Load() != 0 {
					t.Fatalf("known returning path: receipt=%+v row=%+v legacy puts=%d", f, facts[0], legacy.puts.Load())
				}
			} else if f.ID != "" || legacy.puts.Load() != 1 {
				t.Fatalf("write-only path claimed an identity or wrote twice: %+v, writes=%d", f, legacy.puts.Load())
			}
		})
	}
}

func TestR4IdentityRPCConcurrentClientsGetOwnCommittedRows(t *testing.T) {
	dir, _ := startServer(t, "")
	type result struct {
		agent string
		text  string
		fact  memory.Fact
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, agent := range []string{"project-one", "project-two"} {
		client := dialT(t, dir)
		go func() {
			<-start
			text := "specific evidence for " + agent
			fact, err := client.PutWithOptionalIdentity(context.Background(), agent, text, memory.WriteOptions{})
			results <- result{agent, text, fact, err}
		}()
	}
	close(start)
	read := dialT(t, dir)
	ids := make(map[string]bool)
	for i := 0; i < 2; i++ {
		out := <-results
		facts, err := read.List(out.agent)
		if out.err != nil || err != nil || len(facts) != 1 || out.fact.ID == "" || out.fact.ID != facts[0].ID ||
			out.fact.AgentID != out.agent || out.fact.Text != out.text || facts[0].Text != out.text || ids[out.fact.ID] {
			t.Errorf("client received another write: %+v, stored rows=%+v / %v", out, facts, err)
		}
		ids[out.fact.ID] = true
	}
}
