package memory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/angelnicolasc/graymatter/internal/providerurl"
)

// Existing configurations keep their method sets.
var (
	_ ExtractConfig     = (*testConsolidateCfg)(nil)
	_ ConsolidateConfig = (*testConsolidateCfg)(nil)
)

type configWithOptionalBaseURL struct {
	*testConsolidateCfg
	base string
}

func (c configWithOptionalBaseURL) GetAnthropicBaseURL() string { return c.base }

func TestLegacyAnthropicConfigResolvesEnvironmentPerOperation(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	cfg := &testConsolidateCfg{apiKey: "fake-extraction-key", model: "claude-test"}
	explicitEmpty := configWithOptionalBaseURL{testConsolidateCfg: cfg}
	ctx := context.Background()
	for _, test := range []struct {
		label  string
		config ExtractConfig
	}{{"legacy", cfg}, {"empty-getter", explicitEmpty}} {
		t.Run(test.label, func(t *testing.T) {
			const sentinel = "Synthetic legacy extraction"
			for range 2 {
				var calls atomic.Int32
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != http.MethodPost || r.URL.Path != "/legacy/v1/messages" {
						t.Errorf("unexpected Messages request %s %s", r.Method, r.URL.Path)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, anthropicTextMessage("[\""+sentinel+"\"]"))
				}))
				defer gateway.Close()
				t.Setenv("ANTHROPIC_BASE_URL", gateway.URL+"/legacy")
				result, err := ExtractFactsWithProvenance(ctx, "synthetic input", test.config)
				if err != nil || result.Source != ExtractionLLM || !reflect.DeepEqual(result.Facts, []string{sentinel}) {
					t.Fatalf("unexpected extraction result: %+v, %v", result, err)
				}
				if calls.Load() != 1 {
					t.Fatalf("current gateway got %d calls; want one extraction", calls.Load())
				}
			}
		})
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://%zz/")
	var typed *providerurl.ConfigError
	if _, err := ExtractFactsWithProvenance(ctx, "synthetic input", cfg); !errors.As(err, &typed) {
		t.Fatalf("legacy malformed environment was not rejected before extraction: %v", err)
	}
}
