package memory

import (
	"github.com/anthropics/anthropic-sdk-go"

	"github.com/angelnicolasc/graymatter/internal/anthropicclient"
	"github.com/angelnicolasc/graymatter/internal/providerurl"
)

type anthropicBaseURLConfig interface {
	GetAnthropicBaseURL() string
}

func anthropicBaseURL(cfg any) string {
	var explicit string
	if config, ok := cfg.(anthropicBaseURLConfig); ok {
		explicit = config.GetAnthropicBaseURL()
	}
	return providerurl.Select(explicit, providerurl.AnthropicEnv, providerurl.AnthropicDefault)
}

func newAnthropicMessages(cfg ExtractConfig, key string) (anthropic.MessageService, error) {
	return anthropicclient.NewMessages(anthropicBaseURL(cfg), key)
}
