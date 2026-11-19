package daemon

import (
	"context"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func (b rememberBackend) ConfidenceCapabilities() []string {
	var capabilities []string
	if _, ok := b.AdvancedStore.(rpc.ConfidenceWriter); ok {
		capabilities = append(capabilities, rpc.ConfidenceWriteV1)
	}
	if _, ok := b.AdvancedStore.(rpc.ConfidenceRecaller); ok {
		capabilities = append(capabilities, rpc.ConfidenceRecallV1)
	}
	if _, ok := b.AdvancedStore.(rpc.ConfidenceLifecycle); ok {
		capabilities = append(capabilities, rpc.ConfidenceLifecycleV1)
	}
	if _, ok := b.AdvancedStore.(rpc.SharedConfidenceWriter); ok {
		capabilities = append(capabilities, rpc.ConfidenceSharedWriteV1)
	}
	return capabilities
}

func (b rememberBackend) DefaultConfidenceWeight() float64 { return b.mem.Config().ConfidenceWeight }

func (b rememberBackend) PutSharedWithOptionsReturningFact(ctx context.Context, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	store, ok := b.AdvancedStore.(rpc.SharedConfidenceWriter)
	if !ok {
		return memory.Fact{}, memory.ErrConfidenceUnsupported
	}
	// RememberShared deliberately does not schedule consolidation.
	return store.PutSharedWithOptionsReturningFact(ctx, text, opts)
}

func resolveBackendRecallOptions(opts memory.RecallOptions, defaultWeight float64) (memory.RecallOptions, bool) {
	weight := defaultWeight
	if opts.ConfidenceWeight != nil {
		weight = *opts.ConfidenceWeight
	}
	legacyMetadata := !opts.Requested() && weight == 0
	if opts.ConfidenceWeight == nil {
		opts.ConfidenceWeight = &weight
	}
	return opts, legacyMetadata
}

func (b rememberBackend) afterConfidenceWrite(agentID string) {
	if cfg := b.mem.Config(); cfg.AsyncConsolidate {
		if store, ok := b.AdvancedStore.(interface {
			LaunchAsyncConsolidate(string, memory.ConsolidateConfig)
		}); ok {
			store.LaunchAsyncConsolidate(agentID, cfg)
		}
	}
}

func (b rememberBackend) PutWithOptionsReturningFact(ctx context.Context, agentID, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	store, ok := b.AdvancedStore.(rpc.ConfidenceWriter)
	if !ok {
		return memory.Fact{}, memory.ErrConfidenceUnsupported
	}
	fact, err := store.PutWithOptionsReturningFact(ctx, agentID, text, opts)
	if fact.ID != "" {
		b.afterConfidenceWrite(agentID)
	}
	return fact, err
}

func (b rememberBackend) ReviseWithOptions(ctx context.Context, agentID, target, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	store, ok := b.AdvancedStore.(rpc.ConfidenceWriter)
	if !ok {
		return memory.Fact{}, memory.ErrConfidenceUnsupported
	}
	fact, err := store.ReviseWithOptions(ctx, agentID, target, text, opts)
	if fact.ID != "" {
		b.afterConfidenceWrite(agentID)
	}
	return fact, err
}

func (b rememberBackend) ReviseFactsWithOptions(ctx context.Context, agentID, text string, opts memory.WriteOptions, victims ...memory.Fact) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	store, ok := b.AdvancedStore.(rpc.ConfidenceWriter)
	if !ok {
		return memory.Fact{}, memory.ErrConfidenceUnsupported
	}
	fact, err := store.ReviseFactsWithOptions(ctx, agentID, text, opts, victims...)
	if fact.ID != "" {
		b.afterConfidenceWrite(agentID)
	}
	return fact, err
}

func (b rememberBackend) RecallWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallResult, error) {
	if err := opts.Validate(); err != nil {
		return memory.RecallResult{}, err
	}
	store, ok := b.AdvancedStore.(rpc.ConfidenceRecaller)
	if !ok {
		return memory.RecallResult{}, memory.ErrConfidenceUnsupported
	}
	if topK <= 0 {
		topK = b.mem.Config().TopK
	}
	opts, legacyMetadata := resolveBackendRecallOptions(opts, b.DefaultConfidenceWeight())
	result, err := store.RecallWithOptions(ctx, agentID, query, topK, opts)
	if legacyMetadata && result.Retrieval != nil && result.Retrieval.VectorFallback == "" {
		result.Retrieval = nil
	}
	return result, err
}

func (b rememberBackend) RecallExplainWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallExplainResult, error) {
	if err := opts.Validate(); err != nil {
		return memory.RecallExplainResult{}, err
	}
	store, ok := b.AdvancedStore.(rpc.ConfidenceRecaller)
	if !ok {
		return memory.RecallExplainResult{}, memory.ErrConfidenceUnsupported
	}
	if topK <= 0 {
		topK = b.mem.Config().TopK
	}
	opts, legacyMetadata := resolveBackendRecallOptions(opts, b.DefaultConfidenceWeight())
	result, err := store.RecallExplainWithOptions(ctx, agentID, query, topK, opts)
	if legacyMetadata {
		if result.Retrieval != nil && result.Retrieval.VectorFallback == "" {
			result.Retrieval = nil
		}
		for i := range result.Receipts {
			result.Receipts[i].Ranking = nil
		}
	}
	return result, err
}

func (b rememberBackend) RecallAllWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallResult, error) {
	if err := opts.Validate(); err != nil {
		return memory.RecallResult{}, err
	}
	store, ok := b.AdvancedStore.(rpc.ConfidenceRecaller)
	if !ok {
		return memory.RecallResult{}, memory.ErrConfidenceUnsupported
	}
	if topK <= 0 {
		topK = b.mem.Config().TopK
	}
	opts, legacyMetadata := resolveBackendRecallOptions(opts, b.DefaultConfidenceWeight())
	result, err := store.RecallAllWithOptions(ctx, agentID, query, topK, opts)
	if legacyMetadata && result.Retrieval != nil && result.Retrieval.VectorFallback == "" {
		result.Retrieval = nil
	}
	return result, err
}

func (b rememberBackend) Retire(agentID string, victims ...memory.Fact) error {
	store, ok := b.AdvancedStore.(rpc.ConfidenceLifecycle)
	if !ok {
		return memory.ErrConfidenceUnsupported
	}
	return store.Retire(agentID, victims...)
}

func (b rememberBackend) SetPinned(agentID string, pinned bool, victims ...memory.Fact) error {
	store, ok := b.AdvancedStore.(rpc.ConfidenceLifecycle)
	if !ok {
		return memory.ErrConfidenceUnsupported
	}
	return store.SetPinned(agentID, pinned, victims...)
}
