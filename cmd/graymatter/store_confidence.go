package main

import (
	"context"
	"errors"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

// These capabilities stay outside cliStore so older fakes remain valid.
var (
	_ rpc.ConfidenceWriter       = daemonStore{}
	_ rpc.ConfidenceWriter       = (*directStore)(nil)
	_ rpc.ConfidenceWriter       = (*reconnectingStore)(nil)
	_ rpc.ConfidenceRecaller     = daemonStore{}
	_ rpc.ConfidenceRecaller     = (*directStore)(nil)
	_ rpc.ConfidenceRecaller     = (*reconnectingStore)(nil)
	_ rpc.ConfidenceLifecycle    = daemonStore{}
	_ rpc.ConfidenceLifecycle    = (*directStore)(nil)
	_ rpc.ConfidenceLifecycle    = (*reconnectingStore)(nil)
	_ rpc.SharedConfidenceWriter = daemonStore{}
	_ rpc.SharedConfidenceWriter = (*directStore)(nil)
	_ rpc.SharedConfidenceWriter = (*reconnectingStore)(nil)
)

func (d *directStore) DefaultConfidenceWeight() float64 { return d.mem.Config().ConfidenceWeight }
func (r *reconnectingStore) DefaultConfidenceWeight() float64 {
	if provider, ok := r.snapshot().(rpc.DefaultConfidenceProvider); ok {
		return provider.DefaultConfidenceWeight()
	}
	return memory.DefaultConfidenceWeight
}

func (d *directStore) PutSharedWithOptionsReturningFact(ctx context.Context, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	store, ok := d.store.(rpc.SharedConfidenceWriter)
	if !ok {
		return memory.Fact{}, memory.ErrConfidenceUnsupported
	}
	return store.PutSharedWithOptionsReturningFact(ctx, text, opts)
}

func resolveDirectRecallOptions(opts memory.RecallOptions, defaultWeight float64) (memory.RecallOptions, bool) {
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

func (d *directStore) afterConfidenceWrite(agentID string) {
	if cfg := d.mem.Config(); cfg.AsyncConsolidate {
		if store, ok := d.store.(interface {
			LaunchAsyncConsolidate(string, memory.ConsolidateConfig)
		}); ok {
			store.LaunchAsyncConsolidate(agentID, cfg)
		}
	}
}

func (d *directStore) PutWithOptionsReturningFact(ctx context.Context, agentID, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	store, ok := d.store.(rpc.ConfidenceWriter)
	if !ok {
		return memory.Fact{}, memory.ErrConfidenceUnsupported
	}
	fact, err := store.PutWithOptionsReturningFact(ctx, agentID, text, opts)
	if fact.ID != "" {
		d.afterConfidenceWrite(agentID)
	}
	return fact, err
}

func (d *directStore) ReviseWithOptions(ctx context.Context, agentID, target, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	store, ok := d.store.(rpc.ConfidenceWriter)
	if !ok {
		return memory.Fact{}, memory.ErrConfidenceUnsupported
	}
	fact, err := store.ReviseWithOptions(ctx, agentID, target, text, opts)
	if fact.ID != "" {
		d.afterConfidenceWrite(agentID)
	}
	return fact, err
}

func (d *directStore) ReviseFactsWithOptions(ctx context.Context, agentID, text string, opts memory.WriteOptions, victims ...memory.Fact) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	store, ok := d.store.(rpc.ConfidenceWriter)
	if !ok {
		return memory.Fact{}, memory.ErrConfidenceUnsupported
	}
	fact, err := store.ReviseFactsWithOptions(ctx, agentID, text, opts, victims...)
	if fact.ID != "" {
		d.afterConfidenceWrite(agentID)
	}
	return fact, err
}

func (d *directStore) RecallWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallResult, error) {
	if err := opts.Validate(); err != nil {
		return memory.RecallResult{}, err
	}
	store, ok := d.store.(rpc.ConfidenceRecaller)
	if !ok {
		return memory.RecallResult{}, memory.ErrConfidenceUnsupported
	}
	if topK <= 0 {
		topK = d.mem.Config().TopK
	}
	opts, legacyMetadata := resolveDirectRecallOptions(opts, d.DefaultConfidenceWeight())
	result, err := store.RecallWithOptions(ctx, agentID, query, topK, opts)
	if legacyMetadata && result.Retrieval != nil && result.Retrieval.VectorFallback == "" {
		result.Retrieval = nil
	}
	return result, err
}

