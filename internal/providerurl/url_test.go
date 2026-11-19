package providerurl

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestSelectPrecedence(t *testing.T) {
	for _, provider := range []struct{ name, key, fallback string }{
		{"anthropic", AnthropicEnv, AnthropicDefault},
		{"openai", OpenAIEnv, OpenAIDefault},
		{"voyage", VoyageEnv, VoyageDefault},
	} {
		t.Run(provider.name, func(t *testing.T) {
			for _, env := range []string{"", "http://env.test", "http://["} {
				t.Setenv(provider.key, env)
				for _, explicit := range []string{"", "http://explicit.test", "http://not valid"} {
					want := explicit
					if want == "" {
						want = env
					}
					if want == "" {
						want = provider.fallback
					}
					if got := Select(explicit, provider.key, provider.fallback); got != want {
						t.Fatalf("Select(%q, env=%q) = %q, want %q", explicit, env, got, want)
					}
				}
			}
		})
	}
}

func TestParseAndEndpointPreserveEscapedPrefix(t *testing.T) {
	for _, tc := range []struct{ name, base, suffix, want string }{
		{"anthropic", AnthropicDefault, "v1/messages", "https://api.anthropic.com/v1/messages"},
		{"anthropic-versioned", "https://gateway.test/v1///", "v1/messages", "https://gateway.test/v1/v1/messages"},
		{"openai", OpenAIDefault, "embeddings", "https://api.openai.com/v1/embeddings"},
		{"voyage", VoyageDefault, "embeddings", "https://api.voyageai.com/v1/embeddings"},
		{"literal-path", "http://[::1]:80/edge/embeddings/", "embeddings", "http://[::1]:80/edge/embeddings/embeddings"},
		{"escaped-prefix", "http://gateway.test/edge//%2F/%25/%20/%C3%B1/%23/a%2f///", "v1/messages", "http://gateway.test/edge//%2F/%25/%20/%C3%B1/%23/a%2f/v1/messages"},
		{"escaped-dot-in-segment", "https://gateway.test/a%2F%2E/b", "embeddings", "https://gateway.test/a%2F%2E/b/embeddings"},
		{"literal-unicode", "https://gateway.test/ñ", "embeddings", "https://gateway.test/%C3%B1/embeddings"},
		{"ipv6-zone", "http://[fe80::1%25An%20Interface]:65535/a", "embeddings", "http://[fe80::1%25An%20Interface]:65535/a/embeddings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := Parse("anthropic", tc.base)
			if err != nil {
				t.Fatal(err)
			}
			wantBase := strings.TrimSuffix(tc.want, tc.suffix)
			if base.String() != wantBase || Endpoint(base, tc.suffix) != tc.want || base.String() != wantBase {
				t.Fatalf("base=%q endpoint=%q, want base=%q endpoint=%q", base, Endpoint(base, tc.suffix), wantBase, tc.want)
			}
			if decoded, err := url.PathUnescape(base.RawPath); err != nil || decoded != base.Path {
				t.Fatalf("inconsistent escaped and decoded paths: %q / %q", base.RawPath, base.Path)
			}
		})
	}
}

func TestParseRejectsInvalidConfigurationsWithSafeError(t *testing.T) {
	for _, base := range []string{
		"fragment-canary#", "http://gateway.test/private-canary#fragment", "http://gateway.test/private-canary?", "http://gateway.test/?token=private-canary",
		"http://private-canary@gateway.test/", "http://gateway.test/private-canary path", "http://gateway.test/\u00a0", "http://[private-canary",
		"relative-canary", "//gateway.test/canary", "ftp://gateway.test/canary", "http:opaque-canary", "http:/missing-host", "http://:123/canary",
		"http://gateway.test:/canary", "http://[::1]:/canary", "http://gateway.test:0/canary", "http://gateway.test:65536/canary",
		"http://gateway.test:+12/canary", "http://gateway.test:-12/canary", "http://gateway.test:1x/canary", "http://gate[way]/canary",
		"http://[not-ipv6]:22/canary", "http://[127.0.0.1]/canary", "http://::1/canary", "http://gateway.test/%2/canary",
		"http://gateway.test/./canary", "http://gateway.test/../canary", "http://gateway.test/%2e/canary", "http://gateway.test/.%2E/canary",
	} {
		parsed, err := Parse("voyage", base)
		var typed *ConfigError
		if parsed != nil || !errors.As(err, &typed) || typed.Provider != "voyage" || typed.Field != "VoyageBaseURL" || typed.Reason == "" {
			t.Errorf("Parse(%q) = (%v, %v)", base, parsed, err)
			continue
		}
		if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "gateway.test") {
			t.Errorf("unsafe error: %s", err)
		}
	}
}

func TestAuthorityReturnsEffectivePortAndPreservesHost(t *testing.T) {
	for _, tc := range []struct {
		scheme, input, host string
		port                uint16
	}{
		{"http", "I.example.test", "I.example.test", 80}, {"https", "İ.example.test", "İ.example.test", 443},
		{"http", "example.test:1", "example.test", 1}, {"http", "example.test:65535", "example.test", 65535},
		{"https", "[0:0:0:0:0:0:0:1]", "0:0:0:0:0:0:0:1", 443},
		{"https", "[fe80::1%Wi Fi]:0443", "fe80::1%Wi Fi", 443},
	} {
		if host, port, reason := Authority(tc.scheme, tc.input); host != tc.host || port != tc.port || reason != "" {
			t.Errorf("Authority(%q) = (%q, %d, %q), want (%q, %d)", tc.input, host, port, reason, tc.host, tc.port)
		}
	}
}
