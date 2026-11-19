package main

import (
	"context"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

var (
	_ rpc.CitationBackend = daemonStore{}
	_ rpc.CitationBackend = (*directStore)(nil)
	_ rpc.CitationBackend = (*reconnectingStore)(nil)
)

func (d *directStore) SupportsCitationEvidence() bool {
	store, ok := d.store.(rpc.CitationBackend)
	return ok && store.SupportsCitationEvidence()
}

func (d *directStore) FetchCitation(ctx context.Context, namespace, factID, hash string) (memory.CitationResult, error) {
	store, ok := d.store.(rpc.CitationBackend)
	if !ok {
		return memory.CitationResult{}, memory.ErrCitationUnsupported
	}
	return store.FetchCitation(ctx, namespace, factID, hash)
}

func (d *directStore) RecallForHooks(ctx context.Context, namespace, query string, topK int) ([]string, error) {
	store, ok := d.store.(rpc.CitationBackend)
	if !ok {
		return nil, memory.ErrCitationUnsupported
	}
	if topK <= 0 {
		topK = d.mem.Config().TopK
	}
	return store.RecallForHooks(ctx, namespace, query, topK)
}

func (r *reconnectingStore) SupportsCitationEvidence() bool {
	store, ok := r.snapshot().(rpc.CitationBackend)
	return ok && store.SupportsCitationEvidence()
}

func (r *reconnectingStore) FetchCitation(ctx context.Context, namespace, factID, hash string) (memory.CitationResult, error) {
	if err := ctx.Err(); err != nil {
		return memory.CitationResult{}, err
	}
	var result memory.CitationResult
	err := r.do(func(current cliStore) error {
		store, ok := current.(rpc.CitationBackend)
		if !ok {
			return memory.ErrCitationUnsupported
		}
		var err error
		result, err = store.FetchCitation(ctx, namespace, factID, hash)
		return err
	})
	return result, err
}

func (r *reconnectingStore) RecallForHooks(ctx context.Context, namespace, query string, topK int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result []string
	err := r.do(func(current cliStore) error {
		store, ok := current.(rpc.CitationBackend)
		if !ok {
			return memory.ErrCitationUnsupported
		}
		var err error
		result, err = store.RecallForHooks(ctx, namespace, query, topK)
		return err
	})
	return result, err
}

func hookRecallProjection(ctx context.Context, store cliStore, namespace, query string, topK int, shared bool) ([]string, error) {
	if backend, ok := store.(interface {
		RecallForHooks(context.Context, string, string, int) ([]string, error)
	}); ok {
		if shared {
			namespace = memory.SharedAgentID
		}
		return backend.RecallForHooks(ctx, namespace, query, topK)
	}
	// Compatibility with third-party adapters lacking the optional projection.
	// An adapter that implements it must surface its errors, never fall back.
	if shared {
		return store.RecallShared(ctx, query, topK)
	}
	return store.Recall(ctx, namespace, query, topK)
}
