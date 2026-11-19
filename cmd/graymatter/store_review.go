package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

type reviewReader interface {
	ReviewList(context.Context, string) ([]memory.ReviewRecord, error)
	ReviewInspect(context.Context, string, string) (memory.ReviewRecord, error)
	Close() error
}

type reviewWriter interface {
	ReviewAnalyze(context.Context, string, string, string) ([]memory.ReviewRecord, error)
	ReviewApply(context.Context, string, string, memory.ReviewAction) error
	Close() error
}

func existingDiagnosticsDaemon(dir string) *daemon.Client {
	if noDaemon || os.Getenv("GRAYMATTER_NO_DAEMON") == "1" {
		return nil
	}
	client, err := daemon.ConnectNoSpawn(dir)
	if err != nil {
		return nil
	}
	return client
}

type statusReader interface {
	StoreOverview() (*daemon.StoreOverviewResponse, error)
	KGState() (*daemon.KGStateResponse, error)
	TokenSummary(int) (harness.TokenUsageSummary, error)
	List(string) ([]memory.Fact, error)
	Close() error
}

type statusDirectReader struct {
	directStore
	db *memory.Store
}

func (d *statusDirectReader) Close() error { return d.db.Close() }

func openStatusReader() (statusReader, string, error) {
	info, err := os.Stat(filepath.Join(dataDir, "gray.db"))
	if os.IsNotExist(err) {
		return nil, "in-process", errDoctorNoDatabase
	}
	if err != nil {
		return nil, "in-process", err
	}
	if !info.Mode().IsRegular() {
		return nil, "in-process", fmt.Errorf("gray.db is not a regular file (%s)", info.Mode())
	}
	if client := existingDiagnosticsDaemon(dataDir); client != nil {
		return client, "via daemon", nil
	}
	store, err := openDiagnosticMemory(dataDir)
	if err != nil {
		return nil, "in-process", err
	}
	return &statusDirectReader{directStore: directStore{store: store}, db: store}, "in-process", nil
}

func openReviewReader() (reviewReader, error) {
	if err := doctorDBLeaf(dataDir); err != nil {
		return nil, err
	}
	if client := existingDiagnosticsDaemon(dataDir); client != nil {
		return client, nil
	}
	return openDiagnosticMemory(dataDir)
}

func openReviewWriter() (reviewWriter, error) {
	if err := doctorDBLeaf(dataDir); err != nil {
		return nil, err
	}
	store, err := openStore()
	if err != nil {
		return nil, err
	}
	writer, ok := store.(reviewWriter)
	if !ok {
		_ = store.Close()
		return nil, rpc.ErrReviewUnsupported
	}
	return writer, nil
}

func (d *directStore) ReviewJournalSupported() bool {
	_, ok := d.store.(rpc.ReviewBackend)
	return ok
}

func (d *directStore) ReviewAnalyze(ctx context.Context, ns, provider, model string) ([]memory.ReviewRecord, error) {
	backend, ok := d.store.(rpc.ReviewBackend)
	if !ok {
		return nil, rpc.ErrReviewUnsupported
	}
	options, err := memory.ReviewOptionsFromConfig(d.mem.Config(), provider, model)
	if err != nil {
		return nil, err
	}
	return backend.ReviewAnalyze(ctx, ns, options)
}

func (d *directStore) ReviewApply(ctx context.Context, ns, id string, action memory.ReviewAction) error {
	backend, ok := d.store.(rpc.ReviewBackend)
	if !ok {
		return rpc.ErrReviewUnsupported
	}
	return backend.ReviewApply(ctx, ns, id, action)
}

func (r *reconnectingStore) ReviewJournalSupported() bool {
	store, ok := r.snapshot().(interface{ ReviewJournalSupported() bool })
	return ok && store.ReviewJournalSupported()
}

func (r *reconnectingStore) ReviewAnalyze(ctx context.Context, ns, provider, model string) ([]memory.ReviewRecord, error) {
	var records []memory.ReviewRecord
	err := r.mutate(func(store cliStore) error {
		writer, ok := store.(reviewWriter)
		if !ok {
			return rpc.ErrReviewUnsupported
		}
		var err error
		records, err = writer.ReviewAnalyze(ctx, ns, provider, model)
		return err
	})
	return records, err
}

func (r *reconnectingStore) ReviewApply(ctx context.Context, ns, id string, action memory.ReviewAction) error {
	return r.mutate(func(store cliStore) error {
		writer, ok := store.(reviewWriter)
		if !ok {
			return rpc.ErrReviewUnsupported
		}
		return writer.ReviewApply(ctx, ns, id, action)
	})
}

func reviewOverviewText(overview *memory.ReviewOverview) string {
	if overview == nil {
		return "unavailable on this daemon"
	}
	return fmt.Sprintf("%d pending · %d stale · %d rejected · %d unresolved · %d resolved · %d dismissed · %d logical bytes",
		overview.Pending, overview.Stale, overview.Rejected, overview.Unresolved, overview.Resolved, overview.Dismissed, overview.LogicalBytes)
}
