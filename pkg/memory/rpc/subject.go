package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const SubjectHeadsV1 = "subject-heads-v1"

type SubjectBackend interface {
	SupportsSubjectHeads() bool
	PutManyReturningReceipts(context.Context, string, []memory.WriteFact) ([]memory.WriteReceipt, error)
	ResolveFactTargets(context.Context, string, string, string, bool) ([]memory.Fact, error)
	SuggestSimilarFacts(context.Context, string, string) (memory.SuggestionResult, error)
}

type PutManyReceiptsRequest PutManyRequest
type PutManyReceiptsResponse struct {
	Receipts  []memory.WriteReceipt
	Error     string `json:",omitempty"`
	ErrorCode string `json:",omitempty"`
}
type ResolveFactTargetsRequest struct {
	Context                     InspectionContext
	AgentID, Target, TargetType string
	IncludeRetired              bool
}
type ResolveFactTargetsResponse struct {
	Facts     []memory.Fact
	Error     string `json:",omitempty"`
	ErrorCode string `json:",omitempty"`
}
type SuggestSimilarFactsRequest struct {
	Context       InspectionContext
	AgentID, Text string
}
type SuggestSimilarFactsResponse struct {
	Result    memory.SuggestionResult
	Error     string `json:",omitempty"`
	ErrorCode string `json:",omitempty"`
}

func (r *PutManyReceiptsRequest) UnmarshalJSON(data []byte) error {
	var old PutManyRequest
	if err := json.Unmarshal(data, &old); err != nil {
		return err
	}
	*r = PutManyReceiptsRequest(old)
	return nil
}

func subjectRequested(opts memory.WriteOptions) bool {
	return opts.HasSubjectIntent()
}
func (s *Server) subjectBackend() SubjectBackend {
	b, ok := s.backend.(SubjectBackend)
	if !ok || !b.SupportsSubjectHeads() {
		return nil
	}
	return b
}
func (s *Server) checkSubjectOptions(opts memory.WriteOptions) error {
	if subjectRequested(opts) && s.subjectBackend() == nil {
		return memory.ErrSubjectUnsupported
	}
	return nil
}
func (c *Client) requireSubject(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.requireCapability(SubjectHeadsV1); err != nil {
		if errors.Is(err, ErrUnsupportedCapability) {
			return memory.ErrSubjectUnsupported
		}
		return err
	}
	return ctx.Err()
}
func (c *Client) SupportsSubjectHeads() bool { return c.requireSubject(context.Background()) == nil }

// The named endpoint lets bounded hook calls negotiate through their one
// existing dispatch rather than adding an unbounded capability round trip.
func (s *Server) RecallForHooksWithSubjects(req *RecallForHooksRequest, resp *RecallForHooksResponse) error {
	if s.subjectBackend() == nil {
		return memory.ErrSubjectUnsupported
	}
	if s.citationBackend() == nil {
		return memory.ErrCitationUnsupported
	}
	return s.RecallForHooks(req, resp)
}

