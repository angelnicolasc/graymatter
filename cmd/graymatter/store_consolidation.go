package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

type reportingStore interface {
	ConsolidateWithReport(context.Context, string) (memory.ConsolidationReport, error)
	Close() error
}

// openReportingStore selects the owner before opening a dedicated daemon connection.
func openReportingStore(ctx context.Context) (reportingStore, error) {
	return selectReportingStore(ctx, dataDir, noDaemon, os.Getenv("GRAYMATTER_NO_DAEMON"), func(dir string) (reportingStore, error) {
		return openDirectStoreAt(dir)
	})
}

func selectReportingStore(ctx context.Context, dir string, directFlag bool, envValue string, openDirect func(string) (reportingStore, error)) (reportingStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if directFlag || envValue == "1" {
		return openDirect(dir)
	}
	return daemonReportingStore{dir: dir}, nil
}

type daemonReportingStore struct {
	dir string
}

func (d daemonReportingStore) Close() error { return nil }

func (d daemonReportingStore) ConsolidateWithReport(ctx context.Context, agentID string) (memory.ConsolidationReport, error) {
	ctx, cancel := rpc.ConsolidationContext(ctx)
	defer cancel()
	client, err := daemon.ConnectContext(ctx, d.dir)
	if err != nil {
		return memory.ConsolidationReport{}, err
	}
	defer func() { _ = client.Close() }()
	return client.Client.ConsolidateWithReport(ctx, agentID)
}

func (d *directStore) ConsolidateWithReport(ctx context.Context, agentID string) (memory.ConsolidationReport, error) {
	return d.mem.ConsolidateWithReport(ctx, agentID)
}
