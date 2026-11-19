package daemon

import (
	"context"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func (b rememberBackend) SupportsCitationEvidence() bool {
	store, ok := b.AdvancedStore.(rpc.CitationBackend)
	return ok && store.SupportsCitationEvidence()
}
func (b rememberBackend) FetchCitation(ctx context.Context, namespace, factID, contentHash string) (memory.CitationResult, error) {
	store, ok := b.AdvancedStore.(rpc.CitationBackend)
	if !ok {
		return memory.CitationResult{}, memory.ErrCitationUnsupported
	}
	return store.FetchCitation(ctx, namespace, factID, contentHash)
}
func (b rememberBackend) RecallForHooks(ctx context.Context, namespace, query string, topK int) ([]string, error) {
	store, ok := b.AdvancedStore.(rpc.CitationBackend)
	if !ok {
		return nil, memory.ErrCitationUnsupported
	}
	if topK <= 0 {
		topK = b.mem.Config().TopK
	}
	return store.RecallForHooks(ctx, namespace, query, topK)
}

func (b rememberBackend) RecallForHooksWithOptions(ctx context.Context, namespace, query string, topK int, opts memory.RecallOptions) ([]string, error) {
	store, ok := b.AdvancedStore.(interface {
		RecallForHooksWithOptions(context.Context, string, string, int, memory.RecallOptions) ([]string, error)
	})
	if !ok {
		return nil, memory.ErrCitationUnsupported
	}
	if topK <= 0 {
		topK = b.mem.Config().TopK
	}
	return store.RecallForHooksWithOptions(ctx, namespace, query, topK, opts)
}
