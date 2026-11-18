package daemon

import (
	"context"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

func (b rememberBackend) SupportsAuthoritativeLineage() bool {
	store, ok := b.AdvancedStore.(rpc.LineageBackend)
	return ok && store.SupportsAuthoritativeLineage()
}

func (b rememberBackend) InspectLineage(ctx context.Context, namespace, factID, contentHash string) (memory.LineageInspection, error) {
	store, ok := b.AdvancedStore.(rpc.LineageBackend)
	if !ok {
		return memory.LineageInspection{}, rpc.ErrLineageUnsupported
	}
	return store.InspectLineage(ctx, namespace, factID, contentHash)
}

func (b rememberBackend) ExportLineage(ctx context.Context, namespace string) (memory.LineageArchive, error) {
	store, ok := b.AdvancedStore.(rpc.LineageBackend)
	if !ok {
		return memory.LineageArchive{}, rpc.ErrLineageUnsupported
	}
	return store.ExportLineage(ctx, namespace)
}
