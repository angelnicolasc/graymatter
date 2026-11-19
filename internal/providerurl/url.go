// Package providerurl selects and validates base URLs for provider clients.
package providerurl

import (
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	AnthropicEnv = "ANTHROPIC_BASE_URL"
	OpenAIEnv    = "OPENAI_BASE_URL"
	VoyageEnv    = "VOYAGE_BASE_URL"

	AnthropicDefault = "https://api.anthropic.com"
	OpenAIDefault    = "https://api.openai.com/v1"
	VoyageDefault    = "https://api.voyageai.com/v1"

	Code = "invalid_provider_base_url"
)

// ConfigError reports a rejected effective base URL without exposing its value.
type ConfigError struct {
	Provider string
	Field    string
	Reason   string
}

func (e *ConfigError) Error() string {
	return Code + ": provider=" + e.Provider + " field=" + e.Field + " reason=" + e.Reason
}

// Select returns the explicit value, the environment value, or the fallback.
// Selection does not parse the result, so callers can defer validation until
// they know the provider will be used.
func Select(explicit, envKey, fallback string) string {
	if explicit != "" {
		return explicit
	}
	if value := os.Getenv(envKey); value != "" {
		return value
	}
	return fallback
}

// Parse validates an effective base URL and normalizes it as a directory URL.
func Parse(provider, base string) (*url.URL, error) {
	u, reason := parseBase(base)
	if reason == "" {
		return u, nil
	}
	field := "base URL"
	switch provider {
	case "anthropic":
		field = "AnthropicBaseURL"
	case "openai":
		field = "OpenAIBaseURL"
	case "voyage":
		field = "VoyageBaseURL"
	default:
		provider = "unknown"
	}
	return nil, &ConfigError{Provider: provider, Field: field, Reason: reason}
}

func parseBase(base string) (*url.URL, string) {
	if strings.Contains(base, "#") {
		return nil, "fragment"
	}
	if strings.IndexFunc(base, unicode.IsSpace) >= 0 {
		return nil, "whitespace"
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, "syntax"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "scheme"
	}
	if u.Opaque != "" || u.OmitHost {
		return nil, "url_shape"
	}
	if u.User != nil {
		return nil, "userinfo"
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, "query"
	}
	if _, _, reason := Authority(u.Scheme, u.Host); reason != "" {
		return nil, reason
	}
	path := u.EscapedPath()
	for _, segment := range strings.Split(path, "/") {
		decoded, _ := url.PathUnescape(segment) // EscapedPath always returns valid escapes.
		if decoded == "." || decoded == ".." {
			return nil, "dot_segment"
		}
	}
	u.RawPath = strings.TrimRight(path, "/") + "/"
	u.Path, _ = url.PathUnescape(u.RawPath)
	return u, ""
}

// Endpoint appends a literal relative endpoint suffix to a directory from Parse.
// suffix is a provider-owned path such as "embeddings" or "v1/messages".
func Endpoint(base *url.URL, suffix string) string {
	u := *base
	u.RawPath = base.EscapedPath() + suffix
	u.Path, _ = url.PathUnescape(u.RawPath)
	return u.String()
}

// Authority validates a decoded URL authority, returning the original hostname,
// effective port and, on failure, a fixed reason. It does not resolve the host.
func Authority(scheme, authority string) (host string, port uint16, reason string) {
	if authority == "" || !utf8.ValidString(authority) {
		return "", 0, "authority"
	}
	host, portSuffix := authority, ""
	if strings.HasPrefix(authority, "[") {
		end := strings.LastIndexByte(authority, ']')
		if end < 0 {
			return "", 0, "authority"
		}
		host, portSuffix = authority[1:end], authority[end+1:]
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() {
			return "", 0, "authority"
		}
	} else if index := strings.IndexByte(authority, ':'); index >= 0 {
		host, portSuffix = authority[:index], authority[index:]
		if strings.Contains(portSuffix[1:], ":") {
			return "", 0, "authority"
		}
	}
	if host == "" {
		return "", 0, "authority"
	}
	port = 80
	if scheme == "https" {
		port = 443
	}
	if portSuffix != "" {
		var ok bool
		port, ok = explicitPort(portSuffix)
		if !ok {
			return "", 0, "port"
		}
	}
	// This function also accepts Request.Host, which might have been assigned
	// directly. Require a valid authority when encoded as a URL, rather than
	// interpreting delimiters or percent escapes in it as another component.
	wire := (&url.URL{Scheme: scheme, Host: authority}).String()
	parsed, err := url.Parse(wire)
	if err != nil || parsed.Host != authority || parsed.User != nil || parsed.Path != "" || parsed.Fragment != "" || parsed.RawQuery != "" {
		return "", 0, "authority"
	}
	if !strings.HasPrefix(authority, "[") && strings.ContainsAny(host, "[]") {
		return "", 0, "authority"
	}
	return host, port, ""
}

func explicitPort(suffix string) (uint16, bool) {
	if !strings.HasPrefix(suffix, ":") || len(suffix) == 1 {
		return 0, false
	}
	for index := 1; index < len(suffix); index++ {
		if suffix[index] < '0' || suffix[index] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseUint(suffix[1:], 10, 16)
	return uint16(value), err == nil && value != 0
}
