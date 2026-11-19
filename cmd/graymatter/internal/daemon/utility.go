package daemon

import (
	"context"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func (b rememberBackend) UtilitySupported() bool {
	_, ok := b.AdvancedStore.(memory.UtilityStore)
	return ok
}

func (b rememberBackend) SearchScoped(ctx context.Context, agentID string, queries []string, topK int, all bool, opts memory.RecallOptions) (memory.ScopedSearchResult, error) {
	store, ok := b.AdvancedStore.(memory.UtilityStore)
	if !ok {
		return memory.ScopedSearchResult{}, memory.ErrUtilityUnsupported
	}
	if topK <= 0 {
		topK = b.mem.Config().TopK
	}
	resolved, legacyMetadata := resolveBackendRecallOptions(opts, b.DefaultConfidenceWeight())
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

func (b rememberBackend) PutManyReturningFacts(ctx context.Context, agentID string, facts []memory.WriteFact) ([]memory.Fact, error) {
	if err := memory.ValidateWriteFacts(agentID, facts); err != nil {
		return nil, err
	}
	store, ok := b.AdvancedStore.(memory.UtilityStore)
	if !ok {
		return nil, memory.ErrUtilityUnsupported
	}
	written, err := store.PutManyReturningFacts(ctx, agentID, facts)
	if err == nil && len(written) != 0 {
		b.afterConfidenceWrite(agentID)
	}
	return written, err
}
