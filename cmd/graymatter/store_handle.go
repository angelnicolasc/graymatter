package main

import (
	"context"
	"fmt"
	"os"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// cliStore is the persistence surface CLI commands and the TUI consume.
//
// There are exactly two implementations and one place that picks between
// them (openStore): the daemon client (default — concurrent-safe, issue #8)
// and the in-process direct store (--no-daemon / GRAYMATTER_NO_DAEMON=1,
// for debugging and air-gapped inspection). Commands never open bbolt
// themselves and never branch on the mode.
type cliStore interface {
	// Core memory surface.
	Remember(ctx context.Context, agentID, text string) error
	PutShared(ctx context.Context, text string) error
	RecallDefault(ctx context.Context, agentID, query string) ([]string, error)
	Recall(ctx context.Context, agentID, query string, topK int) ([]string, error)
	RecallShared(ctx context.Context, query string, topK int) ([]string, error)
	RecallAll(ctx context.Context, agentID, query string, topK int) ([]string, error)
	List(agentID string) ([]memory.Fact, error)
	ListAgents() ([]string, error)
	Stats(agentID string) (memory.MemoryStats, error)
	Delete(agentID, factID string) error
	UpdateFact(agentID string, f memory.Fact) error
	// Consolidate runs with the store owner's own policy: through the daemon
	// that is the daemon's configuration, and clients cannot override it.
	Consolidate(ctx context.Context, agentID string) error

	// Host-level surface (checkpoints, sessions, KG, audit, tokens).
	CheckpointSave(cp session.Checkpoint) (session.Checkpoint, error)
	CheckpointLoad(agentID, checkpointID string) (*session.Checkpoint, error)
	CheckpointResume(agentID string) (*session.Checkpoint, error)
	CheckpointList(agentID string) ([]session.Checkpoint, error)
	SessionsList() ([]harness.HarnessSession, error)
	SessionKill(id string) error
	SessionResolve(agentID, sessionID string) (string, error)
	KGNodes() ([]kg.Node, error)
	KGLink(from, to, relation string) error
	ExportGraphObsidian(outDir string) error
	AuditWrite(e audit.Entry) error
	TokenSummary(days int) (harness.TokenUsageSummary, error)
	TokenRecord(agent, model string, input, output, cacheRead, cacheWrite uint64) error
	SessionSave(hs harness.HarnessSession) error

	// IsReadOnly reports a degraded direct open (--no-daemon while another
	// process holds the write lock). Always false through the daemon.
	IsReadOnly() bool

	// Ready reports whether the store answers right now, reconnecting first if
	// the connection died. Long-lived callers use it to tell "serving" apart
	// from "serving nothing but errors".
	Ready() error

	Close() error
}

// daemonStore adapts *daemon.Client to cliStore (only the bits the client
// doesn't already satisfy structurally).
type daemonStore struct {
	*daemon.Client
}

func (d daemonStore) IsReadOnly() bool { return false }

// ExportGraphObsidian runs host-side: the graph lives on the daemon's bbolt
// handle, so only the destination path crosses the wire.
func (d daemonStore) ExportGraphObsidian(outDir string) error {
	return d.Client.KGExportObsidian(outDir)
}

// Ready uses the RPC protocol ping, which is the cheapest call that proves the
// daemon is both reachable and speaking a compatible protocol.
func (d daemonStore) Ready() error { return d.Ping() }

// compile-time checks: both implementations satisfy cliStore.
var (
	_ cliStore = daemonStore{}
	_ cliStore = (*directStore)(nil)
)

// openStore is the single entry point commands use to reach the store.
//
// Daemon handles come wrapped so a daemon restart does not strand whoever is
// holding one. Short-lived commands never notice; the TUI and the REST server
// hold a handle for as long as they run, and used to die permanently when the
// daemon went away.
//
// The direct store is deliberately not wrapped: there is no connection to lose
// in-process, and a "reconnect" would re-open bbolt while the first handle is
// still live, which the single-writer lock would refuse.
func openStore() (cliStore, error) {
	if noDaemon || os.Getenv("GRAYMATTER_NO_DAEMON") == "1" {
		return openDirectStore()
	}
	s, err := reopenStore()
	if err != nil {
		return nil, err
	}
	return newReconnectingStore(s), nil
}

// --- direct (in-process) implementation --------------------------------------

// directStore wraps a Memory + raw db for --no-daemon operation. It is also
// what unit tests construct to exercise command logic without a daemon.
type directStore struct {
	mem   *graymatter.Memory
	store graymatter.AdvancedStore
}

func openDirectStore() (*directStore, error) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dataDir
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		return nil, err
	}
	store := mem.Advanced()
	if store == nil {
		_ = mem.Close()
		return nil, fmt.Errorf("store not initialised")
	}
	ds := &directStore{mem: mem, store: store}
	if os.Getenv("GRAYMATTER_KG") == "1" {
		if err := maybeWireKG(store); err != nil {
			_ = mem.Close()
			return nil, err
		}
	}
	return ds, nil
}

