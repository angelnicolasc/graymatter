package main

import (
	"context"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

var (
	_ memory.ScopedBatchRecaller = (*directStore)(nil)
	_ memory.ScopedBatchRecaller = daemonStore{}
	_ memory.ScopedBatchRecaller = (*reconnectingStore)(nil)
)

func (d *directStore) BatchRecallScoped(ctx context.Context, agentID string, queries []string, topK int, all bool, opts *memory.RecallOptions) ([]memory.BatchResult, error) {
	recaller, ok := d.store.(memory.ScopedBatchRecaller)
	if !ok {
		return nil, memory.ErrBatchRecallUnsupported
	}
	if topK <= 0 {
		topK = d.mem.Config().TopK
	}
	return recaller.BatchRecallScoped(ctx, agentID, queries, topK, all, opts)
}

func (r *reconnectingStore) BatchRecallScoped(ctx context.Context, agentID string, queries []string, topK int, all bool, opts *memory.RecallOptions) ([]memory.BatchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.Ready(); err != nil {
		return nil, err
	}
	recaller, ok := r.snapshot().(memory.ScopedBatchRecaller)
	if !ok {
		return nil, memory.ErrBatchRecallUnsupported
	}
	// Reconnect before dispatch. Alias activity may already have committed
	// when a batch response is lost.
	return recaller.BatchRecallScoped(ctx, agentID, queries, topK, all, opts)
}
