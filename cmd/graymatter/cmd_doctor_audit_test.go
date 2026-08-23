package main

import (
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/docaudit"
)

// TestAuditExitCode pins the documented exit contract of doctor --audit:
// exit 1 only when a failure-level finding exists; warnings and clean runs
// stay at 0.
func TestAuditExitCode(t *testing.T) {
	cases := []struct {
		name string
		rep  *docaudit.Report
		want int
	}{
		{"nil report", nil, 0},
		{"clean", &docaudit.Report{}, 0},
		{"warnings only", &docaudit.Report{WarnCount: 3}, 0},
		{"one failure", &docaudit.Report{FailCount: 1, WarnCount: 2}, 1},
	}
	for _, tc := range cases {
		if got := auditExitCode(tc.rep); got != tc.want {
			t.Errorf("%s: auditExitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}