func (d *directStore) RecallExplainWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallExplainResult, error) {
	if err := opts.Validate(); err != nil {
		return memory.RecallExplainResult{}, err
	}
	store, ok := d.store.(rpc.ConfidenceRecaller)
	if !ok {
		return memory.RecallExplainResult{}, memory.ErrConfidenceUnsupported
	}
	if topK <= 0 {
		topK = d.mem.Config().TopK
	}
	opts, legacyMetadata := resolveDirectRecallOptions(opts, d.DefaultConfidenceWeight())
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

func (d *directStore) RecallAllWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallResult, error) {
	if err := opts.Validate(); err != nil {
		return memory.RecallResult{}, err
	}
	store, ok := d.store.(rpc.ConfidenceRecaller)
	if !ok {
		return memory.RecallResult{}, memory.ErrConfidenceUnsupported
	}
	if topK <= 0 {
		topK = d.mem.Config().TopK
	}
	opts, legacyMetadata := resolveDirectRecallOptions(opts, d.DefaultConfidenceWeight())
	result, err := store.RecallAllWithOptions(ctx, agentID, query, topK, opts)
	if legacyMetadata && result.Retrieval != nil && result.Retrieval.VectorFallback == "" {
		result.Retrieval = nil
	}
	return result, err
}

func (d *directStore) Retire(agentID string, victims ...memory.Fact) error {
	store, ok := d.store.(rpc.ConfidenceLifecycle)
	if !ok {
		return memory.ErrConfidenceUnsupported
	}
	return store.Retire(agentID, victims...)
}

func (d *directStore) SetPinned(agentID string, pinned bool, victims ...memory.Fact) error {
	store, ok := d.store.(rpc.ConfidenceLifecycle)
	if !ok {
		return memory.ErrConfidenceUnsupported
	}
	return store.SetPinned(agentID, pinned, victims...)
}

// confidenceWrite probes/reconnects before sending the mutation. Once sent,
// any failure is returned to the caller: its commit status may be unknown.
// Only the following request can recover; no mutation is replayed blindly.
func (r *reconnectingStore) confidenceWrite(fn func(cliStore) error) error {
	if err := r.Ready(); err != nil {
		return err
	}
	return fn(r.snapshot())
}

func (r *reconnectingStore) PutWithOptionalIdentity(ctx context.Context, agentID, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	if err := ctx.Err(); err != nil {
		return memory.Fact{}, err
	}
	var fact memory.Fact
	err := r.mutate(func(s cliStore) error {
		var err error
		switch writer := s.(type) {
		case interface {
			PutWithOptionalIdentity(context.Context, string, string, memory.WriteOptions) (memory.Fact, error)
		}:
			fact, err = writer.PutWithOptionalIdentity(ctx, agentID, text, opts)
			return err
		case rpc.ConfidenceWriter:
			fact, err = writer.PutWithOptionsReturningFact(ctx, agentID, text, opts)
		default:
			if opts.Confidence != nil {
				return memory.ErrConfidenceUnsupported
			}
			if writer, ok := s.(returningFactStore); ok {
				fact, err = writer.PutReturningFact(ctx, agentID, text)
			} else {
				return s.Remember(ctx, agentID, text)
			}
		}
		if err == nil && fact.ID == "" {
			return errors.New("write returned no committed fact identity")
		}
		return err
	})
	if errors.Is(err, memory.ErrMutationOutcomeUnknown) {
		return memory.Fact{}, err
	}
	return fact, err
}

func (r *reconnectingStore) PutWithOptionsReturningFact(ctx context.Context, agentID, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	var fact memory.Fact
	err := r.confidenceWrite(func(s cliStore) error {
		writer, ok := s.(rpc.ConfidenceWriter)
		if !ok {
			return memory.ErrConfidenceUnsupported
		}
		var err error
		fact, err = writer.PutWithOptionsReturningFact(ctx, agentID, text, opts)
		return err
	})
	return fact, err
}

