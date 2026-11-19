package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/spf13/cobra"
)

// importCmd imports fact projections as new writes, never as restored history.
func importCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <agent> <memories.json>",
		Short: "Import JSON fact projections as new facts in one namespace",
		Long: `Imports the memories.json array produced by export --format json.
The complete document is validated before the atomic write. Text, declared
confidence, source and literal quote are preserved. Every row receives a new ID
and receipt time in the explicit destination namespace. Original IDs, timestamps,
weights, access counters, pins, relationships and review ownership are not
restored. Alias rows, retired rows, subject projections and lineage audit
archives are rejected.
Use a complete database backup/restore to preserve historical identities.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("destination agent must be non-empty")
			}
			file, err := os.Open(args[1])
			if err != nil {
				return fmt.Errorf("open import: %w", err)
			}
			facts, origins, parseErr := memory.ParseFactsJSON(file)
			closeErr := file.Close()
			if parseErr != nil {
				return fmt.Errorf("validate import: %w", parseErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close import: %w", closeErr)
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			defer store.Close()
			writer, ok := store.(memory.UtilityStore)
			if !ok {
				return memory.ErrUtilityUnsupported
			}
			written, err := writer.PutManyReturningFacts(cmd.Context(), args[0], facts)
			if err != nil {
				return fmt.Errorf("import facts: %w", err)
			}
			if len(written) != len(origins) {
				return fmt.Errorf("import committed but returned an inconsistent ID count; do not repeat the import")
			}
			mapped := make([]memory.ImportedFact, len(written))
			for i, fact := range written {
				mapped[i] = memory.ImportedFact{SourceNamespace: origins[i].Namespace, SourceID: origins[i].FactID, ID: fact.ID}
			}
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Mode  string                `json:"mode"`
					Facts []memory.ImportedFact `json:"facts"`
				}{"new_facts", mapped})
			}
			if !quiet {
				fmt.Fprintf(cmd.OutOrStdout(), "Imported %d new facts into %q. Historical IDs, timestamps, retention, pins and relationships were not restored.\n", len(mapped), args[0])
				for _, fact := range mapped {
					fmt.Fprintf(cmd.OutOrStdout(), "%q/%q -> %s\n", fact.SourceNamespace, fact.SourceID, fact.ID)
				}
			}
			return nil
		},
	}
}
