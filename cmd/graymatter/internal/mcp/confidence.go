package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/mark3labs/mcp-go/mcp"
)

type confidenceWriter interface {
	PutWithOptionsReturningFact(context.Context, string, string, memory.WriteOptions) (memory.Fact, error)
}
type optionalIdentityWriter interface {
	PutWithOptionalIdentity(context.Context, string, string, memory.WriteOptions) (memory.Fact, error)
}

type returningFactWriter interface {
	PutReturningFact(context.Context, string, string) (memory.Fact, error)
}

// Select the route before dispatch. A failure after invoking a writer never
// permits a fallback write because the first operation may have committed.
func (s *Server) putWithIdentity(ctx context.Context, agentID, text string, opts memory.WriteOptions) (memory.Fact, error) {
	if writer, ok := s.backend.(optionalIdentityWriter); ok {
		return writer.PutWithOptionalIdentity(ctx, agentID, text, opts)
	}
	if writer, ok := s.backend.(confidenceWriter); ok {
		return confirmedIdentity(writer.PutWithOptionsReturningFact(ctx, agentID, text, opts))
	}
	if opts.Confidence != nil {
		return memory.Fact{}, memory.ErrConfidenceUnsupported
	}
	if writer, ok := s.backend.(returningFactWriter); ok {
		return confirmedIdentity(writer.PutReturningFact(ctx, agentID, text))
	}
	// Older in-process backends may only support Remember. A product daemon
	// uses the optional identity capability path, including its policy gate.
	return memory.Fact{}, s.backend.Remember(ctx, agentID, text)
}

func confirmedIdentity(fact memory.Fact, err error) (memory.Fact, error) {
	if err == nil && fact.ID == "" {
		return memory.Fact{}, errors.New("write returned no committed fact identity")
	}
	return fact, err
}

func writeIdentityNotice(id, confidence string) string {
	if id == "" {
		return ""
	}
	if confidence == "" {
		return fmt.Sprintf(" fact_id %s.", id)
	}
	return fmt.Sprintf(" Confidence %s. fact_id %s.", confidence, id)
}

type confidenceReviser interface {
	ReviseFactsWithOptions(context.Context, string, string, memory.WriteOptions, ...memory.Fact) (memory.Fact, error)
}
type confidenceRecaller interface {
	RecallWithOptions(context.Context, string, string, int, memory.RecallOptions) (memory.RecallResult, error)
	RecallExplainWithOptions(context.Context, string, string, int, memory.RecallOptions) (memory.RecallExplainResult, error)
}

func useConfidenceRecall(backend Backend, opts memory.RecallOptions) bool {
	_, supported := backend.(confidenceRecaller)
	// Resolve defaults on the current connection rather than routing from a
	// cached value that can become stale when the daemon restarts.
	return supported || opts.Requested() || memory.DefaultConfidenceWeight != 0
}

func batchRecallOptions(backend Backend, opts memory.RecallOptions) (memory.RecallOptions, bool, error) {
	weight := memory.DefaultConfidenceWeight
	if provider, ok := backend.(interface{ DefaultConfidenceWeight() float64 }); ok {
		if ready, ok := backend.(interface{ Ready() error }); ok {
			if err := ready.Ready(); err != nil {
				return opts, false, err
			}
		}
		weight = provider.DefaultConfidenceWeight()
	}
	if opts.ConfidenceWeight != nil {
		weight = *opts.ConfidenceWeight
	}
	useOptions := opts.Requested() || weight != 0
	if useOptions && opts.ConfidenceWeight == nil {
		opts.ConfidenceWeight = &weight
	}
	return opts, useOptions, opts.Validate()
}

func committedWriteError(action string, fact memory.Fact, err error) (*mcp.CallToolResult, error) {
	if fact.ID != "" {
		return toolError(fmt.Sprintf("%s failed after committing fact_id %s (confidence %s): %v", action, fact.ID, memory.EffectiveConfidence(fact.Confidence), err))
	}
	return toolError(fmt.Sprintf("%s failed: %v", action, err))
}

func confidenceWriteSchema() mcp.ToolOption {
	return mcp.WithString("confidence", mcp.Enum("verified", "inferred", "unverified"), mcp.Description("Optional writer-declared confidence; omitted add stores legacy metadata, effectively inferred. This is not automated verification."))
}
func confidenceFilterSchema() mcp.ToolOption {
	return mcp.WithString("min_confidence", mcp.Enum("verified", "inferred", "unverified"), mcp.Description("Optional minimum effective confidence, applied before ranking and top-k. Legacy empty labels are inferred; unknown historical labels are unverified. Any explicit filter suppresses graph hints without receipts."))
}
func confidenceWeightSchema() mcp.ToolOption {
	return mcp.WithNumber("confidence_weight", mcp.Min(0), mcp.Max(0.5), mcp.Description("Optional finite preference weight in [0,0.5], default zero during opt-in rollout. Final score = base RRF * (1 + weight*c), c=1 verified, 0 inferred, -1 unverified. Zero retains an explicit filter."))
}

