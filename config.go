package graymatter

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// EmbeddingMode controls how GrayMatter generates vector embeddings.
type EmbeddingMode int

const (
	// EmbeddingAuto detects the best available provider at runtime.
	// Detection order: Ollama → OpenAI → Voyage → keyword-only.
	EmbeddingAuto EmbeddingMode = iota
	// EmbeddingOllama forces Ollama (requires a running Ollama instance).
	EmbeddingOllama
	// EmbeddingAnthropic resolves to the Voyage-backed embeddings slot.
	// Deprecated: Anthropic has no embeddings API; kept for numeric stability.
	// Use EmbeddingVoyage with VOYAGE_API_KEY instead.
	EmbeddingAnthropic
	// EmbeddingKeyword disables vector search; uses keyword+recency scoring only.
	EmbeddingKeyword
	// EmbeddingOpenAI forces OpenAI Embeddings API (requires OPENAI_API_KEY).
	EmbeddingOpenAI
	// EmbeddingVoyage forces the Voyage AI Embeddings API (requires VOYAGE_API_KEY).
	EmbeddingVoyage
)

// Config holds all GrayMatter configuration. All fields have sane defaults
// via DefaultConfig(). Zero-value Config is not valid — always call DefaultConfig().
type Config struct {
	// DataDir is the directory where gray.db and vector files are stored.
	// Default: ".graymatter"
	DataDir string

	// TopK is the maximum number of facts returned by Recall.
	// Default: 8
	TopK int

	// EmbeddingMode controls which embedding backend is used.
	// Default: EmbeddingAuto (Ollama → OpenAI → Anthropic → keyword)
	EmbeddingMode EmbeddingMode

	// OllamaURL is the base URL of the Ollama API.
	// Default: value of GRAYMATTER_OLLAMA_URL env var, or "http://localhost:11434"
	OllamaURL string

	// OllamaModel is the embedding model used with Ollama.
	// Default: value of GRAYMATTER_OLLAMA_MODEL env var, or "nomic-embed-text"
	OllamaModel string

	// OllamaConsolidateModel is the local model used for consolidation
	// summarisation when ConsolidateLLM is "ollama".
	// Default: value of GRAYMATTER_OLLAMA_CONSOLIDATE_MODEL env var, or "llama3.2"
	OllamaConsolidateModel string

	// AnthropicAPIKey for the Anthropic consolidation endpoints.
	// Default: value of ANTHROPIC_API_KEY env var.
	AnthropicAPIKey string

	// VoyageAPIKey for the Voyage AI embeddings endpoint (api.voyageai.com),
	// which is what the third slot of the embedding chain actually dials.
	// Before v0.15 that slot dialled an Anthropic embeddings endpoint that
	// does not exist, silently degrading every store to keyword-only.
	// Default: value of VOYAGE_API_KEY env var.
	VoyageAPIKey string

	// OpenAIAPIKey for the OpenAI Embeddings API (text-embedding-3-small).
	// Default: value of OPENAI_API_KEY env var.
	OpenAIAPIKey string

	// OpenAIModel overrides the OpenAI embedding model.
	// Default: value of GRAYMATTER_OPENAI_MODEL env var, or "text-embedding-3-small"
	OpenAIModel string

	// ConsolidateLLM specifies which LLM provider drives memory consolidation.
	// Values: "anthropic", "ollama", "" (disable consolidation).
	// Default: "anthropic" if ANTHROPIC_API_KEY is set, else "" (disabled).
	// To use Ollama as the consolidation LLM, set this field explicitly to "ollama".
	ConsolidateLLM string

	// ConsolidateModel is the model used for consolidation summarisation.
	// Default: "claude-haiku-4-5-20251001"
	ConsolidateModel string

	// ConsolidateThreshold is the minimum fact count that triggers consolidation.
	// Default: 20
	ConsolidateThreshold int

	// DecayHalfLife is the half-life for the exponential weight decay curve.
	// Facts not accessed within this window lose half their retrieval weight.
	// Default: 720h (30 days)
	DecayHalfLife time.Duration

	// AsyncConsolidate runs consolidation in a background goroutine after Remember.
	// Default: true
	AsyncConsolidate bool

	// MaxAsyncConsolidations bounds how many consolidation goroutines may run
	// concurrently. Additional triggers while at capacity are silently dropped.
	// Default: 2
	MaxAsyncConsolidations int

	// OnConsolidateError is called when an async consolidation goroutine returns
	// an error. If nil, errors are discarded. The callback must be safe for
	// concurrent use.
	OnConsolidateError func(agentID string, err error)

	// OnVectorIndexError is called when the vector store fails to index a fact
	// after the bbolt write has already succeeded. The fact is durably marked
	// as pending and will be retried on the next reconcile tick or process
	// restart. If nil, errors are silently retried. Must be safe for concurrent
	// use.
	OnVectorIndexError func(agentID, factID string, err error)

	// VectorReconcileInterval controls how often the background reconciler
	// drains the pending-vector queue. Vectors that failed to index inline
	// (e.g. transient embedder error) are retried on this cadence.
	// Default: 30s. Set to 0 to disable the background loop (reconciliation
	// will then only run at Open()).
	VectorReconcileInterval time.Duration

	// ReadOnly opens the store in read-only mode, skipping all mutating
	// operations. When false (default), the TUI and CLI automatically fall
	// back to read-only if the write lock is held by another process (e.g.
	// opencode running in the same directory).
	ReadOnly bool

	// StrictWrite disables the automatic read-only fallback: if the write
	// lock cannot be acquired, Open fails immediately instead of degrading.
	// The store daemon sets this — a store owner that silently came up
	// read-only would break every connected client. StrictWrite wins over
	// ReadOnly when both are set.
	StrictWrite bool

	// SignalWeights sets how much vector similarity, keyword relevance and
	// recency each contribute to the fused ranking.
	// Default: nil, meaning memory.DefaultSignalWeights() — vector 1.0,
	// keyword 1.0, recency 0.5, the values hardcoded before v0.10.0.
	SignalWeights *memory.SignalWeights

	// MinRelevance drops recalled facts scoring below this fraction of the
	// best score in the same result set.
	// Default: 0 — no cut, Recall returns exactly TopK as it always has.
	MinRelevance float64

	// StemKeywords folds English morphology into the keyword signal so a
	// question about "backups" reaches a fact about "backup retention". Pure
	// Go, no model, no download, no state.
	//
	// ON by default since the measurement that earned it: on a corpus this
	// change was not designed against — the scale corpus, 35 probes, 5k/10k/30k
	// facts — it lifts one-query retrieval from 25/35 to 29/35 at every size,
	// and the four it fixes are morphology and nothing else (backups/backup,
	// rotations/rotation, releases/release, deployment/deploy). The queries it
	// fails with stemming are a STRICT SUBSET of the ones it fails without: it
	// wins four and loses none. That subset property is the revert criterion,
	// and TestStemmingNeverLosesAQueryItWinsWithout pins it.
	//
	// The cost is latency on the scan path, and it is not small. Two machines
	// measured it at +61%/+65% and at +90%/+90% (10k and 30k facts), so the
	// durable statement is the ratio and not either point: the scan pays
	// roughly TWICE, and the candidate-set path pays NOTHING — 5 ms either way
	// at 10k, 11 ms at 30k, on both machines.
	//
	// That gap is structural, not luck. On the scan path keywordScoreDetailed
	// re-tokenises every stored fact on every query, so each token of each fact
	// walks the suffix rules once per query. The index stems the corpus once at
	// write time and stems only the query's own terms at read time: O(N) versus
	// O(query). The cost falls away entirely on the candidate-set path, which
	// is default ON now — its gate was the counterpart of this default, not a
	// competing optimisation. Below a few thousand facts it is single-digit
	// milliseconds either way.
	//
	// Env: GRAYMATTER_STEM_KEYWORDS=0 opts out.
	StemKeywords bool

	// UsageAliasLearning lets the store promote its own vocabulary from
	// observed reformulations: a weak match followed by a strong one from the
	// same agent is evidence that the agent's word and the store's word mean
	// the same thing, and the second independent observation of the pair
	// promotes an alias. No agent action, no server-side semantics, no model.
	// Off by default; see pkg/memory/usagealias.go for the guardrails.
	//
	// Env: GRAYMATTER_USAGE_ALIAS=1
	UsageAliasLearning bool

	// UsageAliasAffinityMin is how much lexical affinity an unknown word needs
	// with the working word before the store promotes a usage alias: the
	// minimum shared leading prefix, in characters.
	//
	// 0 (the zero value) means the conservative default of 3 — the
	// morphology class, where co-occurrence evidence is decisive. -1 removes
	// the gate and lets the store learn the synonym class as well.
	//
	// Measured on a blind evaluation corpus with real agent reformulations, the
	// open gate is worth +2 families out of 40 and promotes about one junk
	// pair in three. That is why it is reachable and not a default.
	//
	// Env: GRAYMATTER_USAGE_ALIAS_AFFINITY (e.g. -1)
	UsageAliasAffinityMin int

	// CandidateRetrieval answers a recall from an inverted index and a
	// recency spine instead of loading and re-tokenising every stored fact.
	// The ranking is identical — pinned by a test that runs both paths over
	// one store and compares fused scores, per-signal ranks and lineage — so
	// this changes what gets read, never what gets returned.
	//
	// ON by default since both of its gates went green on two machines: the
	// 30k latency gate measured p99 20.4 ms and 21.2 ms against the 40 ms bar,
	// with a harness that verifies every measured recall returned facts, and
	// the write-cost bar (<= 3 ms) was ratified by the owner — a write-once,
	// read-many store pays ~1.5 ms per Put to keep every read sub-linear.
	// A store written before this default keeps answering: the first recall
	// rebuilds the index and the ranking does not change.
	//
	// Env: GRAYMATTER_CANDIDATE_RETRIEVAL=0 opts out.
	CandidateRetrieval bool
}

