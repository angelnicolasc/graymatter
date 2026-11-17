package providerhttp

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type observedBody struct {
	*strings.Reader
	closes int
}

func (b *observedBody) Close() error {
	b.closes++
	return nil
}

func TestRedirectBodyOwnership(t *testing.T) {
	const (
		origin   = "https://a.example.test"
		rejected = "https://b.example.test"
		payload  = "synthetic provider payload"
		result   = "synthetic provider response"
	)
	veto := errors.New("callback canary")
	type redirectCase struct {
		name     string
		location string
		status   int
		callback error
		reason   string
		sends    int
		control  bool
	}
	cases := []redirectCase{
		{"R01-veto-307-net-http", "/next", 307, veto, "", 1, true},
		{"R01-veto-307-wrapper", "/next", 307, veto, "callback_veto", 1, false},
		{"R02-veto-308", "/next", 308, veto, "callback_veto", 1, false},
		{"R02-origin-307", rejected, 307, nil, "origin", 1, false},
		{"R02-origin-308", rejected, 308, nil, "origin", 1, false},
		{"R02-limit-307", "/loop", 307, nil, "limit", 10, false},
		{"R02-limit-308", "/loop", 308, nil, "limit", 10, false},
		{"R03-last-response", "/next", 307, http.ErrUseLastResponse, "callback_veto", 1, false},
		{"R04-allow-307-net-http", "/next", 307, nil, "", 2, true},
		{"R04-allow-307-wrapper", "/next", 307, nil, "", 2, false},
		{"R04-allow-308-net-http", "/next", 308, nil, "", 2, true},
		{"R04-allow-308-wrapper", "/next", 308, nil, "", 2, false},
		{"R05-terminal-307-net-http", "", 307, nil, "", 1, true},
		{"R05-terminal-308-net-http", "", 308, nil, "", 1, true},
		{"R05-terminal-307", "", 307, nil, "terminal_status", 1, false},
		{"R05-terminal-308", "", 308, nil, "terminal_status", 1, false},
		{"R05-terminal-300", "", 300, nil, "terminal_status", 1, false},
		{"R05-terminal-304", "", 304, nil, "terminal_status", 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests, responses []*observedBody
			body := func() (io.ReadCloser, error) {
				b := &observedBody{Reader: strings.NewReader(payload)}
				requests = append(requests, b)
				return b, nil
			}
			req, err := http.NewRequest(http.MethodPost, origin+"/start", strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			req.Body, _ = body()
			req.GetBody = body
			req.Header.Set("Authorization", "Bearer synthetic-authorization")
			req.Header.Set("X-Api-Key", "synthetic-key")
			var paths []string
			base := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.String())
				content, readErr := io.ReadAll(r.Body)
				closeErr := r.Body.Close()
				if readErr != nil || closeErr != nil || string(content) != payload ||
					r.Method != http.MethodPost || r.URL.Host != "a.example.test" ||
					r.Header.Get("Authorization") != "Bearer synthetic-authorization" ||
					r.Header.Get("X-Api-Key") != "synthetic-key" {
					t.Errorf("unexpected request %s %s, payload %q, read error %v, close error %v", r.Method, r.URL, content, readErr, closeErr)
				}
				status, headers := tc.status, make(http.Header)
				if r.URL.Path == "/next" {
					status = http.StatusOK
				} else if tc.location != "" {
					headers.Set("Location", tc.location)
				}
				b := &observedBody{Reader: strings.NewReader(result)}
				responses = append(responses, b)
				return &http.Response{StatusCode: status, Header: headers, Body: b, Request: r, ContentLength: int64(len(result))}, nil
			})}
			if tc.callback != nil {
				base.CheckRedirect = func(*http.Request, []*http.Request) error { return tc.callback }
			}
			var client interface {
				Do(*http.Request) (*http.Response, error)
			} = base
			if !tc.control {
				parsed, parseErr := url.Parse(origin)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				client = New("anthropic", parsed, base)
			}

			response, err := client.Do(req)
			switch {
			case tc.reason != "":
				var blocked *RedirectBlockedError
				if !errors.As(err, &blocked) || blocked.Reason != tc.reason || strings.Contains(err.Error(), "callback canary") {
					t.Errorf("expected safe redirect error %q, got %v", tc.reason, err)
				}
				if response == nil || response.Body != http.NoBody {
					t.Errorf("expected closed response with preserved metadata, got %v", response)
				}
			case tc.callback != nil:
				if !errors.Is(err, tc.callback) {
					t.Errorf("expected veto from plain net/http, got %v", err)
				}
			case tc.control && tc.location == "":
				if err != nil || response == nil || response.StatusCode != tc.status {
					t.Fatalf("expected untouched response %d, got %v, %v", tc.status, response, err)
				}
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
			default:
				if err != nil || response == nil || response.StatusCode != http.StatusOK {
					t.Fatalf("expected successful response, got %v, %v", response, err)
				}
				content, readErr := io.ReadAll(response.Body)
				closeErr := response.Body.Close()
				if readErr != nil || closeErr != nil || string(content) != result {
					t.Errorf("unexpected final response %q, read error %v, close error %v", content, readErr, closeErr)
				}
			}
			if len(paths) != tc.sends || len(responses) != tc.sends {
				t.Errorf("expected %d dispatches to A, got %v", tc.sends, paths)
			}
			reconstructedRejected := tc.location != "" && (tc.callback != nil || tc.reason != "")
			wantBodies := tc.sends
			if reconstructedRejected {
				wantBodies++
			}
			if len(requests) != wantBodies {
				t.Errorf("created %d request bodies, want %d", len(requests), wantBodies)
			}
			for i, b := range requests {
				want := 1
				if i < wantBodies-1 || (tc.location == "" && (tc.status == 307 || tc.status == 308)) {
					want = 2 // The accepted transport call and net/http's redirect cleanup.
				}
				t.Logf("request %d closes=%d want=%d", i, b.closes, want)
				if b.closes != want {
					t.Errorf("request %d closed %d times, want %d", i, b.closes, want)
				}
			}
			for i, b := range responses {
				if b.closes != 1 {
					t.Errorf("response %d closed %d times, want 1", i, b.closes)
				}
			}
		})
	}
}
