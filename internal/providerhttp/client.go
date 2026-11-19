// Package providerhttp confines authenticated provider requests to their origin.
package providerhttp

import (
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/angelnicolasc/graymatter/internal/providerurl"
)

const (
	RedirectBlockedCode = "provider_redirect_blocked"
	TransportErrorCode  = "provider_transport_error"
)

// RedirectBlockedError reports a request rejected by the origin or redirect
// policy. Error deliberately omits the URL, Location and the underlying cause.
type RedirectBlockedError struct {
	Provider string
	Reason   string
	cause    error
}

func (e *RedirectBlockedError) Error() string {
	return RedirectBlockedCode + ": provider=" + e.Provider + " reason=" + e.Reason
}

func (e *RedirectBlockedError) Unwrap() error { return e.cause }

// TransportError reports a network or HTTP client error without printing the
// request URL or the raw error. The cause remains available to Go callers.
type TransportError struct {
	Provider string
	cause    error
}

func (e *TransportError) Error() string { return TransportErrorCode + ": provider=" + e.Provider }
func (e *TransportError) Unwrap() error { return e.cause }

type origin struct {
	scheme string
	host   string
	port   uint16
}

// Client sends POST requests only to the configured HTTP origin. It preserves
// the template's transport, jar and timeout. An existing CheckRedirect can
// veto a hop using metadata copies, but cannot edit the dispatched request.
type Client struct {
	provider string
	origin   origin
	initErr  string
	template http.Client
}

// New creates a private client template for a base URL validated by providerurl.
func New(provider string, base *url.URL, template *http.Client) *Client {
	if template == nil {
		template = http.DefaultClient
	}
	switch provider {
	case "anthropic", "openai", "voyage":
	default:
		provider = "unknown"
	}
	selected, reason := urlOrigin(base)
	return &Client{provider: provider, origin: selected, initErr: reason, template: *template}
}

// Do preserves the redirect response's status and headers for retry decisions
// when a hop is blocked. A body already closed by the client is not passed on.
func (c *Client) Do(request *http.Request) (*http.Response, error) {
	reason := c.initErr
	if reason == "" {
		reason = c.validateRequest(request)
	}
	if reason == "" && request.GetBody == nil {
		reason = "request_framing"
	}
	if reason != "" {
		closeRequestBody(request)
		return nil, c.blocked(reason, nil)
	}

	auth := captureAuth(request.Header)
	client := c.template
	previous := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		return c.checkRedirect(next, via, auth, previous)
	}
	resp, err := client.Do(request)
	if err != nil {
		var blocked *RedirectBlockedError
		if errors.As(err, &blocked) {
			return withoutBody(resp), c.blocked(blocked.Reason, err)
		}
		return withoutBody(resp), &TransportError{Provider: c.provider, cause: err}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_ = resp.Body.Close()
		return withoutBody(resp), c.blocked("terminal_status", nil)
	}
	return resp, nil
}

func (c *Client) checkRedirect(next *http.Request, via []*http.Request, auth apiAuth, previous func(*http.Request, []*http.Request) error) error {
	if reason := c.validateRedirect(next, via); reason != "" {
		return c.blocked(reason, nil)
	}
	auth.restore(next.Header)
	if previous == nil {
		return nil
	}
	history := make([]*http.Request, len(via))
	for i, entry := range via {
		history[i] = observeRequest(entry)
	}
	if err := previous(observeRequest(next), history); err != nil {
		return c.blocked("callback_veto", err)
	}
	if reason := c.validateRedirect(next, via); reason != "" {
		return c.blocked(reason, nil)
	}
	auth.restore(next.Header)
	return nil
}

func (c *Client) validateRedirect(next *http.Request, via []*http.Request) string {
	if len(via) >= 10 {
		return "limit"
	}
	if next.Response == nil || (next.Response.StatusCode != http.StatusTemporaryRedirect && next.Response.StatusCode != http.StatusPermanentRedirect) {
		return "status"
	}
	if strings.Contains(next.Response.Header.Get("Location"), "#") {
		return "fragment"
	}
	if next.ContentLength != via[0].ContentLength {
		return "request_framing"
	}
	return c.validateRequest(next)
}

