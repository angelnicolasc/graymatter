// Package anthropicclient builds Anthropic Messages services for a fixed URL.
package anthropicclient

import (
	"net/http"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/angelnicolasc/graymatter/internal/providerhttp"
	"github.com/angelnicolasc/graymatter/internal/providerurl"
)

// NewMessages builds a Messages service for the already selected base URL.
// It reads authentication from the environment, with an optional API key
// override. Base URL selection is the caller's responsibility.
func NewMessages(resolvedBaseURL, explicitAPIKey string) (anthropic.MessageService, error) {
	return newMessages(resolvedBaseURL, explicitAPIKey, http.DefaultClient)
}

func newMessages(resolvedBaseURL, explicitAPIKey string, template *http.Client) (anthropic.MessageService, error) {
	base, err := providerurl.Parse("anthropic", resolvedBaseURL)
	if err != nil {
		return anthropic.MessageService{}, err
	}
	// NewClient would prepend ANTHROPIC_BASE_URL, so its option could fail
	// before an explicit URL is applied. Reuse the SDK's Messages service
	// with the same remaining default options in their original order.
	opts := []option.RequestOption{option.WithEnvironmentProduction()}
	if key, present := os.LookupEnv("ANTHROPIC_API_KEY"); present {
		opts = append(opts, option.WithAPIKey(key))
	}
	if token, present := os.LookupEnv("ANTHROPIC_AUTH_TOKEN"); present {
		opts = append(opts, option.WithAuthToken(token))
	}
	if explicitAPIKey != "" {
		opts = append(opts, option.WithAPIKey(explicitAPIKey))
	}
	opts = append(opts, option.WithBaseURL(base.String()),
		option.WithHTTPClient(providerhttp.New("anthropic", base, template)))
	return anthropic.NewMessageService(opts...), nil
}
