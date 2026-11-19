package mcp

import (
	"context"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func (b *DirectBackend) UtilitySupported() bool {
	_, ok := b.mem.Advanced().(memory.UtilityStore)
	return ok
}

func (b *DirectBackend) SearchScoped(ctx context.Context, agentID string, queries []string, topK int, all bool, opts memory.RecallOptions) (memory.ScopedSearchResult, error) {
	store, ok := b.mem.Advanced().(memory.UtilityStore)
	if !ok {
		return memory.ScopedSearchResult{}, memory.ErrUtilityUnsupported
	}
	if topK <= 0 {
		topK = b.mem.Config().TopK
	}
	return store.SearchScoped(ctx, agentID, queries, topK, all, opts)
}

func (b *DirectBackend) PutManyReturningFacts(ctx context.Context, agentID string, facts []memory.WriteFact) ([]memory.Fact, error) {
	if err := memory.ValidateWriteFacts(agentID, facts); err != nil {
		return nil, err
	}
	store, ok := b.mem.Advanced().(memory.UtilityStore)
	if !ok {
		return nil, memory.ErrUtilityUnsupported
	}
	written, err := store.PutManyReturningFacts(ctx, agentID, facts)
	if err == nil && len(written) != 0 && b.mem.Config().AsyncConsolidate {
		if launcher, ok := b.mem.Advanced().(interface {
			LaunchAsyncConsolidate(string, memory.ConsolidateConfig)
		}); ok {
			launcher.LaunchAsyncConsolidate(agentID, b.mem.Config())
		}
	}
	return written, err
}