func (s *Server) PutManyReturningReceipts(req *PutManyReceiptsRequest, resp *PutManyReceiptsResponse) error {
	defer s.touch()
	b := s.subjectBackend()
	if b == nil {
		return memory.ErrSubjectUnsupported
	}
	if err := memory.ValidateWriteFacts(req.AgentID, req.Facts); err != nil {
		return err
	}
	for _, input := range req.Facts {
		if err := s.checkCitationOptions(input.Options); err != nil {
			return err
		}
	}
	_, local := s.backend.(*memory.Store)
	provider, known := s.backend.(interface{ MaintenanceOnly() bool })
	if !local && (!known || !provider.MaintenanceOnly()) {
		return ErrMaintenancePolicyUnsupported
	}
	ctx, finish, err := s.beginRevision(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	resp.Receipts, err = b.PutManyReturningReceipts(ctx, req.AgentID, req.Facts)
	if err != nil {
		resp.Error, resp.ErrorCode = err.Error(), revisionErrorCode(err)
	}
	return nil
}

func (c *Client) PutManyReturningReceipts(ctx context.Context, namespace string, inputs []memory.WriteFact) ([]memory.WriteReceipt, error) {
	inputs = append([]memory.WriteFact(nil), inputs...)
	for i := range inputs {
		inputs[i].Options = inputs[i].Options.Clone()
	}
	if err := memory.ValidateWriteFacts(namespace, inputs); err != nil {
		return nil, err
	}
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := c.requireSubject(ctx); err != nil {
		return nil, err
	}
	for _, input := range inputs {
		if citationRequested(input.Options) {
			if err := c.requireCitation(ctx); err != nil {
				return nil, err
			}
			break
		}
	}
	if err := c.requireMaintenanceOnly(ctx); err != nil {
		return nil, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return nil, err
	}
	req := PutManyReceiptsRequest{Context: meta, AgentID: namespace, Facts: inputs}
	resp, err := rpcInspectionCall[PutManyReceiptsResponse](c, ctx, "PutManyReturningReceipts", &req, &meta, true)
	if err != nil {
		return nil, err
	}
	if len(resp.Receipts) > len(inputs) || resp.Error == "" && len(resp.Receipts) != len(inputs) {
		return nil, fmt.Errorf("%w: incomplete subject write receipts", memory.ErrMutationOutcomeUnknown)
	}
	seen := map[string]bool{}
	for i, receipt := range resp.Receipts {
		f := receipt.Fact
		if f.ID == "" || seen[f.ID] || f.AgentID != namespace || f.Text != inputs[i].Text {
			return nil, fmt.Errorf("%w: invalid subject write identity", memory.ErrMutationOutcomeUnknown)
		}
		seen[f.ID] = true
		if inputs[i].Options.HasEvidence() {
			opts := inputs[i].Options
			if f.Evidence == nil || opts.Source != nil && f.Evidence.Source != *opts.Source || opts.Quote != nil && f.Evidence.Quote != *opts.Quote {
				return nil, fmt.Errorf("%w: subject write citation mismatch", memory.ErrMutationOutcomeUnknown)
			}
		}
	}
	if resp.Error != "" {
		return resp.Receipts, revisionError(resp.ErrorCode, resp.Error)
	}
	return resp.Receipts, nil
}

func (s *Server) ResolveFactTargets(req *ResolveFactTargetsRequest, resp *ResolveFactTargetsResponse) error {
	defer s.touch()
	b := s.subjectBackend()
	if b == nil {
		return memory.ErrSubjectUnsupported
	}
	ctx, finish, err := s.beginInspection(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	resp.Facts, err = b.ResolveFactTargets(ctx, req.AgentID, req.Target, req.TargetType, req.IncludeRetired)
	if err != nil {
		resp.Error, resp.ErrorCode = err.Error(), revisionErrorCode(err)
	}
	return nil
}
func (c *Client) ResolveFactTargets(ctx context.Context, namespace, target, targetType string, includeRetired bool) ([]memory.Fact, error) {
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := c.requireSubject(ctx); err != nil {
		return nil, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return nil, err
	}
	req := ResolveFactTargetsRequest{Context: meta, AgentID: namespace, Target: target, TargetType: targetType, IncludeRetired: includeRetired}
	resp, err := rpcInspectionCall[ResolveFactTargetsResponse](c, ctx, "ResolveFactTargets", &req, &meta, false)
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, revisionError(resp.ErrorCode, resp.Error)
	}
	for _, f := range resp.Facts {
		if f.AgentID != namespace || f.ID == "" || !includeRetired && f.IsSuperseded() {
			return nil, errors.New("target resolver returned invalid namespace or state")
		}
	}
	return resp.Facts, nil
}
func (s *Server) SuggestSimilarFacts(req *SuggestSimilarFactsRequest, resp *SuggestSimilarFactsResponse) error {
	defer s.touch()
	b := s.subjectBackend()
	if b == nil {
		return memory.ErrSubjectUnsupported
	}
	ctx, finish, err := s.beginInspection(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	resp.Result, err = b.SuggestSimilarFacts(ctx, req.AgentID, req.Text)
	if err != nil {
		resp.Error, resp.ErrorCode = err.Error(), revisionErrorCode(err)
	}
	return nil
}
func (c *Client) SuggestSimilarFacts(ctx context.Context, namespace, text string) (memory.SuggestionResult, error) {
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := c.requireSubject(ctx); err != nil {
		return memory.SuggestionResult{}, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return memory.SuggestionResult{}, err
	}
	req := SuggestSimilarFactsRequest{Context: meta, AgentID: namespace, Text: text}
	resp, err := rpcInspectionCall[SuggestSimilarFactsResponse](c, ctx, "SuggestSimilarFacts", &req, &meta, false)
	if err != nil {
		return memory.SuggestionResult{}, err
	}
	if resp.Error != "" {
		return memory.SuggestionResult{}, revisionError(resp.ErrorCode, resp.Error)
	}
	return resp.Result, nil
}
