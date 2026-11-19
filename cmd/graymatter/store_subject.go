package main

import (
	"context"
	"errors"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

var (
	_ rpc.SubjectBackend = daemonStore{}
	_ rpc.SubjectBackend = (*directStore)(nil)
	_ rpc.SubjectBackend = (*reconnectingStore)(nil)
)

func (d *directStore) SupportsSubjectHeads() bool {
	store, ok := d.store.(rpc.SubjectBackend)
	return ok && store.SupportsSubjectHeads()
}

func (d *directStore) SubjectIssues(ctx context.Context, namespaces []string) (memory.SubjectIssues, error) {
	reader, ok := d.store.(rpc.SubjectIssueReader)
	if !ok {
		return memory.SubjectIssues{}, nil
	}
	return reader.SubjectIssues(ctx, namespaces)
}

func (r *reconnectingStore) SubjectIssues(ctx context.Context, namespaces []string) (memory.SubjectIssues, error) {
	var issues memory.SubjectIssues
	err := r.do(func(store cliStore) error {
		reader, ok := store.(rpc.SubjectIssueReader)
		if !ok {
			return nil
		}
		var err error
		issues, err = reader.SubjectIssues(ctx, namespaces)
		return err
	})
	return issues, err
}

func (r *readOnlyTUIStore) SubjectIssues(ctx context.Context, namespaces []string) (memory.SubjectIssues, error) {
	reader, ok := r.cliStore.(rpc.SubjectIssueReader)
	if !ok {
		return memory.SubjectIssues{}, nil
	}
	return reader.SubjectIssues(ctx, namespaces)
}

func readSubjectIssues(ctx context.Context, store cliStore, namespaces ...string) (*memory.SubjectIssues, error) {
	reader, ok := store.(rpc.SubjectIssueReader)
	if !ok {
		return nil, nil
	}
	issues, err := reader.SubjectIssues(ctx, namespaces)
	if err != nil || issues.Total == 0 {
		return nil, err
	}
	return &issues, nil
}

func (d *directStore) PutManyReturningReceipts(ctx context.Context, namespace string, facts []memory.WriteFact) ([]memory.WriteReceipt, error) {
	if err := memory.ValidateWriteFacts(namespace, facts); err != nil {
		return nil, err
	}
	store, ok := d.store.(rpc.SubjectBackend)
	if !ok || !store.SupportsSubjectHeads() {
		return nil, memory.ErrSubjectUnsupported
	}
	written, err := store.PutManyReturningReceipts(ctx, namespace, facts)
	if err == nil && len(written) != 0 {
		d.afterConfidenceWrite(namespace)
	}
	return written, err
}

func (d *directStore) ResolveFactTargets(ctx context.Context, namespace, target, targetType string, includeRetired bool) ([]memory.Fact, error) {
	store, ok := d.store.(rpc.SubjectBackend)
	if !ok || !store.SupportsSubjectHeads() {
		return nil, memory.ErrSubjectUnsupported
	}
	return store.ResolveFactTargets(ctx, namespace, target, targetType, includeRetired)
}

func (d *directStore) SuggestSimilarFacts(ctx context.Context, namespace, text string) (memory.SuggestionResult, error) {
	store, ok := d.store.(rpc.SubjectBackend)
	if !ok || !store.SupportsSubjectHeads() {
		return memory.SuggestionResult{}, memory.ErrSubjectUnsupported
	}
	return store.SuggestSimilarFacts(ctx, namespace, text)
}

func (r *reconnectingStore) SupportsSubjectHeads() bool {
	store, ok := r.snapshot().(rpc.SubjectBackend)
	return ok && store.SupportsSubjectHeads()
}

func (r *reconnectingStore) PutManyReturningReceipts(ctx context.Context, namespace string, inputs []memory.WriteFact) ([]memory.WriteReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inputs = append([]memory.WriteFact(nil), inputs...)
	for i := range inputs {
		inputs[i].Options = inputs[i].Options.Clone()
	}
	if err := memory.ValidateWriteFacts(namespace, inputs); err != nil {
		return nil, err
	}
	var result []memory.WriteReceipt
	err := r.mutate(func(current cliStore) error {
		store, ok := current.(rpc.SubjectBackend)
		if !ok || !store.SupportsSubjectHeads() {
			return memory.ErrSubjectUnsupported
		}
		var err error
		result, err = store.PutManyReturningReceipts(ctx, namespace, inputs)
		return err
	})
	if errors.Is(err, memory.ErrMutationOutcomeUnknown) {
		return nil, err
	}
	return result, err
}

func (r *reconnectingStore) ResolveFactTargets(ctx context.Context, namespace, target, targetType string, includeRetired bool) ([]memory.Fact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result []memory.Fact
	err := r.do(func(current cliStore) error {
		store, ok := current.(rpc.SubjectBackend)
		if !ok || !store.SupportsSubjectHeads() {
			return memory.ErrSubjectUnsupported
		}
		var err error
		result, err = store.ResolveFactTargets(ctx, namespace, target, targetType, includeRetired)
		return err
	})
	return result, err
}

func (r *reconnectingStore) SuggestSimilarFacts(ctx context.Context, namespace, text string) (memory.SuggestionResult, error) {
	if err := ctx.Err(); err != nil {
		return memory.SuggestionResult{}, err
	}
	var result memory.SuggestionResult
	err := r.do(func(current cliStore) error {
		store, ok := current.(rpc.SubjectBackend)
		if !ok || !store.SupportsSubjectHeads() {
			return memory.ErrSubjectUnsupported
		}
		var err error
		result, err = store.SuggestSimilarFacts(ctx, namespace, text)
		return err
	})
	return result, err
}
