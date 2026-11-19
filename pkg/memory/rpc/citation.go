package rpc

import (
	"context"
	"errors"
	"fmt"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const CitationEvidenceV1 = "citation-evidence-v1"

type CitationBackend interface {
	SupportsCitationEvidence() bool
	FetchCitation(context.Context, string, string, string) (memory.CitationResult, error)
	RecallForHooks(context.Context, string, string, int) ([]string, error)
}

type FetchCitationRequest struct {
	Context                        InspectionContext
	Namespace, FactID, ContentHash string
}
type FetchCitationResponse struct{ Result memory.CitationResult }
type RecallForHooksRequest struct {
	AgentID, Query string
	TopK           int
}
type RecallForHooksResponse struct{ Facts []string }

func citationRequested(opts memory.WriteOptions) bool { return opts.HasEvidence() }

func cloneCitationOptions(opts memory.WriteOptions) memory.WriteOptions {
	return opts.Clone()
}

func (s *Server) citationBackend() CitationBackend {
	b, ok := s.backend.(CitationBackend)
	if !ok || !b.SupportsCitationEvidence() {
		return nil
	}
	return b
}

func (s *Server) checkCitationOptions(opts memory.WriteOptions) error {
	if err := s.checkSubjectOptions(opts); err != nil {
		return err
	}
	if citationRequested(opts) && s.citationBackend() == nil {
		return memory.ErrCitationUnsupported
	}
	return nil
}

func (c *Client) requireCitation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.requireCapability(CitationEvidenceV1); err != nil {
		if errors.Is(err, ErrUnsupportedCapability) {
			return memory.ErrCitationUnsupported
		}
		return err
	}
	return ctx.Err()
}

func (c *Client) SupportsCitationEvidence() bool {
	return c.requireCitation(context.Background()) == nil
}

func (c *Client) prepareCitationOptions(ctx context.Context, opts memory.WriteOptions) (memory.WriteOptions, error) {
	opts = cloneCitationOptions(opts)
	if err := opts.Validate(); err != nil {
		return opts, err
	}
	if subjectRequested(opts) {
		if err := c.requireSubject(ctx); err != nil {
			return opts, err
		}
	}
	if citationRequested(opts) {
		return opts, c.requireCitation(ctx)
	}
	return opts, nil
}

func (s *Server) FetchCitation(req *FetchCitationRequest, resp *FetchCitationResponse) error {
	defer s.touch()
	b := s.citationBackend()
	if b == nil {
		return memory.ErrCitationUnsupported
	}
	if req.Namespace == "" || req.FactID == "" || len(req.Namespace) > maxLineageKeyBytes || len(req.FactID) > maxLineageKeyBytes || len(req.ContentHash) != 64 {
		return errors.New("citation requires namespace, fact_id and an exact content_hash")
	}
	ctx, finish, err := s.beginInspection(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	resp.Result, err = b.FetchCitation(ctx, req.Namespace, req.FactID, req.ContentHash)
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func (c *Client) FetchCitation(ctx context.Context, namespace, factID, contentHash string) (memory.CitationResult, error) {
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := c.requireCitation(ctx); err != nil {
		return memory.CitationResult{}, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return memory.CitationResult{}, err
	}
	req := FetchCitationRequest{Context: meta, Namespace: namespace, FactID: factID, ContentHash: contentHash}
	resp, err := rpcInspectionCall[FetchCitationResponse](c, ctx, "FetchCitation", &req, &meta, false)
	if err != nil {
		return memory.CitationResult{}, lineageRPCError(inspectionError(err))
	}
	ref := resp.Result.Reference
	if ref.Namespace != namespace || ref.FactID != factID || ref.ContentHash != contentHash || resp.Result.Evidence == nil {
		return memory.CitationResult{}, fmt.Errorf("citation RPC returned an incomplete or mismatched version")
	}
	return resp.Result, nil
}

func (s *Server) RecallForHooks(req *RecallForHooksRequest, resp *RecallForHooksResponse) error {
	defer s.touch()
	b := s.citationBackend()
	if b == nil {
		return memory.ErrCitationUnsupported
	}
	if recaller, ok := s.backend.(interface {
		RecallForHooksWithOptions(context.Context, string, string, int, memory.RecallOptions) ([]string, error)
	}); ok && s.hasConfidenceCapability(ConfidenceRecallV1) && !s.rawLegacyEmptyTopK(req.TopK) {
		var err error
		resp.Facts, err = recaller.RecallForHooksWithOptions(context.Background(), req.AgentID, req.Query, req.TopK, legacyRecallOptions())
		return err
	}
	var err error
	resp.Facts, err = b.RecallForHooks(context.Background(), req.AgentID, req.Query, req.TopK)
	return err
}

func (c *Client) RecallForHooks(ctx context.Context, namespace, query string, topK int) ([]string, error) {
	if err := c.requireCitation(ctx); err != nil {
		return nil, err
	}
	if err := c.requireSubject(ctx); err != nil {
		return nil, err
	}
	var resp RecallForHooksResponse
	err := c.call("RecallForHooksWithSubjects", &RecallForHooksRequest{AgentID: namespace, Query: query, TopK: topK}, &resp)
	if err == nil {
		err = ctx.Err()
	}
	return resp.Facts, err
}
