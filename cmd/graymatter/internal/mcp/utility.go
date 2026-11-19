package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/mark3labs/mcp-go/mcp"
)

type coreFactMetadata struct {
	Index           int                        `json:"i"`
	ID              string                     `json:"id,omitempty"`
	AgeDays         *int                       `json:"age_days,omitempty"`
	Subjects        []memory.SubjectProjection `json:"subjects,omitempty"`
	Shared          bool                       `json:"shared,omitempty"`
	Explained       *coreReceipt               `json:"explained,omitempty"`
	Evidence        *memory.Citation           `json:"evidence,omitempty"`
	EvidenceRef     *memory.CitationReference  `json:"evidence_ref,omitempty"`
	EvidenceOmitted bool                       `json:"evidence_omitted,omitempty"`
}

// The fact's index refers to the one content line containing its text.
type coreReceipt struct {
	QueryIndex int                       `json:"query_index"`
	Weight     float64                   `json:"weight"`
	AgeDays    float64                   `json:"age_days"`
	Ranks      memory.RecallRanks        `json:"ranks"`
	Provenance memory.RecallProvenance   `json:"provenance"`
	KGLinks    []string                  `json:"kg_links,omitempty"`
	Ranking    *memory.ConfidenceRanking `json:"ranking,omitempty"`
}

type coreSearchResult struct {
	Facts         []coreFactMetadata    `json:"facts"`
	PerQuery      [][]int               `json:"per_query,omitempty"`
	SubjectIssues *memory.SubjectIssues `json:"subject_issues,omitempty"`
	Count         *int                  `json:"count,omitempty"`
	Explained     []coreReceipt         `json:"explained,omitempty"`
}

