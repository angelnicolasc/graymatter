package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const subjectWriteAlternatives = "subject and replaces are alternatives"

func (s *Server) coreAddOptions(ctx context.Context, namespace string, args map[string]any) (memory.WriteOptions, error) {
	opts, err := writeOptions(args)
	if err != nil && err.Error() == subjectWriteAlternatives && opts.Subject != nil {
		for _, id := range opts.Replaces {
			missing := fmt.Errorf("target ID %q%s", id, missingCanonicalTarget)
			if detail := s.targetIDDiagnostic(ctx, namespace, missing); detail != missing.Error() {
				return opts, fmt.Errorf("%s", detail)
			}
		}
		err = s.subjectAlternativeError(ctx, namespace, opts)
	}
	return opts, err
}

func (s *Server) coreAddError(ctx context.Context, namespace string, inputs []memory.WriteFact, err error) error {
	switch err.Error() {
	case subjectWriteAlternatives, "put facts: " + subjectWriteAlternatives:
		var guidance []string
		var rows []int
		for i, input := range inputs {
			opts := input.Options
			if opts.Subject != nil && len(opts.Replaces) == 1 {
				guidance = append(guidance, s.subjectAlternativeError(ctx, namespace, opts).Error())
				rows = append(rows, i)
			}
		}
		if len(guidance) == 1 {
			return fmt.Errorf("%s", guidance[0])
		}
		if len(guidance) > 1 {
			for i, detail := range guidance {
				guidance[i] = fmt.Sprintf("if facts[%d] is the conflict, %s", rows[i], detail)
			}
			return fmt.Errorf("%s; %s", subjectWriteAlternatives, strings.Join(guidance, "; "))
		}
	}
	return err
}

// The store decides whether a write is eligible for recovery. This read runs
// only after rejection and supplies guidance without reissuing the write.
func (s *Server) subjectAlternativeError(ctx context.Context, namespace string, opts memory.WriteOptions) error {
	resolver, ok := s.backend.(subjectTargetResolver)
	if ok {
		var guidance []string
		var subjects map[string]string
		for _, target := range opts.Replaces {
			selected, err := resolver.ResolveFactTargets(ctx, namespace, target, "id", true)
			if err != nil || len(selected) != 1 || selected[0].AgentID != namespace {
				continue
			}
			if subjects == nil {
				facts, err := s.backend.List(namespace)
				if err != nil {
					break
				}
				subjects = make(map[string]string)
				for _, fact := range facts {
					if len(fact.Subjects) == 1 {
						subjects[fact.ID] = fact.Subjects[0].Name
					}
				}
			}
			if name, ok := subjects[selected[0].ID]; ok {
				guidance = append(guidance, fmt.Sprintf("to replace %s, reuse its subject %q", target, name))
			}
		}
		if len(guidance) != 0 {
			return fmt.Errorf("%s", strings.Join(guidance, "; "))
		}
	}
	return fmt.Errorf("%s; to update subject %q, omit replaces; to replace by ID, omit subject and pass a current fact ID",
		subjectWriteAlternatives, *opts.Subject)
}
