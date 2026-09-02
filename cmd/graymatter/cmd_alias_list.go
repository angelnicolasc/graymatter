package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/spf13/cobra"
)

// aliasListCmd is the read half of the vocabulary surface.
//
// Aliases are non-injectable by construction: they never appear in a recall,
// never occupy a top-k slot, never contribute a document frequency. That is
// the property that makes them safe, and it is also what made them invisible
// — until now the only way to see what a store's vocabulary had become was to
// open bbolt and filter facts by kind.
//
// That was tolerable while every alias was authored by an agent that knew it
// had written one. It stopped being tolerable when the store started
// promoting its own (StoreConfig.UsageAliasLearning): a store that silently
// rewrites the queries it is asked, with no way to ask what rules it is
// applying, is a store you cannot debug and should not trust. The `source`
// column is the point — it separates what an agent taught from what the store
// concluded on its own.
func aliasListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list <agent-id>",
		Short: "Show the vocabulary a store has been taught, and what it taught itself",
		Long: `List the alias facts for an agent: the term, its equivalents, and who
authored each one.

  authored   written by an agent through ` + "`alias`" + ` / memory_alias
  usage      promoted by the store from observed reformulations, with no
             agent action (StoreConfig.UsageAliasLearning / GRAYMATTER_USAGE_ALIAS)

Retired aliases stop expanding queries and are hidden unless --all is given.`,
		Example: `  graymatter alias list backend
  graymatter alias list backend --all --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			facts, err := store.List(agentID)
			if err != nil {
				return fmt.Errorf("list facts: %w", err)
			}

			type row struct {
				Term        string   `json:"term"`
				Equivalents []string `json:"equivalents"`
				Source      string   `json:"source"`
				Retired     bool     `json:"retired"`
				FactID      string   `json:"fact_id"`
			}
			var rows []row
			for _, f := range facts {
				if f.Kind != memory.KindAlias {
					continue
				}
				retired := f.IsSuperseded()
				if retired && !all {
					continue
				}
				term, eqs := parseAliasText(f.Text)
				source := f.AliasSource
				if source == "" {
					// The zero value predates the field and means an agent
					// wrote it; usage promotion has always stamped itself.
					source = "authored"
				}
				rows = append(rows, row{term, eqs, source, retired, f.ID})
			}
			sort.Slice(rows, func(i, j int) bool {
				if rows[i].Source != rows[j].Source {
					return rows[i].Source < rows[j].Source
				}
				return rows[i].Term < rows[j].Term
			})

			if jsonOut {
				data, err := json.Marshal(map[string]any{
					"agent_id": agentID,
					"aliases":  rows,
					"count":    len(rows),
				})
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			if len(rows) == 0 {
				if !quiet {
					fmt.Printf("No aliases for [%s].\n", agentID)
				}
				return nil
			}
			for _, r := range rows {
				mark := ""
				if r.Retired {
					mark = " (retired)"
				}
				fmt.Printf("  [%s] %s = %s%s\n", r.Source, r.Term, strings.Join(r.Equivalents, ", "), mark)
			}
			if !quiet {
				fmt.Printf("\n%d alias for [%s].\n", len(rows), agentID)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include retired aliases")
	return cmd
}

// parseAliasText splits an alias fact's text back into term and equivalents.
// It mirrors pkg/memory's own parser rather than exporting it: this is a
// display concern, and a malformed alias must render as something a human can
// see and fix, not vanish the way the ranking parser correctly makes it vanish.
func parseAliasText(text string) (string, []string) {
	body := strings.TrimSpace(strings.TrimPrefix(text, "alias:"))
	left, right, ok := strings.Cut(body, "=")
	if !ok {
		return body, nil
	}
	var eqs []string
	for _, e := range strings.Split(right, ",") {
		if e = strings.TrimSpace(e); e != "" {
			eqs = append(eqs, e)
		}
	}
	return strings.TrimSpace(left), eqs
}