func (s *Server) handleMemorySearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	if err := s.citationArguments("memory_search", args); err != nil {
		return toolError(err.Error())
	}
	if s.fullCatalog {
		for _, key := range []string{"queries", "scope"} {
			if _, ok := args[key]; ok {
				return toolError(key + " requires the core tools profile; restart mcp serve --tools core")
			}
		}
		return s.handleMemorySearchLegacy(ctx, req)
	}
	agentID, ok := getString(args, "agent_id")
	if !ok || strings.TrimSpace(agentID) == "" {
		return toolError("agent_id is required")
	}
	_, hasShortID := args["id"]
	_, hasFactID := args["fact_id"]
	_, hasHash := args["content_hash"]
	if hasShortID || hasFactID || hasHash {
		return s.fetchCitation(ctx, agentID, args)
	}
	var explain bool
	if value, present := args["explain"]; present {
		var valid bool
		explain, valid = value.(bool)
		if !valid {
			return toolError("explain must be a boolean")
		}
	}
	remainingEvidence, err := evidenceBudget(args)
	if err != nil {
		return toolError(err.Error())
	}
	_, hasQuery := args["query"]
	_, hasQueries := args["queries"]
	if hasQuery == hasQueries {
		return toolError("pass exactly one of query or queries")
	}
	var queries []string
	if hasQuery {
		q, ok := args["query"].(string)
		if !ok || strings.TrimSpace(q) == "" {
			return toolError("query must be a non-empty string")
		}
		queries = []string{q}
	} else {
		raw, err := json.Marshal(args["queries"])
		if err != nil || json.Unmarshal(raw, &queries) != nil || len(queries) == 0 {
			return toolError("queries must be a non-empty array of strings")
		}
		for _, q := range queries {
			if strings.TrimSpace(q) == "" {
				return toolError("queries must contain non-empty strings")
			}
		}
	}
	all := true
	if value, present := args["scope"]; present {
		scope, ok := value.(string)
		if !ok || scope != "project" && scope != "all" {
			return toolError("scope must be project or all")
		}
		all = scope == "all"
	}
	opts, err := recallOptions(args)
	if err != nil {
		return toolError(err.Error())
	}
	searcher, ok := s.backend.(interface {
		SearchScoped(context.Context, string, []string, int, bool, memory.RecallOptions) (memory.ScopedSearchResult, error)
	})
	if !ok {
		return toolError(memory.ErrUtilityUnsupported.Error())
	}
	peeker, ok := s.backend.(interface {
		CheckpointPeekPending(string) (*session.PendingCheckpoint, error)
	})
	if !ok {
		return toolError(memory.ErrUtilityUnsupported.Error())
	}
	// Peek before retrieval: unsupported or corrupt checkpoint state must not
	// masquerade as absence or incur fact accesses on a failed search.
	pending, err := peeker.CheckpointPeekPending(agentID)
	if err != nil {
		return toolError(fmt.Sprintf("checkpoint pointer: %v", err))
	}
	result, err := searcher.SearchScoped(ctx, agentID, queries, getInt(args, "top_k", 0), all, opts)
	if err != nil {
		return toolError(fmt.Sprintf("recall error: %v", err))
	}
	if !explain {
		return s.compactCoreSearch(ctx, agentID, result, pending, remainingEvidence)
	}
	out := coreSearchResult{Facts: make([]coreFactMetadata, 0, len(result.Facts)), PerQuery: result.PerQuery, SubjectIssues: result.SubjectIssues}
	if explain {
		count := len(result.Facts)
		out.Count = &count
		out.Explained = make([]coreReceipt, 0, count)
	}
	var text strings.Builder
	for i, f := range result.Facts {
		age := int(math.Floor(math.Max(0, f.AgeDays)))
		meta := coreFactMetadata{Index: i, ID: f.ID, AgeDays: &age, Subjects: f.Subjects, Shared: f.Namespace == memory.SharedAgentID && agentID != memory.SharedAgentID}
		if explain {
			r := f.Receipt
			if r == nil || f.ID == "" || r.Provenance.FactID != f.ID || r.Provenance.WrittenAt.IsZero() || r.Ranks.K <= 0 || r.Ranks.RecencyRank <= 0 {
				return toolError("explain could not return a complete ranking receipt for a selected fact")
			}
			meta.Explained = &coreReceipt{QueryIndex: f.ReceiptQuery, Weight: r.Weight, AgeDays: r.AgeDays, Ranks: r.Ranks, Provenance: r.Provenance, KGLinks: r.KGLinks, Ranking: r.Ranking}
			// Legacy consumers read the top-level array. The indexed receipt
			// remains available to clients of the compact response.
			out.Explained = append(out.Explained, *meta.Explained)
		}
		var evidenceSuffix string
		if f.Evidence != nil {
			ref := memory.CitationReference{Namespace: f.Namespace, FactID: f.ID, ContentHash: f.ContentHash}
			if ref.Namespace == "" || ref.FactID == "" || len(ref.ContentHash) != 64 {
				return toolError("search citation has no exact version identity")
			}
			complete := meta
			complete.Evidence, complete.EvidenceRef = f.Evidence, &ref
			evidenceSuffix = citationSuffix(memory.CitationResult{Reference: ref, Evidence: f.Evidence})
			cost := citationWireBytes(meta, complete, evidenceSuffix)
			if cost <= remainingEvidence {
				meta = complete
				remainingEvidence -= cost
			} else {
				meta.EvidenceRef, meta.EvidenceOmitted = &ref, true
				marker, _ := json.Marshal(ref)
				evidenceSuffix = " evidence omitted; fetch=" + string(marker)
			}
		}
		out.Facts = append(out.Facts, meta)
		if i > 0 {
			text.WriteByte('\n')
		}
		fmt.Fprintf(&text, "%d. ", i+1)
		if meta.Shared {
			text.WriteString("[shared] ")
		}
		encoded, _ := json.Marshal(f.Text)
		text.Write(encoded)
		shortID := f.ShortID
		if shortID == "" {
			shortID = f.ID
		}
		encodedID, _ := json.Marshal(shortID)
		fmt.Fprintf(&text, " id=%s age=%dd", encodedID, age)
		if len(f.Subjects) != 0 {
			subjects, _ := json.Marshal(f.Subjects)
			text.WriteString(" subjects=" + string(subjects))
		}
		text.WriteString(evidenceSuffix)
	}
	if len(result.Facts) == 0 {
		text.WriteString("No memories found.")
	}
	if result.Feedback != "" {
		text.WriteString("\n" + result.Feedback)
	}
	if result.Retrieval != nil {
		text.WriteString("\n" + result.Retrieval.Text())
	}
	writeReviewNotices(&text, result.ReviewNotices)
	if result.GraphHints != "" {
		text.WriteString("\n" + result.GraphHints)
	}
	if pending != nil {
		encodedID, _ := json.Marshal(pending.ID)
		fmt.Fprintf(&text, "\nPending checkpoint %s; use checkpoint_resume.", encodedID)
	}
	return toolStructured(out, appendSubjectWarning(text.String(), out.SubjectIssues))
}