func writeOptions(args map[string]any) (memory.WriteOptions, error) {
	var opts memory.WriteOptions
	if value, present := args["confidence"]; present {
		s, ok := value.(string)
		if !ok {
			return opts, fmt.Errorf("confidence must be a string")
		}
		opts.Confidence = &s
	}
	return opts, opts.Validate()
}

func recallOptions(args map[string]any) (memory.RecallOptions, error) {
	var opts memory.RecallOptions
	if value, present := args["min_confidence"]; present {
		s, ok := value.(string)
		if !ok {
			return opts, fmt.Errorf("min_confidence must be a string")
		}
		opts.MinConfidence = &s
	}
	if value, present := args["confidence_weight"]; present {
		var w float64
		switch n := value.(type) {
		case float64:
			w = n
		case float32:
			w = float64(n)
		case int:
			w = float64(n)
		case int64:
			w = float64(n)
		case json.Number:
			var err error
			w, err = n.Float64()
			if err != nil {
				return opts, fmt.Errorf("confidence_weight must be a number")
			}
		default:
			return opts, fmt.Errorf("confidence_weight must be a number")
		}
		opts.ConfidenceWeight = &w
	}
	return opts, opts.Validate()
}

// Validate only the new options. The existing MCP validation contract remains
// intact; a recognized option on the wrong tool must never disappear silently.
func (s *Server) confidenceGuard(tool string, next func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		for _, key := range []string{"confidence", "min_confidence", "confidence_weight"} {
			if _, present := args[key]; !present {
				continue
			}
			allowed := (key == "confidence" && (tool == "memory_add" || tool == "memory_reflect")) || (key != "confidence" && (tool == "memory_search" || tool == "memory_search_batch"))
			if !allowed {
				return toolError(fmt.Sprintf("%s is not supported by %s", key, tool))
			}
		}
		if _, present := args["confidence"]; present {
			if tool == "memory_reflect" && args["action"] != "add" && args["action"] != "update" {
				return toolError("confidence is supported only by reflect add/update")
			}
			if _, err := writeOptions(args); err != nil {
				return toolError(err.Error())
			}
		}
		if _, err := recallOptions(args); err != nil {
			return toolError(err.Error())
		}
		return next(ctx, req)
	}
}

func (b *DirectBackend) PutWithOptionsReturningFact(ctx context.Context, agentID, text string, opts memory.WriteOptions) (memory.Fact, error) {
	return b.mem.RememberWithOptions(ctx, agentID, text, opts)
}

func (b *DirectBackend) DefaultConfidenceWeight() float64 { return b.mem.Config().ConfidenceWeight }

func (b *DirectBackend) ReviseFactsWithOptions(ctx context.Context, agentID, text string, opts memory.WriteOptions, victims ...memory.Fact) (memory.Fact, error) {
	store, ok := b.mem.Advanced().(confidenceReviser)
	if !ok {
		return memory.Fact{}, memory.ErrConfidenceUnsupported
	}
	f, err := store.ReviseFactsWithOptions(ctx, agentID, text, opts, victims...)
	if f.ID != "" && b.mem.Config().AsyncConsolidate {
		if launcher, ok := b.mem.Advanced().(interface {
			LaunchAsyncConsolidate(string, memory.ConsolidateConfig)
		}); ok {
			launcher.LaunchAsyncConsolidate(agentID, b.mem.Config())
		}
	}
	return f, err
}

func (b *DirectBackend) RecallWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallResult, error) {
	store, ok := b.mem.Advanced().(confidenceRecaller)
	if !ok {
		return memory.RecallResult{}, memory.ErrConfidenceUnsupported
	}
	if topK <= 0 {
		topK = b.mem.Config().TopK
	}
	return store.RecallWithOptions(ctx, agentID, query, topK, opts)
}
func (b *DirectBackend) RecallExplainWithOptions(ctx context.Context, agentID, query string, topK int, opts memory.RecallOptions) (memory.RecallExplainResult, error) {
	store, ok := b.mem.Advanced().(confidenceRecaller)
	if !ok {
		return memory.RecallExplainResult{}, memory.ErrConfidenceUnsupported
	}
	if topK <= 0 {
		topK = b.mem.Config().TopK
	}
	return store.RecallExplainWithOptions(ctx, agentID, query, topK, opts)
}
func (b *DirectBackend) SetPinned(agentID string, pinned bool, victims ...memory.Fact) error {
	store, ok := b.mem.Advanced().(interface {
		SetPinned(string, bool, ...memory.Fact) error
	})
	if !ok {
		return memory.ErrConfidenceUnsupported
	}
	return store.SetPinned(agentID, pinned, victims...)
}
func (b *DirectBackend) Retire(agentID string, victims ...memory.Fact) error {
	store, ok := b.mem.Advanced().(interface {
		Retire(string, ...memory.Fact) error
	})
	if !ok {
		return memory.ErrConfidenceUnsupported
	}
	return store.Retire(agentID, victims...)
}
