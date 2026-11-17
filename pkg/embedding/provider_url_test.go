package embedding

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/angelnicolasc/graymatter/internal/providerhttp"
	"github.com/angelnicolasc/graymatter/internal/providerurl"
)

type initializedProvider interface {
	Provider
	InitializationError() error
}

func TestConstructorsCaptureBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name, variable string
		create         func() initializedProvider
	}{
		{"openai", "OPENAI_BASE_URL", func() initializedProvider { return NewOpenAI(Config{OpenAIAPIKey: "fixture-key"}) }},
		{"voyage", "VOYAGE_BASE_URL", func() initializedProvider { return NewVoyage(Config{VoyageAPIKey: "fixture-key"}) }},
		{"legacy-anthropic-alias", "VOYAGE_BASE_URL", func() initializedProvider { return NewAnthropic(Config{VoyageAPIKey: "fixture-key"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.EscapedPath() != "/embedded%23/embeddings" ||
					r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
				}
				calls.Add(1)
				_, _ = fmt.Fprint(w, `{"data":[{"embedding":[1,0]}]}`)
			}))
			defer gateway.Close()

			t.Setenv(tc.variable, "http://%zz/")
			invalid := tc.create()
			var typed *providerurl.ConfigError
			if !errors.As(invalid.InitializationError(), &typed) {
				t.Fatalf("bad URL did not initialize with typed error: %v", invalid.InitializationError())
			}

			t.Setenv(tc.variable, gateway.URL+"/embedded%23")
			valid := tc.create()
			if err := valid.InitializationError(); err != nil {
				t.Fatal(err)
			}
			if _, err := invalid.Embed(context.Background(), "synthetic invalid case"); err != invalid.InitializationError() {
				t.Fatalf("Embed lost initialization error: %v", err)
			}

			t.Setenv(tc.variable, "http://%zz/")
			vector, err := valid.Embed(context.Background(), "synthetic valid case")
			if err != nil || !reflect.DeepEqual(vector, []float32{1, 0}) || calls.Load() != 1 {
				t.Fatalf("captured provider returned vector=%v calls=%d err=%v", vector, calls.Load(), err)
			}
		})
	}
}

func TestEmbeddingRequestsBlockCrossOriginRedirect(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "http://%zz/")
	t.Setenv("VOYAGE_BASE_URL", "http://%zz/")
	for _, tc := range []struct {
		name        string
		newProvider func(string) Provider
	}{
		{"openai", func(base string) Provider { return NewOpenAI(Config{OpenAIAPIKey: "fixture-key", OpenAIBaseURL: base}) }},
		{"voyage", func(base string) Provider { return NewVoyage(Config{VoyageAPIKey: "fixture-key", VoyageBaseURL: base}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var aCalls, bCalls atomic.Int32
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				bCalls.Add(1)
				w.WriteHeader(400)
			}))
			defer b.Close()
			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				aCalls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/v1/embeddings" ||
					r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Error("initial request did not match expected embeddings request")
				}
				w.Header().Set("Location", b.URL+"/gateway-location-canary")
				w.Header().Set("X-Should-Retry", "true")
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer a.Close()
			p := tc.newProvider(a.URL + "/v1")
			_, err := p.Embed(context.Background(), "synthetic-private-prompt")
			var blocked *providerhttp.RedirectBlockedError
			if !errors.As(err, &blocked) || strings.Contains(err.Error(), "gateway-location-canary") {
				t.Fatalf("unsafe or missing redirect error: %v", err)
			}
			if aCalls.Load() != 1 || bCalls.Load() != 0 {
				t.Fatalf("A calls=%d, B calls=%d", aCalls.Load(), bCalls.Load())
			}
		})
	}
}
