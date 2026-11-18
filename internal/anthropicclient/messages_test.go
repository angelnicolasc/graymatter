package anthropicclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/angelnicolasc/graymatter/internal/providerhttp"
)

const fixtureMessage = `{"id":"msg_test","type":"message","role":"assistant","model":"claude-fixture","content":[{"type":"text","text":"synthetic Messages result"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`

func fixtureParams() anthropic.MessageNewParams {
	return anthropic.MessageNewParams{
		Model: "claude-fixture", MaxTokens: 512,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("synthetic fixture prompt"))},
	}
}

func TestMessagesMatchSDKHeadersAndBody(t *testing.T) {
	type wireRequest struct {
		method, path, body string
		header             http.Header
	}
	observations := make(chan wireRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read Messages request: %v", err)
		}
		select {
		case observations <- wireRequest{r.Method, r.URL.EscapedPath(), string(body), r.Header.Clone()}:
		default:
			t.Error("unexpected extra Messages request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, fixtureMessage)
	}))
	defer server.Close()

	for _, test := range []struct {
		name, ambientKey, token, override, expectedAPIKey, expectedBearer string
	}{
		{"no-auth", "", "", "", "", ""},
		{"ambient-key", "fixture-ambient-key", "", "", "fixture-ambient-key", ""},
		{"ambient-bearer", "", "fixture-token", "", "", "Bearer fixture-token"},
		{"both-plus-key-override", "fixture-ambient-key", "fixture-token", "fixture-explicit-key", "fixture-explicit-key", "Bearer fixture-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			setSDKCredential(t, "ANTHROPIC_API_KEY", test.ambientKey)
			setSDKCredential(t, "ANTHROPIC_AUTH_TOKEN", test.token)
			t.Setenv("ANTHROPIC_BASE_URL", server.URL+"/gateway")
			opts := []option.RequestOption{option.WithHTTPClient(server.Client())}
			if test.override != "" {
				opts = append(opts, option.WithAPIKey(test.override))
			}
			before := anthropic.NewClient(opts...)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := before.Messages.New(ctx, fixtureParams()); err != nil {
				t.Fatal(err)
			}

			t.Setenv("ANTHROPIC_BASE_URL", "http://%zz/")
			service, err := newMessages(server.URL+"/gateway", test.override, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.New(ctx, fixtureParams())
			if err != nil || result == nil || len(result.Content) != 1 || result.Content[0].Text != "synthetic Messages result" {
				t.Fatalf("Messages response not consumed: %+v %v", result, err)
			}
			want, got := <-observations, <-observations
			if got.method != "POST" || got.path != "/gateway/v1/messages" || got.method != want.method || got.path != want.path || got.body != want.body {
				t.Fatalf("wire request changed: want %s %s %s; got %s %s %s", want.method, want.path, want.body, got.method, got.path, got.body)
			}
			if got.header.Get("X-Api-Key") != test.expectedAPIKey || got.header.Get("Authorization") != test.expectedBearer {
				t.Error("authentication differed from the SDK contract")
			}
			for key, values := range want.header {
				if !reflect.DeepEqual(values, got.header.Values(key)) {
					t.Errorf("headers differ for %s", key)
				}
			}
			if len(want.header) != len(got.header) {
				t.Errorf("header counts differ: before=%d after=%d", len(want.header), len(got.header))
			}
		})
	}
}

func setSDKCredential(t *testing.T, variable, value string) {
	t.Helper()
	t.Setenv(variable, value)
	if value == "" {
		if err := os.Unsetenv(variable); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMessagesPreserveSDKRetryPolicy(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	for _, tc := range []struct{ status, wantCalls int }{{401, 1}, {429, 3}, {500, 3}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			var calls atomic.Int32
			var firstBody atomic.Pointer[string]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				body := string(raw)
				firstBody.CompareAndSwap(nil, &body)
				if body == "" || body != *firstBody.Load() || r.Method != "POST" || r.URL.Path != "/v1/messages" {
					t.Error("retry changed the Messages request")
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After-Ms", "1")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"synthetic error"}}`)
			}))
			defer server.Close()
			service, err := NewMessages(server.URL, "fixture-key")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := service.New(ctx, fixtureParams()); err == nil {
				t.Fatal("expected SDK HTTP error")
			}
			if n := int(calls.Load()); n != tc.wantCalls {
				t.Fatalf("got %d requests, want %d", n, tc.wantCalls)
			}
		})
	}
}

func TestMessagesBlockCrossOriginWithSDKRetryPolicy(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "synthetic-token")

	for _, tc := range []struct {
		retryHeader string
		wantCalls   int32
	}{{"false", 1}, {"true", 3}} {
		t.Run(tc.retryHeader, func(t *testing.T) {
			var firstCalls, secondCalls atomic.Int32
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondCalls.Add(1)
				http.Error(w, "received unintended cross-origin request", http.StatusBadRequest)
			}))
			defer second.Close()
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				firstCalls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" ||
					r.Header.Get("X-Api-Key") != "synthetic-key" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Error("initial Messages request or auth changed")
				}
				w.Header().Set("Location", second.URL+"/sensitive-gateway-canary")
				w.Header().Set("X-Should-Retry", tc.retryHeader)
				w.Header().Set("Retry-After-Ms", "1")
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer first.Close()
			svc, err := NewMessages(first.URL, "synthetic-key")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err = svc.New(ctx, fixtureParams())
			var blocked *providerhttp.RedirectBlockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("wrong redirect error type: %v", err)
			}
			for _, secret := range []string{"sensitive-gateway-canary", "synthetic-key", "synthetic-token", "synthetic fixture prompt"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("redirect error exposed fixture secret %q", secret)
				}
			}
			if firstCalls.Load() != tc.wantCalls || secondCalls.Load() != 0 {
				t.Fatalf("requests to initial=%d, want %d; to other origin=%d", firstCalls.Load(), tc.wantCalls, secondCalls.Load())
			}
		})
	}
}

func TestMessagesPreserveRequestCancellation(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
	}))
	defer server.Close()
	defer close(release)
	service, err := newMessages(server.URL, "fixture-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := service.New(ctx, fixtureParams())
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("fixture did not receive Messages request")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Messages call remained blocked after cancellation")
	}
}
