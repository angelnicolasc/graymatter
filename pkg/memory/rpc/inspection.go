package rpc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const InspectionV1 = "inspection-v1"
const CurationV1 = "curation-v1"
const InspectionHealthV1 = "inspection-health-v1"

// AtomicRevisionV1 guarantees replacement and source retirement commit together.
const AtomicRevisionV1 = "atomic-revision-v1"

type inspectionCapabilityProvider interface{ InspectionCapabilities() []string }
type healthInspector interface {
	InspectHealth(context.Context) (memory.InspectionHealth, error)
}

type FactPageRequest struct {
	Context                       InspectionContext
	AgentID, State, Query, Cursor string
	Limit                         int
}
type FactPageResponse struct{ Page memory.FactPage }
type CurateFactRequest struct {
	Context                       InspectionContext
	AgentID, FactID, Action, Text string
	Expected                      memory.Fact
}
type CurateFactResponse struct {
	Fact      memory.Fact
	Error     string
	ErrorCode string
}
type InspectionHealthRequest struct{ Context InspectionContext }
type InspectionHealthResponse struct{ Health memory.InspectionHealth }
type RecallPreviewRequest struct {
	Context        InspectionContext
	AgentID, Query string
	TopK           int
}

func (s *Server) supportsAtomicRevision() bool {
	backend, ok := s.backend.(interface{ AtomicRevisionSupported() bool })
	return ok && backend.AtomicRevisionSupported()
}

func (s *Server) inspectionCapabilities() []string {
	var caps []string
	if p, ok := s.backend.(inspectionCapabilityProvider); ok {
		caps = append(caps, p.InspectionCapabilities()...)
	} else {
		if _, ok := s.backend.(memory.Inspector); ok {
			caps = append(caps, InspectionV1)
		}
		if _, ok := s.backend.(memory.FactCurator); ok {
			caps = append(caps, CurationV1)
		}
		if _, ok := s.backend.(healthInspector); ok {
			caps = append(caps, InspectionHealthV1)
		}
	}
	if s.supportsAtomicRevision() {
		caps = append(caps, AtomicRevisionV1)
	}
	if len(caps) > 0 {
		caps = append(caps, InspectionContextV1)
	}
	return caps
}

func (c *Client) requireAtomicRevision(ctx context.Context) error {
	err := c.requireInspection(ctx, AtomicRevisionV1)
	if errors.Is(err, memory.ErrInspectionUnsupported) {
		return unsupported(AtomicRevisionV1)
	}
	return err
}