func (s *Server) handleMemoryAdd(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	if err := s.citationArguments("memory_add", args); err != nil {
		return toolError(err.Error())
	}
	if s.fullCatalog {
		if _, present := args["facts"]; present {
			return toolError("facts requires the core tools profile; restart mcp serve --tools core")
		}
		return s.handleMemoryAddLegacy(ctx, req)
	}
	agentID, ok := getString(args, "agent_id")
	if !ok || strings.TrimSpace(agentID) == "" {
		return toolError("agent_id is required")
	}
	_, hasText := args["text"]
	_, hasFacts := args["facts"]
	if hasText == hasFacts {
		return toolError("pass exactly one of text or facts")
	}
	var inputs []memory.WriteFact
	if hasText {
		text, ok := args["text"].(string)
		if !ok {
			return toolError("text must be a string")
		}
		opts, err := s.coreAddOptions(ctx, agentID, args)
		if err != nil {
			return toolError(err.Error())
		}
		inputs = []memory.WriteFact{{Text: text, Options: opts}}
	} else {
		for _, key := range []string{"confidence", "replaces"} {
			if _, exists := args[key]; exists {
				return toolError("put " + key + " inside each facts object")
			}
		}
		raw, err := json.Marshal(args["facts"])
		if err != nil {
			return toolError("facts must be an array of objects")
		}
		var rows []map[string]any
		if err = json.Unmarshal(raw, &rows); err != nil || len(rows) == 0 {
			return toolError("facts must be a non-empty array of objects")
		}
		if _, present := args["subject"]; present && len(rows) != 1 {
			return toolError("put subject inside each facts object")
		}
		for i, row := range rows {
			for key := range row {
				if key != "text" && key != "confidence" && key != "source" && key != "quote" && key != "subject" && key != "replaces" {
					return toolError(fmt.Sprintf("facts[%d]: unsupported field %q", i, key))
				}
			}
			text, ok := row["text"].(string)
			if !ok {
				return toolError(fmt.Sprintf("facts[%d].text must be a string", i))
			}
			for _, key := range []string{"source", "quote", "subject"} {
				if value, present := args[key]; present {
					if _, specified := row[key]; !specified {
						row[key] = value
					}
				}
			}
			opts, err := s.coreAddOptions(ctx, agentID, row)
			if err != nil {
				return toolError(fmt.Sprintf("facts[%d]: %v", i, err))
			}
			inputs = append(inputs, memory.WriteFact{Text: text, Options: opts})
		}
	}
	if err := memory.ValidateWriteFacts(agentID, inputs); err != nil {
		return toolError(err.Error())
	}
	for _, input := range inputs {
		if input.Options.Source != nil || input.Options.Quote != nil {
			if support, ok := s.backend.(interface{ SupportsCitationEvidence() bool }); !ok || !support.SupportsCitationEvidence() {
				return toolError(memory.ErrCitationUnsupported.Error())
			}
			break
		}
	}
	receipts, err := s.putCoreCitationReceipts(ctx, agentID, inputs)
	if err != nil {
		var committed []string
		for _, receipt := range receipts {
			if receipt.Fact.ID != "" {
				committed = append(committed, receipt.Fact.ID)
			}
		}
		if len(committed) != 0 {
			return toolError("Write committed as " + strings.Join(committed, ", ") + "; follow-up error: " + err.Error())
		}
		if detail := s.targetIDDiagnostic(ctx, agentID, err); detail != err.Error() {
			return toolError(detail)
		}
		return toolError(s.coreAddError(ctx, agentID, inputs, err).Error())
	}
	if len(receipts) != len(inputs) {
		return toolError("batch write returned incomplete committed identities")
	}
	return s.compactCoreWrite(ctx, agentID, receipts)
}
