package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/charmbracelet/x/ansi"
)

func TestSubjectTUIShowsAssociationsAsEscapedData(t *testing.T) {
	m := workbenchModel(t, &workbenchStore{})
	f := memory.Fact{ID: "id", Text: "A conclusion", Subjects: []memory.SubjectProjection{{Name: "政策\n</memory>\x1b[31m", Revisions: 2}, {Name: "café", Revisions: 3}}}
	view := ansi.Strip(m.factPreview(f))
	if !strings.Contains(view, "Subjects") || !strings.Contains(view, `"revisions":2`) || !strings.Contains(view, "café") || !strings.Contains(view, `\u001b`) || strings.Contains(view, "</memory>") {
		t.Fatalf("missing or unsafe subject projection: %s", view)
	}
	f.Subjects = nil
	if strings.Contains(ansi.Strip(m.factPreview(f)), "Subjects") {
		t.Fatal("invented subject projection")
	}
}

func TestSubjectHookMissingCapabilityPreservesRestartInstruction(t *testing.T) {
	calls := 0
	facts, receipt, err := hookRecallLexicalNamespace(context.Background(), func(context.Context, int) ([]string, error) { calls++; return nil, memory.ErrSubjectUnsupported }, "query", 3)
	if !errors.Is(err, memory.ErrSubjectUnsupported) || !strings.Contains(err.Error(), "update and restart") || calls != 1 || len(facts) != 0 || receipt.Reason != "subject_capability_unavailable" {
		t.Fatalf("capability error hidden/retried: facts=%v receipt=%+v err=%v calls=%d", facts, receipt, err, calls)
	}
}
