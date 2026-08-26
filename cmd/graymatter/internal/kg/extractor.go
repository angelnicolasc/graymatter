package kg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ExtractorConfig configures an EntityExtractor.
type ExtractorConfig struct {
	// UseLLM enables LLM-enhanced extraction via the Anthropic API.
	// When false (default), pure regex extraction is used with zero API calls.
	UseLLM bool

	// APIKey is the Anthropic API key for LLM extraction.
	// If empty, ANTHROPIC_API_KEY env var is used.
	APIKey string

	// Model is the Anthropic model for LLM extraction.
	// Default: "claude-haiku-4-5-20251001" (fast + cheap).
	Model string
}

// EntityExtractor pulls entities from a text string, returning Nodes and Edges
// to upsert into the knowledge graph.
type EntityExtractor interface {
	Extract(text string) ([]Node, []Edge, error)
}

// NewExtractor returns an EntityExtractor based on cfg.
// The default (UseLLM=false) extractor uses only regex — zero API calls.
func NewExtractor(cfg ExtractorConfig) EntityExtractor {
	if cfg.UseLLM {
		model := cfg.Model
		if model == "" {
			model = "claude-haiku-4-5-20251001"
		}
		return NewLLMExtractor(cfg.APIKey, model)
	}
	return &regexExtractor{}
}

// NewLLMExtractor builds the LLM-backed extractor with its deterministic
// safety net. Every failure mode - unreachable endpoint, invalid JSON, a
// node with an unknown type - degrades to the regex extractor for that text,
// never to silence: an agent that opted into LLM extraction keeps getting a
// graph even when the model is down.
func NewLLMExtractor(apiKey, model string) EntityExtractor {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &llmFallback{llm: &llmExtractor{apiKey: apiKey, model: model}, fallback: &regexExtractor{}}
}

// ExtractorFromEnv honours GRAYMATTER_KG_LLM_EXTRACT=1 as the opt-in switch
// for LLM extraction, so daemon and direct-store wiring agree without each
// knowing flag plumbing.
func ExtractorFromEnv() EntityExtractor {
	useLLM := os.Getenv("GRAYMATTER_KG_LLM_EXTRACT") == "1"
	return NewExtractor(ExtractorConfig{UseLLM: useLLM})
}

// --- regex extractor (default, zero deps) ---

type regexExtractor struct{}