// DefaultConfig returns a Config with all defaults applied from environment
// variables and runtime probes. Safe to call multiple times.
func DefaultConfig() Config {
	return Config{
		DataDir:                 ".graymatter",
		TopK:                    8,
		EmbeddingMode:           EmbeddingAuto,
		OllamaURL:               envOrDefault("GRAYMATTER_OLLAMA_URL", "http://localhost:11434"),
		OllamaModel:             envOrDefault("GRAYMATTER_OLLAMA_MODEL", "nomic-embed-text"),
		OllamaConsolidateModel:  envOrDefault("GRAYMATTER_OLLAMA_CONSOLIDATE_MODEL", "llama3.2"),
		AnthropicAPIKey:         os.Getenv("ANTHROPIC_API_KEY"),
		VoyageAPIKey:            os.Getenv("VOYAGE_API_KEY"),
		OpenAIAPIKey:            os.Getenv("OPENAI_API_KEY"),
		OpenAIModel:             envOrDefault("GRAYMATTER_OPENAI_MODEL", "text-embedding-3-small"),
		ConsolidateLLM:          resolveConsolidateLLM(),
		ConsolidateModel:        "claude-haiku-4-5-20251001",
		ConsolidateThreshold:    20,
		VectorReconcileInterval: 30 * time.Second,
		DecayHalfLife:           720 * time.Hour,
		AsyncConsolidate:        true,
		MaxAsyncConsolidations:  2,
		StemKeywords:            envBoolDefault("GRAYMATTER_STEM_KEYWORDS", true),
		UsageAliasLearning:      envBool("GRAYMATTER_USAGE_ALIAS"),
		UsageAliasAffinityMin:   envInt("GRAYMATTER_USAGE_ALIAS_AFFINITY"),
		CandidateRetrieval:      envBoolDefault("GRAYMATTER_CANDIDATE_RETRIEVAL", true),
	}
}

