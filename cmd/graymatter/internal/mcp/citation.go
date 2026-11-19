package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/mark3labs/mcp-go/mcp"
)

func (b *DirectBackend) SupportsCitationEvidence() bool {
	store, ok := b.mem.Advanced().(interface{ SupportsCitationEvidence() bool })
	return ok && store.SupportsCitationEvidence()
}
func (b *DirectBackend) FetchCitation(ctx context.Context, namespace, factID, contentHash string) (memory.CitationResult, error) {
	store, ok := b.mem.Advanced().(interface {
		FetchCitation(context.Context, string, string, string) (memory.CitationResult, error)
	})
	if !ok {
		return memory.CitationResult{}, memory.ErrCitationUnsupported
	}
	return store.FetchCitation(ctx, namespace, factID, contentHash)
}

func evidenceBudget(args map[string]any) (int, error) {
	value, ok := args["evidence_budget_bytes"]
	if !ok {
		return memory.DefaultEvidenceResponseBytes, nil
	}
	var n float64
	switch v := value.(type) {
	case float64:
		n = v
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return 0, fmt.Errorf("evidence_budget_bytes must be a non-negative integer")
		}
	default:
		return 0, fmt.Errorf("evidence_budget_bytes must be a non-negative integer")
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || math.Trunc(n) != n || n > float64(math.MaxInt32) {
		return 0, fmt.Errorf("evidence_budget_bytes must be a non-negative integer <= %d", math.MaxInt32)
	}
	return int(n), nil
}

func citationSuffix(result memory.CitationResult) string {
	raw, _ := json.Marshal(result)
	return " evidence(untrusted)=" + string(raw)
}

// Count bytes as they actually appear in the outer MCP JSON: the text suffix
// is encoded again as a JSON string, and structured metadata carries its own
// literal copy. The assertion and existing U2 metadata are not charged twice.
func citationWireBytes(base, with coreFactMetadata, suffix string) int {
	a, _ := json.Marshal(base)
	b, _ := json.Marshal(with)
	text, _ := json.Marshal(suffix)
	return len(b) - len(a) + len(text) - 2
}

func (s *Server) fetchCitation(ctx context.Context, agentID string, args map[string]any) (*mcp.CallToolResult, error) {
	for _, key := range []string{"query", "queries", "scope", "top_k", "explain", "min_confidence", "confidence_weight", "evidence_budget_bytes"} {
		if _, ok := args[key]; ok {
			return toolError(key + " cannot be combined with citation fetch")
		}
	}
	label := "Citation"
	var id, hash string
	if value, present := args["id"]; present {
		_, hasFactID := args["fact_id"]
		_, hasHash := args["content_hash"]
		if hasFactID || hasHash {
			return toolError("id cannot be combined with fact_id or content_hash")
		}
		marker, ok := value.(string)
		if !ok || strings.TrimSpace(marker) == "" {
			return toolError("citation fetch requires a non-empty id")
		}
		resolver, ok := s.backend.(subjectTargetResolver)
		if !ok {
			return toolError(memory.ErrCitationUnsupported.Error())
		}
		facts, err := resolver.ResolveFactTargets(ctx, agentID, marker, "id", false)
		if err != nil {
			return toolError(err.Error())
		}
		if len(facts) != 1 || facts[0].AgentID != agentID || facts[0].ID == "" || facts[0].IsSuperseded() {
			return toolError("citation id did not resolve to one current fact")
		}
		id, hash = facts[0].ID, memory.NewEvidenceSnapshot(facts[0]).ContentHash
		label = marker
	} else {
		id, _ = args["fact_id"].(string)
		hash, _ = args["content_hash"].(string)
		if strings.TrimSpace(id) == "" || len(hash) != 64 {
			return toolError("citation fetch requires fact_id and an exact content_hash")
		}
	}
	fetcher, ok := s.backend.(interface {
		FetchCitation(context.Context, string, string, string) (memory.CitationResult, error)
	})
	if !ok {
		return toolError(memory.ErrCitationUnsupported.Error())
	}
	result, err := fetcher.FetchCitation(ctx, agentID, id, hash)
	if err != nil {
		return toolError(err.Error())
	}
	if result.Evidence == nil || result.Reference.Namespace != agentID || result.Reference.FactID != id || result.Reference.ContentHash != hash {
		return toolError("citation fetch returned an incomplete or mismatched version")
	}
	issues, err := s.subjectIssues(ctx, agentID)
	if err != nil {
		return toolError(fmt.Sprintf("subject diagnostics: %v", err))
	}
	citation := *result.Evidence
	if _, byID := args["id"]; byID && citation.Source != "" {
		sources, err := s.listSourceQuotes(agentID)
		if err != nil {
			return toolError(fmt.Sprintf("citation lookup: %v", err))
		}
		citation.Quote = sources.longestContaining(citation.Source, citation.Quote)
	}
	source, _ := json.Marshal(citation.Source)
	quote, _ := json.Marshal(citation.Quote)
	text := label + " source=" + string(source) + " quote=" + string(quote)
	return toolText(appendSubjectWarning(text, issues))
}

func (s *Server) citationArguments(tool string, args map[string]any) error {
	for _, key := range []string{"source", "quote", "fact_id", "content_hash", "evidence_budget_bytes"} {
		if _, present := args[key]; !present {
			continue
		}
		if s.fullCatalog {
			return fmt.Errorf("%s requires the core tools profile; restart mcp serve --tools core", key)
		}
		write := key == "source" || key == "quote"
		if write {
			if tool != "memory_add" && tool != "memory_reflect" {
				return fmt.Errorf("%s is not supported by %s", key, tool)
			}
			if tool == "memory_reflect" && args["action"] != "add" && args["action"] != "update" {
				return fmt.Errorf("%s is supported only by reflect add/update", key)
			}
		} else if tool != "memory_search" && !(tool == "memory_search_batch" && key == "evidence_budget_bytes") {
			return fmt.Errorf("%s is not supported by %s", key, tool)
		}
	}
	return s.subjectArguments(tool, args)
}
