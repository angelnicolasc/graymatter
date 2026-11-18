package memory

import (
	"fmt"
	"strings"
)

// ReviewOptionsFromConfig selects provider credentials and defaults from the
// store owner. An explicit provider is required even when the owner already
// has credentials configured for other operations.
func ReviewOptionsFromConfig(cfg ConsolidateConfig, provider, model string) (ReviewAnalyzeOptions, error) {
	if cfg == nil {
		return ReviewAnalyzeOptions{}, fmt.Errorf("%w: review provider configuration is unavailable", ErrReviewInvalid)
	}
	options := ReviewAnalyzeOptions{Provider: strings.TrimSpace(provider), Model: strings.TrimSpace(model)}
	switch options.Provider {
	case "anthropic":
		options.APIKey = cfg.GetAnthropicAPIKey()
		if options.Model == "" {
			options.Model = cfg.GetConsolidateModel()
		}
	case "ollama":
		options.Endpoint = cfg.GetOllamaURL()
		if options.Model == "" {
			options.Model = cfg.GetOllamaConsolidateModel()
		}
	default:
		return ReviewAnalyzeOptions{}, fmt.Errorf("%w: specify anthropic or ollama as the review provider", ErrReviewInvalid)
	}
	return options, nil
}