func (c *Client) requireInspection(ctx context.Context, capability string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.capMu.Lock()
	known := c.capKnown
	c.capMu.Unlock()
	if !known {
		resp, err := rpcInspectionCall[PingResponse](c, ctx, "Ping", &PingRequest{}, nil, false)
		if err != nil {
			return err
		}
		if err := c.acceptPing(resp); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.capMu.Lock()
	supported := c.capabilities[capability] && c.capabilities[InspectionContextV1]
	c.capMu.Unlock()
	if !supported {
		return fmt.Errorf("%w: %s and %s required; update and restart the daemon", memory.ErrInspectionUnsupported, capability, InspectionContextV1)
	}
	return nil
}

func inspectionError(err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{memory.ErrFactChanged, memory.ErrStoreReadOnly, memory.ErrInspectionUnsupported, context.Canceled, context.DeadlineExceeded} {
		if strings.HasPrefix(err.Error(), sentinel.Error()) {
			return fmt.Errorf("%w: %s", sentinel, err.Error())
		}
	}
	return err
}

func (s *Server) RecallPreview(req *RecallPreviewRequest, resp *RecallExplainResponse) error {
	defer s.touch()
	store, ok := s.backend.(memory.Inspector)
	if !ok {
		return memory.ErrInspectionUnsupported
	}
	ctx, finish, err := s.beginInspection(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	if err := ctx.Err(); err != nil {
		return err
	}
	resp.Receipts, err = store.RecallPreview(ctx, req.AgentID, req.Query, req.TopK)
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func (s *Server) ListFacts(req *FactPageRequest, resp *FactPageResponse) error {
	defer s.touch()
	store, ok := s.backend.(memory.Inspector)
	if !ok {
		return memory.ErrInspectionUnsupported
	}
	ctx, finish, err := s.beginInspection(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	if err := ctx.Err(); err != nil {
		return err
	}
	resp.Page, err = store.ListFacts(ctx, req.AgentID, req.State, req.Query, req.Cursor, req.Limit)
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func (s *Server) CurateFact(req *CurateFactRequest, resp *CurateFactResponse) error {
	defer s.touch()
	store, ok := s.backend.(memory.FactCurator)
	if !ok {
		return memory.ErrInspectionUnsupported
	}
	if req.Action == "revise" && !s.supportsAtomicRevision() {
		return unsupported(AtomicRevisionV1)
	}
	ctx, finish, err := s.beginInspection(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	if err := ctx.Err(); err != nil {
		return err
	}
	fact, err := store.CurateFact(ctx, req.AgentID, req.FactID, req.Action, req.Text, req.Expected)
	resp.Fact = fact
	// Preserve a committed identity if an adapter reports a later failure.
	if err != nil {
		resp.Error = err.Error()
		if req.Action == "revise" {
			resp.ErrorCode = revisionErrorCode(err)
			return nil
		}
		switch {
		case errors.Is(err, memory.ErrFactChanged):
			resp.ErrorCode = "fact_changed"
		case errors.Is(err, memory.ErrStoreReadOnly):
			resp.ErrorCode = "read_only"
		case errors.Is(err, memory.ErrInspectionUnsupported):
			resp.ErrorCode = "unsupported"
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			resp.ErrorCode = "canceled"
		}
	}
	return nil
}

func (s *Server) InspectHealth(req *InspectionHealthRequest, resp *InspectionHealthResponse) error {
	defer s.touch()
	store, ok := s.backend.(healthInspector)
	if !ok {
		return memory.ErrInspectionUnsupported
	}
	ctx, finish, err := s.beginInspection(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	if err := ctx.Err(); err != nil {
		return err
	}
	resp.Health, err = store.InspectHealth(ctx)
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func (c *Client) RecallPreview(ctx context.Context, agent, query string, topK int) ([]memory.RecallReceipt, error) {
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.requireInspection(ctx, InspectionV1); err != nil {
		return nil, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := rpcInspectionCall[RecallExplainResponse](c, ctx, "RecallPreview", &RecallPreviewRequest{Context: meta, AgentID: agent, Query: query, TopK: topK}, &meta, false)
	return resp.Receipts, inspectionError(err)
}

// InspectRecall is the TUI-facing spelling for side-effect-free recall.
func (c *Client) InspectRecall(ctx context.Context, agent, query string, topK int) ([]memory.RecallReceipt, error) {
	return c.RecallPreview(ctx, agent, query, topK)
}

func (c *Client) ListFacts(ctx context.Context, agent, state, query, cursor string, limit int) (memory.FactPage, error) {
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return memory.FactPage{}, err
	}
	if err := c.requireInspection(ctx, InspectionV1); err != nil {
		return memory.FactPage{}, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return memory.FactPage{}, err
	}
	resp, err := rpcInspectionCall[FactPageResponse](c, ctx, "ListFacts", &FactPageRequest{Context: meta, AgentID: agent, State: state, Query: query, Cursor: cursor, Limit: limit}, &meta, false)
	return resp.Page, inspectionError(err)
}

func (c *Client) CurateFact(ctx context.Context, agent, id, action, text string, expected memory.Fact) (memory.Fact, error) {
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return memory.Fact{}, err
	}
	if err := c.requireInspection(ctx, CurationV1); err != nil {
		return memory.Fact{}, err
	}
	if action == "revise" {
		if err := c.requireAtomicRevision(ctx); err != nil {
			return memory.Fact{}, err
		}
		if err := c.requireLineage(ctx); err != nil {
			return memory.Fact{}, err
		}
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return memory.Fact{}, err
	}
	resp, err := rpcInspectionCall[CurateFactResponse](c, ctx, "CurateFact", &CurateFactRequest{Context: meta, AgentID: agent, FactID: id, Action: action, Text: text, Expected: expected}, &meta, true)
	if err != nil {
		return resp.Fact, inspectionError(mapCapabilityError(err))
	}
	if resp.Error != "" {
		if action == "revise" {
			return resp.Fact, revisionError(resp.ErrorCode, resp.Error)
		}
		if resp.ErrorCode == "canceled" {
			return resp.Fact, fmt.Errorf("%w: %s", memory.ErrMutationOutcomeUnknown, resp.Error)
		}
		var sentinel error
		switch resp.ErrorCode {
		case "fact_changed":
			sentinel = memory.ErrFactChanged
		case "read_only":
			sentinel = memory.ErrStoreReadOnly
		case "unsupported":
			sentinel = memory.ErrInspectionUnsupported
		}
		if sentinel != nil {
			return resp.Fact, fmt.Errorf("%w: %s", sentinel, resp.Error)
		}
		return resp.Fact, inspectionError(errors.New(resp.Error))
	}
	return resp.Fact, nil
}

func (c *Client) InspectHealth(ctx context.Context) (memory.InspectionHealth, error) {
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return memory.InspectionHealth{}, err
	}
	if err := c.requireInspection(ctx, InspectionHealthV1); err != nil {
		return memory.InspectionHealth{}, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return memory.InspectionHealth{}, err
	}
	resp, err := rpcInspectionCall[InspectionHealthResponse](c, ctx, "InspectHealth", &InspectionHealthRequest{Context: meta}, &meta, false)
	return resp.Health, inspectionError(err)
}
