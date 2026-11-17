package memory

import (
	"github.com/anthropics/anthropic-sdk-go"

	"github.com/angelnicolasc/graymatter/internal/anthropicclient"
	"github.com/angelnicolasc/graymatter/internal/providerurl"
)

type anthropicBaseURLConfig interface {
	GetAnthropicBaseURL() string
}

func newAnthropicMessages(cfg any, key string) (anthropic.MessageService, error) {
	var explicit string
	if config, ok := cfg.(anthropicBaseURLConfig); ok {
		explicit = config.GetAnthropicBaseURL()
	}
	base := providerurl.Select(explicit, providerurl.AnthropicEnv, providerurl.AnthropicDefault)
	return anthropicclient.NewMessages(base, key)
}
