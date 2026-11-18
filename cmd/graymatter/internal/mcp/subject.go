package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

type subjectWriter interface {
	PutManyReturningReceipts(context.Context, string, []memory.WriteFact) ([]memory.WriteReceipt, error)
}

type subjectTargetResolver interface {
	ResolveFactTargets(context.Context, string, string, string, bool) ([]memory.Fact, error)
}

func (s *Server) subjectIssues(ctx context.Context, namespace string) (*memory.SubjectIssues, error) {
	reader, ok := s.backend.(rpc.SubjectIssueReader)
	if !ok {
		return nil, nil
	}
	issues, err := reader.SubjectIssues(ctx, []string{namespace})
	if err != nil || issues.Total == 0 {
		return nil, err
	}
	return &issues, nil
}

func appendSubjectWarning(text string, issues *memory.SubjectIssues) string {
	warning := issues.Warning()
	if warning == "" {
		return text
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + warning
}

type coreWriteMetadata struct {
	Index            int                        `json:"i"`
	Replaced         []string                   `json:"replaced,omitempty"`
	Subjects         []memory.SubjectProjection `json:"subjects,omitempty"`
	Suggestions      []string                   `json:"suggestions,omitempty"`
	SuggestionsTotal int                        `json:"suggestions_total,omitempty"`
}

func hasSubjectIntent(opts memory.WriteOptions) bool {
	return opts.Subject != nil || opts.Replaces != nil
}

func supportsSubjects(backend Backend) bool {
	support, ok := backend.(interface{ SupportsSubjectHeads() bool })
	return ok && support.SupportsSubjectHeads()
}

func (s *Server) subjectArguments(tool string, args map[string]any) error {
	for _, key := range []string{"subject", "replaces", "target_type"} {
		value, present := args[key]
		if !present {
			continue
		}
		if s.fullCatalog {
			return fmt.Errorf("%s requires the core tools profile; restart mcp serve --tools core", key)
		}
		if key == "target_type" {
			if tool != "memory_reflect" || args["action"] != "update" && args["action"] != "forget" && args["action"] != "pin" && args["action"] != "unpin" {
				return fmt.Errorf("target_type is supported only by reflect update/forget/pin/unpin")
			}
			selector, ok := value.(string)
			if !ok || selector != "id" && selector != "text" {
				return fmt.Errorf("target_type must be id or text")
			}
		} else if tool != "memory_add" && (tool != "memory_reflect" || args["action"] != "add") {
			return fmt.Errorf("%s is supported only by memory_add or reflect add", key)
		}
	}
	return nil
}

// Determine the write route before dispatch. An error from a chosen writer
// never permits a fallback: the operation may already have committed.
func (s *Server) putCoreReceipts(ctx context.Context, namespace string, inputs []memory.WriteFact) ([]memory.WriteReceipt, error) {
	if supportsSubjects(s.backend) {
		writer, ok := s.backend.(subjectWriter)
		if !ok {
			return nil, memory.ErrSubjectUnsupported
		}
		if resolver, ok := s.backend.(subjectTargetResolver); ok {
			for i := range inputs {
				options := &inputs[i].Options
				if options.Subject == nil || len(options.Replaces) != 1 {
					continue
				}
				target := options.Replaces[0]
				facts, err := resolver.ResolveFactTargets(ctx, namespace, target, "id", false)
				if err != nil {
					// A recorded anchor can still authorize recovery after its
					// canonical row disappears. The store verifies that case.
					if err.Error() == fmt.Sprintf("target ID %q has no eligible canonical row", target) {
						continue
					}
					return nil, err
				}
				if len(facts) != 1 || facts[0].AgentID != namespace || facts[0].IsSuperseded() ||
					!strings.HasPrefix(facts[0].ID, target) {
					return nil, fmt.Errorf("recovery target did not resolve to one live fact")
				}
				options.Replaces = []string{facts[0].ID}
			}
		}
		return writer.PutManyReturningReceipts(ctx, namespace, inputs)
	}
	for _, input := range inputs {
		if hasSubjectIntent(input.Options) {
			return nil, memory.ErrSubjectUnsupported
		}
	}
	writer, ok := s.backend.(interface {
		PutManyReturningFacts(context.Context, string, []memory.WriteFact) ([]memory.Fact, error)
	})
	if !ok {
		return nil, memory.ErrUtilityUnsupported
	}
	facts, err := writer.PutManyReturningFacts(ctx, namespace, inputs)
	out := make([]memory.WriteReceipt, len(facts))
	for i, f := range facts {
		out[i].Fact = f
	}
	return out, err
}

func (s *Server) curationTargets(ctx context.Context, namespace, target string, args map[string]any, includeRetired bool) ([]memory.Fact, error) {
	selector, _ := args["target_type"].(string)
	if !s.fullCatalog && supportsSubjects(s.backend) {
		resolver, ok := s.backend.(subjectTargetResolver)
		if !ok {
			return nil, memory.ErrSubjectUnsupported
		}
		return resolver.ResolveFactTargets(ctx, namespace, target, selector, includeRetired)
	}
	if selector == "id" {
		return nil, memory.ErrSubjectUnsupported
	}
	facts, err := s.backend.List(namespace)
	if err != nil {
		return nil, err
	}
	matched := findByText(facts, target, includeRetired)
	if !s.fullCatalog && len(matched) == 0 && selector != "text" {
		return nil, memory.ErrSubjectUnsupported
	}
	return matched, nil
}

func (s *Server) targetResolutionError(err error) string {
	if s.fullCatalog {
		return fmt.Sprintf("list facts: %v", err)
	}
	return fmt.Sprintf("resolve target: %v", err)
}

func factPrefix(text string) string {
	runes := []rune(text)
	if len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	return text
}

// Only IDs and indices are duplicated as metadata. The replaced/suggested
// fact prefix is emitted once, JSON-escaped, in the textual receipt.
func writeReceiptDetails(index int, receipt memory.WriteReceipt) (coreWriteMetadata, string) {
	meta := coreWriteMetadata{Index: index, Subjects: receipt.Subjects, SuggestionsTotal: receipt.SuggestionsTotal}
	var text strings.Builder
	for _, old := range receipt.Replaced {
		meta.Replaced = append(meta.Replaced, old.ID)
		encoded, _ := json.Marshal(factPrefix(old.Text))
		id, _ := json.Marshal(old.ID)
		fmt.Fprintf(&text, " Replaced %s %s.", id, encoded)
	}
	for _, similar := range receipt.Suggestions {
		meta.Suggestions = append(meta.Suggestions, similar.ID)
		encoded, _ := json.Marshal(factPrefix(similar.Text))
		id, _ := json.Marshal(similar.ID)
		fmt.Fprintf(&text, " Similar %s %s; unchanged.", id, encoded)
	}
	if receipt.SuggestionsTotal > len(receipt.Suggestions) {
		fmt.Fprintf(&text, " Similar suggestions shown %d/%d.", len(receipt.Suggestions), receipt.SuggestionsTotal)
	}
	return meta, text.String()
}

func hasWriteMetadata(meta coreWriteMetadata) bool {
	return len(meta.Replaced) != 0 || len(meta.Subjects) != 0 || len(meta.Suggestions) != 0 || meta.SuggestionsTotal != 0
}
