package mcp

import (
	"context"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func (b *DirectBackend) SupportsSubjectHeads() bool {
	store, ok := b.mem.Advanced().(rpc.SubjectBackend)
	return ok && store.SupportsSubjectHeads()
}

func (b *DirectBackend) SubjectIssues(ctx context.Context, namespaces []string) (memory.SubjectIssues, error) {
	reader, ok := b.mem.Advanced().(rpc.SubjectIssueReader)
	if !ok {
		return memory.SubjectIssues{}, nil
	}
	return reader.SubjectIssues(ctx, namespaces)
}

func (b *DirectBackend) PutManyReturningReceipts(ctx context.Context, namespace string, facts []memory.WriteFact) ([]memory.WriteReceipt, error) {
	if err := memory.ValidateWriteFacts(namespace, facts); err != nil {
		return nil, err
	}
	store, ok := b.mem.Advanced().(rpc.SubjectBackend)
	if !ok || !store.SupportsSubjectHeads() {
		return nil, memory.ErrSubjectUnsupported
	}
	written, err := store.PutManyReturningReceipts(ctx, namespace, facts)
	if err == nil && len(written) != 0 && b.mem.Config().AsyncConsolidate {
		if launcher, ok := b.mem.Advanced().(interface {
			LaunchAsyncConsolidate(string, memory.ConsolidateConfig)
		}); ok {
			launcher.LaunchAsyncConsolidate(namespace, b.mem.Config())
		}
	}
	return written, err
}

func (b *DirectBackend) ResolveFactTargets(ctx context.Context, namespace, target, targetType string, includeRetired bool) ([]memory.Fact, error) {
	store, ok := b.mem.Advanced().(rpc.SubjectBackend)
	if !ok || !store.SupportsSubjectHeads() {
		return nil, memory.ErrSubjectUnsupported
	}
	return store.ResolveFactTargets(ctx, namespace, target, targetType, includeRetired)
}

func (b *DirectBackend) SuggestSimilarFacts(ctx context.Context, namespace, text string) (memory.SuggestionResult, error) {
	store, ok := b.mem.Advanced().(rpc.SubjectBackend)
	if !ok || !store.SupportsSubjectHeads() {
		return memory.SuggestionResult{}, memory.ErrSubjectUnsupported
	}
	return store.SuggestSimilarFacts(ctx, namespace, text)
}
