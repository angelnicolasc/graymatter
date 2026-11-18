package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// AuthoritativeLineageV1 advertises transactional revisions, protected full
// updates, and versioned lineage inspection on this connection.
const AuthoritativeLineageV1 = "lineage-v1"

var (
	// ErrLineageUnsupported means no lineage operation was dispatched to the peer.
	ErrLineageUnsupported = errors.New("daemon does not support authoritative lineage. Update and restart the daemon")
	// ErrLineageArchiveTooLarge directs large exports to an exclusive direct reader.
	ErrLineageArchiveTooLarge = errors.New("lineage archive exceeds the daemon's 8 MiB response limit. Export from the store in direct mode")
)

const (
	maxLineageArchiveBytes = 8 << 20
	// A stored bbolt key cannot exceed 32 KiB.
	maxLineageKeyBytes = 32 << 10
)

type LineageBackend interface {
	SupportsAuthoritativeLineage() bool
	InspectLineage(context.Context, string, string, string) (memory.LineageInspection, error)
	ExportLineage(context.Context, string) (memory.LineageArchive, error)
}

type LineageInspectionRequest struct {
	Context                        InspectionContext
	Namespace, FactID, ContentHash string
}

type LineageInspectionResponse struct{ Lineage memory.LineageInspection }

type LineageArchiveRequest struct {
	Context   InspectionContext
	Namespace string
}

// LineageArchiveResponse holds pre-encoded rows so the server can bound the
// complete response before the JSON-RPC codec writes it.
type LineageArchiveResponse struct {
	Version   int
	Namespace string
	Facts     []json.RawMessage
	Evidence  []json.RawMessage
}

func (s *Server) lineageBackend() LineageBackend {
	if backend, ok := s.backend.(LineageBackend); ok && backend.SupportsAuthoritativeLineage() {
		return backend
	}
	return nil
}

func (c *Client) requireLineage(ctx context.Context) error {
	err := c.requireInspection(ctx, AuthoritativeLineageV1)
	if errors.Is(err, memory.ErrInspectionUnsupported) {
		return ErrLineageUnsupported
	}
	return err
}

func lineageRPCError(err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{
		ErrLineageUnsupported, ErrLineageArchiveTooLarge,
		memory.ErrFactNotFound, memory.ErrRevisionRequired, memory.ErrEvidenceQuota,
	} {
		if errors.Is(err, sentinel) {
			return err
		}
		if err.Error() == sentinel.Error() {
			return sentinel
		}
		if detail, ok := strings.CutPrefix(err.Error(), sentinel.Error()+": "); ok {
			return fmt.Errorf("%w: %s", sentinel, detail)
		}
	}
	return err
}

func (s *Server) InspectLineage(req *LineageInspectionRequest, resp *LineageInspectionResponse) error {
	defer s.touch()
	backend := s.lineageBackend()
	if backend == nil {
		return ErrLineageUnsupported
	}
	if len(req.Namespace) > maxLineageKeyBytes || len(req.FactID) > maxLineageKeyBytes || len(req.ContentHash) > 64 {
		return errors.New("lineage selector exceeds its size limit")
	}
	ctx, finish, err := s.beginInspection(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	resp.Lineage, err = backend.InspectLineage(ctx, req.Namespace, req.FactID, req.ContentHash)
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func (s *Server) ExportLineage(req *LineageArchiveRequest, resp *LineageArchiveResponse) error {
	defer s.touch()
	backend := s.lineageBackend()
	if backend == nil {
		return ErrLineageUnsupported
	}
	if len(req.Namespace) > maxLineageKeyBytes {
		return errors.New("lineage namespace exceeds its size limit")
	}
	ctx, finish, err := s.beginInspection(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	archive, err := backend.ExportLineage(ctx, req.Namespace)
	if err != nil {
		return err
	}
	*resp, err = encodeLineageArchive(ctx, archive)
	return err
}

func encodeLineageArchive(ctx context.Context, archive memory.LineageArchive) (LineageArchiveResponse, error) {
	resp := LineageArchiveResponse{Version: archive.Version, Namespace: archive.Namespace}
	header, err := json.Marshal(resp)
	if err != nil {
		return LineageArchiveResponse{}, err
	}
	// The header includes null for each array, which takes at least as much
	// space as empty brackets. Budget an extra byte per row for separators.
	remaining := maxLineageArchiveBytes - len(header)
	resp.Facts, err = encodeLineageRows(ctx, archive.Facts, &remaining)
	if err != nil {
		return LineageArchiveResponse{}, err
	}
	resp.Evidence, err = encodeLineageRows(ctx, archive.Evidence, &remaining)
	if err != nil {
		return LineageArchiveResponse{}, err
	}
	return resp, ctx.Err()
}

func encodeLineageRows[T any](ctx context.Context, rows []T, remaining *int) ([]json.RawMessage, error) {
	if rows == nil {
		return nil, nil
	}
	encoded := make([]json.RawMessage, 0, min(len(rows), 256))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if fact, ok := any(row).(memory.Fact); ok && len(fact.Text) > *remaining {
			return nil, ErrLineageArchiveTooLarge
		}
		data, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		*remaining -= len(data) + 1
		if *remaining < 0 {
			return nil, ErrLineageArchiveTooLarge
		}
		encoded = append(encoded, data)
	}
	return encoded, nil
}

func (c *Client) InspectLineage(ctx context.Context, namespace, factID, contentHash string) (memory.LineageInspection, error) {
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := c.requireLineage(ctx); err != nil {
		return memory.LineageInspection{}, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return memory.LineageInspection{}, err
	}
	req := &LineageInspectionRequest{Context: meta, Namespace: namespace, FactID: factID, ContentHash: contentHash}
	resp, err := rpcInspectionCall[LineageInspectionResponse](c, ctx, "InspectLineage", req, &meta, false)
	return resp.Lineage, lineageRPCError(inspectionError(err))
}

func (c *Client) ExportLineage(ctx context.Context, namespace string) (memory.LineageArchive, error) {
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := c.requireLineage(ctx); err != nil {
		return memory.LineageArchive{}, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return memory.LineageArchive{}, err
	}
	req := &LineageArchiveRequest{Context: meta, Namespace: namespace}
	resp, err := rpcInspectionCall[memory.LineageArchive](c, ctx, "ExportLineage", req, &meta, false)
	return resp, lineageRPCError(inspectionError(err))
}
