package main

import (
	"context"
	"errors"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

var (
	_ memory.UtilityStore = daemonStore{}
	_ memory.UtilityStore = (*directStore)(nil)
	_ memory.UtilityStore = (*reconnectingStore)(nil)
)

func (d *directStore) UtilitySupported() bool { _, ok := d.store.(memory.UtilityStore); return ok }

func (d *directStore) SearchScoped(ctx context.Context, agentID string, queries []string, topK int, all bool, opts memory.RecallOptions) (memory.ScopedSearchResult, error) {
	store, ok := d.store.(memory.UtilityStore)
	if !ok {
		return memory.ScopedSearchResult{}, memory.ErrUtilityUnsupported
	}
	if topK <= 0 {
		topK = d.mem.Config().TopK
	}
	resolved, legacyMetadata := resolveDirectRecallOptions(opts, d.DefaultConfidenceWeight())
	result, err := store.SearchScoped(ctx, agentID, queries, topK, all, resolved)
	if legacyMetadata && result.Retrieval != nil && result.Retrieval.VectorFallback == "" {
		result.Retrieval = nil
	}
	if legacyMetadata {
		for i := range result.Facts {
			if result.Facts[i].Receipt != nil {
				result.Facts[i].Receipt.Ranking = nil
			}
		}
	}
	return result, err
}

func (d *directStore) PutManyReturningFacts(ctx context.Context, agentID string, facts []memory.WriteFact) ([]memory.Fact, error) {
	if err := memory.ValidateWriteFacts(agentID, facts); err != nil {
		return nil, err
	}
	store, ok := d.store.(memory.UtilityStore)
	if !ok {
		return nil, memory.ErrUtilityUnsupported
	}
	written, err := store.PutManyReturningFacts(ctx, agentID, facts)
	if err == nil && len(written) != 0 {
		d.afterConfidenceWrite(agentID)
	}
	return written, err
}

func (r *reconnectingStore) SearchScoped(ctx context.Context, agentID string, queries []string, topK int, all bool, opts memory.RecallOptions) (memory.ScopedSearchResult, error) {
	if err := ctx.Err(); err != nil {
		return memory.ScopedSearchResult{}, err
	}
	var out memory.ScopedSearchResult
	err := r.do(func(s cliStore) error {
		store, ok := s.(memory.UtilityStore)
		if !ok {
			return memory.ErrUtilityUnsupported
		}
		var err error
		out, err = store.SearchScoped(ctx, agentID, queries, topK, all, opts)
		return err
	})
	return out, err
}

func (r *reconnectingStore) PutManyReturningFacts(ctx context.Context, agentID string, facts []memory.WriteFact) ([]memory.Fact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := memory.ValidateWriteFacts(agentID, facts); err != nil {
		return nil, err
	}
	var out []memory.Fact
	err := r.mutate(func(s cliStore) error {
		store, ok := s.(memory.UtilityStore)
		if !ok {
			return memory.ErrUtilityUnsupported
		}
		var err error
		out, err = store.PutManyReturningFacts(ctx, agentID, facts)
		return err
	})
	if errors.Is(err, memory.ErrMutationOutcomeUnknown) {
		return nil, err
	}
	return out, err
}
