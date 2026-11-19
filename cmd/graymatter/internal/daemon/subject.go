package daemon

import (
	"context"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func (b rememberBackend) SupportsSubjectHeads() bool {
	store, ok := b.AdvancedStore.(rpc.SubjectBackend)
	return ok && store.SupportsSubjectHeads()
}

func (b rememberBackend) SubjectIssues(ctx context.Context, namespaces []string) (memory.SubjectIssues, error) {
	reader, ok := b.AdvancedStore.(rpc.SubjectIssueReader)
	if !ok {
		return memory.SubjectIssues{}, nil
	}
	return reader.SubjectIssues(ctx, namespaces)
}

func (b rememberBackend) SubjectIssuesByNamespace(ctx context.Context) (map[string]memory.SubjectIssues, error) {
	reader, ok := b.AdvancedStore.(interface {
		SubjectIssuesByNamespace(context.Context) (map[string]memory.SubjectIssues, error)
	})
	if !ok {
		return nil, nil
	}
	return reader.SubjectIssuesByNamespace(ctx)
}

func (b rememberBackend) PutManyReturningReceipts(ctx context.Context, namespace string, facts []memory.WriteFact) ([]memory.WriteReceipt, error) {
	if err := memory.ValidateWriteFacts(namespace, facts); err != nil {
		return nil, err
	}
	store, ok := b.AdvancedStore.(rpc.SubjectBackend)
	if !ok || !store.SupportsSubjectHeads() {
		return nil, memory.ErrSubjectUnsupported
	}
	written, err := store.PutManyReturningReceipts(ctx, namespace, facts)
	if err == nil && len(written) != 0 {
		b.afterConfidenceWrite(namespace)
	}
	return written, err
}

func (b rememberBackend) ResolveFactTargets(ctx context.Context, namespace, target, targetType string, includeRetired bool) ([]memory.Fact, error) {
	store, ok := b.AdvancedStore.(rpc.SubjectBackend)
	if !ok || !store.SupportsSubjectHeads() {
		return nil, memory.ErrSubjectUnsupported
	}
	return store.ResolveFactTargets(ctx, namespace, target, targetType, includeRetired)
}

func (b rememberBackend) SuggestSimilarFacts(ctx context.Context, namespace, text string) (memory.SuggestionResult, error) {
	store, ok := b.AdvancedStore.(rpc.SubjectBackend)
	if !ok || !store.SupportsSubjectHeads() {
		return memory.SuggestionResult{}, memory.ErrSubjectUnsupported
	}
	return store.SuggestSimilarFacts(ctx, namespace, text)
}