func (c *Client) validateRequest(request *http.Request) string {
	if request == nil || request.Method != http.MethodPost || request.RequestURI != "" {
		return "request_shape"
	}
	// Provider calls always send buffered JSON. Confirm that the candidate has
	// the same fixed-length framing before we allow net/http to dispatch it.
	if request.Body == nil || request.Body == http.NoBody || request.ContentLength <= 0 || len(request.TransferEncoding) != 0 || len(request.Trailer) != 0 {
		return "request_framing"
	}
	actual, reason := urlOrigin(request.URL)
	if reason != "" {
		return reason
	}
	if actual != c.origin {
		return "origin"
	}
	if !sameAuthority(actual, request.Host) {
		return "host_override"
	}
	for key, values := range request.Header {
		if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Transfer-Encoding") || strings.EqualFold(key, "Trailer") {
			return "request_framing"
		}
		if strings.EqualFold(key, "Host") {
			for _, value := range values {
				if !sameAuthority(actual, value) {
					return "host_override"
				}
			}
		}
	}
	return ""
}

func urlOrigin(u *url.URL) (origin, string) {
	if u == nil || u.Opaque != "" || u.OmitHost || u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return origin{}, "url_shape"
	}
	scheme := foldASCII(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return origin{}, "scheme"
	}
	if u.RawPath != "" && u.EscapedPath() != u.RawPath {
		return origin{}, "raw_path"
	}
	return authorityOrigin(scheme, u.Host)
}

func authorityOrigin(scheme, authority string) (origin, string) {
	host, port, reason := providerurl.Authority(scheme, authority)
	if reason != "" {
		return origin{}, reason
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.String()
	} else {
		host = foldASCII(host)
	}
	return origin{scheme: scheme, host: host, port: port}, ""
}

func sameAuthority(selected origin, authority string) bool {
	if authority == "" {
		return true
	}
	candidate, reason := authorityOrigin(selected.scheme, authority)
	return reason == "" && candidate == selected
}

// foldASCII does not equate Unicode spellings that might dial different origins.
func foldASCII(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			if out == nil {
				out = []byte(s)
			}
			out[i] = s[i] + ('a' - 'A')
		}
	}
	if out == nil {
		return s
	}
	return string(out)
}

func closeRequestBody(request *http.Request) {
	if request != nil && request.Body != nil {
		_ = request.Body.Close()
	}
}

func withoutBody(response *http.Response) *http.Response {
	if response == nil {
		return nil
	}
	copy := *response
	copy.Body = http.NoBody
	return &copy
}

func (c *Client) blocked(reason string, cause error) *RedirectBlockedError {
	return &RedirectBlockedError{Provider: c.provider, Reason: reason, cause: cause}
}

var authNames = [...]string{"Authorization", "X-Api-Key"}

type authHeader struct {
	present bool
	values  []string
}

type apiAuth [len(authNames)]authHeader

func captureAuth(header http.Header) apiAuth {
	var auth apiAuth
	for i, name := range authNames {
		var keys []string
		for key := range header {
			if strings.EqualFold(key, name) {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		auth[i].present = len(keys) != 0
		for _, key := range keys {
			auth[i].values = append(auth[i].values, header[key]...)
		}
	}
	return auth
}

func (auth apiAuth) restore(header http.Header) {
	for i, name := range authNames {
		for key := range header {
			if strings.EqualFold(key, name) {
				delete(header, key)
			}
		}
		if auth[i].present {
			header[name] = slices.Clone(auth[i].values)
		}
	}
}

func observeRequest(request *http.Request) *http.Request {
	view := &http.Request{
		Method: request.Method, Host: request.Host, ContentLength: request.ContentLength,
		Header: request.Header.Clone(), Body: http.NoBody,
	}
	if u := request.URL; u != nil {
		view.URL = &url.URL{
			Scheme: u.Scheme, Opaque: u.Opaque, Host: u.Host, Path: u.Path, RawPath: u.RawPath,
			ForceQuery: u.ForceQuery, RawQuery: u.RawQuery, Fragment: u.Fragment, RawFragment: u.RawFragment, OmitHost: u.OmitHost,
		}
	}
	if resp := request.Response; resp != nil {
		view.Response = &http.Response{
			Status: resp.Status, StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: http.NoBody,
		}
	}
	return view
}