var (
	// Capitalized multi-word names: "Maria Rodriguez", "Acme Corp", "Sebastián Yañez".
	// Unicode classes so accented names survive intact instead of fragmenting
	// at the first non-ASCII letter.
	// Hyphenated surname components count as name parts: "El-Sayed",
	// "Al-Rashid", "García-Marquez" are single people, and splitting at the
	// hyphen manufactured phantom entities ("Ahmed El") while stranding the
	// real fragment below the occurrence gate.
	reCapNames = regexp.MustCompile(`\b(\p{Lu}\p{Ll}+(?:[\s-]+\p{Lu}\p{Ll}+)+)\b`)
	// Single capitalized words (occurrence-gated; see singleCapMinCount)
	reCaps = regexp.MustCompile(`\b(\p{Lu}\p{Ll}{2,})\b`)
	// Sentence-function words are never entities, however often they appear:
	// "The" opens most English sentences and would clear any occurrence
	// threshold. Matched case-insensitively against the lowercased candidate.
	singleCapStop = map[string]bool{
		"the": true, "this": true, "that": true, "these": true, "those": true,
		"there": true, "then": true, "when": true, "what": true, "why": true,
		"how": true, "but": true, "and": true, "or": true, "not": true,
		"all": true, "any": true, "some": true, "now": true, "once": true,
		"after": true, "before": true, "if": true, "it": true, "its": true,
		"his": true, "her": true, "their": true, "our": true, "your": true,
	}
	// A single capitalized word becomes an entity at this many occurrences.
	// 2 produced measured noise on a 320-fact corpus ("The", hyphen-split
	// name fragments like "Modigliani" from "Modigliani-Miller", single-word
	// title fragments); 3 removed that class while every genuine single-word
	// entity in the corpus cleared it. Measured, not guessed — see
	// extractor_testdata/golden_facts.json.
	singleCapMinCount = 3
	// All-caps role titles, optionally followed by a capitalized name:
	// "CTO", "VP Finance". Case-sensitive on purpose: a lowercase token
	// after the title is a verb ("the CTO approved"), not a name.
	reRoleTitle = regexp.MustCompile(`\b(VP|CTO|CEO|CFO|COO|CIO)(\s+[A-Z][a-z]+)?\b`)
	// Role words matched with word boundaries. Substring matching typed
	// "Hector Salazar" as a role because "heCTOR" contains "cto".
	reRoleWord = regexp.MustCompile(`\b(vp|ceo|cto|cfo|coo|cio|director|manager)\b`)
	// Sentence-initial determiners glued onto a name: "The Atlas Migration".
	// Stripped after matching so the entity is the name, not the sentence.
	determiners = map[string]bool{"the": true, "a": true, "an": true, "and": true, "or": true}
	// Organizational suffixes — membership of the LAST word marks an organization.
	orgSuffixes = map[string]bool{
		"corp": true, "inc": true, "ltd": true, "llc": true, "company": true,
		"labs": true, "capital": true, "group": true, "partners": true,
		"retail": true, "manufacturing": true, "consulting": true,
		"logistics": true, "insurance": true, "media": true, "biotech": true,
		"systems": true, "analytics": true, "freight": true, "legal": true,
		"foods": true, "ventures": true, "utilities": true,
		"semiconductors": true, "health": true,
		"institutions": true, "school": true, "review": true, "journal": true,
		"institute": true, "press": true,
	}
	// Spanish/LatAm corporate HEAD words — membership of the FIRST word marks
	// an organization, because ES company names lead with the entity kind
	// ("Grupo Ávila", "Talleres Ibáñez", "Clínica Nogal") instead of trailing
	// a suffix the way English ones do.
	orgHeads = map[string]bool{
		"grupo": true, "talleres": true, "clínica": true, "clinica": true,
		"editorial": true, "cafetal": true, "logística": true, "logistica": true,
		"laboratorios": true, "banca": true, "bodega": true, "constructora": true,
	}
	lowercaseRoles = []string{"director", "manager", "advisor", "registrar", "directora", "gerente"}
	// @mentions
	reMention = regexp.MustCompile(`@([A-Za-z0-9_]+)`)
	// Quoted strings (double-quote only, 3–60 chars)
	reQuoted = regexp.MustCompile(`"([^"]{3,60})"`)
)

