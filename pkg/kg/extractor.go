package kg

import (
	"context"
	"encoding/json"
	"fmt"
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
		return &llmExtractor{apiKey: cfg.APIKey, model: model}
	}
	return &regexExtractor{}
}

// --- regex extractor (default, zero deps) ---

type regexExtractor struct{}

var (
	// Capitalized multi-word names: "Maria Rodriguez", "Acme Corp", "VP Sales"
	reCapNames = regexp.MustCompile(`\b([A-Z][a-z]+(?: [A-Z][a-z]+)+)\b`)
	// Single capitalized words (≥2 occurrences required to be considered significant)
	reCaps = regexp.MustCompile(`\b([A-Z][a-z]{2,})\b`)
	// URLs
	reURL = regexp.MustCompile(`https?://[^\s"'<>]+`)
	// ISO dates: 2026-04-09
	reDate = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)
	// @mentions
	reMention = regexp.MustCompile(`@([A-Za-z0-9_]+)`)
	// Quoted strings (double-quote only, 3–60 chars)
	reQuoted = regexp.MustCompile(`"([^"]{3,60})"`)
)

func (e *regexExtractor) Extract(text string) ([]Node, []Edge, error) {
	var nodes []Node
	seen := make(map[string]bool)

	add := func(label, entityType string) {
		id := canonicalID(label)
		if seen[id] {
			return
		}
		seen[id] = true
		nodes = append(nodes, Node{
			ID:         id,
			Label:      label,
			EntityType: entityType,
		})
	}

	// Multi-word capitalized names → persons or organizations.
	for _, m := range reCapNames.FindAllString(text, -1) {
		add(m, classifyCapName(m))
	}

	// Single capitalized words — require ≥2 occurrences to filter noise.
	capCounts := make(map[string]int)
	for _, m := range reCaps.FindAllString(text, -1) {
		if !seen[canonicalID(m)] {
			capCounts[m]++
		}
	}
	for label, count := range capCounts {
		if count >= 2 {
			add(label, "fact")
		}
	}

	// URLs → reference entity.
	for _, m := range reURL.FindAllString(text, -1) {
		add(m, "reference")
	}

	// ISO dates → date entity.
	for _, m := range reDate.FindAllString(text, -1) {
		add(m, "date")
	}

	// @mentions → person entity.
	for _, sub := range reMention.FindAllStringSubmatch(text, -1) {
		add("@"+sub[1], "person")
	}

	// Quoted strings → preference entity.
	for _, sub := range reQuoted.FindAllStringSubmatch(text, -1) {
		add(sub[1], "preference")
	}

	// Link consecutive entity pairs as "co_mentioned".
	var edges []Edge
	for i := 0; i < len(nodes)-1; i++ {
		edges = append(edges, Edge{
			From:     nodes[i].ID,
			To:       nodes[i+1].ID,
			Relation: "co_mentioned",
		})
	}

	return nodes, edges, nil
}

// classifyCapName heuristically assigns an entity type to a capitalized name.
func classifyCapName(name string) string {
	lc := strings.ToLower(name)
	switch {
	case strings.Contains(lc, "corp") || strings.Contains(lc, "inc") ||
		strings.Contains(lc, "ltd") || strings.Contains(lc, "llc") ||
		strings.Contains(lc, "company"):
		return "organization"
	case strings.Contains(lc, "vp") || strings.Contains(lc, "ceo") ||
		strings.Contains(lc, "cto") || strings.Contains(lc, "director") ||
		strings.Contains(lc, "manager"):
		return "role"
	default:
		parts := strings.Fields(name)
		if len(parts) == 2 && isProperNoun(parts[0]) && isProperNoun(parts[1]) {
			return "person"
		}
		return "fact"
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

// canonicalID produces a stable, lowercase ID from a label string.
func canonicalID(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

// --- LLM extractor (opt-in via ExtractorConfig.UseLLM=true) ---

type llmExtractor struct {
	apiKey string
	model  string
}

const extractionPrompt = `Extract named entities from the following text.
Return a JSON object with two arrays:
- "nodes": [{"id":"<lowercase_id>","label":"<display_name>","entity_type":"<person|organization|project|decision|preference|fact>"}]
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
		if rn.ID == "" && rn.Label != "" {
			rn.ID = canonicalID(rn.Label)
		}
		nodes = append(nodes, Node{
			ID:         rn.ID,
			Label:      rn.Label,
			EntityType: rn.EntityType,
		})
	}
	edges := make([]Edge, 0, len(r.Edges))
	for _, re := range r.Edges {
		edges = append(edges, Edge{
			From:     re.From,
			To:       re.To,
			Relation: re.Relation,
		})
	}
	return nodes, edges, nil
}
