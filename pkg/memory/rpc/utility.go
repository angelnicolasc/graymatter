package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// UtilityV1 promises scoped identity-preserving search and atomic batch writes.
// It is separate from legacy endpoints so an old daemon cannot ignore options.
const UtilityV1 = "compact-memory-v1"

type SearchScopedRequest struct {
	Context InspectionContext
	AgentID string
	Queries []string
	TopK    int
	All     bool
	Options memory.RecallOptions
}

type SearchScopedResponse struct {
	Result    memory.ScopedSearchResult
	Error     string `json:",omitempty"`
	ErrorCode string `json:",omitempty"`
}

type PutManyRequest struct {
	Context InspectionContext
	AgentID string
	Facts   []memory.WriteFact
}

type PutManyResponse struct {
	Facts     []memory.Fact
	Error     string `json:",omitempty"`
	ErrorCode string `json:",omitempty"`
}

func (r *SearchScopedRequest) UnmarshalJSON(data []byte) error {
	if err := validateWireOptions(data, "min_confidence", "confidence_weight"); err != nil {
		return err
	}
	type plain SearchScopedRequest
	return json.Unmarshal(data, (*plain)(r))
}

func (r *PutManyRequest) UnmarshalJSON(data []byte) error {
	var envelope struct{ Facts []json.RawMessage }
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	for i, fact := range envelope.Facts {
		if err := validateWireOptions(fact, "confidence", "source", "quote", "subject", "replaces"); err != nil {
			return fmt.Errorf("facts[%d]: %w", i, err)
		}
	}
	type plain PutManyRequest
	return json.Unmarshal(data, (*plain)(r))
}

func (s *Server) utilityStore() (memory.UtilityStore, bool) {
	store, ok := s.backend.(memory.UtilityStore)
	if provider, wrapped := s.backend.(interface{ UtilitySupported() bool }); wrapped {
		ok = ok && provider.UtilitySupported()
	}
	return store, ok
}

func (s *Server) SearchScoped(req *SearchScopedRequest, out *SearchScopedResponse) error {
	defer s.touch()
	store, ok := s.utilityStore()
	if !ok {
		return memory.ErrUtilityUnsupported
	}
	if err := req.Options.Validate(); err != nil {
		return err
	}
	ctx, finish, err := s.beginRevision(req.Context)
	if err != nil {
		return err
	}
	defer finish()
	out.Result, err = store.SearchScoped(ctx, req.AgentID, req.Queries, req.TopK, req.All, req.Options)
	if err != nil {
		out.Error, out.ErrorCode = err.Error(), revisionErrorCode(err)
	}
	return nil
}

func (s *Server) PutManyReturningFacts(req *PutManyRequest, out *PutManyResponse) error {
	defer s.touch()
	for _, input := range req.Facts {
		if err := s.checkCitationOptions(input.Options); err != nil {
			return err
		}
	}
	if err := memory.ValidateWriteFacts(req.AgentID, req.Facts); err != nil {
		return err
	}
	store, ok := s.utilityStore()
	if !ok {
		return memory.ErrUtilityUnsupported
	}
	// This endpoint cannot be used to bypass the policy negotiation by sending
	// raw RPC requests to a wrapper with an unknown maintenance policy.
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
	out.Facts, err = store.PutManyReturningFacts(ctx, req.AgentID, req.Facts)
	if err != nil {
		out.Error, out.ErrorCode = err.Error(), revisionErrorCode(err)
	}
	return nil
}

func (c *Client) requireUtility(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.requireCapability(UtilityV1); err != nil {
		if errors.Is(err, ErrUnsupportedCapability) {
			return memory.ErrUtilityUnsupported
		}
		return err
	}
	return ctx.Err()
}

func (c *Client) SearchScoped(ctx context.Context, agentID string, queries []string, topK int, all bool, opts memory.RecallOptions) (memory.ScopedSearchResult, error) {
	if err := opts.Validate(); err != nil {
		return memory.ScopedSearchResult{}, err
	}
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	if err := c.requireUtility(ctx); err != nil {
		return memory.ScopedSearchResult{}, err
	}
	if err := c.requireCitation(ctx); err != nil {
		return memory.ScopedSearchResult{}, err
	}
	if err := c.requireSubject(ctx); err != nil {
		return memory.ScopedSearchResult{}, err
	}
	resolved, legacyMetadata := c.resolveClientRecallOptions(opts)
	// The request can still be encoding after cancellation returns to the caller.
	// Own all pointer/slice storage before handing it to the async RPC sender.
	queries = append([]string(nil), queries...)
	if resolved.MinConfidence != nil {
		value := *resolved.MinConfidence
		resolved.MinConfidence = &value
	}
	if resolved.ConfidenceWeight != nil {
		value := *resolved.ConfidenceWeight
		resolved.ConfidenceWeight = &value
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return memory.ScopedSearchResult{}, err
	}
	req := SearchScopedRequest{Context: meta, AgentID: agentID, Queries: queries, TopK: topK, All: all, Options: resolved}
	resp, err := rpcInspectionCall[SearchScopedResponse](c, ctx, "SearchScoped", &req, &meta, false)
	if err != nil {
		return memory.ScopedSearchResult{}, err
	}
	if resp.Error != "" {
		return memory.ScopedSearchResult{}, revisionError(resp.ErrorCode, resp.Error)
	}
	if legacyMetadata && resp.Result.Retrieval != nil && resp.Result.Retrieval.VectorFallback == "" {
		resp.Result.Retrieval = nil
	}
	if legacyMetadata {
		for i := range resp.Result.Facts {
			if resp.Result.Facts[i].Receipt != nil {
				resp.Result.Facts[i].Receipt.Ranking = nil
			}
		}
	}
	return resp.Result, nil
}

func (c *Client) PutManyReturningFacts(ctx context.Context, agentID string, inputs []memory.WriteFact) ([]memory.Fact, error) {
	inputs = append([]memory.WriteFact(nil), inputs...)
	for i := range inputs {
		inputs[i].Options = cloneCitationOptions(inputs[i].Options)
	}
	if err := memory.ValidateWriteFacts(agentID, inputs); err != nil {
		return nil, err
	}
	ctx, cancel := c.inspectionContext(ctx)
	defer cancel()
	for _, input := range inputs {
		if subjectRequested(input.Options) {
			if err := c.requireSubject(ctx); err != nil {
				return nil, err
			}
		}
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
	if err := c.requireUtility(ctx); err != nil {
		return nil, err
	}
	meta, err := newInspectionContext(ctx)
	if err != nil {
		return nil, err
	}
	req := PutManyRequest{Context: meta, AgentID: agentID, Facts: inputs}
	resp, err := rpcInspectionCall[PutManyResponse](c, ctx, "PutManyReturningFacts", &req, &meta, true)
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, revisionError(resp.ErrorCode, resp.Error)
	}
	if len(resp.Facts) != len(inputs) {
		return nil, fmt.Errorf("%w: RPC returned an incomplete batch receipt", memory.ErrMutationOutcomeUnknown)
	}
	seen := make(map[string]bool, len(resp.Facts))
	for i, f := range resp.Facts {
		if f.ID == "" || seen[f.ID] || f.AgentID != agentID || f.Text != inputs[i].Text {
			return nil, fmt.Errorf("%w: RPC returned invalid batch identity", memory.ErrMutationOutcomeUnknown)
		}
		seen[f.ID] = true
	}
	return resp.Facts, nil
}
