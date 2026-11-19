package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) compactCoreSearch(ctx context.Context, namespace string, result memory.ScopedSearchResult, pending *session.PendingCheckpoint, remainingEvidence int) (*mcp.CallToolResult, error) {
	var text strings.Builder
	var sourcesByNamespace map[string]sourceQuotes
	for _, fact := range result.Facts {
		if text.Len() != 0 {
			text.WriteByte('\n')
		}
		id := fact.ShortID
		if id == "" {
			id = s.compactCoreID(ctx, fact.Namespace, fact.ID)
		}
		text.WriteString(id)
		if fact.Namespace == memory.SharedAgentID && namespace != memory.SharedAgentID {
			text.WriteString(" [shared]")
		}
		claim, _ := json.Marshal(fact.Text)
		text.WriteByte(' ')
		text.Write(claim)

		if fact.Evidence != nil {
			source, _ := json.Marshal(fact.Evidence.Source)
			text.WriteString(" source=")
			text.Write(source)
			quote := fact.Evidence.Quote
			if fact.Evidence.Source != "" {
				if sourcesByNamespace == nil {
					sourcesByNamespace = make(map[string]sourceQuotes, 2)
				}
				sources, loaded := sourcesByNamespace[fact.Namespace]
				if !loaded {
					var err error
					sources, err = s.listSourceQuotes(fact.Namespace)
					if err != nil {
						return toolError(fmt.Sprintf("citation lookup: %v", err))
					}
					sourcesByNamespace[fact.Namespace] = sources
				}
				quote = sources.longestContaining(fact.Evidence.Source, quote)
			}
			suffix := " literal"
			if quote != fact.Text {
				encoded, _ := json.Marshal(quote)
				suffix = " quote=" + string(encoded)
			}
			wire, _ := json.Marshal(suffix)
			cost := len(wire) - 2
			if cost > remainingEvidence {
				text.WriteString(" citation=omitted")
			} else {
				text.WriteString(suffix)
				remainingEvidence -= cost
			}
		}
		for _, subject := range fact.Subjects {
			name, _ := json.Marshal(subject.Name)
			text.WriteString(" subject=")
			text.Write(name)
		}
	}
	if text.Len() == 0 {
		text.WriteString("No memories found.")
	}
	if feedback := compactVocabularyNotice(result); feedback != "" {
		text.WriteByte('\n')
		text.WriteString(feedback)
	}
	if result.Retrieval != nil {
		text.WriteString("\n" + result.Retrieval.Text())
	}
	if len(result.ReviewNotices) != 0 {
		text.WriteString("\nOpen memory conflicts affect these results. Use explain for details.")
	}
	if pending != nil {
		id, _ := json.Marshal(pending.ID)
		fmt.Fprintf(&text, "\nPending checkpoint %s; use checkpoint_resume.", id)
	}
	return mcp.NewToolResultText(appendSubjectWarning(text.String(), result.SubjectIssues)), nil
}

func compactVocabularyNotice(result memory.ScopedSearchResult) string {
	if result.Feedback == "" || !result.HasUnshownFacts {
		return ""
	}
	for _, line := range strings.Split(result.Feedback, "\n") {
		terms, ok := strings.CutPrefix(line, "nearby vocabulary: ")
		if !ok {
			continue
		}
		notice := "Weak match. Try "
		separator := ""
		for _, term := range strings.Split(terms, ", ") {
			if len(notice)+len(separator)+len(term)+1 > 160 {
				break
			}
			notice += separator + term
			separator = ", "
		}
		if separator != "" {
			return notice + "."
		}
	}
	return "Weak match. Try a more specific term."
}

func (s *Server) compactCoreWrite(ctx context.Context, namespace string, receipts []memory.WriteReceipt) (*mcp.CallToolResult, error) {
	ids := make([]string, len(receipts))
	short := make(map[string]string)
	idFor := func(id string) string {
		if value, ok := short[id]; ok {
			return value
		}
		value := s.compactCoreID(ctx, namespace, id)
		short[id] = value
		return value
	}
	var details strings.Builder
	for i, receipt := range receipts {
		if receipt.Fact.ID == "" {
			return toolError("batch write returned no committed identity")
		}
		ids[i] = idFor(receipt.Fact.ID)
		for _, replaced := range receipt.Replaced {
			prefix, _ := json.Marshal(factPrefix(replaced.Text))
			fmt.Fprintf(&details, " Replaced %s %s.", idFor(replaced.ID), prefix)
		}
		for _, similar := range receipt.Suggestions {
			prefix, _ := json.Marshal(factPrefix(similar.Text))
			fmt.Fprintf(&details, " Similar %s %s remains unchanged.", idFor(similar.ID), prefix)
		}
		if receipt.SuggestionsTotal > len(receipt.Suggestions) {
			fmt.Fprintf(&details, " Similar suggestions shown %d/%d.", len(receipt.Suggestions), receipt.SuggestionsTotal)
		}
	}
	return mcp.NewToolResultText("Stored " + strings.Join(ids, ", ") + "." + details.String()), nil
}

func (s *Server) compactCoreID(ctx context.Context, namespace, id string) string {
	resolver, ok := s.backend.(subjectTargetResolver)
	if !ok || !supportsSubjects(s.backend) {
		return id
	}
	for length := 8; length < len(id); length++ {
		if !utf8.RuneStart(id[length]) {
			continue
		}
		prefix := id[:length]
		facts, err := resolver.ResolveFactTargets(ctx, namespace, prefix, "id", true)
		if err == nil && len(facts) == 1 && facts[0].ID == id {
			return prefix
		}
		if ctx.Err() != nil {
			break
		}
	}
	return id
}
