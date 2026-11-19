package main

import (
	"context"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

type lineageInspector interface {
	InspectLineage(context.Context, string, string, string) (memory.LineageInspection, error)
}

type lineageExporter interface {
	ExportLineage(context.Context, string) (memory.LineageArchive, error)
}

func (d *directStore) InspectLineage(ctx context.Context, namespace, factID, hash string) (memory.LineageInspection, error) {
	store, ok := d.store.(lineageInspector)
	if !ok {
		return memory.LineageInspection{}, rpc.ErrLineageUnsupported
	}
	return store.InspectLineage(ctx, namespace, factID, hash)
}

func (d *directStore) ExportLineage(ctx context.Context, namespace string) (memory.LineageArchive, error) {
	store, ok := d.store.(lineageExporter)
	if !ok {
		return memory.LineageArchive{}, rpc.ErrLineageUnsupported
	}
	return store.ExportLineage(ctx, namespace)
}

func (r *reconnectingStore) InspectLineage(ctx context.Context, namespace, factID, hash string) (memory.LineageInspection, error) {
	var inspection memory.LineageInspection
	err := r.do(func(s cliStore) error {
		store, ok := s.(lineageInspector)
		if !ok {
			return rpc.ErrLineageUnsupported
		}
		var err error
		inspection, err = store.InspectLineage(ctx, namespace, factID, hash)
		return err
	})
	return inspection, err
}

func (r *reconnectingStore) ExportLineage(ctx context.Context, namespace string) (memory.LineageArchive, error) {
	var archive memory.LineageArchive
	err := r.do(func(s cliStore) error {
		store, ok := s.(lineageExporter)
		if !ok {
			return rpc.ErrLineageUnsupported
		}
		var err error
		archive, err = store.ExportLineage(ctx, namespace)
		return err
	})
	return archive, err
}

func (r *readOnlyTUIStore) InspectLineage(ctx context.Context, namespace, factID, hash string) (memory.LineageInspection, error) {
	store, ok := r.cliStore.(lineageInspector)
	if !ok {
		return memory.LineageInspection{}, rpc.ErrLineageUnsupported
	}
	return store.InspectLineage(ctx, namespace, factID, hash)
}
