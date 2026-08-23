package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/docaudit"
)

// runDoctorAudit is the `doctor --audit [path]` mode: a free auditor for
// instruction documents that requires no store, no daemon and no adoption.
// It reads, measures, reports — and writes nothing.
func runDoctorAudit(cmd *cobra.Command, root string) error {
	rep, err := docaudit.AuditPath(root, docaudit.Options{Now: time.Now()})
	if err != nil {
		return err
	}

	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		printAuditReport(cmd, rep)
	}
	if rep.FailCount > 0 {
		os.Exit(1)
	}
	return nil
}

func printAuditReport(cmd *cobra.Command, rep *docaudit.Report) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Auditing instruction documents under %q\n\n", rep.Root)
	fmt.Fprintf(out, "  Tokenizer: %s\n", rep.Tokenizer)
	for _, t := range rep.Thresholds {
		fmt.Fprintf(out, "  Threshold: %s\n", t)
	}
	fmt.Fprintln(out)

	if len(rep.Files) == 0 {
		fmt.Fprintln(out, "  No CLAUDE.md / AGENTS.md found at this location; nothing to audit.")
		return
	}

	for _, fr := range rep.Files {
		fmt.Fprintf(out, "%s — %d lines, ~%d tokens/prompt\n", fr.Path, fr.Lines, fr.Tokens)
		if len(fr.Blocks) > 0 {
			var parts []string
			for _, b := range fr.Blocks {
				s := fmt.Sprintf("%s L%d–L%d", b.Kind, b.Begin, b.End)
				if b.Verified != nil {
					if *b.Verified {
						s += " (verified)"
					} else {
						s += " (HASH MISMATCH)"
					}
				}
				parts = append(parts, s)
			}
			fmt.Fprintf(out, "  managed blocks: %s\n", join(parts, "; "))
		} else {
			fmt.Fprintln(out, "  managed blocks: none")
		}
		if len(fr.Duplicates) > 0 {
			for _, d := range fr.Duplicates {
				fmt.Fprintf(out, "  near-duplicate: lines %d ↔ %d (Jaccard %.2f)\n", d.ALine, d.BLine, d.Score)
			}
		}
		if st := fr.Staleness; st != nil {
			switch st.Available {
			case true:
				fmt.Fprintf(out, "  staleness: ≤30d %d · 31–90d %d · >90d %d · uncommitted %d (median %.0f days)\n",
					st.Recent, st.Aging, st.Stale, st.Uncommitted, st.MedianAgeDays)
			default:
				fmt.Fprintf(out, "  staleness: unavailable (%s)\n", st.Reason)
			}
		}
		fmt.Fprintln(out)
	}

	if len(rep.Findings) == 0 {
		fmt.Fprintln(out, "No findings.")
		return
	}
	glyph := map[docaudit.Severity]string{docaudit.SevInfo: "·", docaudit.SevWarn: "!", docaudit.SevFail: "✗"}
	for _, f := range rep.Findings {
		fmt.Fprintf(out, "  %s %-10s %s\n      %s\n", glyph[f.Severity], f.Check, f.File, f.Detail)
	}
	fmt.Fprintf(out, "\n%d warning(s), %d failure(s).\n", rep.WarnCount, rep.FailCount)
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
