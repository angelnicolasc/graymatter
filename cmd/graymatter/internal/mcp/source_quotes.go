package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const (
	sourceQuoteConflict      = "Not stored. Source %q already has this quote: %q. One source keeps one quote. Resend with a quote that contains it, the whole source text, or reuse it exactly."
	sourceLongQuoteConflict  = "Not stored. Source %q already has a quote on %s; read it with memory_search id. One source keeps one quote. Resend with a quote that contains it, the whole source text, or reuse it exactly."
	sourceBatchQuoteConflict = "Not stored. Facts from source %q carry different quotes. One source keeps one quote: give the whole source text once for the call."
	sourceConflictQuoteLimit = 600
)

type sourceQuote struct {
	text string
	id   string
}

type sourceQuotes map[string][]sourceQuote

func (s *Server) listSourceQuotes(namespace string) (sourceQuotes, error) {
	facts, err := s.backend.List(namespace)
	if err != nil {
		return nil, err
	}
	sources := make(sourceQuotes)
	for _, fact := range facts {
		if fact.AgentID == namespace && !fact.IsSuperseded() && fact.Evidence != nil &&
			fact.Evidence.Source != "" && fact.Evidence.Quote != "" {
			source := fact.Evidence.Source
			sources[source] = append(sources[source], sourceQuote{text: fact.Evidence.Quote, id: fact.ID})
		}
	}
	return sources, nil
}

func (quotes sourceQuotes) longestContaining(source, own string) string {
	longest := own
	for _, candidate := range quotes[source] {
		if strings.Contains(candidate.text, own) &&
			(len(candidate.text) > len(longest) || len(candidate.text) == len(longest) && candidate.text < longest) {
			longest = candidate.text
		}
	}
	return longest
}

func compatibleSourceQuotes(left, right string) bool {
	return strings.Contains(left, right) || strings.Contains(right, left)
}

func (s *Server) putCoreCitationReceipts(ctx context.Context, namespace string, inputs []memory.WriteFact) ([]memory.WriteReceipt, error) {
	incoming := make(map[string][]string)
	for _, input := range inputs {
		source, quote := input.Options.Source, input.Options.Quote
		if source == nil || quote == nil || *source == "" || *quote == "" {
			continue
		}
		for _, earlier := range incoming[*source] {
			if !compatibleSourceQuotes(earlier, *quote) {
				return nil, fmt.Errorf(sourceBatchQuoteConflict, *source)
			}
		}
		incoming[*source] = append(incoming[*source], *quote)
	}
	if len(incoming) == 0 {
		return s.putCoreReceipts(ctx, namespace, inputs)
	}

	s.citationWriteMu.Lock()
	defer s.citationWriteMu.Unlock()
	saved, err := s.listSourceQuotes(namespace)
	if err != nil {
		return nil, fmt.Errorf("citation lookup: %w", err)
	}
	for _, input := range inputs {
		source, quote := input.Options.Source, input.Options.Quote
		if source == nil || quote == nil || *source == "" || *quote == "" {
			continue
		}
		if err := s.checkSourceQuote(ctx, namespace, *source, *quote, saved[*source]); err != nil {
			return nil, err
		}
	}
	return s.putCoreReceipts(ctx, namespace, inputs)
}

func (s *Server) checkSourceQuote(ctx context.Context, namespace, source, quote string, saved []sourceQuote) error {
	var conflict *sourceQuote
	for i := range saved {
		if saved[i].text == quote {
			// An exact reuse remains possible for data written outside core.
			return nil
		}
		if !compatibleSourceQuotes(saved[i].text, quote) &&
			(conflict == nil || len(saved[i].text) > len(conflict.text)) {
			conflict = &saved[i]
		}
	}
	if conflict == nil {
		// A substring common to two incompatible legacy quotes must
		// neither extend just one branch nor introduce a third branch.
		for i := range saved {
			if strings.Contains(quote, saved[i].text) {
				continue
			}
			for j := range saved {
				if !compatibleSourceQuotes(saved[i].text, saved[j].text) {
					conflict = &saved[i]
					break
				}
			}
			if conflict != nil {
				break
			}
		}
	}
	if conflict == nil {
		return nil
	}
	if len(conflict.text) > sourceConflictQuoteLimit {
		return fmt.Errorf(sourceLongQuoteConflict, source, s.compactCoreID(ctx, namespace, conflict.id))
	}
	return fmt.Errorf(sourceQuoteConflict, source, conflict.text)
}