func (e *regexExtractor) Extract(text string) ([]Node, []Edge, error) {
	var nodes []Node
	seen := make(map[string]bool)
	// seenLabel tracks lowercased labels across types: the occurrence gate
	// below must not recount a single-cap word that already entered as part
	// of a multi-word name, whatever type that name was classified as.
	seenLabel := make(map[string]bool)

	add := func(label, entityType string) {
		id := canonicalID(label, entityType)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		seenLabel[strings.ToLower(strings.TrimSpace(label))] = true
		nodes = append(nodes, Node{
			ID:         id,
			Label:      label,
			EntityType: entityType,
		})
	}

	// All-caps role titles first, so "VP Finance" is consumed as one role
	// entity and "Finance" is not left behind as a stray single-cap candidate.
	for _, m := range reRoleTitle.FindAllString(text, -1) {
		add(m, "role")
	}

	// Lowercase contextual roles right after a determiner: "the director",
	// "our manager" — and their Spanish counterparts ("el director", "la
	// gerente"), which reach the graph the same way. Without this, roles
	// written in prose are invisible.
	for _, role := range lowercaseRoles {
		for _, article := range []string{"the ", "The ", "our ", "Our ", "el ", "El ", "la ", "La ", "al ", "del "} {
			idx := 0
			needle := article + role
			for {
				at := strings.Index(strings.ToLower(text[idx:]), needle)
				if at < 0 {
					break
				}
				start := idx + at
				before := byte(' ')
				if start > 0 {
					before = text[start-1]
				}
				if before == ' ' || start == 0 {
					add(role, "role")
				}
				idx = start + len(needle)
			}
		}
	}

	// Multi-word capitalized names → persons or organizations. Leading
	// determiners are stripped so "The Atlas Migration" yields the name,
	// never a sentence fragment. A match that collapses to a single token
	// after stripping ("The Goal" → "Goal") is a sentence opening, not a
	// name: single-word entities enter through the occurrence-gated
	// single-cap path instead of bypassing it.
	for _, m := range reCapNames.FindAllString(text, -1) {
		label := stripDeterminers(m)
		if label == "" {
			continue
		}
		if len(strings.Fields(label)) == 1 && len(strings.Fields(m)) > 1 {
			continue
		}
		add(label, classifyCapName(label))
	}

	// Single capitalized words — occurrence-gated and stopword-filtered.
	// URLs and ISO dates are deliberately NOT entities: they are attributes
	// of the fact (visible in the fact text itself, the TUI and the export),
	// and as graph nodes they contributed meaningless co_mentioned cliques
	// (URL↔date) while reading as noise in every surface that renders them.
	capCounts := make(map[string]int)
	for _, m := range reCaps.FindAllString(text, -1) {
		lower := strings.ToLower(m)
		if singleCapStop[lower] {
			continue
		}
		if !seenLabel[lower] {
			capCounts[m]++
		}
	}
	for label, count := range capCounts {
		if count >= singleCapMinCount {
			add(label, "concept")
		}
	}

	// @mentions → person entity.
	for _, sub := range reMention.FindAllStringSubmatch(text, -1) {
		add("@"+sub[1], "person")
	}

	// Quoted strings → preference entity.
	for _, sub := range reQuoted.FindAllStringSubmatch(text, -1) {
		add(sub[1], "preference")
	}

	// Link ALL entity pairs within the fact as "co_mentioned": co-mention
	// means mentioned together, which is a clique, not a chain. A fact naming
	// five entities contributes its full local structure to the graph.
	var edges []Edge
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			edges = append(edges, Edge{
				From:     nodes[i].ID,
				To:       nodes[j].ID,
				Relation: "co_mentioned",
			})
		}
	}

	return nodes, edges, nil
}

// stripDeterminers removes leading/trailing articles and conjunctions from a
// matched name sequence, returning the remaining name ("" if nothing remains).
func stripDeterminers(seq string) string {
	tokens := strings.Fields(seq)
	for len(tokens) > 0 && determiners[strings.ToLower(tokens[0])] {
		tokens = tokens[1:]
	}
	for len(tokens) > 0 && determiners[strings.ToLower(tokens[len(tokens)-1])] {
		tokens = tokens[:len(tokens)-1]
	}
	return strings.Join(tokens, " ")
}

// classifyCapName heuristically assigns an entity type to a capitalized name.
func classifyCapName(name string) string {
	tokens := strings.Fields(name)
	lc := strings.ToLower(name)
	switch {
	case len(tokens) > 0 && orgSuffixes[strings.ToLower(tokens[len(tokens)-1])]:
		return "organization"
	case len(tokens) > 1 && orgHeads[strings.ToLower(tokens[0])]:
		return "organization"
	case reRoleWord.MatchString(lc):
		return "role"
	default:
		parts := strings.Fields(name)
		if len(parts) == 2 && isProperNoun(parts[0]) && isProperNoun(parts[1]) {
			return "person"
		}
		return "concept"
	}
}

// isProperNoun returns true if s starts uppercase followed by lowercase.
func isProperNoun(s string) bool {
	runes := []rune(s)
	if len(runes) < 2 {
		return false
	}
	return unicode.IsUpper(runes[0]) && unicode.IsLower(runes[1])
}

// canonicalID produces a stable, type-scoped ID from a label. Scoping by
// entity type keeps distinct things distinct: "apple" the organization and
// "apple" the concept must not merge into one node just because their labels
// lowercase to the same string. Empty types fall back to "unknown", matching
// the placeholder nodes Link auto-creates.
func canonicalID(label, entityType string) string {
	t := strings.ToLower(strings.TrimSpace(entityType))
	if t == "" {
		t = "unknown"
	}
	return t + ":" + strings.ToLower(strings.TrimSpace(label))
}