// maybeWireKG turns on knowledge-graph auto-population for an already-open
// advanced store: the graph opens on the same bbolt handle and the regex
// extractor feeds consolidation. Opt-in via GRAYMATTER_KG=1 (direct mode) or
// daemon --kg.
func maybeWireKG(adv graymatter.AdvancedStore) error {
	g, err := kg.Open(adv.DB())
	if err != nil {
		return fmt.Errorf("knowledge graph: %w", err)
	}
	graphAdapter := kg.NewGraphAdapter(g)
	extractor := kg.NewExtractorAdapter(kg.NewExtractor(kg.ExtractorConfig{}))
	adv.SetKG(graphAdapter, extractor)
	return nil
}

func (d *directStore) Remember(ctx context.Context, agentID, text string) error {
	return d.mem.Remember(ctx, agentID, text)
}

func (d *directStore) PutShared(ctx context.Context, text string) error {
	return d.mem.RememberShared(ctx, text)
}

func (d *directStore) RecallDefault(ctx context.Context, agentID, query string) ([]string, error) {
	return d.mem.Recall(ctx, agentID, query)
}

func (d *directStore) Recall(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	if topK <= 0 {
		return d.mem.Recall(ctx, agentID, query)
	}
	return d.store.Recall(ctx, agentID, query, topK)
}

func (d *directStore) RecallShared(ctx context.Context, query string, topK int) ([]string, error) {
	if topK <= 0 {
		return d.mem.RecallShared(ctx, query)
	}
	return d.store.RecallShared(ctx, query, topK)
}

func (d *directStore) RecallAll(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	if topK <= 0 {
		return d.mem.RecallAll(ctx, agentID, query)
	}
	if ra, ok := d.store.(interface {
		RecallAll(ctx context.Context, agentID, query string, topK int) ([]string, error)
	}); ok {
		return ra.RecallAll(ctx, agentID, query, topK)
	}
	return d.mem.RecallAll(ctx, agentID, query)
}

func (d *directStore) List(agentID string) ([]memory.Fact, error) { return d.store.List(agentID) }
func (d *directStore) ListAgents() ([]string, error)              { return d.store.ListAgents() }
func (d *directStore) Stats(agentID string) (memory.MemoryStats, error) {
	return d.store.Stats(agentID)
}
func (d *directStore) Delete(agentID, factID string) error { return d.store.Delete(agentID, factID) }
func (d *directStore) Consolidate(ctx context.Context, agentID string) error {
	return d.mem.Consolidate(ctx, agentID)
}

// Ready round-trips to bbolt. In-process there is no connection to lose, but
// the caller should not have to know which implementation it holds.
func (d *directStore) Ready() error {
	_, err := d.store.ListAgents()
	return err
}
func (d *directStore) UpdateFact(agentID string, f memory.Fact) error {
	return d.store.UpdateFact(agentID, f)
}

func (d *directStore) CheckpointSave(cp session.Checkpoint) (session.Checkpoint, error) {
	return session.Save(d.store.DB(), cp)
}

func (d *directStore) CheckpointLoad(agentID, checkpointID string) (*session.Checkpoint, error) {
	return session.Load(d.store.DB(), agentID, checkpointID)
}

func (d *directStore) CheckpointResume(agentID string) (*session.Checkpoint, error) {
	return session.Resume(d.store.DB(), agentID)
}

func (d *directStore) CheckpointList(agentID string) ([]session.Checkpoint, error) {
	return session.List(d.store.DB(), agentID)
}

func (d *directStore) SessionsList() ([]harness.HarnessSession, error) {
	return harness.ListSessionsDB(d.store.DB())
}

func (d *directStore) SessionKill(id string) error {
	return harness.KillSessionDB(d.store.DB(), id)
}

func (d *directStore) SessionResolve(agentID, sessionID string) (string, error) {
	return harness.ResolveSessionIDDB(d.store.DB(), agentID, sessionID)
}

func (d *directStore) KGNodes() ([]kg.Node, error) {
	g, err := kg.Open(d.store.DB())
	if err != nil {
		return nil, nil // no graph yet: empty, not an error
	}
	return g.AllNodes()
}

func (d *directStore) KGLink(from, to, relation string) error {
	g, err := kg.Open(d.store.DB())
	if err != nil {
		return fmt.Errorf("knowledge graph not available: %w", err)
	}
	return kg.NewGraphAdapter(g).LinkNodes(from, to, relation)
}

// ExportGraphObsidian writes the graph's entity notes and canvas into outDir.
// Direct mode opens the graph on this process's bbolt handle.
func (d *directStore) ExportGraphObsidian(outDir string) error {
	g, err := kg.Open(d.store.DB())
	if err != nil {
		return fmt.Errorf("knowledge graph not available: %w", err)
	}
	return g.ExportObsidian(outDir)
}

func (d *directStore) AuditWrite(e audit.Entry) error {
	return audit.Write(d.store.DB(), e)
}

func (d *directStore) TokenSummary(days int) (harness.TokenUsageSummary, error) {
	return harness.LoadTokenUsageSummary(d.store.DB(), days)
}

func (d *directStore) TokenRecord(agent, model string, input, output, cacheRead, cacheWrite uint64) error {
	return harness.RecordTokenUsage(d.store.DB(), agent, model, input, output, cacheRead, cacheWrite)
}

func (d *directStore) SessionSave(hs harness.HarnessSession) error {
	return harness.SaveSessionDB(d.store.DB(), hs)
}

func (d *directStore) IsReadOnly() bool { return d.store.IsReadOnly() }

func (d *directStore) Close() error { return d.mem.Close() }
