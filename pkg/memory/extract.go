package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// ExtractConfig is the subset of configuration needed for fact extraction.
// Satisfied by graymatter.Config via the same method-set pattern as ConsolidateConfig.
type ExtractConfig interface {
	GetAnthropicAPIKey() string
	GetConsolidateModel() string // reuses the same model field
}

const extractSystemPrompt = `You are a memory extraction assistant.
Extract up to 5 atomic, self-contained facts from the text the user provides.
Return ONLY a JSON array of strings. Each string must be a single declarative
sentence. Omit filler, greetings, and anything not factual.
Example output: ["Alice prefers bullet points.", "Deadline is Q2 2026."]`

// ExtractionResult records which path supplied the returned facts. A valid LLM
// extraction may reproduce the input verbatim, so text equality cannot reveal
// whether the result was generated.
type ExtractionResult struct {
	Facts  []string
	Source string
}

const (
	ExtractionLLM      = "llm"
	ExtractionFallback = "fallback"
)

// ExtractFacts calls the configured LLM and returns a slice of atomic facts
// extracted from text. Each element is a self-contained declarative sentence
// suitable for passing directly to Put / Remember.
//
// Without an Anthropic API key, ExtractFacts returns the raw text as a single
// fact (graceful degradation — useful in offline or test contexts).
func ExtractFacts(ctx context.Context, text string, cfg ExtractConfig) ([]string, error) {
	result, err := ExtractFactsWithProvenance(ctx, text, cfg)
	return result.Facts, err
}

// ExtractFactsWithProvenance preserves ExtractFacts' graceful fallback while
// exposing its source to callers that persist generated facts conservatively.
func ExtractFactsWithProvenance(ctx context.Context, text string, cfg ExtractConfig) (ExtractionResult, error) {
	if text == "" {
		return ExtractionResult{Source: ExtractionFallback}, nil
	}
	if cfg.GetAnthropicAPIKey() == "" {
		return ExtractionResult{Facts: []string{text}, Source: ExtractionFallback}, nil
	}
	return extractViaAnthropicWithProvenance(ctx, text, cfg)
}

func extractViaAnthropic(ctx context.Context, text string, cfg ExtractConfig) ([]string, error) {
	result, err := extractViaAnthropicWithProvenance(ctx, text, cfg)
	return result.Facts, err
}

func extractViaAnthropicWithProvenance(ctx context.Context, text string, cfg ExtractConfig) (ExtractionResult, error) {
	messages, err := newAnthropicMessages(cfg, cfg.GetAnthropicAPIKey())
	if err != nil {
		return ExtractionResult{}, fmt.Errorf("extract facts: %w", err)
	}
	msg, err := messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(cfg.GetConsolidateModel()),
		MaxTokens: 1024,
		System: []anthropic.TextBlockParam{
			{Text: extractSystemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(text)),
		},
	})
	if err != nil {
		return ExtractionResult{}, fmt.Errorf("extract facts: %w", err)
	}
	if len(msg.Content) == 0 {
		return ExtractionResult{}, fmt.Errorf("extract facts: empty response from model")
	}
	raw := strings.TrimSpace(msg.Content[0].Text)
	// Strip markdown code fences if the model wraps the JSON.
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var facts []string
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		// Fallback: treat the full response as one fact rather than failing the caller.
		return ExtractionResult{Facts: []string{text}, Source: ExtractionFallback}, nil
	}

	// Filter empty strings that models occasionally emit.
	out := facts[:0]
	for _, f := range facts {
		if strings.TrimSpace(f) != "" {
			out = append(out, f)
		}
	}
	return ExtractionResult{Facts: out, Source: ExtractionLLM}, nil
}
