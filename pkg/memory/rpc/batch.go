package rpc

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// ScopedBatchRecallV1 lets one daemon operation record alias activity once
// across the whole batch without changing the legacy recall endpoints.
const ScopedBatchRecallV1 = "scoped-batch-recall-v1"

type BatchRecallScopedRequest struct {
	AgentID string
	Queries []string
	TopK    int
	All     bool
	Options *memory.RecallOptions `json:",omitempty"`
}

func (r *BatchRecallScopedRequest) UnmarshalJSON(data []byte) error {
	if err := validateWireOptions(data, "min_confidence", "confidence_weight"); err != nil {
		return err
	}
	type plain BatchRecallScopedRequest
	return json.Unmarshal(data, (*plain)(r))
}

type batchRecallRow struct {
	memory.BatchResult
	Error string `json:",omitempty"`
}

type BatchRecallScopedResponse struct {
	Results []batchRecallRow
	Error   string `json:",omitempty"`
}

// A wrapper may implement the method but lack a capable underlying store.
func (s *Server) scopedBatchRecaller() (memory.ScopedBatchRecaller, bool) {
	recaller, ok := s.backend.(memory.ScopedBatchRecaller)
	if provider, wrapped := s.backend.(interface{ BatchRecallScopedSupported() bool }); wrapped {
		ok = ok && provider.BatchRecallScopedSupported()
	}
	return recaller, ok
}

func (s *Server) BatchRecallScoped(req *BatchRecallScopedRequest, resp *BatchRecallScopedResponse) error {
	defer s.touch()
	recaller, ok := s.scopedBatchRecaller()
	if !ok {
		return memory.ErrBatchRecallUnsupported
	}
	opts := req.Options
	if opts != nil {
		if err := opts.Validate(); err != nil {
			return err
		}
	} else if _, supports := s.backend.(ConfidenceRecaller); supports && s.hasConfidenceCapability(ConfidenceRecallV1) && !s.rawLegacyEmptyTopK(req.TopK) {
		// Keep the scalar RPC's zero policy and option-aware recall path.
		legacy := legacyRecallOptions()
		opts = &legacy
	}
	rows, err := recaller.BatchRecallScoped(context.Background(), req.AgentID, req.Queries, req.TopK, req.All, opts)
	for _, row := range rows {
		if req.Options == nil {
			row.Retrieval = nil
		}
		wire := batchRecallRow{BatchResult: row}
		if row.Err != nil {
			wire.Error = row.Err.Error()
		}
		resp.Results = append(resp.Results, wire)
	}
	// Keep partial results even if the batch itself reports an error.
	if err != nil {
		resp.Error = err.Error()
	}
	return nil
}

func (c *Client) BatchRecallScoped(ctx context.Context, agentID string, queries []string, topK int, all bool, opts *memory.RecallOptions) ([]memory.BatchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts != nil {
		if err := opts.Validate(); err != nil {
			return nil, err
		}
	}
	if err := c.requireCapability(ScopedBatchRecallV1); err != nil {
		if errors.Is(err, ErrUnsupportedCapability) {
			return nil, memory.ErrBatchRecallUnsupported
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var legacyMetadata bool
	if opts != nil {
		var resolved memory.RecallOptions
		resolved, legacyMetadata = c.resolveClientRecallOptions(*opts)
		opts = &resolved
	}
	var resp BatchRecallScopedResponse
	req := BatchRecallScopedRequest{AgentID: agentID, Queries: queries, TopK: topK, All: all, Options: opts}
	// Once sent, a failed response may follow recorded alias activity.
	// A remote error must never look like a pre-dispatch fallback signal.
	if err := c.call("BatchRecallScoped", &req, &resp); err != nil {
		return nil, err
	}
	var rows []memory.BatchResult
	for _, wire := range resp.Results {
		row := wire.BatchResult
		if legacyMetadata {
			row.Retrieval = nil
			row.Feedback = ""
		}
		if wire.Error != "" {
			row.Err = errors.New(wire.Error)
		}
		rows = append(rows, row)
	}
	if resp.Error != "" {
		return rows, errors.New(resp.Error)
	}
	return rows, nil
}
