package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// doctor --embeddings audits the vector channel as the store itself observed
// it: how many live facts carry a vector, how many writes degraded to
// keyword-only because the embedder failed, what the last failure said, and
// how much of the retry backlog remains. It reads only store bytes, so the
// same store produces byte-identical output on every run — the same contract
// as --health. It works even when the daemon is down, which is precisely
// when you most need to know whether memory was quietly losing its vectors.

type embeddingsFinding struct {
	Rule   string `json:"rule"`
	Status string `json:"status"` // ok | info | warn | fail
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type embeddingsReport struct {
	EmbedDims      int                 `json:"embed_dims"`
	LiveFacts      int                 `json:"live_facts"`
	FactsWithVec   int                 `json:"facts_with_vector"`
	DegradedFacts  int                 `json:"degraded_facts"`
	LastDegradErr  string              `json:"last_degrade_error,omitempty"`
	PendingVectors int                 `json:"pending_vectors"`
	Findings       []embeddingsFinding `json:"findings"`
	Verdict        string              `json:"verdict"`
}

func runDoctorEmbeddings(cmd *cobra.Command) error {
	store, err := memory.Open(memory.StoreConfig{DataDir: dataDir, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("open store read-only: %w", err)
	}
	defer func() { _ = store.Close() }()

	rep := buildEmbeddingsReport(store)

	out := cmd.OutOrStdout()
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	renderEmbeddings(out, rep)
	return nil
}

// buildEmbeddingsReport runs every rule over the store in fixed order.
// Findings depend only on the store's bytes.
func buildEmbeddingsReport(store *memory.Store) embeddingsReport {
	rep := embeddingsReport{Verdict: "ok"}

	h, err := store.EmbeddingHealth()
	if err == nil {
		rep.EmbedDims = h.EmbedDims
		rep.DegradedFacts = h.DegradedFacts
		rep.LastDegradErr = h.LastDegradError
		rep.PendingVectors = h.PendingVectors
	}
	withVec, total, err := store.CountEmbeddings()
	if err == nil {
		rep.FactsWithVec = withVec
		rep.LiveFacts = total
	}

	cover := ruleVectorCoverage(rep)
	degr := ruleEmbedDegradation(rep)
	pend := rulePendingVectors(rep)
	rep.Findings = append(rep.Findings, cover, degr, pend)

	for _, f := range rep.Findings {
		if severity(f.Status) > severity(rep.Verdict) {
			rep.Verdict = f.Status
		}
	}
	return rep
}

func ruleVectorCoverage(rep embeddingsReport) embeddingsFinding {
	f := embeddingsFinding{Rule: "vector-coverage"}
	switch {
	case rep.LiveFacts == 0:
		f.Status = "info"
		f.Detail = "no live facts stored yet - nothing to index"
	case rep.FactsWithVec == rep.LiveFacts:
		f.Status = "ok"
		f.Detail = fmt.Sprintf("every live fact carries a vector (%d/%d, dims %d)", rep.FactsWithVec, rep.LiveFacts, rep.EmbedDims)
	case rep.FactsWithVec > 0:
		f.Status = "info"
		f.Detail = fmt.Sprintf("mixed store: %d of %d live facts carry a vector", rep.FactsWithVec, rep.LiveFacts)
		f.Hint = "facts written while no provider was configured stay keyword-only; new writes pick up the current provider automatically"
	default:
		f.Status = "info"
		if rep.DegradedFacts == 0 {
			f.Detail = fmt.Sprintf("keyword-only memory: %d live facts, none indexed, no recorded failures", rep.LiveFacts)
			f.Hint = "keyword-only is a supported configuration (ADR-005); for vectors, make Ollama reachable or set OPENAI_API_KEY / VOYAGE_API_KEY"
		} else {
			f.Status = "warn"
			f.Detail = fmt.Sprintf("%d live facts, none indexed, %d write(s) degraded", rep.LiveFacts, rep.DegradedFacts)
		}
	}
	return f
}

func ruleEmbedDegradation(rep embeddingsReport) embeddingsFinding {
	f := embeddingsFinding{Rule: "embed-degradation"}
	if rep.DegradedFacts == 0 {
		f.Status = "ok"
		f.Detail = "no embedder failures recorded"
		return f
	}
	f.Status = "warn"
	f.Detail = fmt.Sprintf("%d fact(s) were written keyword-only because the embedder failed", rep.DegradedFacts)
	if rep.LastDegradErr != "" {
		f.Detail += "; last error: " + rep.LastDegradErr
	}
	f.Hint = "check your embedding configuration: Ollama reachable (GRAYMATTER_OLLAMA_URL), or OPENAI_API_KEY, or VOYAGE_API_KEY"
	return f
}

func rulePendingVectors(rep embeddingsReport) embeddingsFinding {
	f := embeddingsFinding{Rule: "pending-vectors"}
	if rep.PendingVectors == 0 {
		f.Status = "ok"
		f.Detail = "vector retry queue empty"
		return f
	}
	f.Status = "warn"
	f.Detail = fmt.Sprintf("%d pending vector write(s)", rep.PendingVectors)
	f.Hint = "a queue that never drains means the embedding backend keeps failing - check your embedding configuration"
	return f
}

func renderEmbeddings(out interface{ Write([]byte) (int, error) }, rep embeddingsReport) {
	fmt.Fprintf(out, "Embedding channel audit\n\n")
	fmt.Fprintf(out, "  dims %d   live facts %d   with vector %d   degraded %d   pending %d\n\n",
		rep.EmbedDims, rep.LiveFacts, rep.FactsWithVec, rep.DegradedFacts, rep.PendingVectors)
	for _, f := range rep.Findings {
		fmt.Fprintf(out, "  [%s] %-17s %s\n", f.Status, f.Rule, f.Detail)
		if f.Hint != "" {
			fmt.Fprintf(out, "        hint: %s\n", f.Hint)
		}
	}
	fmt.Fprintf(out, "\n  verdict: %s\n", rep.Verdict)
}