// --- LLM extractor (opt-in via ExtractorConfig.UseLLM=true) ---

// llmFallback tries the LLM first and degrades to the regex extractor on any
// failure. Degradation is per-text and deterministic: the same input always
// produces the same output whether the model is up or down.
type llmFallback struct {
	llm      *llmExtractor
	fallback EntityExtractor
}

func (f *llmFallback) Extract(text string) ([]Node, []Edge, error) {
	nodes, edges, err := f.llm.Extract(text)
	if err == nil && len(nodes) > 0 {
		return nodes, edges, nil
	}
	if err != nil {
		// Surfaced through consolidation's error hook by the caller; here we
		// simply refuse to let a dead endpoint mean an empty graph.
		fn, fe, ferr := f.fallback.Extract(text)
		if ferr != nil {
			return nil, nil, err // report the original LLM failure
		}
		return fn, fe, nil
	}
	// Model answered but found nothing: trust that over regex noise.
	return nodes, edges, err
}

type llmExtractor struct {
	apiKey string
	model  string
}

const extractionPrompt = `Extract named entities from the following text.
Return a JSON object with two arrays:
- "nodes": [{"id":"<lowercase_id>","label":"<display_name>","entity_type":"<person|organization|project|decision|preference|concept>"}]
- "edges": [{"from":"<id>","to":"<id>","relation":"<related_to|mentioned_with|contradicts>"}]

Only include entities that are clearly identifiable. Return valid JSON only, no prose.

Text: %s`

func (e *llmExtractor) Extract(text string) ([]Node, []Edge, error) {
	prompt := fmt.Sprintf(extractionPrompt, text)

	var client anthropic.Client
	if e.apiKey != "" {
		client = anthropic.NewClient(option.WithAPIKey(e.apiKey))
	} else {
		client = anthropic.NewClient()
	}

	msg, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.Model(e.model),
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("kg: llm extract: %w", err)
	}
	if len(msg.Content) == 0 {
		return nil, nil, nil
	}

	return parseLLMExtractionJSON(msg.Content[0].Text)
}

// parseLLMExtractionJSON parses the structured JSON returned by the LLM extractor.
func parseLLMExtractionJSON(raw string) ([]Node, []Edge, error) {
	// Strip optional code fence wrapper.
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	type rawNode struct {
		ID         string `json:"id"`
		Label      string `json:"label"`
		EntityType string `json:"entity_type"`
	}
	type rawEdge struct {
		From     string `json:"from"`
		To       string `json:"to"`
		Relation string `json:"relation"`
	}
	type result struct {
		Nodes []rawNode `json:"nodes"`
		Edges []rawEdge `json:"edges"`
	}

	var r result
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, nil, fmt.Errorf("kg: parse llm json: %w", err)
	}

	nodes := make([]Node, 0, len(r.Nodes))
	for _, rn := range r.Nodes {
		// The model's id field is never trusted: identity derives from
		// label+type under the typed scheme, so extraction stays canonical
		// no matter what the model invents.
		if rn.Label != "" {
			rn.ID = canonicalID(rn.Label, rn.EntityType)
		}
		// Schema gate: unknown types are dropped, not coerced - a hallucinated
		// type would otherwise masquerade as data in exports and analytics.
		if !validEntityTypes[rn.EntityType] {
			continue
		}
		nodes = append(nodes, Node{
			ID:         rn.ID,
			Label:      rn.Label,
			EntityType: rn.EntityType,
		})
	}
	edges := make([]Edge, 0, len(r.Edges))
	for _, re := range r.Edges {
		if !validRelations[re.Relation] {
			continue
		}
		edges = append(edges, Edge{
			From:     re.From,
			To:       re.To,
			Relation: re.Relation,
		})
	}
	return nodes, edges, nil
}

// validEntityTypes / validRelations are the closed vocabularies the LLM is
// allowed to emit. Anything outside them is model noise.
var validEntityTypes = map[string]bool{
	"person": true, "organization": true, "project": true,
	"decision": true, "preference": true, "concept": true, "role": true,
}

var validRelations = map[string]bool{
	"related_to": true, "mentioned_with": true, "contradicts": true, "co_mentioned": true,
}