func (r *reconnectingStore) PutSharedWithOptionsReturningFact(ctx context.Context, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	var fact memory.Fact
	err := r.confidenceWrite(func(s cliStore) error {
		writer, ok := s.(rpc.SharedConfidenceWriter)
		if !ok {
			return memory.ErrConfidenceUnsupported
		}
		var err error
		fact, err = writer.PutSharedWithOptionsReturningFact(ctx, text, opts)
		return err
	})
	return fact, err
}

func (r *reconnectingStore) ReviseWithOptions(ctx context.Context, agentID, target, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	var fact memory.Fact
	err := r.confidenceWrite(func(s cliStore) error {
		writer, ok := s.(rpc.ConfidenceWriter)
		if !ok {
			return memory.ErrConfidenceUnsupported
		}
		var err error
		fact, err = writer.ReviseWithOptions(ctx, agentID, target, text, opts)
		return err
	})
	return fact, err
}

func (r *reconnectingStore) ReviseFactsWithOptions(ctx context.Context, agentID, text string, opts memory.WriteOptions, victims ...memory.Fact) (memory.Fact, error) {
	if err := opts.Validate(); err != nil {
		return memory.Fact{}, err
	}
	var fact memory.Fact
	err := r.confidenceWrite(func(s cliStore) error {
		writer, ok := s.(rpc.ConfidenceWriter)
		if !ok {
			return memory.ErrConfidenceUnsupported
		}
		var err error
		fact, err = writer.ReviseFactsWithOptions(ctx, agentID, text, opts, victims...)
		return err
	})
	return fact, err
}

func (r *reconnectingStore) RecallWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallResult, error) {
	if err := opts.Validate(); err != nil {
		return memory.RecallResult{}, err
	}
	if err := r.Ready(); err != nil {
		return memory.RecallResult{}, err
	}
	var result memory.RecallResult
	err := r.do(func(s cliStore) error {
		store, ok := s.(rpc.ConfidenceRecaller)
		if !ok {
			return memory.ErrConfidenceUnsupported
		}
		var err error
		result, err = store.RecallWithOptions(ctx, agentID, query, topK, opts)
		return err
	})
	return result, err
}

func (r *reconnectingStore) RecallExplainWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallExplainResult, error) {
	if err := opts.Validate(); err != nil {
		return memory.RecallExplainResult{}, err
	}
	if err := r.Ready(); err != nil {
		return memory.RecallExplainResult{}, err
	}
	var result memory.RecallExplainResult
	err := r.do(func(s cliStore) error {
		store, ok := s.(rpc.ConfidenceRecaller)
		if !ok {
			return memory.ErrConfidenceUnsupported
		}
		var err error
		result, err = store.RecallExplainWithOptions(ctx, agentID, query, topK, opts)
		return err
	})
	return result, err
}

func (r *reconnectingStore) RecallAllWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallResult, error) {
	if err := opts.Validate(); err != nil {
		return memory.RecallResult{}, err
	}
	if err := r.Ready(); err != nil {
		return memory.RecallResult{}, err
	}
	var result memory.RecallResult
	err := r.do(func(s cliStore) error {
		store, ok := s.(rpc.ConfidenceRecaller)
		if !ok {
			return memory.ErrConfidenceUnsupported
		}
		var err error
		result, err = store.RecallAllWithOptions(ctx, agentID, query, topK, opts)
		return err
	})
	return result, err
}

func (r *reconnectingStore) Retire(agentID string, victims ...memory.Fact) error {
	return r.confidenceWrite(func(s cliStore) error {
		store, ok := s.(rpc.ConfidenceLifecycle)
		if !ok {
			return memory.ErrConfidenceUnsupported
		}
		return store.Retire(agentID, victims...)
	})
}

func (r *reconnectingStore) SetPinned(agentID string, pinned bool, victims ...memory.Fact) error {
	return r.confidenceWrite(func(s cliStore) error {
		store, ok := s.(rpc.ConfidenceLifecycle)
		if !ok {
			return memory.ErrConfidenceUnsupported
		}
		return store.SetPinned(agentID, pinned, victims...)
	})
}
