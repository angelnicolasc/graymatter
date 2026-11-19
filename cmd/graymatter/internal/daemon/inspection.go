package daemon

import (
	"context"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func (b rememberBackend) AtomicRevisionSupported() bool {
	store, ok := b.AdvancedStore.(interface{ AtomicRevisionSupported() bool })
	return ok && store.AtomicRevisionSupported()
}

func (b rememberBackend) InspectionCapabilities() []string {
	var caps []string
	if _, ok := b.AdvancedStore.(memory.Inspector); ok {
		caps = append(caps, rpc.InspectionV1)
	}
	if _, ok := b.AdvancedStore.(memory.FactCurator); ok {
		caps = append(caps, rpc.CurationV1)
	}
	if _, ok := b.AdvancedStore.(interface {
		InspectHealth(context.Context) (memory.InspectionHealth, error)
	}); ok {
		caps = append(caps, rpc.InspectionHealthV1)
	}
	return caps
}

func (b rememberBackend) RecallPreview(ctx context.Context, agent, query string, topK int) ([]memory.RecallReceipt, error) {
	store, ok := b.AdvancedStore.(memory.Inspector)
	if !ok {
		return nil, memory.ErrInspectionUnsupported
	}
	if topK <= 0 {
		topK = b.mem.Config().TopK
	}
	return store.RecallPreview(ctx, agent, query, topK)
}

func (b rememberBackend) ListFacts(ctx context.Context, agent, state, query, cursor string, limit int) (memory.FactPage, error) {
	store, ok := b.AdvancedStore.(memory.Inspector)
	if !ok {
		return memory.FactPage{}, memory.ErrInspectionUnsupported
	}
	return store.ListFacts(ctx, agent, state, query, cursor, limit)
}

func (b rememberBackend) CurateFact(ctx context.Context, agent, id, action, text string, expected memory.Fact) (memory.Fact, error) {
	store, ok := b.AdvancedStore.(memory.FactCurator)
	if !ok {
		return memory.Fact{}, memory.ErrInspectionUnsupported
	}
	return store.CurateFact(ctx, agent, id, action, text, expected)
}

func (b rememberBackend) InspectHealth(ctx context.Context) (memory.InspectionHealth, error) {
	store, ok := b.AdvancedStore.(interface {
		InspectHealth(context.Context) (memory.InspectionHealth, error)
	})
	if !ok {
		return memory.InspectionHealth{}, memory.ErrInspectionUnsupported
	}
	return store.InspectHealth(ctx)
}
