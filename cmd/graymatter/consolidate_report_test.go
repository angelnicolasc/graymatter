package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/internal/providerurl"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/spf13/cobra"
)

type fixtureReportingStore struct {
	report memory.ConsolidationReport
	err    error
	runs   int
	closes int
}

func (s *fixtureReportingStore) ConsolidateWithReport(context.Context, string) (memory.ConsolidationReport, error) {
	s.runs++
	return s.report, s.err
}

func (s *fixtureReportingStore) Close() error {
	s.closes++
	return nil
}

func TestConsolidationReportingStoreSelection(t *testing.T) {
	dir := t.TempDir()
	for _, flag := range []bool{false, true} {
		for _, value := range []string{"", "1", "0", "true", "TRUE", "yes", "on", "01", " 1", "1 "} {
			count := 0
			direct := &fixtureReportingStore{}
			store, err := selectReportingStore(context.Background(), dir, flag, value, func(gotDir string) (reportingStore, error) {
				if gotDir != dir {
					t.Fatalf("direct opener dir = %q; want %q", gotDir, dir)
				}
				count++
				return direct, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if flag || value == "1" {
				if store != direct || count != 1 {
					t.Fatalf("selector (%v, %q) did not open direct exactly once: %T, %d", flag, value, store, count)
				}
			} else if lazy, ok := store.(daemonReportingStore); !ok || lazy.dir != dir || count != 0 {
				t.Fatalf("selector (%v, %q) did not create a lazy daemon handle: %T, %d", flag, value, store, count)
			}
		}
	}
	wantErr := errors.New("direct open failed")
	store, err := selectReportingStore(context.Background(), dir, false, "1", func(string) (reportingStore, error) {
		return nil, wantErr
	})
	if store != nil || !errors.Is(err, wantErr) {
		t.Fatalf("direct failure unexpectedly selected a fallback: %T, %v", store, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = selectReportingStore(ctx, dir, true, "", func(string) (reportingStore, error) {
		t.Fatal("canceled caller opened direct store")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled selector error = %v", err)
	}
}

func TestConsolidationReportingDirectCLI(t *testing.T) {
	binary := buildE2EBinary(t)
	root := t.TempDir()
	type selection struct {
		env   []string
		flags []string
	}
	forms := []selection{
		{[]string{"GRAYMATTER_NO_DAEMON=1"}, nil},
		{nil, []string{"--no-daemon"}},
		{[]string{"GRAYMATTER_NO_DAEMON=1"}, []string{"--no-daemon=false"}},
	}
	for _, outputFlag := range []string{"--json", "--json=false"} {
		for _, form := range forms {
			store := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			args := append([]string{"--dir", store, outputFlag}, form.flags...)
			args = append(args, "consolidate", "audit")
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Dir, cmd.Env = root, append(providerURLCLIEnv(root), form.env...)
			var out, errOut bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errOut
			err := cmd.Run()
			cancel()
			if err != nil || errOut.Len() != 0 {
				t.Fatalf("real CLI %v %v: %v: %s", form.env, form.flags, err, &errOut)
			}
			if outputFlag == "--json" {
				var payload consolidationCLIResult
				if err := json.Unmarshal(out.Bytes(), &payload); err != nil || payload.Summary == nil ||
					payload.Summary.Reason != "no_facts" || payload.Error != nil || strings.Count(out.String(), "\n") != 1 {
					t.Fatalf("direct JSON = %s, %v", &out, err)
				}
			} else if !strings.Contains(out.String(), "Summary skipped") {
				t.Fatalf("direct human = %s", &out)
			}
			if _, err := os.Stat(filepath.Join(store, "gray.db")); err != nil {
				t.Fatalf("direct DB not created: %v", err)
			}
			for _, artifact := range []string{"graymatter.addr", "daemon.log", "daemon.pid"} {
				if _, err := os.Stat(filepath.Join(store, artifact)); !os.IsNotExist(err) {
					t.Fatalf("direct CLI touched daemon artifact %s: %v", artifact, err)
				}
			}
		}
	}
}

func TestConsolidationReportingOutputMatrix(t *testing.T) {
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	skipped := memory.ConsolidationReport{Status: memory.ConsolidationSkipped, Reason: "threshold_not_exceeded"}
	applied := memory.ConsolidationReport{
		Status: memory.ConsolidationApplied, Reason: "summary_applied", Provider: "anthropic", SummaryFactID: id, SourcesRetired: 1,
	}
	failed := memory.ConsolidationReport{
		Status: memory.ConsolidationFailed, Reason: "provider_error", Provider: "anthropic",
		Diagnostic: &memory.ConsolidationDiagnostic{Code: "provider_http_error", Provider: "anthropic", HTTPStatus: 401, Message: "WIRE-CANARY"},
	}
	partial := memory.ConsolidationReport{
		Status: memory.ConsolidationPartial, Reason: "source_retirement_failed", Provider: "anthropic", SummaryFactID: id,
		Diagnostic: &memory.ConsolidationDiagnostic{Code: "source_retirement_failed"},
	}
	uncertain := memory.ConsolidationReport{
		Status: memory.ConsolidationUncertain, Reason: "source_retirement_uncertain",
		Provider: "anthropic", SummaryFactID: id, SourcesRetired: 1,
		Diagnostic: &memory.ConsolidationDiagnostic{Code: "source_retirement_uncertain"},
	}
	unconfirmedSummary := memory.ConsolidationReport{
		Status: memory.ConsolidationUncertain, Reason: "summary_commit_uncertain", Provider: "anthropic",
		Diagnostic: &memory.ConsolidationDiagnostic{Code: "summary_commit_uncertain"},
	}
	notAttempted := memory.ConsolidationReport{Status: memory.ConsolidationNotAttempted, Reason: "aborted_before_summary"}
	badID := applied
	badID.SummaryFactID = "WIRE-CANARY\r\n"
	cause := errors.New("ERROR-CANARY")
	for _, tc := range []struct {
		name     string
		summary  memory.ConsolidationReport
		fatal    error
		openErr  error
		code     string
		wantText []string
		ok       bool
	}{
		{name: "applied", summary: applied, wantText: []string{id, "Summary applied"}, ok: true},
		{name: "skipped", summary: skipped, wantText: []string{"skipped", "threshold"}, ok: true},
		{name: "failed-no-fatal", summary: failed, wantText: []string{"Summary failed", "HTTP 401"}},
		{name: "failed-and-fatal", summary: failed, fatal: cause, code: "store_error", wantText: []string{"Summary failed", "HTTP 401", "stopped"}},
		{name: "skipped-and-fatal", summary: skipped, fatal: cause, code: "store_error", wantText: []string{"skipped", "threshold", "stopped"}},
		{name: "applied-and-fatal", summary: applied, fatal: cause, code: "store_error", wantText: []string{id, "committed", "stopped"}},
		{name: "partial", summary: partial, fatal: memory.NewConsolidationError("source_retirement_failed", cause),
			code: "source_retirement_failed", wantText: []string{id, "committed", "0 source retirements"}},
		{name: "uncertain", summary: uncertain, fatal: memory.NewConsolidationError("source_retirement_uncertain", cause),
			code: "source_retirement_uncertain", wantText: []string{id, "1 source retirements confirmed", "additional retirements may have committed"}},
		{name: "unconfirmed-summary", summary: unconfirmedSummary, fatal: memory.NewConsolidationError("summary_commit_uncertain", cause),
			code: "summary_commit_uncertain", wantText: []string{"summary commit could not be confirmed", "Inspect the store before retrying"}},
		{name: "not-attempted", summary: notAttempted, fatal: context.Canceled, code: "canceled", wantText: []string{"operation was canceled"}},
		{name: "hostile-id", summary: badID, code: "outcome_unknown", wantText: []string{"outcome was known"}},
		{name: "open-error", openErr: cause, code: "store_error", wantText: []string{"memory-store operation failed"}},
		{name: "predispatch-deadline", openErr: context.DeadlineExceeded, code: "deadline_exceeded", wantText: []string{"exceeded its deadline"}},
		{name: "open-config-error", openErr: &providerurl.ConfigError{Provider: "openai", Reason: "ERROR-CANARY"},
			code: "config_invalid_url", wantText: []string{"Invalid provider base URL configuration"}},
		{name: "unsupported", fatal: memory.NewConsolidationError("unsupported_capability", cause),
			code: "unsupported_capability", wantText: []string{"Consolidation reporting is unavailable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, format := range []struct{ json, quiet bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
				store := &fixtureReportingStore{report: tc.summary, err: tc.fatal}
				opener := func(context.Context) (reportingStore, error) { return store, tc.openErr }
				var out, errOut bytes.Buffer
				err := runReportingCommandForTest(t, format.json, format.quiet, opener, &out, &errOut)
				if (err == nil) != tc.ok || (!tc.ok && !errors.Is(err, errConsolidationReported)) {
					t.Fatalf("success = %v, error %v", tc.ok, err)
				}
				if dump := out.String() + errOut.String(); strings.Contains(dump, "CANARY") || strings.Contains(dump, "Consolidation complete") {
					t.Fatalf("unsanitized or legacy CLI output: %s", dump)
				}
				if format.json {
					var payload consolidationCLIResult
					if err := json.Unmarshal(out.Bytes(), &payload); err != nil || errOut.Len() != 0 ||
						strings.Count(out.String(), "\n") != 1 || payload.SchemaVersion != 1 || payload.AgentID != "audit" {
						t.Fatalf("invalid JSON channel: %s, stderr %q, error %v", &out, &errOut, err)
					}
					if (payload.Summary != nil) != (tc.summary.Status != "" && tc.code != "outcome_unknown") {
						t.Fatalf("invalid summary in %s", &out)
					}
					if (payload.Error == nil) != (tc.code == "") || (payload.Error != nil && payload.Error.Code != tc.code) {
						t.Fatalf("invalid fatal diagnostic in %s", &out)
					}
				} else if tc.ok && format.quiet {
					if out.Len() != 0 || errOut.Len() != 0 {
						t.Fatalf("quiet success wrote output: %q, %q", &out, &errOut)
					}
				} else {
					target, empty := &out, &errOut
					if !tc.ok {
						target, empty = &errOut, &out
					}
					if empty.Len() != 0 {
						t.Fatalf("unexpected stream output: %s", empty)
					}
					for _, text := range tc.wantText {
						if !strings.Contains(target.String(), text) {
							t.Fatalf("missing %q in %s", text, target)
						}
					}
				}
			}
		})
	}
}

func TestConsolidationArgumentErrorsRemainCobraErrors(t *testing.T) {
	root := &cobra.Command{Use: "graymatter"}
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.AddCommand(consolidateCmdWithStore(func(context.Context) (reportingStore, error) {
		t.Fatal("invalid arguments opened a reporting store")
		return nil, nil
	}))
	root.SetArgs([]string{"consolidate"})
	err := root.Execute()
	if err == nil || errors.Is(err, errConsolidationReported) || !strings.Contains(errOut.String(), "accepts 1 arg(s)") {
		t.Fatalf("ordinary argument validation changed: %v; stdout=%q stderr=%q", err, &out, &errOut)
	}
}

func runReportingCommandForTest(t *testing.T, asJSON, isQuiet bool, open func(context.Context) (reportingStore, error), out, errOut io.Writer) error {
	t.Helper()
	prevJSON, prevQuiet := jsonOut, quiet
	defer func() { jsonOut, quiet = prevJSON, prevQuiet }()
	jsonOut, quiet = asJSON, isQuiet
	root := &cobra.Command{Use: "graymatter"}
	root.SetOut(out)
	root.SetErr(errOut)
	root.AddCommand(consolidateCmdWithStore(open))
	root.SetArgs([]string{"consolidate", "audit"})
	return root.Execute()
}

type reportErrorWriter struct {
	bytes.Buffer
	writeCount, prefix int
}

func (w *reportErrorWriter) Write(input []byte) (int, error) {
	w.writeCount++
	n := min(w.prefix, len(input))
	w.Buffer.Write(input[:n])
	return n, errors.New("WRITER-CANARY")
}

func (w *reportErrorWriter) WriteString(input string) (int, error) {
	return w.Write([]byte(input))
}

func TestConsolidationReportingReturnedOutputErrors(t *testing.T) {
	for _, prefix := range []int{0, 8} {
		for _, stderrBroken := range []bool{false, true} {
			out := &reportErrorWriter{prefix: prefix}
			badStderr, goodStderr := &reportErrorWriter{}, &bytes.Buffer{}
			var stderr io.Writer = goodStderr
			if stderrBroken {
				stderr = badStderr
			}
			store := &fixtureReportingStore{report: memory.ConsolidationReport{Status: memory.ConsolidationSkipped, Reason: "no_facts"}}
			err := runReportingCommandForTest(t, true, false, func(context.Context) (reportingStore, error) { return store, nil }, out, stderr)
			if !errors.Is(err, errConsolidationReported) || out.writeCount != 1 || out.Len() != prefix || store.runs != 1 || store.closes != 1 {
				t.Fatalf("output failure retried, was not handled or lost cleanup: %v, %+v, %+v", err, out, store)
			}
			if stderrBroken {
				if badStderr.writeCount != 1 {
					t.Fatalf("broken stderr received %d attempts", badStderr.writeCount)
				}
			} else if got := goodStderr.String(); got != "Could not write consolidation result.\n" {
				t.Fatalf("returned output error diagnostic = %q", got)
			}
		}
	}
	stderr := &reportErrorWriter{prefix: 4}
	store := &fixtureReportingStore{err: errors.New("ERROR-CANARY")}
	err := runReportingCommandForTest(t, false, true, func(context.Context) (reportingStore, error) { return store, nil }, io.Discard, stderr)
	if !errors.Is(err, errConsolidationReported) || stderr.writeCount != 2 || store.runs != 1 || store.closes != 1 {
		t.Fatalf("broken human error stream must try the failure and one fallback: %v, writes=%d, store=%+v", err, stderr.writeCount, store)
	}
}
