package embedding

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// endlessBody streams 'x' forever, the way a hostile or broken endpoint can.
type endlessBody struct{}

func (endlessBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestErrorBody_IsBounded is the H-15 regression test: the three HTTP
// providers read a non-200 body whole with io.ReadAll, so an endpoint that
// never stops sending exhausted the memory of whatever was embedding. The
// Ollama URL is user-configurable, so that endpoint is not always trusted.
func TestErrorBody_IsBounded(t *testing.T) {
	got := errorBody(endlessBody{})
	if len(got) != maxErrorBodyBytes {
		t.Errorf("errorBody read %d bytes from an endless reader, want %d",
			len(got), maxErrorBodyBytes)
	}
}

func TestErrorBody_KeepsShortMessages(t *testing.T) {
	const msg = `{"error":{"message":"invalid api key"}}`
	if got := errorBody(strings.NewReader(msg)); got != msg {
		t.Errorf("errorBody = %q, want %q", got, msg)
	}
}

func TestErrorBody_UnreadableBody(t *testing.T) {
	got := errorBody(errReader{errors.New("connection reset")})
	if !strings.Contains(got, "unreadable") {
		t.Errorf("errorBody = %q, want it to say the body could not be read", got)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// TestOllamaEmbed_BoundsHostileErrorBody drives the bound through the provider
// that actually talks to a user-configurable URL.
func TestOllamaEmbed_BoundsHostileErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		// Far more than the cap, and more than any real error message.
		chunk := strings.Repeat("x", 64<<10)
		for i := 0; i < 8; i++ {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	p := NewOllama(Config{OllamaURL: srv.URL, OllamaModel: "nomic-embed-text"})
	_, err := p.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("Embed against a 500 returned no error")
	}
	// The message carries at most the bounded prefix, plus the wrapper text.
	if len(err.Error()) > maxErrorBodyBytes+512 {
		t.Errorf("error message is %d bytes; the body read is not bounded", len(err.Error()))
	}
}
