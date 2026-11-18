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

// These configurations use the two existing public method sets unchanged.
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
	cfg := anthropicCfg()
	explicitEmpty := configWithOptionalBaseURL{testConsolidateCfg: cfg}
	ctx := context.Background()
	for _, test := range []struct {
		label  string
		config interface {
			ExtractConfig
			ConsolidateConfig
		}
	}{{"legacy", cfg}, {"empty-getter", explicitEmpty}} {
		t.Run(test.label, func(t *testing.T) {
			const sentinel = "Synthetic legacy extraction"
			for i := 0; i < 2; i++ {
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
				if want, err := consolidateViaAnthropic(ctx, "synthetic input", test.config); err != nil || want != "[\""+sentinel+"\"]" {
					t.Fatalf("unexpected consolidation result: %q, %v", want, err)
				}
				if calls.Load() != 2 {
					t.Fatalf("current gateway got %d calls; want both operations", calls.Load())
				}
			}
		})
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://%zz/")
	var typed *providerurl.ConfigError
	if _, err := ExtractFactsWithProvenance(ctx, "synthetic input", cfg); !errors.As(err, &typed) {
		t.Fatalf("legacy malformed environment was not rejected before extraction: %v", err)
	}
	var callbacks int
	store := openHookedStore(t, func(_ string, err error) {
		callbacks++
		if !errors.As(err, &typed) {
			t.Errorf("legacy consolidation callback received %v", err)
		}
	})
	seedBatch(t, store, "audit")
	if err := store.Consolidate(ctx, "audit", cfg); err != nil {
		t.Fatalf("legacy consolidation became fatal: %v", err)
	}
	if callbacks != 1 {
		t.Fatalf("got %d consolidation callbacks, want one", callbacks)
	}
}