// resolveConsolidateLLM returns the best available consolidation LLM based on
// environment variables. It does NOT probe network endpoints at startup.
//
// Detection order:
//  1. "anthropic" — if ANTHROPIC_API_KEY is set
//  2. ""          — disabled (set ConsolidateLLM="ollama" explicitly for Ollama)
//
// Ollama is excluded from auto-detection because probing the Ollama endpoint on
// every process startup would add 500 ms+ of latency. Configure it explicitly:
//
//	cfg := graymatter.DefaultConfig()
//	cfg.ConsolidateLLM = "ollama"
func resolveConsolidateLLM() string {
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "anthropic"
	}
	return ""
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt reads a signed integer knob. Unset or unparseable is 0, which every
// consumer reads as "use the built-in default" — so a typo degrades to the
// documented behaviour instead of to a number nobody chose.
func envInt(key string) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return 0
	}
	return v
}

// envBool reads an opt-in switch: unset means off, and only an explicit yes
// turns it on.
//
// DefaultConfig is the single place the daemon, the CLI's --no-daemon path,
// the MCP server, the REST server and the harness all build their store from,
// so wiring a switch there reaches every surface at once — and keeps the
// README's promise intact, since an environment variable is not a config file.
func envBool(key string) bool {
	return envBoolDefault(key, false)
}

// envBoolDefault reads a switch that has a default in either direction.
//
// envBool alone cannot express a default-on flag: it maps everything that is
// not an explicit yes to false, so "unset" and "0" are the same answer and a
// flag flipped to default-on becomes impossible to turn off. That is the same
// shape as the affinity knob nobody could reach and the memory_alias that
// resolved nowhere — a setting that exists in the documentation and not in the
// product. Three states, so an opt-out is sayable.
func envBoolDefault(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// Config implements memory.ConsolidateConfig so it can be passed directly
// to Store.Consolidate / Store.MaybeConsolidate without an adapter.

func (c Config) GetAnthropicAPIKey() string        { return c.AnthropicAPIKey }
func (c Config) GetConsolidateLLM() string         { return c.ConsolidateLLM }
func (c Config) GetConsolidateModel() string       { return c.ConsolidateModel }
func (c Config) GetOllamaURL() string              { return c.OllamaURL }
func (c Config) GetOllamaConsolidateModel() string { return c.OllamaConsolidateModel }
func (c Config) GetConsolidateThreshold() int      { return c.ConsolidateThreshold }
func (c Config) GetDecayHalfLife() time.Duration   { return c.DecayHalfLife }
