package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/contextblock"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// context-sync projects the store's live facts into a managed block inside
// CLAUDE.md / AGENTS.md, so every prompt carries the currently-true summary
// instead of a file that rots. Opt-in by flag, additive by construction:
// nothing outside the markers is ever touched, a .bak of the previous file is
// kept across every rewrite, and a hand edit is detected against the recorded
// hash and reported — warned about, never silently discarded.

type ctxSyncResult struct {
	File               string `json:"file"`
	Changed            bool   `json:"changed"`
	BackedUp           bool   `json:"backup_created"`
	Facts              int    `json:"facts_selected"`
	Considered         int    `json:"facts_considered"`
	Tokens             int    `json:"tokens_used"`
	ManualEditDetected bool   `json:"manual_edit_detected"`
}

func contextSyncCmd() *cobra.Command {
	var (
		file   string
		agent  string
		budget int
		dryRun bool
		check  bool
	)
	cmd := &cobra.Command{
		Use:   "context-sync",
		Short: "Project live memory into a pruned context block in CLAUDE.md / AGENTS.md",
		Long: `Renders the highest-weight live facts into a managed block that the
agent reads on every session, keeping it inside an explicit token budget.

The block is opt-in and additive: content outside the markers is never
touched, every rewrite leaves the previous file as <file>.bak, and edits
made by hand are detected against the recorded hash and reported before
the next sync replaces them.

Concurrent syncs from several processes serialize their store reads
through the daemon but not the target-file write: last writer wins, and
the next sync converges the block. Nothing corrupts; avoid racing syncs
against one file.

Use --check to report block state without writing; --dry-run to preview
the exact bytes.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if budget <= 0 {
				budget = contextblock.DefaultBudgetTokens
			}
			if budget < contextblock.MinBudgetTokens {
				return fmt.Errorf("budget %d below minimum %d", budget, contextblock.MinBudgetTokens)
			}
			path := resolveContextFile(file)

			if check {
				return runContextCheck(cmd, path)
			}

			s, err := openStore()
			if err != nil {
				return err
			}
			defer s.Close()
			if s.IsReadOnly() {
				return fmt.Errorf("store is read-only (another process holds the write lock); not safe to project")
			}

			body, st, err := projectFacts(s, agent, budget)
			if err != nil {
				return err
			}

			if dryRun {
				block := contextblock.RenderBlock(body, contextblock.SyncMeta{
					SHA256:   contextblock.HashBody(body),
					Facts:    st.Selected,
					SyncedAt: time.Now().UTC(),
				})
				_, err := fmt.Fprint(cmd.OutOrStdout(), adaptBlockToEndings(block, path))
				return err
			}

			res, err := writeContextBlock(path, body, st)
			if err != nil {
				return err
			}
			if res.ManualEditDetected {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"WARNING: %s was edited by hand since the last sync; the managed block was overwritten (previous file kept at %s.bak)\n",
					path, path)
			}
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			switch {
			case !res.Changed:
				fmt.Fprintf(cmd.OutOrStdout(), "Context block up to date (%s): %d fact(s), ~%d tokens.\n", res.File, res.Facts, res.Tokens)
			default:
				note := ""
				if res.BackedUp {
					note = ", previous file saved as .bak"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Context block written (%s): %d of %d fact(s), ~%d tokens%s.\n",
					res.File, res.Facts, res.Considered, res.Tokens, note)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "target instruction file (default: AGENTS.md, falling back to CLAUDE.md)")
	cmd.Flags().StringVar(&agent, "agent", "", "project only one agent's facts (default: every agent in the store)")
	cmd.Flags().IntVar(&budget, "budget", contextblock.DefaultBudgetTokens, "token budget for the rendered block")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the rendered block instead of writing it")
	cmd.Flags().BoolVar(&check, "check", false, "report block state and exit without writing")
	return cmd
}

// resolveContextFile picks the target: explicit --file wins; otherwise the
// first instruction file already present, else AGENTS.md as the canonical
// default (created on write).
func resolveContextFile(explicit string) string {
	if explicit != "" {
		return explicit
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}
	return "AGENTS.md"
}

// projectFacts gathers candidates (one agent or the whole store), selects
// under the budget and renders the full block. List returns tombstoned facts
// too; Select filters them out.
func projectFacts(s cliStore, agentFilter string, budget int) (string, contextblock.Stats, error) {
	agents := []string{agentFilter}
	if agentFilter == "" {
		var err error
		if agents, err = s.ListAgents(); err != nil {
			return "", contextblock.Stats{}, fmt.Errorf("list agents: %w", err)
		}
	}
	var facts []memory.Fact
	for _, a := range agents {
		fs, err := s.List(a)
		if err != nil {
			return "", contextblock.Stats{}, fmt.Errorf("list %s: %w", a, err)
		}
		facts = append(facts, fs...)
	}

	sel, st := contextblock.Select(facts, budget)
	body := contextblock.RenderBody(sel)
	return body, st, nil
}

// syncContextBlock renders and writes in one call — the whole pipeline the
// tests exercise. The command splits it (projectFacts / writeContextBlock)
// only so --dry-run can render without touching files.
func syncContextBlock(s cliStore, path, agentFilter string, budget int) (ctxSyncResult, error) {
	body, st, err := projectFacts(s, agentFilter, budget)
	if err != nil {
		return ctxSyncResult{File: path}, err
	}
	return writeContextBlock(path, body, st)
}

// writeContextBlock applies the safety sequence around one write: manual-edit
// detection first (against the hash recorded by the previous sync), then an
// idempotence check — a verified block whose body already matches is left
// completely alone, timestamp included, because rewriting only to move the
// synced-at line would churn the host file on every run — then line-ending
// adaptation, splice inside markers only, .bak backup of the original bytes,
// single write.
func writeContextBlock(path, body string, st contextblock.Stats) (ctxSyncResult, error) {
	res := ctxSyncResult{File: path, Facts: st.Selected, Considered: st.Considered, Tokens: st.TokensUsed}

	content, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return res, fmt.Errorf("read %s: %w", path, readErr)
	}
	existing := string(content)

	oldBody, _, verified, found := contextblock.Parse(existing)
	if found && !verified {
		res.ManualEditDetected = true
	}
	if found && verified && oldBody == body {
		return res, nil // nothing to say that the file does not already say
	}

	block := contextblock.RenderBlock(body, contextblock.SyncMeta{
		SHA256:   contextblock.HashBody(body),
		Facts:    st.Selected,
		SyncedAt: time.Now().UTC(),
	})
	next := contextblock.Splice(existing, adaptBlockToEndings(block, path))
	if next == existing {
		return res, nil
	}

	res.Changed = true
	res.BackedUp = existing != ""
	if res.BackedUp {
		if err := os.WriteFile(path+".bak", content, 0o644); err != nil {
			return res, fmt.Errorf("write backup %s.bak: %w", path, err)
		}
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return res, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return res, fmt.Errorf("write %s: %w", path, err)
	}
	return res, nil
}

// adaptBlockToEndings matches the target file's dominant line endings, so a
// CRLF host file does not end up half-and-half after a splice. Empty or
// missing targets keep LF.
func adaptBlockToEndings(block, path string) string {
	data, err := os.ReadFile(path)
	if err != nil || !usesCRLF(string(data)) {
		return block
	}
	return toCRLF(block)
}

// runContextCheck reports block state without writing anything.
func runContextCheck(cmd *cobra.Command, path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s does not exist; nothing synced yet.\n", path)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	_, meta, verified, found := contextblock.Parse(string(data))
	if !found {
		fmt.Fprintf(cmd.OutOrStdout(), "%s has no managed context block.\n", path)
		return nil
	}
	if jsonOut {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"file": path, "verified": verified, "facts": meta.Facts,
		})
	}
	if verified {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: context block verified (%d fact(s), last synced %s).\n",
			path, meta.Facts, meta.SyncedAt.Format("2006-01-02 15:04"))
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: context block was EDITED BY HAND since the last sync; the next context-sync will overwrite it (.bak is kept).\n", path)
	}
	return nil
}

