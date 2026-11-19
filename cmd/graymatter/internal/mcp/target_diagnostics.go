package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

const missingCanonicalTarget = " has no eligible canonical row"

// targetIDDiagnostic explains a rejected ID without changing mutation selection.
// The exact legacy error is also the contract carried across the daemon RPC.
func (s *Server) targetIDDiagnostic(ctx context.Context, namespace string, err error) string {
	const writePrefix = "put facts: "
	original := err.Error()
	prefix := ""
	message := original
	if trimmed, ok := strings.CutPrefix(message, writePrefix); ok {
		prefix, message = writePrefix, trimmed
	}
	quoted, ok := strings.CutPrefix(message, "target ID ")
	if !ok {
		return original
	}
	quoted, ok = strings.CutSuffix(quoted, missingCanonicalTarget)
	if !ok {
		return original
	}
	id, err := strconv.Unquote(quoted)
	if err != nil || !displayedFactID(id) {
		return original
	}
	resolver, ok := s.backend.(subjectTargetResolver)
	if !ok {
		return original
	}
	fact, err := diagnosticFact(ctx, resolver, namespace, id)
	switch {
	case err != nil:
		return original
	case fact.ID != "" && !fact.IsSuperseded():
		return original
	case fact.ID != "":
		current, ok := liveReplacement(ctx, resolver, namespace, fact)
		if !ok {
			return original
		}
		if current != "" {
			return prefix + fmt.Sprintf("target ID %q was already replaced; use current ID %q", id,
				s.compactCoreID(ctx, namespace, current))
		}
		if fact.SupersededBy == memory.SupersededByAgent {
			return prefix + fmt.Sprintf("target ID %q was already retired and has no current replacement", id)
		}
		return prefix + fmt.Sprintf("target ID %q was already replaced and has no current replacement", id)
	}

	owner := diagnosticTargetOwner(ctx, s.backend, resolver, namespace, id)
	if owner == "" {
		return original
	}
	return prefix + fmt.Sprintf("target ID %q belongs to scope %q; pass agent_id %q", id, owner, owner)
}

// Only IDs in the format displayed by GrayMatter are candidates for a lookup
// beyond the requested namespace. The backend still verifies the exact prefix.
func displayedFactID(id string) bool {
	if len(id) < 8 || len(id) > 26 || id[0] > '7' {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		return !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", r)
	}) == -1
}

func diagnosticFact(ctx context.Context, resolver subjectTargetResolver, namespace, id string) (memory.Fact, error) {
	facts, err := resolver.ResolveFactTargets(ctx, namespace, id, "id", true)
	if err != nil {
		if err.Error() == fmt.Sprintf("target ID %q%s", id, missingCanonicalTarget) {
			return memory.Fact{}, nil
		}
		return memory.Fact{}, err
	}
	if len(facts) != 1 || facts[0].AgentID != namespace || !strings.HasPrefix(facts[0].ID, id) {
		return memory.Fact{}, fmt.Errorf("target diagnostic did not resolve one fact in its namespace")
	}
	return facts[0], nil
}

func liveReplacement(ctx context.Context, resolver subjectTargetResolver, namespace string, fact memory.Fact) (string, bool) {
	seen := map[string]bool{fact.ID: true}
	for id := fact.SupersededBy; id != "" && id != memory.SupersededByAgent; id = fact.SupersededBy {
		if seen[id] {
			return "", false
		}
		seen[id] = true
		next, err := diagnosticFact(ctx, resolver, namespace, id)
		if err != nil {
			return "", false
		}
		if next.ID == "" {
			return "", true
		}
		if next.ID != id || next.IsAlias() {
			return "", false
		}
		if !next.IsSuperseded() {
			return id, true
		}
		fact = next
	}
	return "", true
}

func diagnosticTargetOwner(ctx context.Context, backend Backend, resolver subjectTargetResolver, namespace, id string) string {
	lister, ok := backend.(interface{ ListAgents() ([]string, error) })
	if !ok {
		return ""
	}
	namespaces, err := lister.ListAgents()
	if err != nil {
		return ""
	}
	owner := ""
	for _, other := range namespaces {
		if other == namespace || other == owner {
			continue
		}
		fact, err := diagnosticFact(ctx, resolver, other, id)
		if err != nil {
			return ""
		}
		if fact.ID != "" {
			if owner != "" {
				return ""
			}
			owner = other
		}
	}
	return owner
}

func (b *DirectBackend) ListAgents() ([]string, error) {
	store := b.mem.Advanced()
	if store == nil {
		return nil, fmt.Errorf("memory store not initialised")
	}
	return store.ListAgents()
}
